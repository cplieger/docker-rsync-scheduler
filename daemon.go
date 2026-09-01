package main

import (
	"cmp"
	"context"
	"log/slog"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/scheduler/v4/trigger"
)

// --- Daemon: the single owner of sync execution ---

// queueCapacity sizes the trigger broker's FIFO: the realistic trigger set
// is one periodic job (Ofelia) plus a manual exec, so 16 is generous
// headroom.
const queueCapacity = 16

// newRequest builds one queued pass request for the given trigger label
// (startup, interval, external). A sync pass takes no arguments — the job
// set comes from the daemon's mounted YAML config.
func newRequest(trig string) *trigger.Job[struct{}] {
	return trigger.NewJob(trig, struct{}{})
}

// daemon carries the executor's dependencies.
type daemon struct {
	queue *trigger.Queue[struct{}]
	// hc owns health-state writes after boot; healthController documents the
	// ordered boot-clear and exit-cleanup exceptions.
	hc        *healthController
	newCmd    scheduler.CommandRunner
	transport transport
	timeout   time.Duration
	// Written at construction, then read and written only by the executor.
	advised [32]byte
}

// runDaemon runs the long-running container (the `daemon` subcommand): it
// fail-fast loads and validates the config, decides the ssh host-key
// posture, binds the trigger socket, wires the health controller, starts the
// executor, and — in built-in mode — drives the interval ticker. It expects
// an already signal-bound context and a configured logger from dispatch.
func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	// Clear stale state before the first operation that can fail: /tmp
	// survives a docker restart.
	marker := health.NewMarker(healthMarkerPath)
	marker.Cleanup()
	defer marker.Cleanup()

	lc, stage, err := loadConfig()
	if err != nil {
		slog.Error("cannot load config", "path", configPath(), "stage", stage, "error", err,
			"hint", "mount a config.yaml at this path — see config.example.yaml in the repo")
		return err
	}
	adviseConfig(lc.cfg)
	timeout := loadSyncTimeout()
	interval, scheduleEnabled := loadInterval()
	hostKeys, err := classifyKnownHosts(knownHostsPath)
	if err != nil {
		slog.Error("cannot determine the ssh host-key posture", "error", err,
			"hint", "mount a non-empty known_hosts file at "+knownHostsPath+", or omit the mount for accept-new")
		return err
	}

	ln, err := trigger.Listen(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}

	hc := newHealthController(marker)
	// Inverted on purpose: built-in scheduling starts UNHEALTHY.
	hc.markInitial(!scheduleEnabled)

	tr := loadTransport(hostKeys)
	d := &daemon{
		queue:     trigger.NewQueue[struct{}](queueCapacity),
		hc:        hc,
		newCmd:    newCmd,
		timeout:   timeout,
		transport: tr,
		advised:   lc.digest,
	}

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		trigger.Execute(ctx, d.queue, d.run)
	}()

	// Announced before admission opens, since the accept loop and ticker
	// fire from other goroutines.
	mode, intervalAttr := "external", "none"
	if scheduleEnabled {
		mode, intervalAttr = "built-in", interval.String()
	}
	slog.Info("container started",
		"mode", mode, "jobs", len(lc.cfg.Jobs), "config", configPath(),
		"interval", intervalAttr, "timeout", timeout,
		"ssh_hostkey_mode", tr.hostKeys.String(),
		"acls", tr.acls, "xattrs", tr.xattrs, "compress", cmp.Or(tr.compress, "off"),
		"socket", socketPath)

	// The broker owns the wire (decode, event relay, handler draining); this
	// hook only supplies the acceptance log line. The library's default
	// rejection warn already matches this app's wording, so no OnRejected
	// hook is needed.
	srv := &trigger.Server[struct{}]{
		Queue:      d.queue,
		OnAccepted: func(struct{}) { slog.Info("triggered sync queued") },
	}
	srv.Serve(ln)

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled)

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Latch unhealthy immediately so observers see the drain before the
	// in-flight pass resolves.
	hc.beginDrain()

	// Stop admission, then wait: the executor delivers the interrupted
	// in-flight pass's result and cancellation results to everything still
	// queued; the ticker returns once its waiting tick resolves; the server
	// returns once every accepted request has its final event on the wire.
	_ = ln.Close()
	d.queue.Close()
	<-executorDone
	<-tickerDone
	srv.Wait()
	slog.Info("shutdown complete")
	return nil
}

// startTicker runs the built-in interval scheduler: a startup pass that
// fires immediately, then one pass per interval, submitted to the queue like
// any other trigger (RunLoop is sequential, so ticks can never pile up
// behind a long pass). Disabled (closed channel returned) in external mode.
func startTicker(ctx context.Context, d *daemon, interval time.Duration, enabled bool) <-chan struct{} {
	done := make(chan struct{})
	if !enabled {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		startupDone := false
		scheduler.RunLoop(ctx, func(context.Context) {
			trig := "interval"
			if !startupDone {
				trig, startupDone = "startup", true
			}
			d.tick(trig)
		}, scheduler.LoopOptions{Interval: interval, FireOnStart: true})
	}()
	return done
}

// tick submits one scheduled pass and waits for its result. A rejected
// submission (queue full, or shutdown racing the tick) is logged and
// skipped: the next interval provides freshness.
func (d *daemon) tick(trig string) {
	r := newRequest(trig)
	if err := d.queue.Submit(r); err != nil {
		slog.Warn("scheduled sync skipped", "trigger", trig, "reason", err)
		return
	}
	<-r.Result()
}

// run performs one request. It reloads the config on every call, so a config
// edit takes effect on the next trigger without a restart, and runs the pass
// under the shutdown-cancellable ctx: SIGTERM interrupts an in-flight rsync.
// An interrupted-clean pass reports OK for the client to map to exit 0.
func (d *daemon) run(ctx context.Context, trig string, _ struct{}) trigger.Outcome {
	start := time.Now()

	lc, stage, err := loadConfig()
	if err != nil {
		slog.Error("config reload failed", "path", configPath(), "stage", stage, "trigger", trig, "error", err)
		d.hc.markUnhealthy()
		return trigger.Outcome{OK: false, Duration: time.Since(start), Reason: "config reload failed"}
	}
	if lc.digest != d.advised {
		adviseConfig(lc.cfg)
		d.advised = lc.digest
	}

	res := runPass(ctx, lc.cfg, d.timeout, d.transport, trig, d.newCmd)
	reportPass(&res)
	d.hc.apply(&res)
	out := trigger.Outcome{OK: res.healthy(), Duration: time.Since(start)}
	if out.OK && res.interrupted {
		out.Reason = "pass cut short by shutdown; remaining jobs did not run"
	}
	return out
}
