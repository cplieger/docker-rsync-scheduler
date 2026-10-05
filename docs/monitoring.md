# Monitoring and alerts

This page lists the log lines docker-rsync-scheduler writes and two Loki alert rules built on them. Read it when you want to be told that a job failed or that passes stopped running.

## Log lines

docker-rsync-scheduler has no metrics endpoint. It writes structured logs in logfmt with UTC timestamps to the container log. Every pass runs inside the long-lived container process, so these lines appear in both scheduling modes.

| Message | Level | Meaning |
| --- | --- | --- |
| `container started` | Info | The container has loaded its config, with the mode, the job count and the interval |
| `sync ok` | Info | One job finished, with its file, byte and deletion counts |
| `sync failed` | Error | One job failed, with `rsync_exit`, `timed_out` and the end of rsync's error output |
| `skip empty source` | Warn | A job's source folder was empty, so the job was skipped |
| `sync completed with vanished source files` | Warn | rsync finished, but source files vanished during the pass. It counts as a success |
| `config reload failed` | Error | The config could not be read before a pass, so no job ran |
| `sync cycle complete` | Info | A pass ended, with its `ok`, `skipped` and `failed` counts |

The `sync cycle complete` line is logged whether a pass finished clean or with failures.

## Alerting

Ship the container's logs to Loki and evaluate these rules with [Loki's ruler](https://grafana.com/docs/loki/latest/alert/). Grafana Alloy's Docker log discovery ships them with no extra configuration. Firing alerts go through your Alertmanager like any Prometheus alert.

```yaml
groups:
  - name: rsync-scheduler
    rules:
      - alert: RsyncSchedulerSyncFailed
        expr: |
          sum by (container) (count_over_time(
            {container="rsync"} |= `level=ERROR` [15m]
          )) > 0
        for: 0m
        labels:
          severity: warning
        annotations:
          summary: "docker-rsync-scheduler logged an error"
          description: >
            docker-rsync-scheduler logged an Error record, so a remote copy
            is now stale. Read the "sync failed" or "config reload failed"
            line, then check the config, the source path, the remote host,
            the SSH key and the connection.
      - alert: RsyncSchedulerStalled
        expr: |
          absent_over_time({container="rsync"} |= `sync cycle complete` [8h])
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "docker-rsync-scheduler has not completed a sync pass in 8h"
          description: >
            No "sync cycle complete" line in 8h, so passes have probably
            stopped running. Rule out a stopped or renamed container and a
            stopped log pipeline, then restart the container.
```

`RsyncSchedulerSyncFailed` fires on any Error record. A failed job logs `sync failed` with `rsync_exit`, `timed_out` and the end of rsync's error output. A source-read failure logs its path and error instead. A config edit the container cannot reload logs `config reload failed`, and then no job runs.

`RsyncSchedulerStalled` relies on the `sync cycle complete` line at the end of every pass. The built-in scheduler runs a pass every `SYNC_INTERVAL`, 6h by default, and a restart keeps that schedule. So two lines are never more than one interval plus one pass apart. The fault rule misses a stopped scheduler, because a stopped scheduler logs no error either.

`RsyncSchedulerStalled` matches an Info record, so it needs `LOG_LEVEL` at `debug` or `info`. At `warn` or `error` the line is never written and the rule fires permanently. The fault rule keys on Error records and works at every level.

The stall rule catches passes that stopped running, or in external mode stopped being triggered. The fault rule catches failed jobs. In external mode, also consider an absence rule on your scheduler's own job log, such as the Ofelia job's completion line, to tell a trigger that stopped firing from a container that died.

Two cases count as a success and never trip the fault rule. One is a source that has silently gone empty, which logs `skip empty source`. The other is a pass whose rsync reports vanished source files, which logs `sync completed with vanished source files`. To catch a vanished source before the remote copy goes stale, alert on the recurring `skip empty source` warning. You can also alert when `sync cycle complete` shows a `skipped` count above zero, or `skipped` equal to `jobs`, across several passes in a row.

Thresholds and the `severity` label are starting points. Size the stall window to your pass cadence plus the longest pass, `SYNC_INTERVAL + jobs × SYNC_TIMEOUT` in built-in mode or your external scheduler's period otherwise. The 8h default assumes 6h with a few jobs at the 10m timeout. Change the `container` selector to the label your log collector sets, such as `job` or `service`, and route by whatever labels your Alertmanager uses.
