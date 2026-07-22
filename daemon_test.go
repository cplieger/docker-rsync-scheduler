package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/scheduler/v2/trigger"
	"github.com/cplieger/slogx/capture"
)

// TestExecutor_MarkerFollowsPassOutcome pins the health contract: the marker
// flips healthy on a clean pass and unhealthy on a failed one — the executor
// (via the health controller) is the marker's single writer.
// Not parallel: sets env (the executor reloads CONFIG_PATH per pass).
func TestExecutor_MarkerFollowsPassOutcome(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t)) // non-empty source: the runner executes
	d, _, _, markerPath := newTestDaemon(t, fixedRunner("true"))

	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("clean pass reported ok=false")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("marker absent after a clean pass: %v (want healthy)", err)
	}

	d.newCmd = fixedRunner("false")
	if out := submitWait(t, d, newRequest("external")); out.OK {
		t.Fatal("failed pass reported ok=true")
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present after a failed pass; stat err = %v, want not-exist (unhealthy)", err)
	}
}

// TestExecutor_ConfigReloadFailureFailsRequestAndMarker pins the per-pass
// config reload: a config that degrades after boot fails the pass with an
// actionable reason, flips the marker unhealthy, and never invokes rsync.
// Not parallel: sets env.
func TestExecutor_ConfigReloadFailureFailsRequestAndMarker(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	invoked := false
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		invoked = true
		return exec.CommandContext(ctx, "true")
	}
	d, _, _, markerPath := newTestDaemon(t, runner)
	// The daemon is already running; now the config "mount breaks".
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))

	out := submitWait(t, d, newRequest("external"))
	if out.OK {
		t.Error("outcome ok=true with an unreadable config, want false")
	}
	if out.Reason == "" {
		t.Error("outcome carries no reason; the client would report a bare failure")
	}
	if invoked {
		t.Error("rsync was invoked despite the config reload failing")
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present after a config reload failure; stat err = %v, want not-exist (unhealthy)", err)
	}
}

// TestExecutor_ShutdownCancelsQueuedButResolvesInFlight pins the drain
// contract: SIGTERM interrupts the in-flight pass (which resolves as an
// interrupted-clean drain, ok=true, per the pass semantics the pre-rewrite
// design pinned) and never starts queued work (it is cancelled with an
// explicit reason). Not parallel: sets env.
func TestExecutor_ShutdownCancelsQueuedButResolvesInFlight(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		once.Do(func() { close(entered) })
		<-proceed
		return exec.CommandContext(ctx, "sleep", "30")
	}
	d, cancel, _, _ := newTestDaemon(t, runner)

	inflight := newRequest("external")
	if err := d.queue.Submit(inflight); err != nil {
		t.Fatalf("Submit(inflight) = %v", err)
	}
	<-entered // the pass is now executing

	queued := newRequest("external")
	if err := d.queue.Submit(queued); err != nil {
		t.Fatalf("Submit(queued) = %v", err)
	}

	cancel()        // SIGTERM lands mid-pass
	d.queue.Close() // daemon stops admission
	close(proceed)  // the in-flight child starts under the cancelled ctx and is reaped

	select {
	case out := <-inflight.Result():
		if !out.OK {
			t.Errorf("in-flight pass outcome ok=false, want true (interrupted-clean drains as success)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight result not delivered")
	}
	select {
	case out := <-queued.Result():
		if out.OK {
			t.Error("queued request outcome ok=true after shutdown, want cancelled")
		}
		if !strings.Contains(out.Reason, "shutting down") {
			t.Errorf("cancellation reason = %q, want a shutting-down explanation", out.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued request's cancellation result not delivered")
	}
}

// TestTick_SkipsWhenQueueRejects pins the ticker's degradation: a rejected
// submission (queue full) is logged and skipped — the tick must not panic or
// block; the next interval provides freshness.
func TestTick_SkipsWhenQueueRejects(t *testing.T) {
	t.Parallel()
	d := &daemon{queue: trigger.NewQueue[struct{}](0)} // zero capacity: every submit is rejected
	done := make(chan struct{})
	go func() { defer close(done); d.tick("interval") }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick() blocked on a rejected submission; it must skip")
	}
}

// TestStartTicker_FiresStartupThenInterval drives the REAL startTicker and
// pins built-in mode's cadence labels through the heartbeat log lines: the
// first pass logs trigger=startup, the next trigger=interval. Not parallel:
// it swaps the global slog default and sets env.
func TestStartTicker_FiresStartupThenInterval(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // empty source: pure-skip passes, no exec
	rec := capture.Default(t)

	d, cancel, execDone, _ := newTestDaemon(t, fixedRunner("true"))

	tickCtx, stopTicker := context.WithCancel(context.Background())
	tickerDone := startTicker(tickCtx, d, 15*time.Millisecond, true)

	// heartbeatTriggers returns each heartbeat's trigger attr, in emit order.
	heartbeatTriggers := func() []string {
		var out []string
		for _, r := range rec.Records() {
			if r.Message != "sync cycle complete" {
				continue
			}
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "trigger" {
					out = append(out, a.Value.String())
				}
				return true
			})
		}
		return out
	}
	waitFor(t, 5*time.Second, func() bool {
		return len(heartbeatTriggers()) >= 2
	}, "ticker did not fire startup + interval within 5s")
	stopTicker()
	<-tickerDone
	cancel()
	d.queue.Close()
	<-execDone

	triggers := heartbeatTriggers()
	if triggers[0] != "startup" {
		t.Errorf("first heartbeat trigger = %q, want startup", triggers[0])
	}
	if triggers[1] != "interval" {
		t.Errorf("second heartbeat trigger = %q, want interval", triggers[1])
	}
}

// TestStartTicker_DisabledInExternalMode pins that external mode runs no
// ticker: the returned channel is already closed and nothing is submitted.
func TestStartTicker_DisabledInExternalMode(t *testing.T) {
	t.Parallel()
	d := &daemon{queue: trigger.NewQueue[struct{}](4)}
	done := startTicker(context.Background(), d, time.Millisecond, false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startTicker(enabled=false) did not return a closed channel")
	}
	time.Sleep(20 * time.Millisecond) // would be several intervals if a loop were running
	if n := len(d.queue.Jobs()); n != 0 {
		t.Errorf("%d requests submitted in external mode, want 0", n)
	}
}

// TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly is the
// composition-root integration test: external mode boots healthy (idle),
// serves a triggered pass over the real socket, and on shutdown removes the
// socket and the marker. Not parallel: it uses the package-global
// healthMarkerPath (the real path the health subcommand probes) and env.
func TestRunDaemon_ExternalModeBootsHealthyServesAndShutsDownCleanly(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // empty source: the triggered pass is a clean skip
	t.Setenv("SYNC_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, fixedRunner("true"))
	}()

	// External mode boots healthy: poll until the marker appears.
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "daemon did not set the health marker healthy on external-mode boot")
	// The socket must be live and serving.
	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")

	if code := runClient(sock); code != 0 {
		t.Errorf("runClient() = %d, want 0 (clean triggered pass)", code)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after shutdown")
	}
	if runErr != nil {
		t.Errorf("runDaemon() = %v, want nil", runErr)
	}
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker not cleaned up on shutdown; stat err = %v, want not-exist", err)
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket file not removed on shutdown; stat err = %v, want not-exist", err)
	}
}
