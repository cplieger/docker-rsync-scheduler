# check=error=true
FROM golang:1.26-trixie@sha256:aaa14c053d35dbb2f3501b18396dddd76ab4b10764c9006e2647ab7a7bf92fae AS go-builder
ENV GOTOOLCHAIN=auto

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
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
RUN apk upgrade --no-cache \
    && apk add --no-cache \
        rsync \
        openssh-client

COPY --chmod=755 --from=go-builder /docker-rsync-scheduler /usr/local/bin/docker-rsync-scheduler

# Runs as root by design: the app must read host-owned source files (e.g.
# uid 568) across multiple bind mounts and write ssh known_hosts on first
# contact (StrictHostKeyChecking=accept-new). A fixed USER would break both.
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
ENTRYPOINT ["/usr/local/bin/docker-rsync-scheduler"]
CMD ["daemon"]
