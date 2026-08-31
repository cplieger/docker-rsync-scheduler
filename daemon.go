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
//
// The executor goroutine is the only in-process caller of d.run.

// queueCapacity sizes the trigger broker's FIFO: the realistic trigger
// set is one periodic job (Ofelia) plus a manual exec, so 16 is generous
// headroom. Full-queue rejection semantics are trigger.NewQueue's.
const queueCapacity = 16

// newRequest builds one queued pass request for the given trigger label
// (startup, interval, external). A sync pass takes no arguments — the job
// set comes from the daemon's mounted YAML config — so the payload is empty.
func newRequest(trig string) *trigger.Job[struct{}] {
	return trigger.NewJob(trig, struct{}{})
}

// daemon carries the executor's dependencies.
type daemon struct {
	queue *trigger.Queue[struct{}]
	// hc owns every health-state write (drain latch, interrupted-clean
	// carve-out); runDaemon's exit defer also unlinks the marker, safe only
	// because it runs after <-executorDone.
	hc        *healthController
	newCmd    scheduler.CommandRunner
	timeout   time.Duration
	transport transport
}

// runDaemon runs the long-running container (the `daemon` subcommand): it
// fail-fast loads and validates the config, decides the ssh host-key posture,
// binds the trigger socket, wires the health controller, starts the executor,
// and — in built-in mode — drives the interval ticker. It expects an already
// signal-bound context and a configured logger from dispatch. newCmd builds
// each rsync child (defaultCommandRunner in production; injected by tests).
// Returning an error exits non-zero. Every error path logs its diagnostic
// before returning, so dispatch stays silent to avoid a duplicate record.
func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	// The marker is created and its removal deferred before the first thing that
	// can fail: /tmp survives a docker restart, so a boot that fails after a crash
	// must not leave the previous life's healthy marker for the healthcheck.
	marker := health.NewMarker(healthMarkerPath)
	defer marker.Cleanup()

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
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
	}

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		trigger.Execute(ctx, d.queue, d.run)
	}()

	// Announced before admission opens: the accept loop and the ticker's first
	// fire log from other goroutines, so the boot record must precede them.
	mode, intervalAttr := "external", "none"
	if scheduleEnabled {
		mode, intervalAttr = "built-in", interval.String()
	}
	slog.Info("container started",
		"mode", mode, "jobs", len(cfg.Jobs), "config", configPath(),
		"interval", intervalAttr, "ssh_hostkey_mode", hostKeys.String(),
		"acls", tr.acls, "xattrs", tr.xattrs, "compress", cmp.Or(tr.compress, "off"),
		"socket", socketPath)

	// The broker owns the wire (decode, event relay, handler draining); the
	// hook only supplies this app's acceptance log line. The library's
	// default rejection warn ("trigger request rejected" + reason) already
	// matches this app's wording, so no OnRejected hook is needed.
	srv := &trigger.Server[struct{}]{
		Queue:      d.queue,
		OnAccepted: func(struct{}) { slog.Info("triggered sync queued") },
	}
	srv.Serve(ln)

	tickerDone := startTicker(ctx, d, interval, scheduleEnabled)

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Latch unhealthy immediately so observers see the drain before the
	// in-flight pass resolves (it is being SIGTERM'd via ctx and drains under
	// the runner's grace window; the latch also blocks a late healthy write).
	hc.beginDrain()

	// Stop admission (socket + queue), then wait: the executor delivers the
	// interrupted in-flight pass's result and cancellation results to
	// everything still queued; the ticker returns once its waiting tick
	// request resolves; the server returns once every accepted request has
	// its final event on the wire.
	_ = ln.Close()
	d.queue.Close()
	<-executorDone
	<-tickerDone
	srv.Wait()
	slog.Info("shutdown complete")
	return nil
}

// startTicker runs the built-in interval scheduler: a startup pass that fires
// immediately for freshness on deploy, then one pass per interval, each
// submitted to the queue like any other trigger and waited on (RunLoop is
// sequential, so ticks can never pile up behind a long pass). Disabled
// (closed channel returned) in external mode. The library re-checks ctx
// before each fire, so no fresh tick is submitted after shutdown begins.
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

// tick submits one scheduled pass and waits for its result (the executor
// writes the health marker; the queue guarantees exactly one result per
// accepted request, including a cancellation result at shutdown, so this
// wait always resolves). A rejected submission — the queue full of external
// requests, or shutdown racing the tick — is logged and skipped: the next
// interval provides freshness.
func (d *daemon) tick(trig string) {
	r := newRequest(trig)
	if err := d.queue.Submit(r); err != nil {
		slog.Warn("scheduled sync skipped", "trigger", trig, "reason", err)
		return
	}
	<-r.Result()
}

// run performs one request. It reloads the config, which is what makes a
// config edit take effect on the next trigger without a restart, and runs the
// pass under the shutdown-cancellable ctx: SIGTERM interrupts an in-flight
// rsync, and passResult preserves an interrupted clean pass as a clean drain.
func (d *daemon) run(ctx context.Context, trig string, _ struct{}) trigger.Outcome {
	start := time.Now()

	cfg, err := loadConfig()
	if err != nil {
		d.hc.markUnhealthy()
		return trigger.Outcome{OK: false, Duration: time.Since(start), Reason: "config reload failed"}
	}

	res := runPass(ctx, cfg, d.timeout, d.transport, trig, d.newCmd)
	reportPass(&res)
	d.hc.apply(&res)
	return trigger.Outcome{OK: res.exitStatus() == 0, Duration: time.Since(start)}
}
