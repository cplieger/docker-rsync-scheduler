#!/bin/sh
# Runtime image smoke test for docker-rsync-scheduler. Invoked by the central
# CI docker job:
#   sh tests/image-smoke.sh <image-ref>
#
# docker-rsync-scheduler pushes local dirs to a remote over rsync+ssh, so a
# sync pass needs a reachable ssh target CI cannot provide. This test instead
# exercises the healthy-WITHOUT-remote path: EXTERNAL-TRIGGER mode
# (SYNC_INTERVAL=off) disables the built-in scheduler, so no pass runs, the
# container idles, and runExternal() writes the health marker healthy on boot
# (main.go: hc.markInitial(true) -> health.Marker.Set(true) -> creates
# /tmp/.healthy). That yields a genuine health-gated (Tier 2) assertion with no
# remote contacted:
#   - the binary runs in the real Alpine base (rsync + openssh-client present),
#   - the mounted YAML config parses and passes validation (config.go),
#   - the referenced ssh_key readability check passes (config.go checkReadable),
#   - external mode writes /tmp/.healthy on boot, and
#   - the shipped `health` subcommand HEALTHCHECK stats it and reports healthy.
#
# Not run with --read-only: an unwritable /tmp puts the health lib in degraded
# mode, where the probe reports healthy without a real marker (a weaker,
# near-tautological assertion). A writable /tmp keeps the marker write real.
set -eu

IMG="${1:?usage: image-smoke.sh <image-ref>}"
NAME="smoke-docker-rsync-scheduler-$$"
# HEALTHCHECK is --interval=60s --timeout=5s --retries=3 --start-period=120s;
# size TIMEOUT to cover the 120s start-period + two 60s intervals. In external
# mode the marker is written at boot, so the container typically reports healthy
# at the first 60s interval; TIMEOUT is only the upper bound before we give up.
TIMEOUT=240

# Minimal operator inputs the container needs to BOOT (not to sync): a valid
# config and a readable ssh_key. loadConfig() parses and validates both before
# external mode marks the container healthy, so without them run() exits 1 and
# the container never reaches the idle, healthy-on-boot state.
workdir=$(mktemp -d)

# shellcheck disable=SC2317,SC2329  # invoked indirectly via trap
cleanup() {
  code=$?
  # Dump container logs only on failure (a passing run stays quiet).
  if [ "$code" -ne 0 ]; then
    printf '%s\n' "--- container logs (tail) ---" >&2
    docker logs "$NAME" 2>&1 | tail -40 >&2 || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  rm -rf "$workdir"
}
trap cleanup EXIT

# One minimal job that passes validation: absolute local/remote paths, a
# regex-valid remote_host in the RFC 5737 doc range, and a readable ssh_key.
# The local dir need not exist (validated as absolute only) and the remote is
# never contacted in external mode.
cat >"$workdir/config.yaml" <<'EOF'
jobs:
  - name: smoke
    local: /sources/smoke
    remote_host: root@192.0.2.10
    remote_path: /srv/smoke
    ssh_key: /config/smoke_key
EOF
# checkReadable() only opens the key for reading; its contents and mode are
# irrelevant at boot because ssh never runs in external mode.
printf '%s\n' "dummy key: never used, external mode runs no sync" >"$workdir/smoke_key"

docker run -d --name "$NAME" \
  -e SYNC_INTERVAL=off \
  -v "$workdir:/config:ro" \
  "$IMG" >/dev/null

i=0
status=starting
while [ "$i" -lt "$TIMEOUT" ]; do
  # Fail fast on an early exit: poll .State.Running before the health status so
  # a crash-boot (e.g. invalid config) is caught by its exit code (more
  # debuggable than "unhealthy") and the verdict never depends on what health a
  # stopped container reports.
  if [ "$(docker inspect --format '{{ .State.Running }}' "$NAME" 2>/dev/null || echo missing)" != "true" ]; then
    ec=$(docker inspect --format '{{ .State.ExitCode }}' "$NAME" 2>/dev/null || echo '?')
    printf 'FAIL: docker-rsync-scheduler container exited early (exit code %s)\n' "$ec" >&2
    exit 1
  fi
  status=$(docker inspect --format '{{ if .State.Health }}{{ .State.Health.Status }}{{ else }}no-healthcheck{{ end }}' "$NAME" 2>/dev/null || echo gone)
  case "$status" in
    healthy)
      printf 'docker-rsync-scheduler image smoke: ok (healthy after %ss)\n' "$i"
      exit 0
      ;;
    unhealthy)
      printf 'FAIL: docker-rsync-scheduler reported unhealthy\n' >&2
      exit 1
      ;;
    no-healthcheck)
      printf 'FAIL: image has no HEALTHCHECK to assert against\n' >&2
      exit 1
      ;;
    gone)
      printf 'FAIL: docker-rsync-scheduler container is gone\n' >&2
      exit 1
      ;;
  esac
  i=$((i + 1))
  sleep 1
done
printf 'FAIL: docker-rsync-scheduler did not become healthy within %ss (last status: %s)\n' "$TIMEOUT" "$status" >&2
exit 1
