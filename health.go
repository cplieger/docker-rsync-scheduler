package main

import (
	"math"
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

// probeOptions returns the healthcheck probe's freshness policy. Built-in mode
// arms a max-age deadline (two intervals plus every job's full SYNC_TIMEOUT) so
// a marker never refreshed eventually probes unhealthy; external mode stays
// unbounded, because a marker between sparse triggers must not expire. An
// unreadable or unparseable config disarms rather than risking a restart loop.
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
	maxAge := maxAgeFor(interval, loadSyncTimeout(), len(cfg.Jobs))
	return []health.ProbeOption{health.WithMaxAge(maxAge)}
}

// maxAgeFor returns the freshness lease for the given number of jobs at this
// interval and per-job timeout. It saturates instead of wrapping: a lease
// longer than a time.Duration can hold means "never expires", where an int64
// wrap would turn it into seconds and restart a healthy container on every
// poll.
func maxAgeFor(interval, timeout time.Duration, jobs int) time.Duration {
	const maxDur = time.Duration(math.MaxInt64)
	if interval > maxDur/2 {
		return maxDur
	}
	lease := 2 * interval
	if jobs > 0 && timeout > (maxDur-lease)/time.Duration(jobs) {
		return maxDur
	}
	return lease + timeout*time.Duration(jobs)
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
// back to healthy, and an interrupted-clean pass — no job failed, and
// shutdown coincided with pass-end — never writes at all. These two guarantees
// are what make the marker reflect the last real pass outcome instead of
// whichever goroutine happened to write last.
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
	if r.interrupted && r.failed == 0 {
		return
	}
	healthy := r.healthy()
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
