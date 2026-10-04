# How docker-rsync-scheduler works

This page explains how a pass runs, what the empty-source guard protects and how health is decided. Read it when you want to know why the container behaves as it does.

## One process runs every pass

One long-lived process runs every pass, whatever triggered it. Two passes can never overlap, and every pass's logs reach the container log in both scheduling modes. Within a pass, the jobs run one after another in the order of the job file. A job runs rsync only, with no command before or after it. To copy a live database consistently, write a dump into the source folder before the pass.

The binary has three commands:

- `daemon` is what the image's `CMD` runs. It owns every pass.
- `sync` hands one pass to the daemon and exits with that pass's result, 0 when no job failed and 1 when any did.
- `health` is the Docker healthcheck probe.

Each job runs rsync from a fixed list of arguments, never through a shell, and every config field is checked at start. One job may run for `SYNC_TIMEOUT`, 10 minutes by default. The container keeps at most 1 MB of rsync's error output for each job.

## The empty-source guard

A job whose source folder is empty at the top level is skipped. This protects a source that failed to mount, which Docker then creates as an empty folder, from a `--delete` pass that would empty the remote copy. A `local` path that does not exist fails the job instead.

The guard looks once, before rsync starts, and rsync builds its own file list after that. It cannot protect a source that becomes empty during a pass, so set `max_delete` as the backstop, as the example config does. The guard ignores only the built-in excludes, not a job's own `excludes`. For a `delete: true` job whose `excludes` can match every entry, set `max_delete` to cap the deletions.

A skip counts as a success, so it never turns the container unhealthy. Each skip logs `skip empty source` at Warn, and the `sync cycle complete` line carries a `skipped` count. [Monitoring and alerts](monitoring.md) shows how to watch for it.

## Health

The healthcheck reads a marker the container sets after each pass. It is healthy when the last pass had no failed job and unhealthy when any job failed. A pass that cannot reload the config runs no job and also leaves the marker unhealthy. The container recovers on the next clean pass, with no restart.

- An empty-source skip counts as a success.
- A pass whose rsync ends with the vanished-files warning, exit 24, counts as a success. It logs `sync completed with vanished source files` at Warn with the exit code and the byte counts.
- The marker is written only after a pass has ended, from the exit status of every rsync it ran. An rsync that cannot start or exits non-zero is a failed job.

Within one container lifetime, health reflects the last pass, whatever triggered it. After a restart, built-in mode can restore health only from the last scheduled pass's record, because triggered passes never write one.

In built-in mode the container starts unhealthy and turns healthy after the startup pass. Size `healthcheck.start_period` for the time that first pass may take. The image allows 120 seconds. When the record shows a successful scheduled pass within the interval, the startup pass is skipped and the container starts healthy on that record. Built-in mode also sets a deadline of `2×SYNC_INTERVAL + jobs×SYNC_TIMEOUT`, so a schedule that stops running while the marker stays in place turns unhealthy in the end.

In external mode the container starts healthy, because it is idle and nothing has failed. Each triggered `sync` updates the marker, and no deadline is set, because a marker between sparse triggers must not expire.

## Shutdown

On a stop, the container turns unhealthy, stops taking triggers and stops the rsync that is running. A pass cut short this way with no failed job writes no marker and no record. A trigger still waiting in the queue gets a failed result, so it does not hang. The next pass covers the jobs that did not run.

rsync writes each file under a temporary name and moves it into place when it is complete. An interrupted job therefore leaves some files updated and none half-written. The next pass brings the rest up to date.
