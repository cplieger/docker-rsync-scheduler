package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v2"
)

// --- Daemon: the single owner of sync execution ---
//
// PID 1 owns every sync pass. Triggers only submit requests: the built-in
// ticker (built-in mode) and the unix-socket clients (`sync` subcommand,
// both modes) all feed one FIFO queue served by one executor goroutine.
// That single-ownership is the design: mutual exclusion is the executor loop
// (nothing else may run a pass), shutdown cancels the in-flight pass's
// context so rsync drains under the existing SIGTERM-then-grace machinery,
// and every pass's log lines land on the container log stream because the
// pass executes in the daemon — in external mode too, which the previous
// exec-child design could not offer.

// daemon carries the executor's dependencies.
type daemon struct {
	queue *runQueue
	// hc is the single writer of the health marker; every pass outcome
	// funnels through it (drain latch, interrupted-clean carve-out).
	hc      *healthController
	newCmd  scheduler.CommandRunner
	timeout time.Duration
}

// runDaemon is the composition root for the long-running container (the
// `daemon` subcommand and the default no-arg command). It configures logging,
// fail-fast loads and validates the config, binds the trigger socket, wires
// the health controller, starts the executor, and — in built-in mode —
// drives the interval ticker. newCmd builds each rsync child
// (defaultCommandRunner in production; injected by tests). Returning an
// error exits non-zero.
func runDaemon(ctx context.Context, socketPath string, newCmd scheduler.CommandRunner) error {
	setupLogger()

	// Boot-time fail-fast: a missing or invalid config refuses to start the
	// container, exactly as before the single-owner rewrite. The executor
	// re-loads the config per pass (see execute), so this cfg's job list is
	// only the boot snapshot; its Interval/ScheduleEnabled select the mode.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	timeout := loadSyncTimeout()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := listenTrigger(socketPath)
	if err != nil {
		slog.Error("cannot bind trigger socket", "path", socketPath, "error", err)
		return err
	}
	defer func() { _ = os.Remove(socketPath) }()

	marker := health.NewMarker(healthMarkerPath)
	defer marker.Cleanup()
	hc := newHealthController(marker)
	// Built-in mode starts unhealthy until the first pass proves the setup
	// (the startup pass flips it); external mode starts healthy — idle,
	// nothing has failed — and each triggered pass updates it.
	hc.markInitial(!cfg.ScheduleEnabled)

	d := &daemon{
		queue:   newRunQueue(queueCapacity),
		hc:      hc,
		newCmd:  newCmd,
		timeout: timeout,
	}

	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		d.runPasses(ctx)
	}()

	srv := &triggerServer{queue: d.queue}
	go srv.serve(ln)

	tickerDone := startTicker(ctx, d, cfg.Interval, cfg.ScheduleEnabled)

	mode := "external"
	if cfg.ScheduleEnabled {
		mode = "built-in"
	}
	slog.Info("container started ("+mode+" scheduling)",
		"jobs", len(cfg.Jobs), "config", configPath(), "interval", cfg.Interval,
		"ssh_hostkey_mode", sshHostKeyMode(), "socket", socketPath)

	<-ctx.Done()
	slog.Info("shutting down", "cause", context.Cause(ctx))
	// Latch unhealthy immediately so observers see the drain before the
	// in-flight pass resolves (it is being SIGTERM'd via ctx and drains under
	// the runner's grace window; the latch also blocks a late healthy write).
	hc.beginDrain()

	// Stop admission (socket + queue), then wait: the executor delivers the
	// interrupted in-flight pass's result and cancellation results to
	// everything still queued; the ticker returns once its waiting tick
	// request resolves; the handlers return once every accepted request has
	// its final event on the wire.
	_ = ln.Close()
	d.queue.close()
	<-executorDone
	<-tickerDone
	srv.handlers.Wait()
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
			trigger := "interval"
			if !startupDone {
				trigger, startupDone = "startup", true
			}
			d.tick(trigger)
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
func (d *daemon) tick(trigger string) {
	r := newRequest(trigger)
	if err := d.queue.submit(r); err != nil {
		slog.Warn("scheduled sync skipped", "trigger", trigger, "reason", err)
		return
	}
	<-r.result
}

// runPasses is the executor: the only code that runs a sync pass. It serves
// the queue strictly in order until the queue is closed and drained. Once
// shutdown is signalled, remaining requests are cancelled — delivered as
// explicit not-ok results with a reason — instead of run, so a stop request
// is never followed by a fresh pass.
func (d *daemon) runPasses(ctx context.Context) {
	for r := range d.queue.requests {
		if ctx.Err() != nil {
			r.finish(runOutcome{ok: false, reason: "cancelled: scheduler shutting down"})
			continue
		}
		d.execute(ctx, r)
	}
}

// execute performs one request: signal the waiter, reload the config (the
// old external `sync` process re-read it on every trigger, so a config edit
// takes effect on the next pass without a restart — per-pass reload keeps
// that contract in both modes, and a config mount that degrades after boot
// fails the pass loudly instead of syncing from a stale snapshot), run the
// pass, route the outcome through the reporter and the health controller,
// and deliver the result.
//
// The pass runs under the shutdown-cancellable ctx on purpose: SIGTERM
// interrupts an in-flight rsync (SIGTERM-then-grace via the command runner),
// and the interrupted-clean machinery in passResult keeps that drain from
// registering as a failure — the same drain semantics the pre-rewrite design
// pinned in its tests.
func (d *daemon) execute(ctx context.Context, r *request) {
	close(r.started)
	start := time.Now()

	cfg, err := loadConfig()
	if err != nil {
		d.hc.markUnhealthy()
		r.finish(runOutcome{ok: false, duration: time.Since(start), reason: "config reload failed"})
		return
	}

	res := runPass(ctx, cfg, d.timeout, r.trigger, d.newCmd)
	reportPass(&res)
	d.hc.apply(&res)
	r.finish(runOutcome{ok: res.exitStatus() == 0, duration: res.duration})
}
