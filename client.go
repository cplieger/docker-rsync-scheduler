package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"

	"github.com/cplieger/scheduler/v4/trigger"
)

// --- `sync` subcommand: the trigger client ---
//
// The library owns the transport (dial, wire order, failure taxonomy); this
// file owns the wording.

// runClient performs one triggered pass via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(ctx context.Context, socketPath string) int {
	final, err := trigger.Submit(ctx, socketPath, struct{}{}, func(ev trigger.Event) {
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
			"hint", "nothing is accepting on this socket: the daemon may be starting or shutting down, or this exec is not the container's user (the socket is owner-only)")
		return 1
	case errors.Is(err, trigger.ErrSend):
		slog.Error("cannot send sync request", "error", err)
		return 1
	case errors.Is(err, context.Canceled):
		slog.Warn("this trigger was interrupted while waiting; the pass continues in the daemon", "error", err)
		return 1
	case err != nil:
		slog.Error("the pass did not report a result", "error", err)
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
	reason := cmp.Or(ev.Reason, "a sync job failed (see the container log stream)")
	slog.Error("triggered sync failed", "duration_ms", ev.DurationMs, "reason", reason)
	return 1
}
