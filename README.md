# docker-rsync-scheduler

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/size.json)](https://github.com/cplieger/docker-rsync-scheduler/pkgs/container/docker-rsync-scheduler) [![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)](https://github.com/cplieger/docker-rsync-scheduler/pkgs/container/docker-rsync-scheduler) [![base: Alpine](https://img.shields.io/badge/base-Alpine-0D597F?logo=alpinelinux)](https://github.com/cplieger/docker-rsync-scheduler/blob/main/Dockerfile) [![Mutation](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/docker-rsync-scheduler/badges/mutation.json)](https://github.com/cplieger/docker-rsync-scheduler/issues?q=label%3Agremlins-tracker) [![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/docker-rsync-scheduler/releases)

<!-- hub-overview BEGIN -->
docker-rsync-scheduler copies chosen folders from your Docker host to a NAS or another server over SSH, on a schedule you set. It pushes one way only and keeps no older versions.

## What it does

docker-rsync-scheduler keeps an up-to-date copy of your folders on a NAS, a backup box or another server:

- Pushes each folder you list with rsync over SSH, every 6 hours by default or on the interval you set.
- Can mirror deletions too, with a cap on how many files one pass may delete.
- Skips a folder that is empty when a pass starts, so a source drive that failed to mount does not wipe the copy.
- Shows a failed job as an unhealthy container, until the next clean pass.

## Who it is for

docker-rsync-scheduler is built for pushing folders from a Docker host to a machine you reach over SSH, without writing cron jobs and rsync options by hand. Each job is a few lines of YAML, checked before the container starts.

You need a remote machine with rsync installed, an SSH key it accepts and a folder to write to.

Two other kinds of tool suit a different need:

- Consider [Syncthing](https://docs.syncthing.net/intro/getting-started.html) if you want folders kept in sync both ways between devices, set up from a web page.
- Consider [restic](https://restic.readthedocs.io/en/stable/010_introduction.html) if you want backups as snapshots you can list and restore.

docker-rsync-scheduler is free software under the Apache-2.0 license.
<!-- hub-overview END -->

## Quick start

The image is on GitHub Container Registry and Docker Hub, for `amd64` and `arm64`. This is the [`compose.yaml`](compose.yaml) in this repository.

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped

    environment:
      SYNC_INTERVAL: "6h"  # time between passes, or "off" to trigger passes from another scheduler
      SYNC_TIMEOUT: "10m"  # how long one job may run before it is stopped and counted as failed

    volumes:
      # Copy config.example.yaml to config.yaml and list your jobs before the first start.
      # Its first job deletes remote files that are gone from the source. Remove delete unless you want that.
      - "./config.yaml:/config/config.yaml:ro"
      # Create a dedicated key with ssh-keygen and add id_ed25519.pub to authorized_keys on the remote.
      # The remote needs rsync installed.
      - "./id_ed25519:/keys/id_ed25519:ro"
      - "./data:/data"  # keeps the schedule across restarts, recreates and image updates
      # One read-only mount for each source folder a job in config.yaml pushes
      - "/srv/source/certs:/sources/certs:ro"
      - "/srv/source/appconfig:/sources/appconfig:ro"
```

1. In the folder that holds `compose.yaml`, copy [`config.example.yaml`](config.example.yaml) to `config.yaml`.
2. In `config.yaml`, set each job's name, its source folder as the container sees it, such as `/sources/certs`, the remote `user@host`, the remote path and the key path `/keys/id_ed25519`.
3. The first example job deletes remote files that are gone from the source, through `delete` and `max_delete`. It also sets the owner of pushed files through `remote_uid` and `remote_gid`, which needs root on the remote. Remove those four lines unless you want both.
4. Create a dedicated key in the same folder with `ssh-keygen -t ed25519 -f id_ed25519 -N ""`. Keep the file at mode `0600`.
5. Add the key to the remote user's `authorized_keys`, for example with `ssh-copy-id -i id_ed25519.pub user@192.0.2.10`.
6. In `compose.yaml`, replace the two source lines with one read-only line for each folder your jobs push.
7. Run `docker compose up -d`.

Run `docker logs rsync`. You should see `container started`, then `sync ok` for each job and `sync cycle complete`. If you see `cannot load config`, `config.yaml` is missing or holds an invalid or misspelled key.

## Configuration reference

Settings come from the environment variables in `compose.yaml` and the jobs in `config.yaml`. The container refuses to start when `config.yaml` is missing or invalid. It reads the file again before every pass. With the single-file mount above, an editor that replaces the file can leave the container on the old copy, so restart the container after an edit, or mount the folder as [Editing the config](docs/configuration.md#editing-the-config-of-a-running-container) describes.

The built-in scheduler runs a pass at start, unless its record in `/data` shows a successful pass within the interval, then one every `SYNC_INTERVAL`. With `/data` mounted, a restart or an image update keeps that schedule. To sync at a set time of day, set `SYNC_INTERVAL` to `off` and run `docker exec rsync docker-rsync-scheduler sync` from cron or [Ofelia](https://github.com/mcuadros/ofelia). [Scheduling](docs/configuration.md#scheduling) covers both modes.

| Variable | Description | Default |
| --- | --- | --- |
| `SYNC_INTERVAL` | Time between passes, such as `6h` or `30m`. `off`, `disabled` or `0` leaves the timing to an external scheduler | `6h` |
| `SYNC_TIMEOUT` | How long one job may run before it is stopped and counted as failed | `10m` |
| `CONFIG_PATH` | Path of the job file inside the container | `/config/config.yaml` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. `warn` and `error` hide the per-pass `sync cycle complete` line that a stall alert needs | `info` |
| `SYNC_ACLS` | `true` adds rsync `-A` to copy ACLs. The remote rsync must support them | `false` |
| `SYNC_XATTRS` | `true` adds rsync `-X` to copy extended attributes. The remote rsync must support them | `false` |
| `SYNC_COMPRESS` | `on` compresses with an algorithm both sides agree on. `zstd`, `lz4` or `zlib` forces one, and every pass fails when the remote lacks it | `off` |

Each entry under `jobs:` in `config.yaml` takes these keys:

| Key | Default | Description |
| --- | --- | --- |
| `name` | required | Unique job name, used in the logs |
| `local` | required | Absolute path of the source folder inside the container |
| `remote_host` | required | `[user@]host`, as a DNS name, an IPv4 address or a bare IPv6 address |
| `remote_path` | required | Absolute path on the remote. Spaces, glob characters and shell characters are refused |
| `ssh_key` | required | Path of the private key inside the container |
| `delete` | `false` | `true` deletes remote files that are gone from the source |
| `max_delete` | _(unset)_ | With `delete`, the most files one pass may delete. A pass that would delete more fails. `0` refuses every deletion |
| `remote_uid`, `remote_gid` | _(unset)_ | Set both to give pushed files this owner on the remote. The remote user needs root or `CAP_CHOWN` |
| `excludes` | _(unset)_ | rsync exclude patterns for this job, added to the built-in ones |

| Mount | Description |
| --- | --- |
| `/config/config.yaml` | The job file, mounted read-only |
| `/config/known_hosts` | Optional. Pins the remote host keys, see [Security](#security) |
| `/keys/<name>` | SSH private keys, mounted read-only. The host file must be mode `0600` |
| `/data` | Optional. Keeps the schedule when the container is recreated. Give it a folder of its own |
| your source folders | The `local` folders of your jobs, mounted read-only |

The container opens no ports. [Configuration](docs/configuration.md) has every accepted value, the rsync command each job runs and the rules for IPv6 hosts.

## Security

The container opens no network port. Outside the built-in schedule, a pass can be started only from inside the container, with `docker exec`. Each job runs rsync without a shell, and every config field is checked at start and before each pass.

The container runs as root so it can read source files owned by any host user. Mount the sources read-only and use a dedicated, least-privilege SSH key on the remote.

On first contact the container trusts the remote's host key and remembers it until the container is recreated. To pin keys instead, run `ssh-keyscan -t ed25519 192.0.2.10 > known_hosts`, check that the file is not empty, and mount it read-only at `/config/known_hosts`. The container then rejects a host whose key does not match, and it refuses to start when that file holds no entries.

[Security](docs/security.md) covers a read-only root filesystem, the checks on each field and what the image contains.

## Troubleshooting

The healthcheck reads a marker the container writes after each pass. Healthy means the last pass had no failed job, and the container recovers on the next clean pass without a restart. With the built-in scheduler it starts unhealthy until the first pass ends, unless `/data` holds a successful pass from within the interval. The image allows 120 seconds for that first pass. In built-in mode, a schedule that stops running also turns it unhealthy.

- The log shows `cannot load config` and the container restarts. `config.yaml` is missing or has an invalid key, and the log line names it.
- A job logs `sync failed`. The `stderr` field holds rsync's own message, often a refused SSH login or a missing remote folder.
- A pass fails after rsync prints `Deletions stopped due to --max-delete limit`. It would have deleted more files than `max_delete` allows, so check the source before you raise the cap.
- A job logs `skip empty source`. Its folder was empty when the pass started, often a mount that failed. The container stays healthy, so watch for this warning.
- The first pass takes longer than 120 seconds. Raise `healthcheck.start_period` in `compose.yaml`.

[How it works](docs/how-it-works.md#health) explains health in each mode.

## Monitoring

docker-rsync-scheduler has no metrics endpoint. It writes logfmt logs to the container log, with a `sync cycle complete` line after every pass in both scheduling modes. [Monitoring and alerts](docs/monitoring.md) lists the log lines and two Loki alert rules, one for a failed job and one for a stalled schedule.

## Documentation

- [Configuration](docs/configuration.md) lists every setting, both scheduling modes and the rsync command each job runs.
- [How docker-rsync-scheduler works](docs/how-it-works.md) explains passes, the empty-source guard and health.
- [Security](docs/security.md) covers hardening, host-key pinning and what the image contains.
- [Monitoring and alerts](docs/monitoring.md) lists the log lines and the alert rules.

## Credits

This project packages [rsync](https://rsync.samba.org/), licensed GPL-3.0-or-later, and the [OpenSSH](https://www.openssh.com/) client, under a BSD license, into a container image. All credit for those tools goes to their upstream maintainers.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers the layout, the guardrails and how to run the checks and the image smoke test locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

Apache-2.0. See [LICENSE](LICENSE). The image carries the license text of every bundled component under `/usr/share/licenses/`. The Alpine packages in the image ship no license file upstream, so their license texts are kept under `licenses/` in this repository and copied in.

The bundled component is rsync itself, which is GPL-3.0-or-later. The build fetches the pinned release tarball `https://download.samba.org/pub/rsync/src/rsync-<version>.tar.gz`, at the version the `RSYNC_VERSION` argument in the `Dockerfile` pins, verifies the detached upstream signature and then the pinned SHA256, and applies no patches to the extracted source. rsync's own `COPYING` travels in the image at `/usr/share/licenses/rsync/COPYING`, and the upstream project is [RsyncProject/rsync](https://github.com/RsyncProject/rsync). That tarball and this repository's `Dockerfile` are the complete recipe for the rsync binary in the image, which is how anyone who receives it gets the corresponding source.
