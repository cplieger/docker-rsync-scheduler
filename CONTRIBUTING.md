# Contributing to docker-rsync-scheduler

A small Go daemon that pushes local directories to a remote host over
rsync+ssh on a schedule, or on an external trigger, as a resident container.
This guide covers what the
[org-wide defaults](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md)
don't: the layout, the guardrails to preserve, the two smoke tests, and how to
run the checks locally.

## Layout

Go module `github.com/cplieger/docker-rsync-scheduler`; binary
`docker-rsync-scheduler`. Flat package. The architecture is **single-owner**:
the daemon executes every pass; triggers only submit requests.

- `main.go`: subcommand dispatch (`daemon` / `sync` / `health`), the one place
  the logger is installed, and the signal context both subcommands receive.
- `daemon.go`: the composition root (`runDaemon`: config load and validation,
  ssh host-key posture, trigger socket, health latch, last-run record) and the
  executor policy (`daemon.run`, driven by `trigger.Execute`; `startTicker`,
  the built-in `scheduler.RunLoop` that submits tick requests like any other
  trigger; `recordScheduled`, which writes the last-run record for startup and
  interval passes only).
- `client.go`: the `sync` subcommand, a thin synchronous client over
  `trigger.Submit` that exits with the pass's own result.
- `sync.go`: the rsync engine (`buildRsyncArgs`, `runJob`, `runPass`), the
  empty-source guard, the `--stats` parser, and the pass-outcome logging.
- `config.go`: YAML parsing and validation, the env knobs (`loadInterval` via
  `scheduler.ParseInterval`, `loadSyncTimeout`, the transport switches), and
  the fixed paths (`defaultConfigPath`, `socketPath`, `stampPath`).
- `health.go`: the marker path, the probe's freshness deadline
  (`probeOptions`), and `applyPassHealth`, the app policy in front of
  `health.Latch`.

The queue, socket server, wire protocol and client come from
`github.com/cplieger/scheduler/v4/trigger`; the last-run record is
`scheduler.Stamp`; the health marker is `github.com/cplieger/health`. Their
mechanics are tested in those libraries. This repo tests the policy on top.

## Guardrails (don't weaken)

- The daemon's single executor runs every pass; that is the overlap guard. Do
  not add a second execution path. The ticker and every `sync` client submit
  through the queue, and every accepted request gets its own pass and its own
  true exit code.
- The outcome of a pass is decided by each `rsync` child's exit status, read
  after the child has exited (`runJob`). The health marker is written once
  the pass has ended, never before. An `rsync` that cannot start is a failed
  job, and `sync` exits 1 for it.
- Only the daemon's own scheduled passes (`startup`, `interval`) write the
  last-run record; a triggered pass never does. A failed scheduled pass
  records failed so the next boot retries at once (`scheduler.RetryFailed`).
  An interrupted-clean pass writes neither the marker nor the record
  (`passResult.vouchable` is the one predicate both sites read). A record the
  daemon cannot rewrite is ignored at boot (that boot runs the startup pass)
  and removed after a failed write, so a stale success never outlives the
  pass it describes.
- rsync runs from an explicit argument slice (no shell), and excludes go
  through `--filter` rules rather than `--exclude`. Every config field is validated at boot and on
  every per-pass reload; `remote_path` is refused shell metacharacters and
  glob characters because it reaches the remote login shell.
- The trigger socket stays owner-only (`0600`) and in-container. No network
  listener.
- The image runs as root by design (host-owned source files across bind
  mounts); do not add a `USER` directive.
- The rsync tarball is verified twice before extraction (`gpgv` against the
  committed `rsync-release.gpg`, then `sha256sum -c`). Keep both gates; the
  digest ARG is recomputed by Renovate, the signature is what proves origin.

## Conventions and gotchas

- Logs are slog logfmt to stderr with UTC timestamps via `slogx`; always
  key/value pairs (the `sloglint` linter enforces it). The message strings
  `sync ok`, `sync failed`, `sync cycle complete` and `container started` are
  a published contract: the README's alert rules key on them.
- `main()` orchestration and the real `rsync` exec are not unit-tested. Two
  smoke tests cover them:
  - `tests/smoke.sh` runs in the Dockerfile `test` stage and asserts the
    source-built rsync links, reports the pinned version, and carries the
    expected feature set.
  - `tests/image-smoke.conf` drives the shared harness
    `tests/image-smoke.sh` (synced from `cplieger/ci`; never edit the synced
    copy). It boots the assembled image in external mode, starts a throwaway
    sshd sidecar built from the same image, pushes a generated file over
    rsync+ssh through the daemon's own `sync` client, reads it back on the
    receiver, and ends with a negative control: it hides a shared library
    rsync links and asserts the next pass is reported failed and health
    flips. The sidecar needs outbound `apk` access; everything else is local.
- Tests are table-driven and live beside the code (`*_test.go`). Fake command
  runners (`fixedRunner("true")` / `"false"`) stand in for rsync. Tests that
  set env or swap the slog default are not parallel; the composition-root
  tests use `useTempStamp(t)` so a last-run record never leaks between runs.

## Running checks locally

Requires Go (see `go.mod`) and `golangci-lint` v2. If this repo is checked out
inside a parent `go.work` that doesn't include it, run module-local commands
with `GOWORK=off`:

```sh
GOWORK=off go build ./...
GOWORK=off go test -race ./...
GOWORK=off golangci-lint run          # config synced from cplieger/ci
GOWORK=off golangci-lint fmt          # gofumpt (extra-rules) + gci ordering
```

`golangci-lint run` reports unformatted files as issues, so run
`golangci-lint fmt` before pushing. Build the image (which runs
`tests/smoke.sh` in its `test` stage) and run the image smoke test against
it with:

```sh
docker build -t docker-rsync-scheduler .
sh tests/image-smoke.sh docker-rsync-scheduler
```

The image smoke test needs `ssh-keygen` on the host and creates a docker
network and a second container, both named with its own pid and removed on
exit.

## Commits and PRs

This repo uses [Conventional Commits](https://www.conventionalcommits.org/)
parsed by git-cliff to generate release notes, so the subject becomes a
changelog line: `feat:` (Added), `fix:` (Fixed), `sec:` (Security),
`chore(deps):` (Dependencies). Keep changes focused; open the PR against `main`
and make sure `ci / validate` is green before merging.

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report vulnerabilities via the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
