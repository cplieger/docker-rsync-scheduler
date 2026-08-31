# docker-rsync-scheduler

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/size.json)](https://github.com/cplieger/docker-rsync-scheduler/pkgs/container/docker-rsync-scheduler)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/coverage.json)](https://github.com/cplieger/docker-rsync-scheduler/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/mutation.json)](https://github.com/cplieger/docker-rsync-scheduler/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13209/badge)](https://www.bestpractices.dev/projects/13209)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/docker-rsync-scheduler/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/docker-rsync-scheduler)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/docker-rsync-scheduler/releases)

<!-- hub-overview BEGIN -->
Push local directories to a remote host over rsync-and-ssh on a schedule. Structured logs, no metrics, no open ports.

## What it does

Reads a YAML config defining _N_ sync jobs. For each job it runs `rsync` over `ssh` to push a local directory one-way to a remote host. Every pass executes inside the long-lived daemon regardless of how it was triggered, so its structured logs (logfmt, UTC timestamps) always land on the container's log stream, ready for a log aggregator (Alloy, Promtail) and alerting.

- One-way mirror of each configured local directory to a `[user@]host:/path`
- Per-job `--delete`, `--chown=uid:gid`, and exclude patterns
- Empty-source guard: A job with an empty top-level source directory is skipped. This protects an unmounted source that Docker materialized as an empty directory from an unbounded `--delete` pass. A `local` path that does not exist fails the job. The guard is a preflight snapshot, and rsync rebuilds its file list after the check. It cannot protect a source that becomes empty during a pass; use `max_delete` as the backstop, as the example shows. The guard checks only the built-in global excludes, not per-job `excludes`. For a `delete: true` job whose own `excludes` can match every entry, set `max_delete` to cap the deletions.
- Built-in interval scheduler, or hand scheduling to an external scheduler (cron, Ofelia, etc.) via the `sync` subcommand
- File-marker healthcheck: unhealthy when any job fails, recovers on the next clean pass
- Logs only: no Prometheus exporter, no HTTP server, no network listener (triggering uses an in-container unix socket)
<!-- hub-overview END -->

## Quick start

The image is published to both GHCR (`ghcr.io/cplieger/docker-rsync-scheduler`) and Docker Hub (`cplieger/docker-rsync-scheduler`); identical contents, use whichever you prefer.

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped
    environment:
      SYNC_INTERVAL: "6h"   # Go duration; "off" disables the built-in scheduler
      SYNC_TIMEOUT: "10m"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - ./id_ed25519:/keys/id_ed25519:ro
      - /srv/source/certs:/sources/certs:ro
```

## Architecture

- _Single-owner daemon._ One long-lived process executes every pass, whatever triggered it: two passes can never overlap, and every pass's logs reach the container log stream in both scheduling modes.
- _Subcommands._ `daemon` (what the image's `CMD` runs), `sync` (submits one pass to the daemon and exits with that pass's result: 0 if all jobs succeed, 1 if any fail), and `health` (the Docker probe).
- _No shell, validated config._ Each job runs with an explicit argument slice (no shell), and every config field is validated at startup. See [Security](#security).
- _Bounded resources._ Per-job timeout (default 10m, override with `SYNC_TIMEOUT`); captured rsync stderr is capped at 1 MB.

## Scheduling modes

The container runs in one of two modes, selected by `SYNC_INTERVAL`.

### Built-in scheduler (default)

Set `SYNC_INTERVAL` to a Go duration (`6h`, `1h`, `30m`, …). The container runs a sync pass at startup and then every interval. This is the zero-dependency default; nothing else is required. On an unset or unparseable (non-sentinel) value it falls back to `6h`.

### External scheduler

Set `SYNC_INTERVAL=off` (aliases: `disabled`, `0`). The container stays running but idle, and you trigger each pass out-of-band by exec'ing the `sync` subcommand:

```bash
docker exec rsync docker-rsync-scheduler sync
```

The `sync` command submits one pass to the daemon and waits: its exit code is non-zero on failure (or when the request is rejected or the daemon is unreachable), and the pass updates the same health marker the long-running container reports. This lets a central scheduler own the cadence.

> **Observability (external mode).** The pass executes inside the daemon, not the exec child, so every log line (including the `sync cycle complete` heartbeat) lands on the container's log stream in external mode too, and every Loki rule under [Alerting](#alerting) works in both scheduling modes. The `sync` client prints only its own lifecycle (`triggered sync accepted/started/complete` plus the result), which your scheduler's job log (for example the Ofelia job result) captures.

Example with [Ofelia](https://github.com/mcuadros/ofelia) labels:

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped
    environment:
      SYNC_INTERVAL: "off"   # disable built-in loop; Ofelia drives it
      SYNC_TIMEOUT: "10m"
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.rsync-sync.schedule: "@every 6h"
      ofelia.job-exec.rsync-sync.command: "docker-rsync-scheduler sync"
      ofelia.job-exec.rsync-sync.no-overlap: "true"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - ./id_ed25519:/keys/id_ed25519:ro
      - /srv/source/certs:/sources/certs:ro
```

Overlapping passes cannot happen in either mode: the daemon runs passes strictly in order. A manual `docker exec … sync` that races a scheduled pass queues behind it and then runs as its own pass with its own result; a full queue rejects the trigger immediately with a clear reason instead of queuing unboundedly. Ofelia's `no-overlap` is still recommended to avoid queuing redundant triggers.

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
| --- | --- | --- | --- |
| `CONFIG_PATH` | Path to the YAML config inside the container | `/config/config.yaml` | No |
| `SYNC_INTERVAL` | Built-in scheduler cadence as a Go duration (e.g. `6h`, `30m`); the first pass runs at startup. Set `off` (or `disabled`/`0`) for external triggering, see [Scheduling modes](#scheduling-modes). Falls back to `6h` on unset or unparseable (non-sentinel) values. | `6h` | No |
| `SYNC_TIMEOUT` | Per-job rsync timeout as a Go duration (e.g. `10m`, `1h`). Falls back to the default on unset, non-positive, or unparseable values, so `0` does not disable the timeout. | `10m` | No |
| `LOG_LEVEL` | Log level: `debug`, `info`, `warn` (or `warning`), or `error`. Values are case-insensitive; surrounding whitespace is ignored. The startup record (`container started`) and the per-pass heartbeat (`sync cycle complete`) are Info records, so `warn` and `error` remove them and disarm the staleness alert below. | `info` | No |
| `SYNC_ACLS` | `true` adds rsync `-A` (`--acls`). The remote rsync must support ACLs; verify against your target, because a restricted wrapper such as `rrsync` can filter the option set. | `false` | No |
| `SYNC_XATTRS` | `true` adds rsync `-X` (`--xattrs`). Same remote precondition as `SYNC_ACLS`. | `false` | No |
| `SYNC_COMPRESS` | Compression: `off` (or `no`/`false`/`0`) disables it; `on` (or `yes`/`true`/`1`/`auto`) adds `-z` and lets rsync negotiate the algorithm; `zstd`, `lz4` or `zlib` adds `-z --compress-choice=<name>`. Name an algorithm only when you know the receiver has it: the remote refuses an algorithm it lacks and EVERY pass then fails. `on` negotiates and is always safe. Any other value logs a warning and leaves compression off. | `off` | No |

### Config schema (`config.yaml`)

A ready-to-edit, annotated [`config.example.yaml`](config.example.yaml) ships in the repo: copy it to `config.yaml` and edit. The container **fails fast** with a clear error if the config is missing or invalid, including unknown or misspelled keys.

Each entry under `jobs:` takes these keys:

| Key | Default | Description |
| --- | --- | --- |
| `name` | _none_ | Required, unique; used as a log key |
| `local` | _none_ | Required; absolute source path inside the container |
| `remote_host` | _none_ | Required; `[user@]host` (DNS name, IPv4, or IPv6 literal) |
| `remote_path` | _none_ | Required; absolute path on the remote |
| `ssh_key` | _none_ | Required; private key path inside the container |
| `remote_uid` | _(unset)_ | With `remote_gid`, adds `--chown=uid:gid`; valid range: 0-4294967294 |
| `remote_gid` | _(unset)_ | With `remote_uid`, adds `--chown=uid:gid`; valid range: 0-4294967294 |
| `delete` | `false` | Adds `--delete` when `true` |
| `max_delete` | _(unset)_ | With `delete`, adds `--max-delete=N` (rsync deletes at most N, then skips the rest and FAILS the pass: exit 25, or 24 if a source file also vanished in the same pass; rsync overwrites 25 with 24, and the app identifies the cap from its stderr line; `sync failed`, unhealthy; `0` refuses every deletion and fails the pass whenever anything would have been deleted); unset leaves deletions uncapped |
| `excludes` | _(unset)_ | Per-job rsync exclude patterns, added to the built-in globals |

Write IPv6 `remote_host` literals as the bare address (`2001:db8::1` or `user@2001:db8::1`); the brackets rsync's `host:path` syntax needs are added for you. A host containing a colon that is not a valid IP (a trailing colon, or an incomplete address) is rejected at startup so it can't be misread as rsync's daemon-mode `::` separator. Link-local IPv6 with a zone id (`fe80::1%eth0`) is not supported; use a global or ULA address, or define an `ssh_config` `Host` alias and reference the alias name.

Two jobs that point at one remote tree with `delete` set warn at startup: each pass can delete what the other put there. rsync excludes are what make such a pair safe, and the container does not try to decide whether yours is.

Every job also receives a fixed set of global excludes: `.stfolder`, `.stversions`, `.DS_Store`, `Thumbs.db`. Each job is pushed with `rsync -rlptD` (archive minus owner/group/ACL/xattr) plus `--stats`, the per-job and global excludes, the `-A`, `-X` and `-z` flags for whichever of `SYNC_ACLS`, `SYNC_XATTRS` and `SYNC_COMPRESS` you set, and the `-e "ssh -i <key> -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10"` transport (strict host-key pinning replaces `accept-new` when a `known_hosts` file is mounted; see [SSH host-key verification](#ssh-host-key-verification)).

### Volumes

| Mount | Description |
| --- | --- |
| `/config/config.yaml` | The YAML config (mount read-only). Override the path with `CONFIG_PATH`. |
| `/config/known_hosts` | Optional SSH known_hosts file (mount read-only). When present, enables strict host-key pinning instead of TOFU. See [SSH host-key verification](#ssh-host-key-verification). |
| `/keys/<name>` | SSH private key(s). Mount read-only; the host file must be mode `0600`. |
| (your sources) | The `local` directories referenced by your jobs. Mount read-only. |

## Healthcheck

The built-in healthcheck (`docker-rsync-scheduler health`) checks for a marker file that is set after each sync pass: healthy when the most recent pass had zero failed jobs, unhealthy when any job failed. Empty-source skips count as success. A pass whose rsync ends with the vanished-files warning (exit 24) also counts as success: it logs `level=WARN msg="sync completed with vanished source files"` with the exit code and the byte counts, and leaves the marker healthy. The container recovers automatically on the next clean pass, no restart required. In built-in mode it begins unhealthy and flips after the startup pass, so size `healthcheck.start_period` for the time the initial pass may take (the baked default is 120s); built-in mode also arms a freshness deadline of `2×SYNC_INTERVAL + jobs×SYNC_TIMEOUT`, so a wedged interval loop (marker present but never refreshed) eventually probes unhealthy. In external mode the container starts healthy (idle, nothing has failed), each triggered `sync` updates the marker, and no deadline is armed (a marker between sparse triggers must not expire).

> An empty source is skipped as a success, so a job whose source silently becomes empty (for example a read-only bind mount that failed to mount and Docker materialised as an empty directory) keeps the container healthy and never logs at `level=ERROR`; it is invisible to both the error-level and heartbeat-absence alerts. Each skip emits a `level=WARN msg="skip empty source"` line and the `sync cycle complete` heartbeat carries a `skipped` count. Alert on a persistently non-zero `skipped` (or `skipped == jobs`) across several consecutive passes, or on the recurring warning, to catch a vanished source before the remote mirror goes stale.

## Alerting

docker-rsync-scheduler has no metrics endpoint; its operational state is in its
logs (structured slog in logfmt). Ship the container's logs to Loki (Grafana
Alloy's Docker log discovery does this with no configuration) and evaluate
these with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/); firing
alerts deliver through your Alertmanager exactly like Prometheus metric alerts.
Every pass executes in the daemon (PID 1), so these rules work in both
scheduling modes.

```yaml
groups:
  - name: docker-rsync-scheduler
    rules:
      - alert: RsyncSchedulerSyncFailed
        expr: |
          sum by (container) (count_over_time(
            {container="rsync"} |= `msg="sync failed"` [15m]
          )) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "docker-rsync-scheduler failed a sync job"
          description: >
            A job logged "sync failed". Rsync failures include rsync_exit,
            timed_out, and a bounded stderr tail. A source-read failure includes
            its path and error instead. That job's remote mirror is now stale.
            Check the source path, remote host, ssh key, and connectivity.
      - alert: RsyncSchedulerStalled
        expr: |
          absent_over_time({container="rsync"} |= `sync cycle complete` [8h])
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "docker-rsync-scheduler has not completed a sync pass in 8h"
          description: >
            docker-rsync-scheduler logs a "sync cycle complete" line at the end
            of every pass that runs; the built-in scheduler runs one at startup
            and then every SYNC_INTERVAL (default 6h). None in 8h while the
            container is up means the scheduler is wedged or dead, which a
            fault-only ruleset misses because a stalled scheduler emits no
            "sync failed" line either. Restart the container.
```

`RsyncSchedulerStalled` matches an Info record, so it needs `LOG_LEVEL` at
`debug` or `info`; at `warn` or `error` the heartbeat is never emitted and the
rule fires permanently. The `sync failed` fault rule is an Error record and is
unaffected.

The "sync cycle complete" line is emitted whether a pass finished clean or with
failures, so the stall rule is a pure deadman for a scheduler that has stopped
running (in external mode, one that has stopped being triggered), while per-job
failures are caught by the fault rule. In external mode, additionally consider
a deadman on your scheduler's own job log (for example, absence of the Ofelia
job's completion line), which distinguishes "the trigger stopped firing" from
"the container died".

Thresholds and the `severity` label are starting points: size the stall window
to your pass cadence (`SYNC_INTERVAL` in built-in mode, your external
scheduler's period otherwise; the 8h default assumes 6h), adjust the
`container` selector (or `job` / `service`, depending on your log collector) to
your deployment, and route by whatever labels your Alertmanager uses. Two
classes count as a success and so never trip the fault rule: a source that has
silently gone empty, and a pass whose rsync reports vanished source files
(grep `sync completed with vanished source files`). See the `skipped` /
`skip empty source` note under [Healthcheck](#healthcheck).

## Security

No network listener, no HTTP server, no exposed ports. Triggering is an in-container unix socket (`/tmp/docker-rsync-scheduler.sock`, owner-only `0600`), so trigger authority is scoped to the container's own user, the same boundary `docker exec` already enforces. The image ships `openssh-client` only, no `sshd`. Each job is executed with an explicit argument slice via `exec.CommandContext`, and the `-e "ssh ..."` value is one argument that rsync splits into an argv vector itself, so nothing on the local side reaches a shell. The destination argument does reach the remote login shell, so `remote_path` alone is held to a shell-metacharacter refusal, and it also refuses glob characters (`*?[]`), which rsync deliberately does not escape: a pattern-shaped path lets the remote side pick whichever tree matches, and under `--delete` that is the wrong tree. Config is validated at startup and reloaded per pass: required fields present, names unique, `local`/`remote_path` absolute, `remote_host` matched against a strict pattern, the ssh key readable, `ssh_key` and `remote_path` free of spaces, and every field refused ASCII control characters.

### SSH host-key verification

By default the container uses `StrictHostKeyChecking=accept-new` (Trust On First Use). This lets a fresh deploy connect without pre-provisioning host keys, but trusts the first key it sees.

For stricter security, mount a read-only `known_hosts` file at `/config/known_hosts`. When this file is present the container switches to `StrictHostKeyChecking=yes` with an explicit `UserKnownHostsFile`, rejecting connections to any host whose key does not match the pinned entry. This prevents MITM attacks at the cost of requiring the operator to maintain the `known_hosts` file.

Generate it from your remote:

```bash
ssh-keyscan -t ed25519 192.0.2.10 > known_hosts
```

Then mount it into the container:

```yaml
volumes:
  - ./known_hosts:/config/known_hosts:ro
```

Confirm the file is not empty before you mount it: `ssh-keyscan` writes nothing and exits non-zero for a host it cannot reach, and a `known_hosts` that pins nothing cannot be used. The container refuses to start when the mounted path is not a regular file or carries no entries.

_Why it runs as root._ The container runs as root by design: it must read host-owned source files (e.g. a host UID like 1000) across multiple bind mounts. A fixed non-root `USER` would break this. Mount sources read-only and use a dedicated, least-privilege SSH key on the remote.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate); base images and Go modules are pinned by digest/version, and `rsync` is compiled from the pinned upstream release tarball with feature parity to the Alpine package it replaced (ACLs, xattrs, xxhash checksums, zstd/lz4 compression). The ACL, xattr and compression halves are reachable through `SYNC_ACLS`, `SYNC_XATTRS` and `SYNC_COMPRESS`; xxhash needs nothing, because it is the negotiated checksum on every pass. The build gates that tarball twice before it is extracted: `gpgv` verifies the detached upstream signature against the rsync release signing key committed as `rsync-release.gpg`, and `sha256sum -c` verifies the pinned digest. A version bump therefore needs no manual step, because the digest is recomputed automatically and a swapped tarball still fails the signature gate. The `openssh-client` package and the base userland (including rsync's runtime libraries) track the digest-pinned Alpine release and move when the image is rebuilt.

| Dependency | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang) |
| alpine | [Docker Hub](https://hub.docker.com/_/alpine) |
| rsync | [rsync upstream](https://github.com/RsyncProject/rsync) (pinned source build) |
| openssh-client | [Alpine](https://pkgs.alpinelinux.org/packages?name=openssh-client) |

Runtime Go modules: [`github.com/cplieger/health`](https://github.com/cplieger/health), [`github.com/cplieger/scheduler/v4`](https://github.com/cplieger/scheduler), [`github.com/cplieger/slogx`](https://github.com/cplieger/slogx), [`github.com/cplieger/envx/v2`](https://github.com/cplieger/envx), [`github.com/cplieger/envx/yamlenv/v2`](https://github.com/cplieger/envx), and [`go.yaml.in/yaml/v3`](https://github.com/yaml/go-yaml).

## Credits

This project packages [rsync](https://rsync.samba.org/) (GPL-3.0) and the [OpenSSH](https://www.openssh.com/) client (BSD) into a container image. All credit for those tools goes to their upstream maintainers.

## Contributing

Issues and pull requests are welcome. Please open an issue first for larger changes so the approach can be discussed before implementation.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE).
