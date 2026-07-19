package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// --- Trigger socket server ---
//
// The daemon listens on an in-container unix socket; each connection is one
// pass request (see protocol.go). The socket replaces the whole cross-process
// /tmp flock lattice of the previous design: mutual exclusion is the
// executor's single goroutine, shutdown reaches waiting clients as explicit
// cancellation results, and completion is an ordinary result delivery — no
// flock, no deferred/lock-error dispositions, no holder-age forensics.

const (
	// requestReadTimeout bounds how long a connected client may take to send
	// its request line, so a silent connection cannot hold a handler
	// goroutine (and shutdown) hostage.
	requestReadTimeout = 30 * time.Second
	// eventWriteTimeout bounds each status write, so a dead client cannot
	// block a handler.
	eventWriteTimeout = 10 * time.Second
)

// listenTrigger binds the unix socket at path with owner-only permissions.
// A stale socket file from a SIGKILLed predecessor is removed first (bind
// fails on an existing path otherwise); /tmp is per-container, so the stale
// file can only be our own previous life's.
func listenTrigger(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	// Owner-only: connecting requires write permission on the socket file,
	// which scopes triggering to the container's own user — the same
	// authority boundary `docker exec` already enforces, now failing loudly
	// at connect for a mismatched exec user.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// triggerServer accepts pass requests and bridges them onto the queue.
type triggerServer struct {
	queue *runQueue
	// handlers tracks per-connection goroutines so shutdown can wait for
	// every accepted request to receive its final event before the daemon
	// exits. Bounded: every submitted request is guaranteed a result, and a
	// not-yet-submitted connection is bounded by requestReadTimeout.
	handlers sync.WaitGroup
}

// serve accepts connections until the listener is closed (daemon shutdown).
func (s *triggerServer) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("trigger socket accept failed", "error", err)
			continue
		}
		s.handlers.Go(func() {
			defer func() { _ = conn.Close() }()
			s.handle(conn)
		})
	}
}

// handle serves one connection: decode the request, submit it, stream events.
func (s *triggerServer) handle(conn net.Conn) {
	var req wireRequest
	_ = conn.SetReadDeadline(time.Now().Add(requestReadTimeout))
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		slog.Warn("trigger request rejected: undecodable", "error", err)
		writeEvent(conn, wireEvent{Event: eventDone, OK: false, Reason: "undecodable request"})
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	r := newRequest("external")
	if err := s.queue.submit(r); err != nil {
		slog.Warn("trigger request rejected", "reason", err)
		writeEvent(conn, wireEvent{Event: eventDone, OK: false, Reason: err.Error()})
		return
	}
	slog.Info("triggered sync queued")
	writeEvent(conn, wireEvent{Event: eventQueued})

	// Relay the start signal if it comes; a request cancelled before starting
	// delivers its result without ever starting, so wait on both.
	started := r.started
	for {
		select {
		case <-started:
			writeEvent(conn, wireEvent{Event: eventStarted})
			started = nil // block this case from now on; wait for the result
		case out := <-r.result:
			writeEvent(conn, wireEvent{
				Event:      eventDone,
				OK:         out.ok,
				DurationMs: out.duration.Milliseconds(),
				Reason:     out.reason,
			})
			return
		}
	}
}

// writeEvent sends one status line, best-effort: a departed client only
// forfeits its own visibility (the pass itself is daemon-owned and its result
// delivery never blocks on the connection).
func writeEvent(conn net.Conn, ev wireEvent) {
	_ = conn.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if err := json.NewEncoder(conn).Encode(ev); err != nil {
		slog.Debug("trigger event write failed (client gone?)", "event", ev.Event, "error", err)
	}
}
