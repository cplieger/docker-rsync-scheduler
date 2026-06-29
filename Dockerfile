# check=error=true
FROM golang:1.26-trixie@sha256:68b7145ec43d1820b9a56704554b53d1520aa2a15cb5233e374188a31b2a1bce AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY *.go ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /docker-rsync-scheduler .

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# No apk version pins: the digest-pinned base fixes the Alpine release line, so
# package-revision pins only strand the build on an Alpine release bump.
# apk upgrade is load-bearing: it floats forward base packages the pinned base
# pre-installs at an older, CVE-affected revision (libcrypto3/libssl3, etc.) —
# plain `apk add` leaves already-satisfied base packages unpatched.
# The floated package set is intentionally not version-pinned or asserted in-image;
# build-time package currency is verified by the advisory CI image scan (trivy/grype
# on the built image), not a build-time gate.
RUN apk upgrade --no-cache \
    && apk add --no-cache \
        rsync \
        openssh-client

COPY --chmod=755 --from=go-builder /docker-rsync-scheduler /usr/local/bin/docker-rsync-scheduler

# Runs as root by design: the app must read host-owned source files (e.g.
# uid 568) across multiple bind mounts and write ssh known_hosts on first
# contact (StrictHostKeyChecking=accept-new). A fixed USER would break both.
# start-period absorbs the first built-in pass (the container is unhealthy until
# it completes). Size it to your slowest expected initial sync; override
# per-deploy via compose healthcheck.start_period. See README "Healthcheck".
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=120s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
ENTRYPOINT ["/usr/local/bin/docker-rsync-scheduler"]
CMD ["daemon"]
