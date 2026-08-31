#!/bin/sh
# Build-time smoke test for docker-rsync-scheduler's rsync payload.
#
# Runs in the Dockerfile `test` stage; the final image stage depends on this
# stage's marker.
#
# Run locally:  sh tests/smoke.sh   (needs rsync + ssh on PATH)
set -eu

fail=0
log() { printf '%s\n' "$*"; }
err() { printf '%s\n' "$*" >&2; }

# Proves the built binary executes and every shared library it links
# resolves in the runtime image.
if ! ver_out=$(rsync --version 2>&1); then
  err "FAIL: 'rsync --version' failed to run"
  err "$ver_out"
  fail=1
  exit "$fail"
fi

# Version assertion against the pinned upstream release (RSYNC_EXPECTED_VERSION,
# set by the Dockerfile test stage from ARG RSYNC_VERSION; unset skips the check
# on a plain local run).
if [ -n "${RSYNC_EXPECTED_VERSION:-}" ]; then
  expected=${RSYNC_EXPECTED_VERSION#v}
  first_line=$(printf '%s\n' "$ver_out" | head -n 1)
  case "$first_line" in
    *"version $expected "*) ;;
    *)
      err "FAIL: 'rsync --version' does not report expected version $expected"
      err "$first_line"
      fail=1
      ;;
  esac
fi

# Feature parity with the Alpine package the source build replaced (ACLs,
# xattrs, xxhash, zstd, lz4). rsync prints disabled features with a "no "
# prefix, which a bare substring match would still match, so the negated form
# is rejected first.
for feature in ACLs xattrs xxhash zstd lz4; do
  case "$ver_out" in
    *"no $feature"*)
      err "FAIL: rsync built without expected feature: $feature (reported as 'no $feature')"
      err "$ver_out"
      fail=1
      ;;
    *"$feature"*) ;;
    *)
      err "FAIL: rsync built without expected feature: $feature"
      err "$ver_out"
      fail=1
      ;;
  esac
done

# sync.go's exit-24 classifier keys on rsyncDelLimitWarn, the verbatim stderr
# line asserted below (rsync assigns 24 after status 25, so this line is the
# only surviving witness that the cap tripped vs. files merely vanishing).
del_dir=$(mktemp -d)
mkdir -p "$del_dir/src" "$del_dir/dst"
printf 'a' >"$del_dir/src/a"
printf 'b' >"$del_dir/src/b"
if rsync -rlptD "$del_dir/src/" "$del_dir/dst/" >/dev/null 2>&1; then
  rm -f "$del_dir/src/a" "$del_dir/src/b"
  del_out=$(rsync -rlptD --delete --max-delete=1 "$del_dir/src/" "$del_dir/dst/" 2>&1) && del_rc=0 || del_rc=$?
  if [ "$del_rc" -eq 0 ]; then
    err "FAIL: 'rsync --delete --max-delete=1' succeeded with the cap tripped (sync.go's exit-24 classifier expects a failure)"
    err "$del_out"
    fail=1
  fi
  case "$del_out" in
    *"Deletions stopped due to --max-delete limit"*) ;;
    *)
      err "FAIL: rsync no longer prints 'Deletions stopped due to --max-delete limit' on a tripped cap."
      err "      sync.go's rsyncDelLimitWarn constant is that exact string; without it a tripped"
      err "      --max-delete reports as a clean healthy pass."
      err "$del_out"
      fail=1
      ;;
  esac
else
  err "FAIL: local rsync seed transfer for the --max-delete check failed"
  fail=1
fi
rm -rf "$del_dir"

# The ssh transport is present (openssh-client stays an apk package; every
# job runs rsync -e ssh).
if ! command -v ssh >/dev/null 2>&1; then
  err "FAIL: ssh not found on PATH (openssh-client missing)"
  fail=1
fi

# Embedded CycloneDX SBOM fragment for the source-built rsync (Dockerfile
# rsync-builder stage) must be present, JSON-shaped, and name rsync at the
# pinned version — without it the source-built rsync is invisible to the
# signed release SBOM. Gated on RSYNC_EXPECTED_VERSION like above. BusyBox has
# no jq, so shape is asserted with head/tail/grep.
if [ -n "${RSYNC_EXPECTED_VERSION:-}" ]; then
  SBOM=/usr/share/sbom/rsync-scheduler.cdx.json
  expected=${RSYNC_EXPECTED_VERSION#v}
  if [ ! -s "$SBOM" ]; then
    err "FAIL: embedded SBOM fragment missing or empty: $SBOM"
    fail=1
  else
    if [ "$(head -c 1 "$SBOM")" != "{" ] || [ "$(tail -c 2 "$SBOM")" != "}" ]; then
      err "FAIL: embedded SBOM fragment is not a JSON object (bad first/last byte)"
      fail=1
    fi
    grep -q '"name": "rsync"' "$SBOM" || {
      err "FAIL: embedded SBOM fragment missing component: rsync"
      fail=1
    }
    grep -q "\"version\": \"$expected\"" "$SBOM" || {
      err "FAIL: embedded SBOM fragment does not carry the pinned rsync version $expected"
      err "$(cat "$SBOM")"
      fail=1
    }
  fi
fi

# sync.go's parseStats matches "Number of regular files transferred:" and the
# transferred-size label in rsync --stats output; a future rsync major
# renaming these would silently zero the parsed stats without failing any
# sync, so pin the labels here at build time.
stats_dir=$(mktemp -d)
mkdir -p "$stats_dir/src" "$stats_dir/dst"
printf 'x' >"$stats_dir/src/f"
if stats_out=$(rsync -rlptD --stats "$stats_dir/src/" "$stats_dir/dst/" 2>&1); then
  for label in "Number of regular files transferred:" "Total transferred file size:"; do
    case "$stats_out" in
      *"$label"*) ;;
      *)
        err "FAIL: rsync --stats output missing label: '$label' (the scheduler's stats parser depends on it)"
        err "$stats_out"
        fail=1
        ;;
    esac
  done
else
  err "FAIL: local rsync --stats transfer failed"
  err "$stats_out"
  fail=1
fi
rm -rf "$stats_dir"

[ "$fail" -eq 0 ] && log "rsync smoke: ok"
exit "$fail"
