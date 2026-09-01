package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/scheduler/v4/trigger"
	"github.com/cplieger/slogx/capture"
)

// The broker mechanics (queue semantics, socket hygiene, wire ordering,
// backpressure rejection, undecodable requests, departed clients) are the
// scheduler library's and are tested in scheduler/v4/trigger. What stays THIS
// app's is the executor's pass-outcome policy: its exit-code surface is pinned
// here, end-to-end over the real socket; the drain-versus-cancel split is
// pinned in daemon_test.go.

// TestRunClient_ExitCodesOverRealSocket pins the trigger contract end-to-end
// (the same `sync` → exit 0/1 surface Ofelia consumes): a clean pass exits 0,
// a failing pass exits 1. Not parallel: sets env.
func TestRunClient_ExitCodesOverRealSocket(t *testing.T) {
	t.Run("clean pass exits zero", func(t *testing.T) {
		writeValidCfg(t, t.TempDir()) // empty source: clean skip pass
		sock, _ := startTestServer(t, fixedRunner("true"))
		if code := runClient(t.Context(), sock); code != 0 {
			t.Errorf("runClient() = %d, want 0", code)
		}
	})
	t.Run("failed pass exits one", func(t *testing.T) {
		writeValidCfg(t, newRunJobSource(t)) // non-empty source: the failing runner executes
		sock, _ := startTestServer(t, fixedRunner("false"))
		if code := runClient(t.Context(), sock); code != 1 {
			t.Errorf("runClient() = %d, want 1", code)
		}
	})
}

// TestRunClient_DaemonUnreachableExitsOne pins the no-daemon failure mode:
// exit 1, never a hang, plus the phase-specific ERROR naming the socket
// path and a remedy — neither of which the generic missing-result arm
// carries. Not parallel: swaps the global slog default.
func TestRunClient_DaemonUnreachableExitsOne(t *testing.T) {
	rec := capture.Default(t)
	sock := filepath.Join(t.TempDir(), "absent.sock")

	if code := runClient(t.Context(), sock); code != 1 {
		t.Errorf("runClient() = %d with no daemon, want 1", code)
	}

	const message = "cannot reach the scheduler daemon"
	if count := rec.CountLevel(slog.LevelError, message); count != 1 {
		t.Errorf("%q ERROR records = %d, want 1; logs = %q", message, count, rec.Messages())
	}
	if !rec.HasAttr(message, "path", sock) {
		t.Errorf("%q missing path=%q; logs = %q", message, sock, rec.Messages())
	}
	if value, ok := rec.AttrValueExact(message, "error"); !ok || value == "" {
		t.Errorf("%q error attribute = %q, %v, want non-empty, true", message, value, ok)
	}
	if value, ok := rec.AttrValueExact(message, "hint"); !ok || value == "" {
		t.Errorf("%q hint attribute = %q, %v, want non-empty, true", message, value, ok)
	}
}

// TestRunClient_LogsLifecycleOverRealSocket pins the two intermediate
// lifecycle records the `sync` client relays to its exec caller's job log, in
// wire order, plus the started record's pointer at the container log stream
// (which is where the pass's own per-job output goes). Not parallel: sets env
// and swaps the global slog default.
func TestRunClient_LogsLifecycleOverRealSocket(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // empty source: clean skip pass
	rec := capture.Default(t)
	sock, _ := startTestServer(t, fixedRunner("true"))

	if code := runClient(t.Context(), sock); code != 0 {
		t.Fatalf("runClient() = %d, want 0", code)
	}

	want := []string{"triggered sync accepted", "triggered sync started"}
	var got []string
	for _, r := range rec.Records() {
		if slices.Contains(want, r.Message) {
			got = append(got, r.Message)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("runClient() lifecycle records = %v, want %v; logs = %q", got, want, rec.Messages())
	}
	const pointer = "full per-job output is on the container log stream"
	if !rec.HasAttr("triggered sync started", "logs", pointer) {
		t.Errorf("started record missing logs=%q; logs = %q", pointer, rec.Messages())
	}
}

func TestRunClient_ConnectionLostReportsMissingResult(t *testing.T) {
	rec := capture.Default(t)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		var request struct{}
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			serverDone <- decodeErr
			return
		}
		serverDone <- json.NewEncoder(conn).Encode(trigger.Event{Kind: trigger.EventQueued})
	}()

	// No deadline: a DeadlineExceeded reaches the same catch-all arm and
	// would satisfy the assertion below, so the truncated stream must be
	// the only route to it.
	if code := runClient(t.Context(), sock); code != 1 {
		t.Errorf("runClient() after a truncated event stream = %d, want 1", code)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("scripted trigger peer: %v", err)
	}

	var failures []string
	for _, record := range rec.Records() {
		if record.Level == slog.LevelError {
			failures = append(failures, record.Message)
		}
	}
	want := []string{"the pass did not report a result"}
	if !slices.Equal(failures, want) {
		t.Errorf("runClient() ERROR messages = %v, want %v; logs = %q", failures, want, rec.Messages())
	}
}

// TestRunClient_CancelledWaitWarnsWithoutPaging pins a cancelled wait as a
// WARN rather than an ERROR: an operator's own interrupt is not a fault
// anyone acts on. Exit stays 1 because the trigger received no final
// result. Not parallel: sets env and swaps the global slog default.
func TestRunClient_CancelledWaitWarnsWithoutPaging(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	rec := capture.Default(t)
	sock, _ := startTestServer(t, fixedRunner("true"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the operator's interrupt, landing before the wait resolves

	if code := runClient(ctx, sock); code != 1 {
		t.Errorf("runClient() on a cancelled wait = %d, want 1", code)
	}
	const message = "this trigger was interrupted while waiting"
	if got := rec.CountLevel(slog.LevelWarn, message); got != 1 {
		t.Errorf("%q WARN records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, ""); got != 0 {
		t.Errorf("%d ERROR record(s) on a cancelled wait, want none; logs = %q", got, rec.Messages())
	}
}

func TestRunClient_CancelledAfterSendReportsAcceptanceUnknown(t *testing.T) {
	rec := capture.Default(t)
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}

	requestRead := make(chan error, 1)
	release := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			requestRead <- acceptErr
			return
		}
		defer func() { _ = conn.Close() }()
		var request struct{}
		decodeErr := json.NewDecoder(conn).Decode(&request)
		requestRead <- decodeErr
		if decodeErr == nil {
			<-release
		}
	}()
	t.Cleanup(func() {
		close(release)
		_ = ln.Close()
		<-serverDone
	})

	ctx, cancel := context.WithCancel(t.Context())
	clientDone := make(chan int, 1)
	go func() { clientDone <- runClient(ctx, sock) }()

	select {
	case err := <-requestRead:
		if err != nil {
			t.Fatalf("decode request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not send its request")
	}
	cancel()

	select {
	case code := <-clientDone:
		if code != 1 {
			t.Errorf("runClient() after cancellation = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runClient() did not return after cancellation")
	}

	const message = "this trigger was interrupted while waiting"
	records := rec.Records()
	if len(records) != 1 {
		t.Fatalf("runClient() logged %d records, want 1; logs = %q", len(records), rec.Messages())
	}
	if records[0].Level != slog.LevelWarn || records[0].Message != message {
		t.Errorf("runClient() record = (%q, %s), want (%q, WARN)", records[0].Message, records[0].Level, message)
	}
	if state, ok := rec.AttrValueExact(message, "acceptance"); !ok || state != "unknown" {
		t.Errorf("runClient() acceptance = %q, %v, want unknown, true", state, ok)
	}
}

// TestRunClient_CancelledAfterAcceptanceReportsObserved pins the other half
// of the cancellation arm: an interrupt landing AFTER the queued event was
// relayed reports acceptance=observed, so a reader can tell "a pass is
// running and I stopped watching" from "nothing was queued". Not parallel:
// sets env and swaps the global slog default.
func TestRunClient_CancelledAfterAcceptanceReportsObserved(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))
	rec := capture.Default(t)
	release := make(chan struct{})
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		<-release
		return exec.CommandContext(ctx, "true")
	}
	sock, _ := startTestServer(t, runner)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	clientDone := make(chan int, 1)
	go func() { clientDone <- runClient(ctx, sock) }()

	waitFor(t, 2*time.Second, func() bool {
		return rec.CountExact("triggered sync accepted") == 1
	}, "client did not observe queue acceptance")
	cancel()

	select {
	case code := <-clientDone:
		if code != 1 {
			t.Errorf("runClient() after observed acceptance and cancellation = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runClient() did not return after cancellation")
	}
	const message = "this trigger was interrupted while waiting"
	if state, ok := rec.AttrValueExact(message, "acceptance"); !ok || state != "observed" {
		t.Errorf("runClient() acceptance = %q, %v, want observed, true", state, ok)
	}
}

// TestFinishResult_LogsOutcome characterizes every final-result shape without a
// transport double: ordinary and caveated success, a daemon-supplied failure
// reason, and the empty reason an ordinary failed pass produces.
func TestFinishResult_LogsOutcome(t *testing.T) {
	tests := []struct {
		name, message, reason string
		event                 trigger.Event
		code                  int
		level                 slog.Level
	}{
		{"success", "triggered sync complete", "", trigger.Event{OK: true, DurationMs: 37}, 0, slog.LevelInfo},
		{"interrupted_clean", "triggered sync ended with a caveat", "pass cut short by shutdown; remaining jobs did not run", trigger.Event{OK: true, DurationMs: 37, Reason: "pass cut short by shutdown; remaining jobs did not run"}, 0, slog.LevelWarn},
		{"daemon_reason", "triggered sync failed", "config reload failed", trigger.Event{DurationMs: 37, Reason: "config reload failed"}, 1, slog.LevelError},
		{"plain_failure", "triggered sync failed", "a sync job failed (see the container log stream)", trigger.Event{DurationMs: 37}, 1, slog.LevelError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			if code := finishResult(tt.event); code != tt.code {
				t.Errorf("finishResult(%+v) = %d, want %d", tt.event, code, tt.code)
			}
			records := rec.Records()
			if len(records) != 1 {
				t.Fatalf("finishResult(%+v) logged %d records, want 1", tt.event, len(records))
			}
			if records[0].Message != tt.message || records[0].Level != tt.level {
				t.Errorf("finishResult(%+v) record = (%q, %s), want (%q, %s)", tt.event, records[0].Message, records[0].Level, tt.message, tt.level)
			}
			if duration, ok := rec.AttrValueExact(tt.message, "duration_ms"); !ok || duration != "37" {
				t.Errorf("finishResult(%+v) duration_ms = %q, %v, want 37, true", tt.event, duration, ok)
			}
			reason, hasReason := rec.AttrValueExact(tt.message, "reason")
			if hasReason != (tt.reason != "") || reason != tt.reason {
				t.Errorf("finishResult(%+v) reason = %q, %v, want %q, %v", tt.event, reason, hasReason, tt.reason, tt.reason != "")
			}
		})
	}
}
