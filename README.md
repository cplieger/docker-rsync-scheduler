# docker-rsync-scheduler

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/docker-rsync-scheduler/size)](https://github.com/cplieger/docker-rsync-scheduler/pkgs/container/docker-rsync-scheduler)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)
[![Go Report Card](https://goreportcard.com/badge/github.com/cplieger/docker-rsync-scheduler)](https://goreportcard.com/report/github.com/cplieger/docker-rsync-scheduler)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/coverage.json)](https://github.com/cplieger/docker-rsync-scheduler/actions/workflows/coverage.yml)
[![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/mutation.json)](https://github.com/cplieger/docker-rsync-scheduler/issues?q=label%3Agremlins-tracker)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13209/badge)](https://www.bestpractices.dev/projects/13209)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/docker-rsync-scheduler/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/docker-rsync-scheduler)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/docker-rsync-scheduler/releases)

Push local directories to a remote host over rsync-and-ssh on a schedule — structured logs, no metrics, no open ports.

## What it does

Reads a YAML config defining _N_ sync jobs. For each job it runs `rsync` over `ssh` to push a local directory one-way to a remote host. Every run emits structured `slog` lines (logfmt) for collection by a log aggregator (Alloy, Promtail) and alerting via Grafana or similar.

- One-way mirror of each configured local directory to a `[user@]host:/path`
- Per-job `--delete`, `--chown=uid:gid`, and exclude patterns
- Empty-source guard: a missing or empty source is skipped so `--delete` can never wipe the remote
- Built-in interval scheduler, or hand scheduling to an external scheduler (cron, Ofelia, etc.) via the `sync` subcommand
- File-marker healthcheck — unhealthy when any job fails, recovers on the next clean pass
- Logs only: no Prometheus exporter, no HTTP server, no listening socket

## Architecture

- _Scheduler your way._ Ships with a self-contained Go interval scheduler so you don't need external cron, systemd timers, or orchestrator-level scheduling. Set `SYNC_INTERVAL` to a Go duration and the container runs one pass at startup (immediate freshness on deploy) then every interval. If you already run a central scheduler (Ofelia, cron), set `SYNC_INTERVAL=off` and trigger passes with `docker exec rsync docker-rsync-scheduler sync` instead. See [Scheduling modes](#scheduling-modes).
- _Overlap lock._ A single advisory file lock (`flock` on `/tmp/.docker-rsync-scheduler.lock`) serialises every sync pass — the built-in ticker racing the startup pass in-process, and an external `sync` exec racing the ticker cross-process — so two passes never run at once.
- _Subcommands._ `daemon` (PID 1, the default command; dispatches built-in vs external based on `SYNC_INTERVAL`), `sync` (one pass, exit 0 if all jobs succeed, 1 if any fail), and `health` (the Docker probe). The built-in startup pass, the interval pass, and the `sync` subcommand share one sync-pass function.
- _No shell._ Each job is executed via `exec.CommandContext` with an explicit argument slice. The `-e "ssh ..."` value is a single argument that rsync splits internally — nothing is ever interpreted by a shell.
- _Injection guardrails._ Config is validated at startup: required fields present, names unique, `local`/`remote_path` absolute, `remote_host` matched against a strict pattern, and every field rejected if it contains shell metacharacters or control characters as defense-in-depth. The ssh key must exist and be readable.
- _Bounded resources._ Per-job timeout via context (default 10m, override with `SYNC_TIMEOUT`); captured rsync stderr is bounded to 1 MB so a chatty subprocess cannot OOM the container.
- _Health._ File-marker pattern via [`github.com/cplieger/health`](https://github.com/cplieger/health) — the marker is set after each pass and probed by the `health` subcommand.

## Quick start

The image is published to both GHCR (`ghcr.io/cplieger/docker-rsync-scheduler`) and Docker Hub (`cplieger/docker-rsync-scheduler`) — identical contents, use whichever you prefer.

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped
    environment:
      LOG_LEVEL: "info"
      CONFIG_PATH: "/config/config.yaml"
      SYNC_INTERVAL: "6h"   # Go duration; "off" disables the built-in scheduler
      SYNC_TIMEOUT: "10m"
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - ./id_ed25519:/keys/id_ed25519:ro
      - /srv/containers/caddy:/sources/caddy:ro
```

## Scheduling modes

The container runs in one of two modes, selected by `SYNC_INTERVAL`.

### Built-in scheduler (default)

Set `SYNC_INTERVAL` to a Go duration (`6h`, `1h`, `30m`, …). The container runs a sync pass at startup and then every interval. This is the zero-dependency default; nothing else is required. On an unset or unparseable (non-sentinel) value it falls back to `6h`.

### External scheduler

Set `SYNC_INTERVAL=off` (aliases: `disabled`, `0`). The container stays running but idle, and you trigger each pass out-of-band by exec'ing the `sync` subcommand:

```bash
docker exec rsync docker-rsync-scheduler sync
```

The pass runs once and exits; its exit code is non-zero on failure, and it updates the same health marker the long-running container reports. This lets a central scheduler own the cadence. Example with [Ofelia](https://github.com/mcuadros/ofelia) labels:

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped
    environment:
      LOG_LEVEL: "info"
      CONFIG_PATH: "/config/config.yaml"
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
      - /srv/containers/caddy:/sources/caddy:ro
```

Overlapping passes are prevented in both modes by an advisory file lock (`flock`) on `/tmp/.docker-rsync-scheduler.lock`, so a manual `docker exec` pass that races a scheduled one (or the built-in ticker) will skip rather than run a second concurrent pass. Ofelia's `no-overlap` is still recommended to avoid queuing redundant triggers.

## Configuration reference

### Environment variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `CONFIG_PATH` | Path to the YAML config inside the container | `/config/config.yaml` | No |
| `SYNC_INTERVAL` | Built-in scheduler cadence as a Go duration (e.g. `6h`, `1h`, `30m`). The first pass runs at startup; subsequent passes fire every interval thereafter. Set to `off` (or `disabled`/`0`) to disable the built-in scheduler and trigger passes externally — see [Scheduling modes](#scheduling-modes). Falls back to `6h` on an unset or unparseable (non-sentinel) value. | `6h` | No |
| `SYNC_TIMEOUT` | Per-job rsync timeout as a Go duration (e.g. `10m`, `1h`). Falls back to the default on unset or unparseable values. | `10m` | No |
| `LOG_LEVEL` | Log level: `debug`, `info`, `warn`, or `error` | `info` | No |

### Config schema (`config.yaml`)

A ready-to-edit [`config.example.yaml`](config.example.yaml) ships in the repo — copy it to `config.yaml` and edit. The container **fails fast** with a clear error if the config is missing or invalid.

```yaml
jobs:
  - name: caddy                          # required, unique, used as a log key
    local: /sources/caddy                # required, absolute path inside the container
    remote_host: root@192.168.1.87       # required, [user@]host
    remote_path: /srv/containers/caddy   # required, absolute path on the remote
    remote_uid: 1000                     # optional; with remote_gid -> rsync --chown=uid:gid
    remote_gid: 1000                     # optional
    ssh_key: /keys/id_ed25519            # required, private key path inside the container
    delete: true                         # optional, default false -> rsync --delete when true
    excludes: ["**/locks", "**/*.lock", "logs"]  # optional, per-job exclude patterns
```

Every job also receives a fixed set of global excludes: `.stfolder`, `.stversions`, `.DS_Store`, `Thumbs.db`. Each job is pushed with `rsync -rlptD` (archive minus owner/group/ACL/xattr) plus `--stats`, the per-job and global excludes, and the `-e "ssh -i <key> -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10"` transport.

### Volumes

| Mount | Description |
|-------|-------------|
| `/config/config.yaml` | The YAML config (mount read-only). Override the path with `CONFIG_PATH`. |
| `/config/known_hosts` | Optional SSH known_hosts file (mount read-only). When present, enables strict host-key pinning instead of TOFU. See [SSH host-key verification](#ssh-host-key-verification). |
| `/keys/<name>` | SSH private key(s). Mount read-only; the host file must be mode `0600`. |
| (your sources) | The `local` directories referenced by your jobs. Mount read-only. |

## Healthcheck

The built-in healthcheck (`docker-rsync-scheduler health`) checks for a marker file that is set after each sync pass: healthy when the most recent pass had zero failed jobs, unhealthy when any job failed. Empty-source skips count as success. The container recovers automatically on the next clean pass — no restart required. In built-in mode it begins unhealthy, runs one pass at startup, and transitions accordingly, so size `healthcheck.start_period` for the time the initial pass may take. In external mode the container starts healthy (idle, nothing has failed) and each triggered `sync` updates the marker.

```dockerfile
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=30s \
    CMD ["/usr/local/bin/docker-rsync-scheduler", "health"]
```

## Security

No network listener, no HTTP server, no exposed ports. The image ships `openssh-client` only — no `sshd`. Each job is executed with an explicit argument slice via `exec.CommandContext`; nothing is passed through a shell. Config fields are validated and rejected if they contain shell metacharacters or control characters, even though the arg-list exec already prevents interpretation.

### SSH host-key verification

By default the container uses `StrictHostKeyChecking=accept-new` (Trust On First Use). This lets a fresh deploy connect without pre-provisioning host keys, but trusts the first key it sees.

For stricter security, mount a read-only `known_hosts` file at `/config/known_hosts`. When this file is present the container switches to `StrictHostKeyChecking=yes` with an explicit `UserKnownHostsFile`, rejecting connections to any host whose key does not match the pinned entry. This prevents MITM attacks at the cost of requiring the operator to maintain the `known_hosts` file.

Generate it from your remote:

```bash
ssh-keyscan -t ed25519 192.168.1.87 > known_hosts
```

Then mount it into the container:

```yaml
volumes:
  - ./known_hosts:/config/known_hosts:ro
```

| Tool | Result |
|------|--------|
| [govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) | No vulnerabilities in call graph |
| [golangci-lint](https://golangci-lint.run/) (gosec, gocritic) | 0 issues |
| [trivy](https://trivy.dev/) | Inherits the Alpine base image scan |
| [gitleaks](https://github.com/gitleaks/gitleaks) | No secrets detected |
| [hadolint](https://github.com/hadolint/hadolint) | Clean |

_Why it runs as root._ The container runs as root by design: it must read host-owned source files (e.g. uid 568) across multiple bind mounts. A fixed non-root `USER` would break this. Mount sources read-only and use a dedicated, least-privilege SSH key on the remote.

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate). Base images and Go modules are pinned by digest/version; the `rsync`/`openssh-client` apk packages are installed unpinned so they track the digest-pinned base.

| Dependency | Source |
|------------|--------|
| golang | [Go](https://hub.docker.com/_/golang) |
| alpine | [Docker Hub](https://hub.docker.com/_/alpine) |
| rsync | [Alpine](https://pkgs.alpinelinux.org/packages?name=rsync) |
| openssh-client | [Alpine](https://pkgs.alpinelinux.org/packages?name=openssh-client) |

Runtime Go modules: [`github.com/cplieger/health`](https://github.com/cplieger/health) and [`gopkg.in/yaml.v3`](https://gopkg.in/yaml.v3).

## Credits

This project packages [rsync](https://rsync.samba.org/) (GPL-3.0) and the [OpenSSH](https://www.openssh.com/) client (BSD) into a container image. All credit for those tools goes to their upstream maintainers.

## Contributing

Issues and pull requests are welcome. Please open an issue first for larger changes so the approach can be discussed before implementation.

## Disclaimer

This image is built with care and follows security best practices, but it is intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
