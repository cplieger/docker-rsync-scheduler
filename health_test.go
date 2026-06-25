package main

import (
	"sync"
	"testing"
	"time"
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
	newHealthController(m).apply(&passResult{disposition: passRan, failed: 0})
	if v, w := m.state(); !v || w != 1 {
		t.Errorf("after clean pass: value=%v writes=%d, want true 1", v, w)
	}
}

func TestHealthController_applyRanFailedIsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	newHealthController(m).apply(&passResult{disposition: passRan, failed: 1})
	if v, _ := m.state(); v {
		t.Error("after failed pass: value=true, want false")
	}
}

func TestHealthController_applyInterruptedIsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	// A pass that finished all jobs clean but was cut by shutdown is unhealthy.
	newHealthController(m).apply(&passResult{disposition: passRan, failed: 0, interrupted: true})
	if v, _ := m.state(); v {
		t.Error("after interrupted pass: value=true, want false")
	}
}

func TestHealthController_applyLockErrIsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	newHealthController(m).apply(&passResult{disposition: passLockErr})
	if v, w := m.state(); v || w != 1 {
		t.Errorf("after lock error: value=%v writes=%d, want false 1", v, w)
	}
}

func TestHealthController_deferredDoesNotWrite(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.markInitial(true) // the only expected write
	hc.apply(&passResult{disposition: passDeferred, holderAge: time.Second})
	if _, w := m.state(); w != 1 {
		t.Errorf("a deferred pass wrote the marker: writes=%d, want 1 (markInitial only)", w)
	}
}

func TestHealthController_drainLatchBlocksLateHealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.beginDrain()                                        // value=false, draining latched
	hc.apply(&passResult{disposition: passRan, failed: 0}) // a late clean pass
	if v, _ := m.state(); v {
		t.Error("value=true after drain, want false (the drain latch must block a late healthy result)")
	}
}

func TestHealthController_drainLatchAllowsUnhealthy(t *testing.T) {
	t.Parallel()
	m := &fakeMarker{}
	hc := newHealthController(m)
	hc.beginDrain()
	hc.apply(&passResult{disposition: passLockErr}) // unhealthy is still allowed while draining
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
