package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
)

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
