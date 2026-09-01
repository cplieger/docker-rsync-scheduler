package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
)

// TestMaxAgeFor_saturatesOverflow pins the published saturating contract at
// both overflow boundaries: an interval past half the Duration range, and a
// jobs*timeout product that would carry the sum past it. Either guard removed
// wraps — negative for the interval arm (disarming health.WithMaxAge), or a
// short positive lease for the jobs arm, which reports false-unhealthy until
// the next clean pass — so the expected value is the contract's own maximum,
// not arithmetic recomputed from the body.
func TestMaxAgeFor_saturatesOverflow(t *testing.T) {
	t.Parallel()
	const maxDur = time.Duration(math.MaxInt64)
	tests := []struct {
		name      string
		freshness freshness
	}{
		{name: "interval", freshness: freshness{interval: maxDur/2 + 1, timeout: time.Minute, jobs: 1}},
		{name: "job_timeouts", freshness: freshness{interval: time.Hour, timeout: maxDur, jobs: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.freshness.lease(); got != maxDur {
				t.Errorf("freshness(%+v).lease() = %v, want %v", tt.freshness, got, maxDur)
			}
		})
	}
}

// TestApplyPassHealth_interruptedCleanDoesNotWrite pins the app-specific
// policy that stays in front of health.Latch. A partially completed pass with
// no failed job keeps the last completed pass's state; writing true would
// refresh the marker mtime and falsely claim a full pass just completed.
func TestApplyPassHealth_interruptedCleanDoesNotWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "healthy")
	state := health.NewLatch(health.NewMarker(path))
	state.Set(true)
	old := time.Unix(1, 0)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("backdate marker: %v", err)
	}

	applyPassHealth(state, &passResult{interrupted: true})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat marker after interrupted-clean pass: %v", err)
	}
	if got := info.ModTime(); !got.Equal(old) {
		t.Errorf("marker mtime after interrupted-clean pass = %v, want unchanged %v", got, old)
	}
}

// TestApplyPassHealth_interruptedFailureWritesUnhealthy is the other half of
// the carve-out: interruption does not hide a real job failure.
func TestApplyPassHealth_interruptedFailureWritesUnhealthy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "healthy")
	state := health.NewLatch(health.NewMarker(path))
	state.Set(true)

	applyPassHealth(state, &passResult{failed: 1, interrupted: true})

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("marker after interrupted failed pass: stat error = %v, want not-exist", err)
	}
}
