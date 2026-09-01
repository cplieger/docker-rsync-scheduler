package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// TestReportPass_ranEmitsHeartbeat verifies the staleness heartbeat carries the
// job tally that Loki absence/skip alerts parse. The message is matched
// exactly (CountExact) because the Loki rules pin it verbatim.
func TestReportPass_ranEmitsHeartbeat(t *testing.T) {
	rec := capture.Default(t)
	reportPass(&passResult{
		trigger: "interval",
		total:   2, ok: 2, emptySkipped: 1, failed: 0, duration: 5 * time.Millisecond,
	})
	const heartbeat = "sync cycle complete"
	if got := rec.CountExact(heartbeat); got != 1 {
		t.Fatalf("heartbeat %q emitted %d time(s), want 1; logs = %q", heartbeat, got, rec.Messages())
	}
	for k, v := range map[string]string{"trigger": "interval", "ok": "2", "skipped": "1", "failed": "0"} {
		if !rec.HasAttr(heartbeat, k, v) {
			t.Errorf("heartbeat missing attr %s=%s", k, v)
		}
	}
}

func TestReportPass_failedEmitsHeartbeat(t *testing.T) {
	rec := capture.Default(t)
	reportPass(&passResult{
		trigger: "external",
		total:   2, ok: 1, failed: 1, duration: 5 * time.Millisecond,
	})

	const heartbeat = "sync cycle complete"
	if got := rec.CountLevel(slog.LevelInfo, heartbeat); got != 1 {
		t.Errorf("failed-pass heartbeat count = %d, want 1; logs = %q", got, rec.Messages())
	}
	if !rec.HasAttr(heartbeat, "failed", "1") {
		t.Errorf("failed-pass heartbeat missing failed=1; logs = %q", rec.Messages())
	}
}

// TestReportPass_interruptedDoesNotEmitHeartbeat verifies a shutdown-interrupted
// pass logs a distinct warn line and NOT the "sync cycle complete" heartbeat
// (so a drain never registers as a healthy completion) and never at error.
func TestReportPass_interruptedDoesNotEmitHeartbeat(t *testing.T) {
	rec := capture.Default(t)
	reportPass(&passResult{
		trigger: "interval", interrupted: true,
		total: 1, ok: 0, failed: 1,
	})
	if !rec.Contains("sync cycle interrupted by shutdown") {
		t.Errorf("logs = %q, want 'sync cycle interrupted by shutdown'", rec.Messages())
	}
	if got := rec.Count("sync cycle complete"); got != 0 {
		t.Errorf("logs = %q, want NO 'sync cycle complete' heartbeat on an interrupted pass", rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, ""); got != 0 {
		t.Errorf("%d ERROR record(s) on a shutdown interruption, want none; logs = %q", got, rec.Messages())
	}
}
