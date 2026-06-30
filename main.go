package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	// Embed the IANA tz database so TZ (default Europe/Paris) is honored regardless
	// of the base image's zoneinfo; without it, on a base that ships no
	// /usr/share/zoneinfo, time.Local silently falls back to UTC.
	_ "time/tzdata"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `sync` runs one pass and exits, anything else (including no argument) runs
// the long-lived daemon. The daemon and the sync subcommand share one
// signal-cancelled context so both drain gracefully on SIGTERM/Interrupt.
func main() {
	// CLI health probe for the Docker healthcheck. Checked before the logger
	// is configured because RunProbe calls os.Exit.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(healthMarkerPath)
	}
	os.Exit(dispatch())
}

// dispatch selects the subcommand and returns the process exit code. It owns
// the signal-cancelled context shared by the daemon and the sync subcommand.
// Returning the code (rather than calling os.Exit here) lets the deferred stop
// run before the process exits.
func dispatch() int {
	cmd := "daemon"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "daemon":
		if err := run(ctx); err != nil {
			return 1
		}
		return 0
	case "sync":
		return runSync(ctx)
	default:
		setupLogger()
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, sync, health")
		return 2
	}
}

// loadRuntime performs the shared startup sequence for the daemon and the
// sync subcommand: configures the logger, loads and validates the config, and
// reads the sync timeout. It returns a health marker without setting or
// cleaning it — callers manage the marker lifecycle differently (the daemon
// defers Cleanup; sync deliberately does not).
func loadRuntime() (config, time.Duration, *health.Marker, error) {
	setupLogger()

	cfg, err := loadConfig()
	if err != nil {
		return config{}, 0, nil, err
	}
	timeout := loadSyncTimeout()
	marker := health.NewMarker(healthMarkerPath)
	return cfg, timeout, marker, nil
}

// run is the composition root for the long-running container (the `daemon`
// subcommand and the default no-arg command). It loads config, wires the
// health controller, and dispatches to the built-in interval scheduler or the
// idle external-trigger loop based on cfg.ScheduleEnabled. Returning an error
// exits non-zero.
func run(ctx context.Context) error {
	cfg, timeout, marker, err := loadRuntime()
	if err != nil {
		return err
	}
	defer marker.Cleanup()

	hc := newHealthController(marker)
	if cfg.ScheduleEnabled {
		runBuiltin(ctx, hc, cfg, timeout)
		return nil
	}
	runExternal(ctx, hc, cfg)
	return nil
}

// runAndReport runs one pass and routes its result to the two consumers of a
// pass outcome — the reporter (the single emitter of the pass log line) and
// the health controller (the single writer of the marker) — then returns the
// result for callers that need its exit status. Funnelling both consumers
// through here means neither the log signal nor the marker write can be
// skipped.
func runAndReport(ctx context.Context, hc *healthController, cfg config, timeout time.Duration, trigger string) passResult {
	r := runPass(ctx, cfg, timeout, trigger, defaultCommandRunner)
	reportPass(&r)
	hc.apply(&r)
	return r
}

// runBuiltin runs the self-contained interval scheduler: a startup pass that
// fires immediately for freshness on deploy, then one pass per cfg.Interval
// tick. Passes run sequentially in this single loop, so two never run at once
// in-process — a tick that fires during a long pass is coalesced by the ticker
// rather than starting a second, overlapping pass. A drain watcher flips the
// marker unhealthy the instant the shutdown signal lands, and the deferred
// Wait keeps run()'s marker.Cleanup from racing that final write.
func runBuiltin(ctx context.Context, hc *healthController, cfg config, timeout time.Duration) {
	hc.markInitial(false) // unhealthy until the first pass completes

	slog.Info("container started (built-in scheduling)",
		"jobs", len(cfg.Jobs), "config", configPath(), "interval", cfg.Interval,
		"ssh_hostkey_mode", sshHostKeyMode())

	var watcher sync.WaitGroup
	watcher.Go(func() {
		<-ctx.Done()
		hc.beginDrain()
	})
	defer watcher.Wait()

	runAndReport(ctx, hc, cfg, timeout, "startup")

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "cause", context.Cause(ctx))
			return
		case <-ticker.C:
			runAndReport(ctx, hc, cfg, timeout, "interval")
		}
	}
}

// runExternal idles until shutdown. The built-in scheduler is disabled
// (SYNC_INTERVAL=off); syncs are triggered out-of-band via the `sync`
// subcommand. The container is healthy on boot (an idle, not-yet-triggered
// container has nothing failed); each `sync` invocation updates the marker.
func runExternal(ctx context.Context, hc *healthController, cfg config) {
	hc.markInitial(true)

	slog.Info("container started (external scheduling)",
		"jobs", len(cfg.Jobs), "config", configPath(),
		"trigger", "docker-rsync-scheduler sync",
		"ssh_hostkey_mode", sshHostKeyMode())

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	hc.beginDrain()
}

// runSync runs one full pass over all jobs and returns the process exit code
// (0 if every job succeeded or the pass deferred to an in-flight holder, 1 on
// any failure or a lock error). It is what the external scheduler invokes.
// Unlike the daemon it does not clean up the marker on exit — the file must
// persist so the running container's healthcheck reflects this run.
func runSync(ctx context.Context) int {
	cfg, timeout, marker, err := loadRuntime()
	if err != nil {
		return 1
	}
	// NOTE: marker.Cleanup() is deliberately NOT deferred. The marker must
	// persist after exit so the long-running container's healthcheck reflects
	// this externally-triggered run.
	hc := newHealthController(marker)
	r := runAndReport(ctx, hc, cfg, timeout, "external")
	return r.exitStatus()
}
