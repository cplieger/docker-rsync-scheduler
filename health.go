package main

import (
	"math"
	"sync"
	"time"

	"github.com/cplieger/health"
)

// healthMarkerPath is where the health marker file lives. Docker's
// HEALTHCHECK re-invokes the binary with the `health` subcommand, which
// stats this path. The daemon is the marker's single writer.
const healthMarkerPath = health.DefaultPath

// probeOptions returns the healthcheck probe's freshness policy. Built-in
// mode arms a max-age deadline (two intervals plus every job's SYNC_TIMEOUT)
// so a marker never refreshed eventually probes unhealthy; external mode
// stays unbounded since a marker between sparse triggers must not expire. An
// unreadable or unparseable config disarms the deadline.
func probeOptions() []health.ProbeOption {
	interval, scheduleEnabled := loadInterval()
	if !scheduleEnabled {
		return nil
	}
	data, err := readCappedConfig(configPath())
	if err != nil {
		return nil
	}
	cfg, err := parseConfig(data)
	if err != nil {
		return nil
	}
	maxAge := (freshness{
		interval: interval,
		timeout:  loadSyncTimeout(),
		jobs:     len(cfg.Jobs),
	}).lease()
	return []health.ProbeOption{health.WithMaxAge(maxAge)}
}

type freshness struct {
	interval time.Duration
	timeout  time.Duration
	jobs     int
}

// lease returns a saturating freshness deadline. A duration overflow would
// otherwise produce a short lease and a permanent false-unhealthy report.
func (f freshness) lease() time.Duration {
	const maxDur = time.Duration(math.MaxInt64)
	if f.interval > maxDur/2 {
		return maxDur
	}
	lease := 2 * f.interval
	if f.jobs > 0 && f.timeout > (maxDur-lease)/time.Duration(f.jobs) {
		return maxDur
	}
	return lease + f.timeout*time.Duration(f.jobs)
}

// healthMarker is the marker behaviour healthController depends on.
// *health.Marker satisfies it; tests inject a fake to observe writes.
type healthMarker interface {
	Set(healthy bool)
}

// healthController owns health-state writes after boot. It enforces one
// invariant the bare marker cannot: once shutdown begins, health is
// monotonic toward unhealthy — a pass finishing during drain can never flip
// it back, and an interrupted-clean pass never writes at all. Two writes
// bypass its mutex but are safe by ordering: runDaemon clears stale state
// before construction and defers marker cleanup until after the executor
// stops.
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
// the idle external-trigger container (nothing has failed).
func (h *healthController) markInitial(healthy bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.marker.Set(healthy)
}

// apply translates a pass result into a marker write, and holds both drain
// rules: an interrupted-clean pass writes nothing (no job failed, so there is
// no state to record and a drain must not emit a healthy transition), and no
// pass writes healthy once shutdown has begun.
func (h *healthController) apply(r *passResult) {
	healthy := r.healthy()
	if r.interrupted && healthy {
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
// see the draining signal before in-flight work finishes. After it, apply can
// never restore healthy.
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
