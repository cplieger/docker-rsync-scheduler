# check=error=true

# renovate: datasource=github-tags depName=RsyncProject/rsync
ARG RSYNC_VERSION=v3.5.0
# The src/ archive path keeps every release; its parent keeps only the current one, so pin from src/.
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

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS rsync-builder

SHELL ["/bin/ash", "-eo", "pipefail", "-c"]

RUN apk add --no-cache \
        acl-dev \
        attr-dev \
        build-base \
        curl \
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
# Andrew Tridgell's rsync release signing key, dearmored (fingerprint
# 9FEF112DCE19A0DC7E882CB81BB24997A8535F6F).
# https://rsync.samba.org/download.html names him as the signer of every
# release from 3.4.0.
COPY rsync-release.gpg /usr/local/share/rsync-release.gpg
# The dist tarball, not the auto-generated GitHub tag archive. Both gates are
# fail-closed and the order matters: a bad signature stops the build before the
# pinned digest is consulted at all.
RUN curl -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 --retry-all-errors \
      -o "rsync-${RSYNC_VERSION#v}.tar.gz" \
      "https://download.samba.org/pub/rsync/src/rsync-${RSYNC_VERSION#v}.tar.gz" \
    && curl -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 --retry-all-errors \
      -o "rsync-${RSYNC_VERSION#v}.tar.gz.asc" \
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

# Syft inventories the final image from APK metadata and Go buildinfo, so the
# source-built rsync is invisible to the release SBOM. The fragment must land
# where the release pipeline's sbom-cataloger picks up *.cdx.json files.
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

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS base

ARG PKG_REFRESH=static
# The echo consumes PKG_REFRESH so a changed value invalidates the cached upgrade layer.
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
COPY --from=rsync-builder /out/rsync-scheduler.cdx.json /usr/share/sbom/rsync-scheduler.cdx.json
COPY --chmod=755 --from=go-builder /docker-rsync-scheduler /usr/local/bin/docker-rsync-scheduler

# The final stage depends on this stage's marker, so the smoke test gates the default target.
FROM base AS test
ARG RSYNC_VERSION
COPY tests/smoke.sh /tmp/tests/smoke.sh
# ${RSYNC_VERSION:?} fails the build if the ARG wiring ever breaks, so the
# smoke test's exact-version assertion can never be skipped in-image (the
# leading v is stripped inside smoke.sh).
RUN RSYNC_EXPECTED_VERSION="${RSYNC_VERSION:?}" sh /tmp/tests/smoke.sh && touch /tests-passed

# The final stage must remain last so the default target produces the runtime image.
FROM base AS final
COPY --from=test /tests-passed /tests-passed

# start-period absorbs the first built-in pass (the container is unhealthy until
# it completes). Size it to your slowest expected initial sync; override
# per-deploy via compose healthcheck.start_period. See README "Healthcheck".
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=120s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
ENTRYPOINT ["/usr/local/bin/docker-rsync-scheduler"]
CMD ["daemon"]
