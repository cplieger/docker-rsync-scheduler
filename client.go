package main

import (
	"errors"
	"log/slog"

	"github.com/cplieger/scheduler/v3/trigger"
)

// --- `sync` subcommand: the trigger client ---
//
// A thin adapter over the scheduler library's synchronous trigger client: it
// submits one pass request and blocks until the daemon reports the pass
// result, exiting 0/1 with that result. The external trigger contract is
// unchanged from the previous design (`docker exec rsync
// docker-rsync-scheduler sync`, exit code = pass outcome, marker updated),
// and the pass itself executes inside the daemon: its logs land on the
// container's log stream in every mode, while this process's output is only
// its own lifecycle lines. The library owns the transport (dial, wire order,
// failure taxonomy); this file owns the wording — the lifecycle lines an
// Ofelia job log captures.

// runClient performs one triggered pass via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(socketPath string) int {
	setupLogger()

	final, err := trigger.Submit(socketPath, struct{}{}, func(ev trigger.Event) {
		switch ev.Kind {
		case trigger.EventQueued:
			slog.Info("triggered sync accepted")
		case trigger.EventStarted:
			slog.Info("triggered sync started",
				"logs", "full per-job output is on the container log stream")
		}
	})
	switch {
	case errors.Is(err, trigger.ErrUnreachable):
		slog.Error("cannot reach the scheduler daemon",
			"path", socketPath, "error", err,
			"hint", "the daemon (PID 1) owns all passes; check the container is up and this exec runs as the container's user (the socket is owner-only)")
		return 1
	case errors.Is(err, trigger.ErrSend):
		slog.Error("cannot send sync request", "error", err)
		return 1
	case err != nil:
		slog.Error("connection lost before the pass completed (daemon stopped?)", "error", err)
		return 1
	}
	return finishResult(final)
}

// finishResult logs the final outcome and maps it to the exit code.
func finishResult(ev trigger.Event) int {
	if ev.OK {
		slog.Info("triggered sync complete", "duration_ms", ev.DurationMs)
		return 0
	}
	reason := ev.Reason
	if reason == "" {
		reason = "a sync job failed (see the container log stream)"
	}
	slog.Error("triggered sync failed", "duration_ms", ev.DurationMs, "reason", reason)
	return 1
}
