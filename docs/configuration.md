# Configuration

This page lists every setting docker-rsync-scheduler reads, how the two scheduling modes work and the rsync command each job runs. Read it when the README's tables do not answer your question.

## Environment variables

| Variable | Description | Default |
| --- | --- | --- |
| `SYNC_INTERVAL` | Time between passes, such as `6h` or `30m`. `off`, `disabled` or `0` leaves the timing to an external scheduler | `6h` |
| `SYNC_TIMEOUT` | How long one job may run before it is stopped and counted as failed | `10m` |
| `CONFIG_PATH` | Path of the job file inside the container | `/config/config.yaml` |
| `LOG_LEVEL` | `debug`, `info`, `warn` or `error`. `warn` and `error` hide the per-pass `sync cycle complete` line that a stall alert needs | `info` |
| `SYNC_ACLS` | `true` adds rsync `-A` to copy ACLs. The remote rsync must support them | `false` |
| `SYNC_XATTRS` | `true` adds rsync `-X` to copy extended attributes. The remote rsync must support them | `false` |
| `SYNC_COMPRESS` | `on` compresses with an algorithm both sides agree on. `zstd`, `lz4` or `zlib` forces one, and every pass fails when the remote lacks it | `off` |

`SYNC_INTERVAL` takes a Go duration. An unset or unparseable value falls back to `6h`. `SYNC_TIMEOUT` also takes a Go duration, such as `10m` or `1h`. An unset, non-positive or unparseable value falls back to `10m`, so `0` does not disable the timeout.

`LOG_LEVEL` also accepts `warning` for `warn`. `SYNC_ACLS` and `SYNC_XATTRS` accept `true`, `1`, `yes` or `on` to turn the option on, and `false`, `0`, `no` or `off` to leave it out. `SYNC_COMPRESS` turns compression off with `off`, `disabled`, `no`, `false` or `0`. It adds `-z` with `on`, `yes`, `true`, `1` or `auto`, and `-z --compress-choice=<name>` with `zstd`, `lz4` or `zlib`. Every variable ignores case and surrounding spaces.

`LOG_LEVEL` at `warn` or `error` removes the `container started` record and the `sync cycle complete` line after each pass, because both are Info records. The stall alert on [Monitoring and alerts](monitoring.md) keys on that line, so it fires permanently at those levels.

`SYNC_ACLS` and `SYNC_XATTRS` need a remote rsync that supports ACLs and extended attributes. Check against your target, because a restricted wrapper such as `rrsync` can filter the options rsync accepts.

For `SYNC_COMPRESS`, name an algorithm only when you know the receiver has it. The remote refuses an algorithm it lacks, and every pass then fails. `on` lets the two sides agree on one and is always safe. Any other value logs a warning and leaves compression off.

## Scheduling

`SYNC_INTERVAL` selects one of two modes.

### Built-in scheduler

Set `SYNC_INTERVAL` to a Go duration such as `6h`, `1h` or `30m`. The container runs a pass at start when one is due, then one every interval. Nothing else is needed.

A restart does not reset the schedule. The container records when its last scheduled pass ended and whether it succeeded, in `/data/.docker-rsync-scheduler-last-run`. When that record shows a successful pass younger than `SYNC_INTERVAL`, the startup pass is skipped. The first pass then runs when the interval since that pass ends. For example, a pass 2 hours old on a 6-hour schedule runs 4 hours after boot. So an image update or a `docker compose up` neither adds a pass nor delays the next one.

- A failed last pass still runs again at boot, so a fixed config gets an immediate result.
- Passes triggered with `sync` do not update the record.
- A record dated in the future, from a restored `/data` or a clock set back, counts as fresh until the clock catches up. The first pass can then land up to one full interval after boot.
- A record the container can read but not rewrite, on a read-only `/data`, is ignored. That boot runs the startup pass and logs why.
- Without a `/data` volume the record lives in the container's writable layer. A `docker restart` keeps the schedule, but recreating the container, for example for an image update, loses it and that boot runs a startup pass.

The built-in scheduler runs on a fixed interval. To sync at a set time of day, use an external scheduler.

### External scheduler

Set `SYNC_INTERVAL=off`, or `disabled` or `0`. The container stays running and idle, and you trigger each pass with the `sync` command:

```bash
docker exec rsync docker-rsync-scheduler sync
```

`sync` hands one pass to the running container and waits for it. It exits non-zero when a job failed, when the request was rejected, or when it could not reach the container. The pass updates the same health marker the built-in mode uses. A pass cut short by a container shutdown exits 0 and logs `triggered sync ended with a caveat` with the reason `pass cut short by shutdown; remaining jobs did not run`. The next pass covers the remaining jobs.

Every pass runs inside the long-lived container process, whatever triggered it. So every log line, the `sync cycle complete` line included, reaches the container log in external mode too, and the alert rules work in both modes. The `sync` command prints only its own progress, `triggered sync accepted`, `started` and `complete` with the result, which your scheduler's job log captures.

An example with [Ofelia](https://github.com/mcuadros/ofelia) labels:

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync-scheduler:latest
    container_name: rsync
    restart: unless-stopped
    environment:
      SYNC_INTERVAL: "off"  # Ofelia triggers every pass
      SYNC_TIMEOUT: "10m"
    labels:
      ofelia.enabled: "true"
      ofelia.job-exec.rsync-sync.schedule: "@every 6h"
      ofelia.job-exec.rsync-sync.command: "docker-rsync-scheduler sync"
      ofelia.job-exec.rsync-sync.no-overlap: "true"
    volumes:
      - "./config.yaml:/config/config.yaml:ro"
      - "./id_ed25519:/keys/id_ed25519:ro"
      - "/srv/source/certs:/sources/certs:ro"
```

### Passes never overlap

The container runs passes strictly one after another in both modes. A manual `sync` that arrives during a scheduled pass waits behind it, then runs as its own pass with its own result. The queue holds 16 requests. When it is full, a new trigger is rejected at once with a reason. Ofelia's `no-overlap` is still worth setting, so it does not queue triggers that add nothing.

## Job file

A ready-to-edit, annotated [`config.example.yaml`](../config.example.yaml) ships in the repository. Copy it to `config.yaml` and edit it. The container refuses to start with a clear error when the file is missing or invalid, unknown or misspelled keys included. It reads the file again before every pass, so an edit takes effect on the next pass. A pass that cannot read the file runs no job, logs `config reload failed` and leaves the container unhealthy.

Each entry under `jobs:` takes these keys:

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

### Ownership on the remote

`remote_uid` and `remote_gid` each take a value from 0 to 4294967294. Together they ask the receiver to apply `--chown=uid:gid` with `--super`. With both `remote_uid` and `remote_gid` set, the remote user must be root or hold `CAP_CHOWN`. Otherwise rsync fails the job with a `chown ... failed` line for each path. Setting only one of the pair logs a warning and skips `--chown`.

### Capping deletions

`max_delete` adds `--max-delete=N`. It works only with `delete: true`, and setting it without `delete` logs a warning. Unset leaves deletions uncapped. rsync deletes at most N files, then skips the rest and fails the pass, which logs `sync failed` and turns the container unhealthy. The cap fails only when more than N files would be deleted, so a value at or above the mirror's file count never fires. `0` refuses every deletion and fails the pass whenever anything would have been deleted.

rsync reports a capped pass with exit code 25, or 24 when a source file also vanished during the same pass, because rsync overwrites 25 with 24. The container recognizes the cap from rsync's `Deletions stopped due to --max-delete limit` line, so both cases count as failed.

### Remote host rules

Write an IPv6 `remote_host` as the bare address, `2001:db8::1` or `user@2001:db8::1`. The container adds the brackets rsync's `host:path` form needs. A host with a colon that is not a valid IP address, such as a trailing colon or an incomplete address, is refused at start. That way rsync can never read it as its `::` daemon-mode separator. A link-local IPv6 address with a zone ID, such as `fe80::1%eth0`, is not supported. Use a global or ULA address, or define a `Host` alias in an `ssh_config` and use the alias name. `remote_host` takes no port number.

### Two jobs on one remote folder

Two jobs that point at one remote tree, where either sets `delete`, log a warning at start. Each pass can delete what the other put there. rsync excludes are what make such a pair safe, and the container does not try to decide whether yours do.

## The rsync command

Every job also gets a fixed set of excludes: `.stfolder`, `.stversions`, `.DS_Store` and `Thumbs.db`. Each job runs `rsync -rlptD`, which is archive mode without owner, group, ACLs and extended attributes, plus `--stats`. It adds the job's own and the built-in excludes. It also adds `-A`, `-X` and `-z` for whichever of `SYNC_ACLS`, `SYNC_XATTRS` and `SYNC_COMPRESS` you set. The SSH transport is:

```text
-e "ssh -i <key> -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10"
```

When a `known_hosts` file is mounted, strict host-key checking replaces `accept-new`, as [Security](security.md#ssh-host-key-verification) describes.

## Editing the config of a running container

For live updates, mount the folder that holds `config.yaml` at `/config` instead of the single file, and replace the file by renaming a complete new file into that folder. A single-file bind mount blocks a rename through the mounted path. When an editor on the host replaces the source file, a single-file mount can stay on the old file, and the container keeps reading the old config.

## Volumes

| Mount | Description |
| --- | --- |
| `/config/config.yaml` | The job file, mounted read-only |
| `/config/known_hosts` | Optional. Pins the remote host keys, see [Security](security.md#ssh-host-key-verification) |
| `/keys/<name>` | SSH private keys, mounted read-only. The host file must be mode `0600` |
| `/data` | Optional. Keeps the schedule when the container is recreated. Give it a folder of its own |
| your source folders | The `local` folders of your jobs, mounted read-only |

Without a `/data` mount the record survives a restart of the same container but not its recreation. Give `/data` a folder of its own rather than one shared with another container, because the container writes the record as root and follows symlinks. In external mode the container does not use `/data`, so you can leave the mount out.
