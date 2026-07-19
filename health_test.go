package main

import (
	"sync"
	"testing"
)

// fakeMarker records marker writes so tests can assert the health controller's
// decisions without touching the filesystem. Fields are ordered largest-first
// for fieldalignment.
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

func TestHealthController_applyRanCleanIsHealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	newHealthController(m).apply(&passResult{failed: 0})
	if v, w := m.state(); !v || w != 1 {
		t.Errorf("after clean pass: value=%v writes=%d, want true 1", v, w)
	}
}

func TestHealthController_applyRanFailedIsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	newHealthController(m).apply(&passResult{failed: 1})
	if v, _ := m.state(); v {
		t.Error("after failed pass: value=true, want false")
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
	hc.beginDrain()                  // value=false, draining latched
	hc.apply(&passResult{failed: 0}) // a late clean pass
	if v, _ := m.state(); v {
		t.Error("value=true after drain, want false (the drain latch must block a late healthy result)")
	}
}

func TestHealthController_drainLatchAllowsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.beginDrain()
	hc.apply(&passResult{failed: 1}) // unhealthy is still allowed while draining
	if v, _ := m.state(); v {
		t.Error("value=true, want false")
	}
}

func TestHealthController_markInitialIgnoredWhileDraining(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.beginDrain()
	hc.markInitial(true)
	if v, _ := m.state(); v {
		t.Error("markInitial(true) flipped to healthy during drain; want false")
	}
}
