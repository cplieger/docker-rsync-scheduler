package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/health"
)

// --- Main ---

// main dispatches on the first argument: `health` runs the Docker probe,
// `sync` runs one pass and exits, anything else (including no argument)
// runs the long-lived daemon.
func main() {
	// CLI health probe for the Docker healthcheck. Checked before the
	// logger is configured because RunProbe calls os.Exit.
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(healthMarkerPath)
	}

	cmd := "daemon"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "daemon":
		if err := run(context.Background()); err != nil {
			os.Exit(1)
		}
	case "sync":
		os.Exit(runSync(context.Background()))
	default:
		setupLogger()
		slog.Error("unknown subcommand", "command", cmd, "valid", "daemon, sync, health")
		os.Exit(2)
	}
}

// loadRuntime performs the shared startup sequence for both the daemon
// and the sync subcommand: configures the logger, loads and validates
// the config, and reads the sync timeout. It returns a health marker
// without setting or cleaning it — callers manage the marker lifecycle
// differently (daemon defers Cleanup; sync deliberately does not).
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
// health marker, and dispatches to the built-in interval scheduler or the
// idle external-trigger loop based on cfg.ScheduleEnabled. Returning an
// error exits non-zero.
func run(ctx context.Context) error {
	cfg, timeout, marker, err := loadRuntime()
	if err != nil {
		return err
	}
	defer marker.Cleanup()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.ScheduleEnabled {
		runBuiltin(ctx, marker, cfg, timeout)
		return nil
	}
	runExternal(ctx, marker, cfg)
	return nil
}

// runBuiltin runs the self-contained interval scheduler: a startup sync
// pass that fires immediately for freshness on deploy, plus a ticker loop
// that fires every cfg.Interval. The flock in runSyncPass guards against
// overlap if a pass runs longer than the interval. Both goroutines share
// the wait group so shutdown waits for in-flight work. Each pass sets the
// health marker from its own failCount.
func runBuiltin(ctx context.Context, marker *health.Marker, cfg config, timeout time.Duration) {
	// Remove any stale marker from a previous run that may have crashed
	// before its defer ran. The first pass flips it to its real value.
	marker.Set(false)

	slog.Info("container started (built-in scheduling)",
		"jobs", len(cfg.Jobs), "config", configPath(), "interval", cfg.Interval)

	var wg sync.WaitGroup
	wg.Go(func() {
		failCount := runSyncPass(ctx, cfg, timeout, "startup", defaultCommandRunner)
		marker.Set(failCount == 0)
	})
	wg.Go(func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				failCount := runSyncPass(ctx, cfg, timeout, "interval", defaultCommandRunner)
				marker.Set(failCount == 0)
			}
		}
	})

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Mark unhealthy immediately so observers see the signal before the
	// pass drain (which may take a while on a slow remote).
	marker.Set(false)

	// Wait for the startup pass and any in-flight ticker pass to drain.
	wg.Wait()
}

// runExternal idles until shutdown. The built-in scheduler is disabled
// (SYNC_INTERVAL=off); syncs are triggered out-of-band via the `sync`
// subcommand. The marker is set healthy on boot so an idle, not-yet-
// triggered container reads healthy; each `sync` invocation updates it.
func runExternal(ctx context.Context, marker *health.Marker, cfg config) {
	marker.Set(true)

	slog.Info("container started (external scheduling)",
		"jobs", len(cfg.Jobs), "config", configPath(),
		"trigger", "docker-rsync-scheduler sync")

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	marker.Set(false)
}

// runSync runs one full sync pass over all jobs, sets the health marker,
// and returns the process exit code: 0 if every job succeeded, 1 if any
// failed (or config loading failed). This is what the external scheduler
// invokes. Unlike the daemon it does not clean up the marker on exit — the
// file must persist so the running container's healthcheck reflects this
// run.
func runSync(ctx context.Context) int {
	cfg, timeout, marker, err := loadRuntime()
	if err != nil {
		return 1
	}
	// NOTE: marker.Cleanup() is deliberately NOT deferred here. The marker
	// must persist after exit so the long-running container's healthcheck
	// reflects this externally-triggered run.

	failCount := runSyncPass(ctx, cfg, timeout, "external", defaultCommandRunner)
	marker.Set(failCount == 0)

	if failCount > 0 {
		return 1
	}
	return 0
}
