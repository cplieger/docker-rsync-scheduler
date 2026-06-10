# docker-rsync

![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue)
[![GitHub release](https://img.shields.io/github/v/release/cplieger/docker-rsync)](https://github.com/cplieger/docker-rsync/releases)
[![Image Size](https://ghcr-badge.egpl.dev/cplieger/docker-rsync/size)](https://github.com/cplieger/docker-rsync/pkgs/container/docker-rsync)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Alpine 3.24.0](https://img.shields.io/badge/base-Alpine_3.24.0-0D597F?logo=alpinelinux)

A minimal Alpine image with [rsync](https://rsync.samba.org/) and the OpenSSH **client**, built for the exec-driven sidecar pattern. Bring your own key, paths, and sync command.

## What it does

Packages `rsync` + `ssh` (client only) on a pinned Alpine base and nothing else. The default command is `sleep infinity`, so the container stays alive and a scheduler (Ofelia, cron, a Kubernetes CronJob) can `exec` rsync/ssh commands into the running container instead of spinning up a fresh container each run.

It can also be used one-shot by overriding the command.

### Why this design

- **Client only** — ships `openssh-client`, not `openssh-server`. No `sshd`, no listening service, no host keys, smaller attack surface. This is a tool that _pushes/pulls_, not a target that _receives_.
- **No bundled logic** — no entrypoint script, no cron, no env-to-config translation. You supply the sync command (and, for scheduled use, the scheduler). This keeps the image generic and the orchestration explicit in your own config.
- **Pinned + reproducible** — Alpine base pinned by digest; `rsync` and `openssh-client` pinned by version and tracked by [Renovate](https://github.com/renovatebot/renovate).
- **Multi-arch** — `linux/amd64` and `linux/arm64`.

## Quick start

### Exec-driven sidecar (recommended)

Run it alongside a scheduler and `exec` the sync on a schedule:

```yaml
services:
  rsync:
    image: ghcr.io/cplieger/docker-rsync:latest
    container_name: rsync
    restart: unless-stopped
    volumes:
      - ./id_ed25519:/key/id_ed25519:ro   # dedicated SSH key, mode 0600
      - ./data:/data:ro                    # what you want to sync
```

```bash
docker exec rsync rsync -a --delete \
    -e "ssh -i /key/id_ed25519 -o StrictHostKeyChecking=accept-new" \
    /data/ user@remote:/dest/
```

### One-shot

```bash
docker run --rm -v "$(pwd)/data:/data:ro" -v "$(pwd)/id_ed25519:/key/id_ed25519:ro" \
    ghcr.io/cplieger/docker-rsync \
    rsync -a -e "ssh -i /key/id_ed25519" /data/ user@remote:/dest/
```

The default `sleep infinity` is overridden by the trailing `rsync ...` arguments.

## Configuration reference

### Volumes

| Mount | Description |
|-------|-------------|
| `/key/<name>` | Your SSH private key(s). Mount read-only; OpenSSH requires mode `0600`, so the source file on the host must be `0600`. |
| (your paths) | Whatever you want to sync. Mount sources read-only and destinations read-write as appropriate. |

### Command

The image defaults to `sleep infinity` (stay-alive sidecar). Override it with any `rsync`/`ssh` invocation for one-shot use. There is no entrypoint wrapper, so arguments are passed straight to the binary.

### SSH host-key verification

For unattended use, either pre-populate a `known_hosts` file and mount it, or pass `-o StrictHostKeyChecking=accept-new` to trust the host key on first contact and pin it thereafter. Avoid `StrictHostKeyChecking=no`, which never pins.

## Healthcheck

The built-in healthcheck verifies the keep-alive process is running:

```dockerfile
HEALTHCHECK --interval=60s --timeout=5s --retries=3 --start-period=5s \
    CMD pidof sleep >/dev/null || exit 1
```

This reflects "the sidecar is up and ready to be exec'd into". It is not meaningful for one-shot runs (which override the command) — that's expected.

## Security

| Tool | Result |
|------|--------|
| [hadolint](https://github.com/hadolint/hadolint) | Clean |
| [gitleaks](https://github.com/gitleaks/gitleaks) | No secrets detected |
| [trivy](https://trivy.dev/) | Inherits the Alpine base image scan |

The image ships `openssh-client` only — no `sshd`, so the container exposes no listening service. It is published with [cosign](https://github.com/sigstore/cosign) signatures and SBOM attestations. Verify a pull:

```bash
cosign verify ghcr.io/cplieger/docker-rsync:latest \
    --certificate-identity-regexp "https://github.com/cplieger/docker-rsync/.github/workflows/.*" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Dependencies

All dependencies are updated automatically via [Renovate](https://github.com/renovatebot/renovate) and pinned by digest or version for reproducibility.

| Dependency | Version | Source |
|------------|---------|--------|
| alpine | `3.24.0` | [Docker Hub](https://hub.docker.com/_/alpine) |
| rsync | `3.4.3-r1` | [Alpine](https://pkgs.alpinelinux.org/package/v3.24/main/x86_64/rsync) |
| openssh-client | `10.3_p1-r0` | [Alpine](https://pkgs.alpinelinux.org/package/v3.24/main/x86_64/openssh-client) |

## Credits

This project packages [rsync](https://rsync.samba.org/) (GPL-3.0) and the [OpenSSH](https://www.openssh.com/) client (BSD) into a container image. All credit for those tools goes to their upstream maintainers.

## Contributing

Issues and pull requests are welcome. Please open an issue first for larger changes so the approach can be discussed before implementation.

## Disclaimer

This image is built with care and follows security best practices, but it is intended for **homelab use**. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

This project is licensed under the [GNU General Public License v3.0](LICENSE).
