package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"time"
)

// --- `sync` subcommand: the trigger client ---
//
// A thin synchronous client for the daemon's trigger socket: it submits one
// pass request and blocks until the daemon reports the pass result, exiting
// 0/1 with that result. The external trigger contract is unchanged from the
// previous design (`docker exec rsync docker-rsync-scheduler sync`, exit
// code = pass outcome, marker updated), but the pass itself now executes
// inside the daemon: its logs land on the container's log stream in every
// mode, and this process's output is only its own lifecycle lines.

// dialTimeout bounds the connection attempt: the daemon is PID 1 in the same
// container, so anything slower than instant means it is not accepting.
const dialTimeout = 5 * time.Second

// runClient performs one triggered pass via the daemon at socketPath and
// returns the process exit code: 0 on success, 1 on failure (including a
// rejected or cancelled request, or a daemon that cannot be reached).
func runClient(socketPath string) int {
	setupLogger()

	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(context.Background(), "unix", socketPath)
	if err != nil {
		slog.Error("cannot reach the scheduler daemon",
			"path", socketPath, "error", err,
			"hint", "the daemon (PID 1) owns all passes; check the container is up and this exec runs as the container's user (the socket is owner-only)")
		return 1
	}
	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(wireRequest{}); err != nil {
		slog.Error("cannot send sync request", "error", err)
		return 1
	}
	return awaitResult(conn)
}

// awaitResult consumes the daemon's event stream until the final result.
// Lifecycle is logged to THIS process's stderr — the trigger's own log (an
// Ofelia job log) — while the pass's full per-job output lands in the
// container log stream.
func awaitResult(conn io.Reader) int {
	dec := json.NewDecoder(conn)
	for {
		var ev wireEvent
		if err := dec.Decode(&ev); err != nil {
			slog.Error("connection lost before the pass completed (daemon stopped?)", "error", err)
			return 1
		}
		switch ev.Event {
		case eventQueued:
			slog.Info("triggered sync accepted")
		case eventStarted:
			slog.Info("triggered sync started",
				"logs", "full per-job output is on the container log stream")
		case eventDone:
			return finishResult(ev)
		default:
			slog.Debug("ignoring unknown event", "event", ev.Event)
		}
	}
}

// finishResult logs the final outcome and maps it to the exit code.
func finishResult(ev wireEvent) int {
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
