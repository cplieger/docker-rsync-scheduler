package main

import (
	"math"
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

// applyPassHealth maps rsync's pass policy onto the shared shutdown latch.
// An interrupted pass with no job failure writes nothing: its partial success
// must not replace the last completed pass's health. Every other result writes
// its ordinary healthy verdict; the latch prevents a late healthy verdict from
// masking shutdown.
func applyPassHealth(latch *health.Latch, result *passResult) {
	healthy := result.healthy()
	if result.interrupted && healthy {
		return
	}
	latch.Set(healthy)
}
