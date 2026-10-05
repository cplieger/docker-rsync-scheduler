# Contributing to docker-rsync-scheduler

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Rules

- Keep the log messages and levels in `docs/monitoring.md` unchanged. Users' Loki alerts and the README's first-run check match them. A renamed `sync cycle complete` fires the stall alert, and an Error line moved to a lower level silences the fault alert.
- A new runtime `apk add` package needs `licenses/APK-MANIFEST` lines for it and its new dependencies, and a `licenses/<origin>/SOURCE` file per new origin. Put upstream's license text beside it, or say in `SOURCE` that upstream publishes none. No check catches a gap.
