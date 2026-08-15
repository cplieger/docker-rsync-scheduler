# check=error=true

# renovate: datasource=github-tags depName=RsyncProject/rsync
ARG RSYNC_VERSION=v3.5.0
# Renovate's github-tags datasource exposes the git sha, not the tarball hash,
# so the repin postUpgradeTask recomputes this SHA256 from the marker URL below
# and commits it in the same commit as the RSYNC_VERSION bump.
# Authenticity does NOT rest on whoever computed this hash: gpgv verifies the
# release's detached .asc inside the builder stage below, against the signing
# key committed beside this file. A recompute (bot or human) that adopted
# swapped bytes would fail that gate, which is why this pin can be automated
# at all — a hash refreshed from the same server that serves the tarball is
# trust-on-first-use on its own.
# The URL points at the src/ ARCHIVE directory. Its parent holds only the
# CURRENT release, so a pin there 404s the moment upstream publishes the next
# version; that is how the v3.4.4 pin stopped building.
# repin: dep=RsyncProject/rsync url=https://download.samba.org/pub/rsync/src/rsync-{version_nov}.tar.gz
ARG RSYNC_SHA256=c7ffd1ef653e99540f661e47cb00b7f9cad1ee6b972399b16f93d672656e0d33

FROM golang:1.26-trixie@sha256:87ffdb09b6a2e29ff910748b745395e8a0299aa80b7c0551cdca9b55e3fd2b3e AS go-builder
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
# build-base for the toolchain and gpgv for the release-signature check.
# hadolint ignore=DL3018
RUN apk add --no-cache \
        acl-dev \
        attr-dev \
        build-base \
        gpgv \
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
# Andrew Tridgell's rsync release signing key as a minimal dearmored keyring
# (v4 RSA-4096, created 2017-09-23, no expiry; fingerprint
# 9FEF112DCE19A0DC7E882CB81BB24997A8535F6F). https://rsync.samba.org/download.html
# names him as the signer of every release from 3.4.0 and points at
# keys.openpgp.org; the committed bytes are that export, cross-checked against
# keyserver.ubuntu.com (byte-identical primary key and subkey packets, which
# differ only in third-party signatures the keyservers each carry).
# Refresh (only if upstream ever rotates the key - verify the new fingerprint
# against multiple authoritative sources first):
# curl -sL "https://keys.openpgp.org/vks/v1/by-email/andrew%40tridgell.net" \
#   | gpg --dearmor > rsync-release.gpg
COPY rsync-release.gpg /usr/local/share/rsync-release.gpg
# Fetch the upstream dist tarball (stable release asset from the project's
# download server, NOT the auto-generated GitHub tag archive) plus its
# detached signature, then apply both gates fail-closed before extracting:
# gpgv authenticates the publisher against the committed keyring, and
# sha256sum -c freezes the exact bytes this repo reviewed. Order matters — a
# bad signature stops the build before the pin is consulted at all.
# Configure flags mirror Alpine 3.24-stable's rsync APKBUILD: ACL + xattr
# support, xxhash checksums, system popt and zlib (not the bundled copies),
# no md2man doc
# generation, and OpenSSL checksums disabled (the APKBUILD disables them
# since the xxhash family is faster); zstd and lz4 compression are enabled
# by configure's default detection of their -dev packages above. Omitted vs
# the APKBUILD: --build/--host (CI builds each arch natively, no
# cross-compile) and --with-rrsync (rrsync is a separate Alpine subpackage
# needing python3; this image never shipped it). LTO matches the APKBUILD's
# CFLAGS. The stripped binary is staged under /out for the runtime COPY.
RUN wget -q --tries=3 --timeout=30 \
      "https://download.samba.org/pub/rsync/src/rsync-${RSYNC_VERSION#v}.tar.gz" \
    && wget -q --tries=3 --timeout=30 \
      "https://download.samba.org/pub/rsync/src/rsync-${RSYNC_VERSION#v}.tar.gz.asc" \
    && gpgv --keyring /usr/local/share/rsync-release.gpg \
        "rsync-${RSYNC_VERSION#v}.tar.gz.asc" "rsync-${RSYNC_VERSION#v}.tar.gz" \
    && echo "${RSYNC_SHA256}  rsync-${RSYNC_VERSION#v}.tar.gz" | sha256sum -c - \
    && tar xzf "rsync-${RSYNC_VERSION#v}.tar.gz" --strip-components=1 --no-same-owner \
    && rm "rsync-${RSYNC_VERSION#v}.tar.gz" "rsync-${RSYNC_VERSION#v}.tar.gz.asc" \
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
# Embedded SBOM fragment. Syft inventories the final image from Alpine's APK
# database and Go buildinfo, so the source-built rsync is invisible to the
# signed release SBOM and to vulnerability scanners (the Go wrapper binary IS
# visible via buildinfo — rsync is the only blind spot). Generate a CycloneDX
# fragment from the same Renovate-tracked version ARG the build uses — a
# Renovate bump keeps the SBOM correct with zero extra maintenance — and ship
# it in the runtime image where the central sbom-cataloger (enabled fleet-wide
# by the cplieger/ci release pipeline; no per-repo .syft.yaml) picks up
# *.cdx.json files. The purl records provenance (the upstream dist tarball
# URL the build fetches above); the CPE uses NVD's canonical vendor:product
# for rsync (samba:rsync, per the NVD CPE dictionary and e.g. CVE-2024-12084's
# applicability configuration).
# ---------------------------------------------------------------------------
RUN cat > /out/rsync-scheduler.cdx.json <<EOF
{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "version": 1,
  "components": [
    {
      "bom-ref": "pkg:generic/rsync@${RSYNC_VERSION#v}?download_url=https://download.samba.org/pub/rsync/src/rsync-${RSYNC_VERSION#v}.tar.gz",
      "type": "application",
      "name": "rsync",
      "version": "${RSYNC_VERSION#v}",
      "purl": "pkg:generic/rsync@${RSYNC_VERSION#v}?download_url=https://download.samba.org/pub/rsync/src/rsync-${RSYNC_VERSION#v}.tar.gz",
      "cpe": "cpe:2.3:a:samba:rsync:${RSYNC_VERSION#v}:*:*:*:*:*:*:*"
    }
  ]
}
EOF

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
# PKG_REFRESH busts the cache for this layer. Without it BuildKit restores the
# layer verbatim on every rebuild, so the `apk upgrade` below floats nothing
# forward after the first build and the image keeps shipping the packages that
# were current then. The central release/CI/scan builds pass today's UTC date.
# The `echo` is load-bearing: BuildKit keys a RUN on the build args it actually
# CONSUMES, so a merely-declared ARG would change nothing.
ARG PKG_REFRESH=static
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apk upgrade --no-cache \
    && apk add --no-cache \
        acl-libs \
        libxxhash \
        lz4-libs \
        openssh-client \
        popt \
        zlib \
        zstd-libs

COPY --chmod=755 --from=rsync-builder /out/usr/bin/rsync /usr/bin/rsync
# CycloneDX SBOM fragment for the source-built rsync (generated in the builder
# stage from the Renovate-tracked version ARG). Placed where the release
# pipeline's sbom-cataloger inventories *.cdx.json, so SBOMs and scanners see
# rsync alongside the APK packages and the Go buildinfo.
COPY --from=rsync-builder /out/rsync-scheduler.cdx.json /usr/share/sbom/rsync-scheduler.cdx.json
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
