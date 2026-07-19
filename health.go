package main

import (
	"os"
	"sync"
	"time"

	"github.com/cplieger/health"
)

// healthMarkerPath is where the health marker file lives. Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which
// stats this path. /tmp is conventional because read-only containers
// mount it as tmpfs. The daemon — the single owner of every pass — is the
// marker's single writer.
const healthMarkerPath = health.DefaultPath

// probeOptions returns the healthcheck probe's freshness policy. Built-in
// mode arms a max-age deadline: the executor refreshes the marker after
// every pass, so a marker present but never refreshed means the interval
// loop is wedged and the container should probe unhealthy and restart. Two
// intervals plus the worst-case pass duration (every configured job hitting
// its full per-job SYNC_TIMEOUT) is generous headroom for a slow-but-
// progressing loop. External mode keeps no deadline: an idle container
// between sparse triggers is healthy, and a trigger-written marker must not
// expire. The config read is quiet and best-effort — an unreadable or
// invalid config disarms the deadline (bare marker probe, the pre-rewrite
// behavior) rather than risking a false-unhealthy restart loop.
func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	info, err := os.Stat(configPath())
	if err != nil || info.Size() > configCapBytes {
		return nil
	}
	data, err := os.ReadFile(configPath()) // #nosec G304 -- trusted, operator-mounted config path
	if err != nil {
		return nil
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return nil
	}
	maxAge := 2*interval + time.Duration(len(cfg.Jobs))*loadSyncTimeout()
	return []health.ProbeOption{health.WithMaxAge(maxAge)}
}

// healthMarker is the marker behaviour healthController depends on.
// *health.Marker satisfies it; tests inject a fake to observe writes.
type healthMarker interface {
	Set(healthy bool)
}

// healthController is the single writer of the health marker. Every write
// funnels through its mutex, and it enforces one invariant the bare marker
// cannot: once shutdown begins, health is monotonic toward unhealthy. A pass
// that finishes right as the container is draining can never flip the marker
// back to healthy, and an interrupted-clean pass — which carries no health
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

// apply translates a pass result into a marker write. An interrupted-clean
// pass carries no health signal and is ignored (see passResult.healthSignal);
// any other pass writes its health value, unless shutdown has begun and that
// value is healthy (the drain latch stops a late success from masking
// shutdown).
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

// markUnhealthy writes an unconditional unhealthy marker for a failure that
// happens outside a pass (the executor's per-pass config reload failing).
// Unhealthy writes are always permitted — draining or not — so this takes
// the lock only to serialize with the other writers.
func (h *healthController) markUnhealthy() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.marker.Set(false)
}
