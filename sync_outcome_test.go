package main

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestReportPass_ranEmitsHeartbeat verifies the staleness heartbeat carries the
// job tally that Loki absence/skip alerts parse.
func TestReportPass_ranEmitsHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{
		disposition: passRan, trigger: "interval",
		total: 2, ok: 2, emptySkipped: 1, failed: 0, duration: 5 * time.Millisecond,
	})
	logs := buf.String()
	for _, want := range []string{"sync cycle complete", "trigger=interval", "ok=2", "skipped=1", "failed=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("heartbeat log = %q, want substring %q", logs, want)
		}
	}
}

// TestReportPass_interruptedDoesNotEmitHeartbeat verifies a shutdown-interrupted
// pass logs a distinct warn line and NOT the "sync cycle complete" heartbeat
// (so a drain never registers as a healthy completion) and never at error.
func TestReportPass_interruptedDoesNotEmitHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{
		disposition: passRan, trigger: "interval", interrupted: true,
		total: 1, ok: 0, failed: 1,
	})
	logs := buf.String()
	if !strings.Contains(logs, "sync cycle interrupted by shutdown") {
		t.Errorf("log = %q, want 'sync cycle interrupted by shutdown'", logs)
	}
	if strings.Contains(logs, "sync cycle complete") {
		t.Errorf("log = %q, want NO 'sync cycle complete' heartbeat on an interrupted pass", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("log = %q, want no ERROR on a shutdown interruption", logs)
	}
}

// TestReportPass_deferredEmitsLivenessNotHeartbeat verifies an overlap defer
// logs a liveness line with holder_age_ms and NOT the heartbeat (a stuck
// holder must still trip the absence alert).
func TestReportPass_deferredEmitsLivenessNotHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{disposition: passDeferred, trigger: "interval", holderAge: 90 * time.Second})
	logs := buf.String()
	if !strings.Contains(logs, "sync deferred, prior pass still running") {
		t.Errorf("log = %q, want the deferred liveness line", logs)
	}
	if !strings.Contains(logs, "holder_age_ms=90000") {
		t.Errorf("log = %q, want holder_age_ms=90000", logs)
	}
	if strings.Contains(logs, "sync cycle complete") {
		t.Errorf("log = %q, want NO heartbeat for a deferred pass", logs)
	}
}

// TestReportPass_lockErrorEmitsError verifies a lock-acquisition failure logs
// at error level so it trips the documented error alert.
func TestReportPass_lockErrorEmitsError(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{disposition: passLockErr, trigger: "interval", err: errors.New("flock boom")})
	logs := buf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("log = %q, want level=ERROR for a lock error", logs)
	}
	if !strings.Contains(logs, "cannot acquire sync lock") {
		t.Errorf("log = %q, want 'cannot acquire sync lock'", logs)
	}
}

// TestPassResult_exitStatus pins the process exit code each disposition maps to:
// a clean pass and a deferred pass exit 0, a pass with job failures and a lock
// error exit 1. The clean-passRan and passLockErr arms are otherwise unexercised
// by the runPass-level tests, so this is the direct guard on exitStatus.
func TestPassResult_exitStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  passResult
		want int
	}{
		{"ran clean is zero", passResult{disposition: passRan, failed: 0}, 0},
		{"ran with failures is one", passResult{disposition: passRan, failed: 2}, 1},
		{"lock error is one", passResult{disposition: passLockErr, err: errors.New("flock boom")}, 1},
		{"deferred is zero", passResult{disposition: passDeferred, holderAge: time.Second}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.res
			if got := r.exitStatus(); got != tt.want {
				t.Errorf("passResult{disposition:%d, failed:%d}.exitStatus() = %d, want %d",
					tt.res.disposition, tt.res.failed, got, tt.want)
			}
		})
	}
}

// TestPassResult_healthSignal pins healthSignal's full (set, healthy) contract
// across every disposition, including the interrupted-clean carve-out (a clean
// pass cut short by shutdown writes no signal so it cannot stamp a
// false-unhealthy marker). healthSignal is otherwise reached only indirectly
// through healthController.apply, which asserts the marker side effect rather
// than the return contract, so this is the direct guard.
func TestPassResult_healthSignal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		res         passResult
		wantSet     bool
		wantHealthy bool
	}{
		{"ran clean writes healthy", passResult{disposition: passRan, failed: 0}, true, true},
		{"ran with failures writes unhealthy", passResult{disposition: passRan, failed: 2}, true, false},
		{"interrupted clean carries no signal", passResult{disposition: passRan, failed: 0, interrupted: true}, false, false},
		{"interrupted with failure writes unhealthy", passResult{disposition: passRan, failed: 1, interrupted: true}, true, false},
		{"lock error writes unhealthy", passResult{disposition: passLockErr}, true, false},
		{"deferred carries no signal", passResult{disposition: passDeferred}, false, false},
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

// TestPassResult_exitStatus_unknownDispositionFailsSafe pins the fail-safe
// default arm of exitStatus (sync.go): an unhandled disposition must exit
// non-zero (1) rather than report success for an outcome no branch understood.
// The three real dispositions are covered by TestPassResult_exitStatus; this
// reaches the final return via an out-of-range passDisposition.
func TestPassResult_exitStatus_unknownDispositionFailsSafe(t *testing.T) {
	t.Parallel()
	r := passResult{disposition: passDisposition(99)}
	if got := r.exitStatus(); got != 1 {
		t.Errorf("exitStatus() for an unhandled disposition = %d, want 1 (fail safe non-zero)", got)
	}
}

// TestReportPass_unknownDispositionEmitsError pins reportPass's fail-safe
// default arm: a passResult carrying a disposition no case handles must still
// emit exactly one structured line, at ERROR level.
func TestReportPass_unknownDispositionEmitsError(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{disposition: passDisposition(99), trigger: "interval"})
	logs := buf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("log = %q, want level=ERROR for an unknown disposition", logs)
	}
	if !strings.Contains(logs, "sync pass completed with unknown disposition") {
		t.Errorf("log = %q, want the unknown-disposition fail-safe line", logs)
	}
}

// TestPassResult_healthSignal_unknownDispositionFailsSafe pins healthSignal's
// fail-safe default arm: an unhandled disposition must report (set=true, healthy=false)
// so the marker is written unhealthy rather than silently left stale.
func TestPassResult_healthSignal_unknownDispositionFailsSafe(t *testing.T) {
	t.Parallel()
	r := passResult{disposition: passDisposition(99)}
	set, healthy := r.healthSignal()
	if !set || healthy {
		t.Errorf("healthSignal() for an unhandled disposition = (set=%v, healthy=%v), want (true, false)", set, healthy)
	}
}
