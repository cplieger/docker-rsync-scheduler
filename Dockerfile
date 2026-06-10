# check=error=true

FROM alpine:3.24.0@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

# renovate: datasource=repology depName=alpine_3_24/rsync versioning=loose
ARG RSYNC_VERSION=3.4.3-r1
# renovate: datasource=repology depName=alpine_3_24/openssh versioning=loose
ARG OPENSSH_VERSION=10.3_p1-r0

RUN apk add --no-cache \
        rsync="${RSYNC_VERSION}" \
        openssh-client="${OPENSSH_VERSION}"

# Exec-driven sidecar by default: the container stays alive so a scheduler
# (Ofelia, cron, Kubernetes) can `exec` rsync/ssh commands into it. One-shot
# users override the command, e.g.:
#   docker run --rm ghcr.io/cplieger/docker-rsyncssh rsync -a src/ host:/dst/
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=5s \
    CMD pidof sleep >/dev/null || exit 1
CMD ["sleep", "infinity"]
