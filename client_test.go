package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"

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
// an immediate exit 1 (the trigger reports a failed job), never a hang.
func TestRunClient_DaemonUnreachableExitsOne(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if code := runClient(t.Context(), sock); code != 1 {
		t.Errorf("runClient() = %d with no daemon, want 1", code)
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

// TestRunClient_CancelledWaitWarnsWithoutPaging pins the arm split off from the
// catch-all: an operator interrupting the `docker exec` cancels the wait, not
// the pass, so the record is a WARN saying the pass continues in the daemon —
// never an ERROR asserting the connection was lost and the daemon stopped,
// which is what the published Loki fault rule pages on. Exit stays 1 (the
// trigger cannot report a result it never received). Not parallel: sets env and
// swaps the global slog default.
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

// TestFinishResult_LogsOutcome characterizes every final-result shape without a
// transport double: successful completion, a daemon-supplied failure reason,
// and the empty reason an ordinary failed pass produces.
func TestFinishResult_LogsOutcome(t *testing.T) {
	tests := []struct {
		name, message, reason string
		event                 trigger.Event
		code                  int
		level                 slog.Level
	}{
		{"success", "triggered sync complete", "", trigger.Event{OK: true, DurationMs: 37}, 0, slog.LevelInfo},
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
