package main

import "github.com/cplieger/health"

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
	lease := health.Lease{
		Interval: interval,
		Cycles:   2,
		Timeout:  loadSyncTimeout(),
		Attempts: len(cfg.Jobs),
	}
	return []health.ProbeOption{health.WithMaxAge(lease.Duration())}
}

// applyPassHealth maps rsync's pass policy onto the shared shutdown latch.
// An unvouchable pass writes nothing: its partial success must not replace
// the last completed pass's health. Every other result writes its ordinary
// healthy verdict; the latch prevents a late healthy verdict from masking
// shutdown.
func applyPassHealth(latch *health.Latch, result *passResult) {
	if result.vouchable() {
		latch.Set(result.healthy())
	}
}
