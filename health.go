package main

import (
	"sync"

	"github.com/cplieger/health"
)

// healthMarkerPath is where the health marker file lives. Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which
// stats this path. /tmp is conventional because read-only containers
// mount it as tmpfs.
const healthMarkerPath = health.DefaultPath

// healthMarker is the marker behaviour healthController depends on.
// *health.Marker satisfies it; tests inject a fake to observe writes.
type healthMarker interface {
	Set(healthy bool)
}

// healthController is the single writer of the health marker. Every write
// funnels through its mutex, and it enforces one invariant the bare marker
// cannot: once shutdown begins, health is monotonic toward unhealthy. A pass
// that finishes right as the container is draining can never flip the marker
// back to healthy, and an overlap-deferred pass — which carries no health
// signal of its own — never writes at all. These two guarantees are what make
// the marker reflect the last real pass outcome instead of whichever
// goroutine happened to write last.
type healthController struct {
	marker   healthMarker
	mu       sync.Mutex
	draining bool
}

// newHealthController returns a controller that writes through marker.
func newHealthController(marker healthMarker) *healthController {
	return &healthController{marker: marker}
}

// markInitial sets the pre-pass state: unhealthy for the built-in scheduler
// (no pass has run yet, so the first completed pass flips it) and healthy for
// the idle external-trigger container (nothing has failed). It is a no-op once
// draining.
func (h *healthController) markInitial(healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining {
		return
	}
	h.marker.Set(healthy)
}

// apply translates a pass result into a marker write. A deferred pass carries
// no health signal and is ignored; a ran or lock-error pass writes its health
// value, unless shutdown has begun and that value is healthy (the drain latch
// stops a late success from masking shutdown).
func (h *healthController) apply(r *passResult) {
	set, healthy := r.healthSignal()
	if !set {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.draining && healthy {
		return
	}
	h.marker.Set(healthy)
}

// beginDrain latches shutdown and marks unhealthy immediately, so observers
// see the draining signal before in-flight work finishes. After it, apply and
// markInitial can never restore healthy.
func (h *healthController) beginDrain() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
	h.marker.Set(false)
}
