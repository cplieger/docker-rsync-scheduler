package main

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fakeMarker records marker writes so tests can assert the health controller's
// decisions without touching the filesystem.
type fakeMarker struct {
	mu     sync.Mutex
	writes int
	value  bool
}

func (m *fakeMarker) Set(healthy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.value = healthy
	m.writes++
}

func (m *fakeMarker) state() (value bool, writes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.value, m.writes
}

// TestMaxAgeFor_saturatesOverflow pins the published saturating contract at
// both overflow boundaries: an interval past half the Duration range, and a
// jobs*timeout product that would carry the sum past it. Either guard removed
// wraps — negative for the interval arm (disarming health.WithMaxAge), a short
// positive lease for the jobs arm (restarting a healthy container) — so the
// expected value is the contract's own maximum, not arithmetic recomputed from
// the body.
func TestMaxAgeFor_saturatesOverflow(t *testing.T) {
	t.Parallel()
	const maxDur = time.Duration(math.MaxInt64)
	tests := []struct {
		name              string
		interval, timeout time.Duration
		jobs              int
	}{
		{name: "interval", interval: maxDur/2 + 1, timeout: time.Minute, jobs: 1},
		{name: "job_timeouts", interval: time.Hour, timeout: maxDur, jobs: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := maxAgeFor(tt.interval, tt.timeout, tt.jobs); got != maxDur {
				t.Errorf("maxAgeFor(%v, %v, %d) = %v, want %v", tt.interval, tt.timeout, tt.jobs, got, maxDur)
			}
		})
	}
}

func TestHealthController_applyInterruptedCleanDoesNotDowngrade(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.markInitial(true) // last real state: healthy (the only expected write)
	// A pass where every job succeeded but a shutdown signal coincided
	// (interrupted, failed==0) must NOT write the marker: it leaves the last
	// real value in place rather than a false-unhealthy that, in external mode,
	// would outlive the interruption until the next sync.
	hc.apply(&passResult{failed: 0, interrupted: true})
	if v, w := m.state(); !v || w != 1 {
		t.Errorf("after interrupted-clean pass: value=%v writes=%d, want true 1 (no downgrade; markInitial only)", v, w)
	}
}

func TestHealthController_applyInterruptedWithFailureIsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	// An interrupted pass that ALSO had a real job failure still writes
	// unhealthy: only the zero-failure interrupted case is spared the downgrade.
	newHealthController(m).apply(&passResult{failed: 1, interrupted: true})
	if v, w := m.state(); v || w != 1 {
		t.Errorf("after interrupted-with-failure pass: value=%v writes=%d, want false 1", v, w)
	}
}

func TestHealthController_markUnhealthyWrites(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.markInitial(true)
	// markUnhealthy is the executor's out-of-pass failure write (a config
	// reload failure): unconditional, allowed before and during drain.
	hc.markUnhealthy()
	if v, w := m.state(); v || w != 2 {
		t.Errorf("after markUnhealthy: value=%v writes=%d, want false 2", v, w)
	}
}

func TestHealthController_drainLatchBlocksLateHealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.markInitial(true)
	hc.beginDrain()
	hc.apply(&passResult{failed: 0})
	if v, w := m.state(); v || w != 2 {
		t.Errorf("after drain and late clean pass: value=%v writes=%d, want false 2", v, w)
	}
}
