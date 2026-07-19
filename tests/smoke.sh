#!/bin/sh
# Build-time smoke test for docker-rsync-scheduler's rsync payload.
#
# Runs in the Dockerfile `test` stage, so the centralized `ci / validate`
# docker build-ability gate executes it on every PR and push (the final image
# stage depends on this stage's marker). Catches a broken rsync source build:
# `rsync --version` exercises the dynamic linker against the runtime stage's
# apk-installed libraries (a missing or misnamed lib package fails here), the
# version line is asserted against the pinned upstream release, and the
# feature set is asserted to keep parity with the Alpine package the source
# build replaced. The ssh transport (openssh-client, deliberately still an
# apk package) is asserted present alongside.
#
# Run locally:  sh tests/smoke.sh   (needs rsync + ssh on PATH)
set -eu

fail=0
log() { printf '%s\n' "$*"; }     # progress + final verdict -> stdout
err() { printf '%s\n' "$*" >&2; } # failures + captured output -> stderr

# 1. rsync runs and reports a version. Proves the built binary executes and
#    every shared library it links resolves in the runtime image.
if ! ver_out=$(rsync --version 2>&1); then
  err "FAIL: 'rsync --version' failed to run"
  err "$ver_out"
  fail=1
  exit "$fail"
fi

# 2. Version assertion: the built binary reports exactly the pinned upstream
#    version (RSYNC_EXPECTED_VERSION, passed by the Dockerfile test stage from
#    ARG RSYNC_VERSION; a leading "v" is stripped here). Unset means a plain
#    local run: the check is skipped. The Dockerfile guards the ARG with :?
#    so the in-image gate can never silently skip.
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

# 3. Feature parity with the Alpine package the source build replaced: ACL and
#    xattr support (rsync -A/-X), the xxhash checksum family, and zstd + lz4
#    compression must all be compiled in. A dropped configure flag or a
#    missing -dev build dep surfaces here, not in production. rsync prints
#    DISABLED capabilities with a "no " prefix ("no ACLs", "no xattrs"), which
#    a bare substring match would still match — so the negated form is
#    rejected first, before the positive match can see it.
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

# 4. The ssh transport is present (openssh-client stays an apk package; every
#    job runs rsync -e ssh).
if ! command -v ssh >/dev/null 2>&1; then
  err "FAIL: ssh not found on PATH (openssh-client missing)"
  fail=1
fi

# 5. --stats label contract: the Go scheduler's parseStats (sync.go) matches
#    "Number of regular files transferred:" and the transferred-size labels in
#    rsync --stats output. Older rsyncs used a different label ("Number of
#    files transferred:"), and a future major could rename again — which would
#    silently zero the parsed files/bytes stats without failing any sync. Pin
#    the coupling here at build time with a real local transfer against the
#    built binary, so a label drift fails the image build instead.
stats_dir=$(mktemp -d)
mkdir -p "$stats_dir/src" "$stats_dir/dst"
printf 'x' > "$stats_dir/src/f"
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
