# check=error=true

# renovate: datasource=github-tags depName=RsyncProject/rsync
ARG RSYNC_VERSION=v3.4.4
# When RSYNC_VERSION is bumped, update this SHA256 to match the new dist
# tarball. Renovate can't recompute it (github-tags exposes the git sha, not
# the tarball hash), so it labels the bump PR `manual-sha-bump` and puts these
# steps in the PR body. Verify the upstream signature FIRST, then hash, so the
# pin records authenticated bytes rather than trust-on-first-use: upstream
# publishes rsync-X.Y.Z.tar.gz.asc beside the tarball, and releases >= 3.4.0
# are signed by Andrew Tridgell <andrew@tridgell.net> (signer named on
# https://rsync.samba.org/download.html; key from https://keys.openpgp.org/,
# fingerprint 9FEF 112D CE19 A0DC 7E88 2CB8 1BB2 4997 A853 5F6F):
# V=<new tag>
# curl -sLO "https://download.samba.org/pub/rsync/rsync-${V#v}.tar.gz"
# curl -sLO "https://download.samba.org/pub/rsync/rsync-${V#v}.tar.gz.asc"
# curl -sL "https://keys.openpgp.org/vks/v1/by-email/andrew%40tridgell.net" \
#   | gpg --dearmor -o rsync-signing-key.gpg
# gpg --show-keys --with-fingerprint rsync-signing-key.gpg  # expect fpr above
# gpgv --keyring ./rsync-signing-key.gpg "rsync-${V#v}.tar.gz.asc" "rsync-${V#v}.tar.gz"
# sha256sum "rsync-${V#v}.tar.gz"   # paste the result here only after gpgv passes
ARG RSYNC_SHA256=bd88cf82fa653da32314fb229136407c5c90f80d1758d8f4b091767877d8fa96

FROM golang:1.26-trixie@sha256:117e07f49461abb984fc8aef661432461ff43d06faa22c3b73af6a49ce325cb9 AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /docker-rsync-scheduler .

# ---------------------------------------------------------------------------
# rsync builder stage - compiles rsync from the pinned upstream release
# tarball. Discarded at the end of the build; only the stripped binary
# reaches the runtime image below.
# ---------------------------------------------------------------------------
FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS rsync-builder

SHELL ["/bin/ash", "-eo", "pipefail", "-c"]

# Build deps are build-only (discarded with this stage, absent from the
# runtime image), so their exact versions never reach the shipped artifact
# and are intentionally left unpinned; they track whatever the Alpine 3.24
# repo serves at build time (the digest pins the base image, not the apk
# index). rsync itself stays version+SHA pinned below, it is the shipped
# artifact. The set mirrors Alpine 3.24-stable's rsync APKBUILD makedepends
# (acl/attr/lz4/popt/xxhash/zlib/zstd headers, linux-headers, perl) plus
# build-base for the toolchain.
# hadolint ignore=DL3018
RUN apk add --no-cache \
        acl-dev \
        attr-dev \
        build-base \
        linux-headers \
        lz4-dev \
        perl \
        popt-dev \
        xxhash-dev \
        zlib-dev \
        zstd-dev

ARG RSYNC_VERSION
ARG RSYNC_SHA256
WORKDIR /build/rsync
# Fetch the upstream dist tarball (stable release asset from the project's
# download server, NOT the auto-generated GitHub tag archive) and verify it
# fail-closed against the pinned SHA256 before extracting. Configure flags
# mirror Alpine 3.24-stable's rsync APKBUILD: ACL + xattr support, xxhash
# checksums, system popt and zlib (not the bundled copies), no md2man doc
# generation, and OpenSSL checksums disabled (the APKBUILD disables them
# since the xxhash family is faster); zstd and lz4 compression are enabled
# by configure's default detection of their -dev packages above. Omitted vs
# the APKBUILD: --build/--host (CI builds each arch natively, no
# cross-compile) and --with-rrsync (rrsync is a separate Alpine subpackage
# needing python3; this image never shipped it). LTO matches the APKBUILD's
# CFLAGS. The stripped binary is staged under /out for the runtime COPY.
RUN wget -q --tries=3 --timeout=30 \
      "https://download.samba.org/pub/rsync/rsync-${RSYNC_VERSION#v}.tar.gz" \
    && echo "${RSYNC_SHA256}  rsync-${RSYNC_VERSION#v}.tar.gz" | sha256sum -c - \
    && tar xzf "rsync-${RSYNC_VERSION#v}.tar.gz" --strip-components=1 --no-same-owner \
    && rm "rsync-${RSYNC_VERSION#v}.tar.gz" \
    && CFLAGS="-O2 -flto=auto" ./configure \
        --prefix=/usr \
        --sysconfdir=/etc \
        --mandir=/usr/share/man \
        --localstatedir=/var \
        --enable-acl-support \
        --enable-xattr-support \
        --enable-xxhash \
        --without-included-popt \
        --without-included-zlib \
        --disable-md2man \
        --disable-openssl \
    && make -j"$(nproc)" \
    && strip rsync \
    && install -D -m 755 rsync /out/usr/bin/rsync

# ---------------------------------------------------------------------------
# Runtime stage - same digest-pinned base as before the source-build
# conversion; only how rsync is obtained changed (COPY from the builder
# instead of installing the Alpine package).
# ---------------------------------------------------------------------------
FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS base

# No apk version pins: the digest-pinned base fixes the Alpine release line, so
# package-revision pins only strand the build on an Alpine release bump.
# apk upgrade is load-bearing: it floats forward base packages the pinned base
# pre-installs at an older, CVE-affected revision (libcrypto3/libssl3, etc.) —
# plain `apk add` leaves already-satisfied base packages unpatched.
# The floated package set is intentionally not version-pinned or asserted in-image;
# build-time package currency is verified by the advisory CI image scan (trivy/grype
# on the built image), not a build-time gate.
# The set is openssh-client (the ssh transport for rsync's -e option; stays an
# apk package by design) plus the shared libraries the built rsync links, per
# Alpine's rsync package depends: libacl.so.1 (acl-libs), liblz4.so.1
# (lz4-libs), libpopt.so.0 (popt), libxxhash.so.0 (libxxhash), libz.so.1
# (zlib), libzstd.so.1 (zstd-libs). The test stage's `rsync --version` run
# fails the build if one is missing or misnamed.
RUN apk upgrade --no-cache \
    && apk add --no-cache \
        acl-libs \
        libxxhash \
        lz4-libs \
        openssh-client \
        popt \
        zlib \
        zstd-libs

COPY --chmod=755 --from=rsync-builder /out/usr/bin/rsync /usr/bin/rsync
COPY --chmod=755 --from=go-builder /docker-rsync-scheduler /usr/local/bin/docker-rsync-scheduler

# ---------------------------------------------------------------------------
# Test stage - runs the build-time smoke test (the built rsync runs against
# the runtime stage's libraries, reports exactly the pinned RSYNC_VERSION,
# and kept feature parity with the Alpine package it replaced; ssh is on
# PATH). A failure here fails the centralized `ci / validate` docker build
# gate, because the final stage below depends on this stage's marker.
# ---------------------------------------------------------------------------
FROM base AS test
ARG RSYNC_VERSION
COPY tests/ /tmp/tests/
# ${RSYNC_VERSION:?} fails the build if the ARG wiring ever breaks, so the
# smoke test's exact-version assertion can never be skipped in-image (the
# leading v is stripped inside smoke.sh).
RUN RSYNC_EXPECTED_VERSION="${RSYNC_VERSION:?}" sh /tmp/tests/smoke.sh && touch /tests-passed

# ---------------------------------------------------------------------------
# Final stage - the runtime image. Must remain last so the CI build gate
# (which builds the default target) produces it; the marker COPY forces the
# test stage to build and pass first.
# ---------------------------------------------------------------------------
FROM base AS final
COPY --from=test /tests-passed /tests-passed

# Runs as root by design: the app must read host-owned source files (e.g. a
# host UID like 1000) across multiple bind mounts and write ssh known_hosts on
# first contact (StrictHostKeyChecking=accept-new). A fixed USER would break both.
# start-period absorbs the first built-in pass (the container is unhealthy until
# it completes). Size it to your slowest expected initial sync; override
# per-deploy via compose healthcheck.start_period. See README "Healthcheck".
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=120s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
ENTRYPOINT ["/usr/local/bin/docker-rsync-scheduler"]
CMD ["daemon"]
