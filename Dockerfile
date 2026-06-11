# check=error=true
FROM golang:1.26-trixie@sha256:e2f47e5638d151001160a3b65ef7b0d6ddc29b0f7e05d40a0e08d189a59cd02a AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /docker-rsync-scheduler .

FROM alpine:3.24.0@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

# renovate: datasource=repology depName=alpine_3_24/rsync versioning=loose
ARG RSYNC_VERSION=3.4.3-r1
# renovate: datasource=repology depName=alpine_3_24/openssh versioning=loose
ARG OPENSSH_VERSION=10.3_p1-r0

# --upgrade pulls patched transitive deps (libcrypto3/libssl3, etc.) that the
# pinned base image pre-installs at an older, CVE-affected revision; plain
# `apk add` would leave the already-satisfied base OpenSSL unpatched.
RUN apk add --no-cache --upgrade \
        rsync="${RSYNC_VERSION}" \
        openssh-client="${OPENSSH_VERSION}"

COPY --chmod=755 --from=go-builder /docker-rsync-scheduler /usr/local/bin/docker-rsync-scheduler

# Runs as root by design: the app must read host-owned source files (e.g.
# uid 568) across multiple bind mounts and write ssh known_hosts on first
# contact (StrictHostKeyChecking=accept-new). A fixed USER would break both.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
ENTRYPOINT ["/usr/local/bin/docker-rsync-scheduler"]
CMD ["daemon"]
