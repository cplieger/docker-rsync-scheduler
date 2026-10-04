# Security

This page covers what docker-rsync-scheduler exposes, what it checks, how to pin SSH host keys, how to run it with a read-only root filesystem and what the image contains. Read it before you deploy it next to sensitive data.

## What it exposes

The container has no network listener, no HTTP server and no exposed ports. Passes are triggered through a unix socket inside the container, `/tmp/docker-rsync-scheduler.sock`, which only its owner can open, mode `0600`. Trigger access is therefore scoped to the container's own user, the same boundary `docker exec` already enforces. The image ships `openssh-client` only, with no `sshd`.

## What it checks

Each job runs with an explicit argument list through Go's `exec.CommandContext`. The `-e "ssh ..."` value is one argument that rsync splits into its own argument list, so nothing on the local side reaches a shell.

The destination argument does reach the remote login shell. So the container refuses shell metacharacters in `remote_path`, the one field that reaches that shell. It also refuses the glob characters `*`, `?`, `[` and `]` there, which rsync deliberately does not escape. A pattern-shaped path lets the remote side pick whichever tree matches, and under `--delete` that is the wrong tree.

The config is checked at start and again before each pass:

- required fields are present and job names are unique
- `local` and `remote_path` are absolute
- `remote_host` matches a strict pattern
- the SSH key is readable
- `remote_path` holds no spaces, and `ssh_key` holds no spaces or quotes
- no field holds an ASCII control character

## SSH host-key verification

By default the container uses `StrictHostKeyChecking=accept-new`, which trusts the first key a host presents. A fresh deploy connects without host keys set up in advance, but it trusts whatever key it sees first.

For stricter checking, mount a read-only `known_hosts` file at `/config/known_hosts`. When the file is present, the container switches to `StrictHostKeyChecking=yes` with an explicit `UserKnownHostsFile` and rejects any host whose key does not match the pinned entry. This prevents a machine-in-the-middle attack, and you then keep the `known_hosts` file up to date yourself.

Generate the file from your remote:

```bash
ssh-keyscan -t ed25519 192.0.2.10 > known_hosts
```

Then mount it into the container:

```yaml
volumes:
  - "./known_hosts:/config/known_hosts:ro"
```

Check that the file is not empty before you mount it. `ssh-keyscan` writes nothing and exits non-zero for a host it cannot reach, and a `known_hosts` that pins nothing cannot be used. The container refuses to start when the mounted path is not a regular file or holds no entries.

## Running as root

The container runs as root by design. It must read source files owned by host users, such as UID 1000, across several bind mounts, and a fixed non-root user would break that. Mount the sources read-only and use a dedicated, least-privilege SSH key on the remote.

## Read-only root filesystem

The container keeps its socket and health marker under `/tmp`. If you add `read_only: true`, add a writable tmpfs at `/tmp`, or the container restarts in a loop. A fully read-only root also needs a mounted `/config/known_hosts`, because `accept-new` writes SSH's own `known_hosts` file.

## What the image contains

| Component | Source |
| --- | --- |
| golang | [Go](https://hub.docker.com/_/golang), the build stage only |
| alpine | [Docker Hub](https://hub.docker.com/_/alpine) |
| rsync | [rsync upstream](https://github.com/RsyncProject/rsync), built from the pinned source release |
| openssh-client | [Alpine](https://pkgs.alpinelinux.org/packages?name=openssh-client) |

rsync is compiled from the pinned upstream release tarball with the same features as the Alpine package: ACLs, extended attributes, xxhash checksums and zstd and lz4 compression. `SYNC_ACLS`, `SYNC_XATTRS` and `SYNC_COMPRESS` switch on the ACL, extended-attribute and compression support. xxhash needs nothing, because it is the checksum rsync agrees on for every pass.

The build checks that tarball twice before it unpacks it. `gpgv` verifies the detached upstream signature against the rsync release signing keys committed as `rsync-release.gpg`, then `sha256sum -c` verifies the pinned digest.

The Go modules linked into the binary are [`github.com/cplieger/envx/v2`](https://github.com/cplieger/envx), [`github.com/cplieger/envx/yamlenv/v2`](https://github.com/cplieger/envx), [`github.com/cplieger/health`](https://github.com/cplieger/health), [`github.com/cplieger/pathinside/v2`](https://github.com/cplieger/pathinside), [`github.com/cplieger/scheduler/v4`](https://github.com/cplieger/scheduler), [`github.com/cplieger/slogx`](https://github.com/cplieger/slogx) and [`go.yaml.in/yaml/v3`](https://github.com/yaml/go-yaml).

## Update automation

[Renovate](https://github.com/renovatebot/renovate) updates every dependency. Base images are pinned by digest and Go modules by version. A new rsync release needs no manual step, because the digest is recomputed with the version and a swapped tarball still fails the signature check. The `openssh-client` package and the base system, rsync's runtime libraries included, follow the digest-pinned Alpine release and move when the image is rebuilt.
