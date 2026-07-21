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

// TestPassResult_exitStatus pins the pass status each outcome maps to (the
// exit code a triggering `sync` client reports): a clean pass exits 0, a pass
// with job failures exits 1, and the interrupted variants agree with
// healthSignal (interrupted-clean is a graceful drain, not a failure).
func TestPassResult_exitStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  passResult
		want int
	}{
		{"ran clean is zero", passResult{failed: 0}, 0},
		{"ran with failures is one", passResult{failed: 2}, 1},
		{"interrupted clean is zero", passResult{failed: 0, interrupted: true}, 0},
		{"interrupted with failure is one", passResult{failed: 1, interrupted: true}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.res
			if got := r.exitStatus(); got != tt.want {
				t.Errorf("passResult{failed:%d, interrupted:%v}.exitStatus() = %d, want %d",
					tt.res.failed, tt.res.interrupted, got, tt.want)
			}
		})
	}
}

// TestPassResult_healthSignal pins healthSignal's full (set, healthy) contract,
// including the interrupted-clean carve-out (a clean pass cut short by
// shutdown writes no signal so it cannot stamp a false-unhealthy marker).
// healthSignal is otherwise reached only indirectly through
// healthController.apply, which asserts the marker side effect rather than
// the return contract, so this is the direct guard.
func TestPassResult_healthSignal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		res         passResult
		wantSet     bool
		wantHealthy bool
	}{
		{"ran clean writes healthy", passResult{failed: 0}, true, true},
		{"ran with failures writes unhealthy", passResult{failed: 2}, true, false},
		{"interrupted clean carries no signal", passResult{failed: 0, interrupted: true}, false, false},
		{"interrupted with failure writes unhealthy", passResult{failed: 1, interrupted: true}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.res
			set, healthy := r.healthSignal()
			if set != tt.wantSet || healthy != tt.wantHealthy {
				t.Errorf("healthSignal() = (set=%v, healthy=%v), want (set=%v, healthy=%v)",
					set, healthy, tt.wantSet, tt.wantHealthy)
			}
		})
	}
}
