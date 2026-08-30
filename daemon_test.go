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
	"testing/synctest"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4/trigger"
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

// TestExecutor_ValidConfigReloadUsesReplacementJobs pins the other half of
// the per-pass reload: an operator who edits the mounted CONFIG_PATH between
// triggers gets the REPLACEMENT job set on the next pass. The oracle is the
// rsync invocation the daemon hands to the unmanaged child, not an internal
// loadConfig call. Not parallel: sets env.
func TestExecutor_ValidConfigReloadUsesReplacementJobs(t *testing.T) {
	firstSource := newRunJobSource(t)
	cfgPath := writeValidCfg(t, firstSource)
	key := filepath.Join(filepath.Dir(cfgPath), "id_ed25519")

	var mu sync.Mutex
	var sources []string
	runner := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		mu.Lock()
		sources = append(sources, args[len(args)-2])
		mu.Unlock()
		return exec.CommandContext(ctx, "true")
	}
	d, _, _, _ := newTestDaemon(t, runner)

	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("first pass reported ok=false")
	}

	secondSource := newRunJobSource(t)
	replacement := "jobs:\n  - name: replacement\n    local: " + secondSource + "\n" +
		"    remote_host: root@192.0.2.10\n    remote_path: /srv/replacement\n" +
		"    ssh_key: " + key + "\n"
	if err := os.WriteFile(cfgPath, []byte(replacement), 0o600); err != nil {
		t.Fatalf("replace config: %v", err)
	}

	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("second pass reported ok=false")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sources) != 2 {
		t.Fatalf("rsync invocation sources = %v, want exactly two", sources)
	}
	if sources[0] != firstSource+"/" {
		t.Errorf("first rsync source = %q, want %q", sources[0], firstSource+"/")
	}
	if sources[1] != secondSource+"/" {
		t.Errorf("second rsync source = %q, want replacement %q", sources[1], secondSource+"/")
	}
}

// TestExecutor_EmptySourceIsRecheckedEachPass pins that the empty-source guard
// is re-evaluated per request: an operator submitting consecutive daemon
// requests while the mounted source empties out must see the later pass skipped
// before rsync runs. The oracle is the injected runner's invocation count, not
// a sourceIsEmpty call count. Not parallel: sets env and swaps the global slog
// default.
func TestExecutor_EmptySourceIsRecheckedEachPass(t *testing.T) {
	rec := capture.Default(t)
	source := newRunJobSource(t)
	writeValidCfg(t, source)

	var mu sync.Mutex
	calls := 0
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		mu.Lock()
		calls++
		mu.Unlock()
		return exec.CommandContext(ctx, "true")
	}
	d, _, _, _ := newTestDaemon(t, runner)

	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("first pass reported ok=false")
	}
	movedFile := filepath.Join(t.TempDir(), "f")
	if err := os.Rename(filepath.Join(source, "f"), movedFile); err != nil {
		t.Fatalf("empty source: %v", err)
	}
	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("second pass reported ok=false")
	}

	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Errorf("rsync invocations across populated then empty passes = %d, want 1", gotCalls)
	}
	const heartbeat = "sync cycle complete"
	if !rec.HasAttr(heartbeat, "skipped", "1") {
		t.Errorf("second pass logs = %q, want heartbeat with skipped=1", rec.Messages())
	}
	if !rec.HasAttr(heartbeat, "failed", "0") {
		t.Errorf("second pass logs = %q, want heartbeat with failed=0", rec.Messages())
	}
}

// TestExecutor_ReloadedJobKeepsStrictHostKeyMode pins that the boot-time
// host-key posture reaches a job first read from a REPLACEMENT config: the
// protocol oracle is the -e ssh command handed to rsync, which under strict
// mode carries StrictHostKeyChecking=yes and the mounted UserKnownHostsFile.
// The key path is fixture data, so only the two options that ARE the posture
// are asserted. Not parallel: sets env.
func TestExecutor_ReloadedJobKeepsStrictHostKeyMode(t *testing.T) {
	firstSource := newRunJobSource(t)
	cfgPath := writeValidCfg(t, firstSource)
	key := filepath.Join(filepath.Dir(cfgPath), "id_ed25519")

	var mu sync.Mutex
	var sshCommands []string
	runner := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		for i, arg := range args {
			if arg == "-e" && i+1 < len(args) {
				mu.Lock()
				sshCommands = append(sshCommands, args[i+1])
				mu.Unlock()
				break
			}
		}
		return exec.CommandContext(ctx, "true")
	}
	d, _, _, _ := newTestDaemon(t, runner)
	d.transport.hostKeys = hostKeyStrict

	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("first pass reported ok=false")
	}
	secondSource := newRunJobSource(t)
	replacement := "jobs:\n  - name: replacement\n    local: " + secondSource + "\n" +
		"    remote_host: root@192.0.2.10\n    remote_path: /srv/replacement\n" +
		"    ssh_key: " + key + "\n"
	if err := os.WriteFile(cfgPath, []byte(replacement), 0o600); err != nil {
		t.Fatalf("replace config: %v", err)
	}
	if out := submitWait(t, d, newRequest("external")); !out.OK {
		t.Fatal("second pass reported ok=false")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sshCommands) != 2 {
		t.Fatalf("captured ssh commands = %v, want exactly two", sshCommands)
	}
	for _, want := range []string{
		"-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile=" + knownHostsPath,
	} {
		if !strings.Contains(sshCommands[1], want) {
			t.Errorf("replacement job ssh command = %q, want it to contain %q", sshCommands[1], want)
		}
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
// block, and the warning is the only record of a scheduled pass that produced
// neither a request nor a result, so it must name the trigger and a reason.
// Not parallel: capture.Default swaps the global slog default.
func TestTick_SkipsWhenQueueRejects(t *testing.T) {
	rec := capture.Default(t)
	d := &daemon{queue: trigger.NewQueue[struct{}](0)} // zero capacity: every submit is rejected
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.tick("interval")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick() blocked on a rejected submission; it must skip")
	}

	const message = "scheduled sync skipped"
	if got := rec.CountLevel(slog.LevelWarn, message); got != 1 {
		t.Errorf("tick() warning count = %d, want 1; logs = %q", got, rec.Messages())
	}
	if !rec.HasAttr(message, "trigger", "interval") {
		t.Errorf("tick() warning missing trigger=interval; logs = %q", rec.Messages())
	}
	reason := ""
	for _, record := range rec.Records() {
		if record.Message != message {
			continue
		}
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "reason" {
				reason = attr.Value.String()
			}
			return true
		})
	}
	if reason == "" {
		t.Errorf("tick() warning reason is empty; logs = %q", rec.Messages())
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

	tickCtx, stopTicker := context.WithCancel(t.Context())
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
// Runs in a synctest bubble so the "no tick fired" half is exact rather than
// probabilistic — everything in the bubble is in-memory (a buffered queue and
// a startTicker that starts no goroutine at all), so the fake clock can
// always advance.
func TestStartTicker_DisabledInExternalMode(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		d := &daemon{queue: trigger.NewQueue[struct{}](4)}
		done := startTicker(t.Context(), d, time.Millisecond, false)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("startTicker(enabled=false) did not return a closed channel")
		}
		// Advance twenty virtual intervals and let the bubble settle: a
		// regression that started the loop would have submitted ~20 requests
		// by the time this returns, so the assertion below cannot pass
		// vacuously because a real-clock goroutine simply had not been
		// scheduled yet.
		synctest.Sleep(20 * time.Millisecond)
		if n := len(d.queue.Jobs()); n != 0 {
			t.Errorf("%d requests submitted in external mode, want 0", n)
		}
	})
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
	ctx, cancel := context.WithCancel(t.Context())
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

	if code := runClient(t.Context(), sock); code != 0 {
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


// TestRunDaemon_BuiltinModeStartsUnhealthyUntilStartupPassCompletes pins the
// built-in arm of hc.markInitial(!cfg.ScheduleEnabled): the container reports
// unhealthy until the startup pass proves rsync can run. The runner blocks
// command construction, so the marker is sampled after built-in
// initialization but before the startup pass can flip it. Not parallel: it
// uses the package-global healthMarkerPath and env.
func TestRunDaemon_BuiltinModeStartsUnhealthyUntilStartupPassCompletes(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))
	t.Setenv("SYNC_INTERVAL", "6h")
	marker := health.NewMarker(healthMarkerPath)
	marker.Cleanup()
	t.Cleanup(marker.Cleanup)

	entered := make(chan struct{})
	proceed := make(chan struct{})
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		close(entered)
		<-proceed
		return exec.CommandContext(ctx, "true")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, testSocketPath(t), runner) }()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("startup pass did not begin")
	}
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("built-in marker before startup completion: stat error = %v, want not-exist", err)
	}

	cancel()
	close(proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDaemon() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after shutdown")
	}
}


// TestRunDaemon_UnusableKnownHostsRefusesStartupWithDiagnostic pins the
// published refusal: a mounted known_hosts the app cannot read a host key out
// of must stop the boot with an actionable record, before the trigger socket
// or the health marker exist. Not parallel: it swaps the global slog default,
// sets env, reassigns knownHostsPath and uses the package-global
// healthMarkerPath.
func TestRunDaemon_UnusableKnownHostsRefusesStartupWithDiagnostic(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	t.Setenv("SYNC_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	_ = os.Remove(healthMarkerPath)

	emptyKnownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(emptyKnownHosts, []byte("# no entries\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	originalKnownHostsPath := knownHostsPath
	knownHostsPath = emptyKnownHosts
	t.Cleanup(func() { knownHostsPath = originalKnownHostsPath })

	rec := capture.Default(t)
	sock := testSocketPath(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- runDaemon(ctx, sock, fixedRunner("true")) }()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("runDaemon did not refuse an unusable known_hosts file")
	}
	if runErr == nil {
		t.Fatal("runDaemon() with an unusable known_hosts file = nil, want error")
	}

	const message = "cannot determine the ssh host-key posture"
	if got := rec.CountLevel(slog.LevelError, message); got != 1 {
		t.Errorf("%q ERROR records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	wantHint := "mount a non-empty known_hosts file at " + emptyKnownHosts + ", or omit the mount for accept-new"
	if !rec.HasAttr(message, "hint", wantHint) {
		t.Errorf("%q missing hint=%q; logs = %q", message, wantHint, rec.Messages())
	}
	if _, err := os.Stat(sock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("trigger socket exists after refused startup; stat error = %v, want not-exist", err)
	}
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("health marker exists after refused startup; stat error = %v, want not-exist", err)
	}
}

// TestRunDaemon_ShutdownCancelsQueuedClientAfterInFlightDrain pins the shutdown
// ORDER, which is a property of five statements in runDaemon and can only be
// observed with work present in BOTH request states at the drain. The executor
// unit test stages both states against the queue directly, so it cannot see the
// listener-close, queue-close, executor-wait, ticker-wait, server-wait sequence;
// the live external-mode test cancels only after its sole client has completed.
// Request A is held inside the pass runner, B's queued wire event is awaited,
// then the context is cancelled: A must get its clean interrupted result, B must
// get the scheduler's explicit cancellation reason and never a started event,
// and runDaemon itself must return.
// Not parallel: sets env and binds a socket.
func TestRunDaemon_ShutdownCancelsQueuedClientAfterInFlightDrain(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))
	t.Setenv("SYNC_INTERVAL", "off")
	marker := health.NewMarker(healthMarkerPath)
	marker.Cleanup()
	t.Cleanup(marker.Cleanup)

	entered := make(chan struct{})
	proceed := make(chan struct{})
	var enterOnce sync.Once
	var proceedOnce sync.Once
	release := func() { proceedOnce.Do(func() { close(proceed) }) }
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		enterOnce.Do(func() { close(entered) })
		<-proceed
		return exec.CommandContext(ctx, "sleep", "30")
	}

	ctx, cancel := context.WithCancel(t.Context())
	sock := testSocketPath(t)
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = runDaemon(ctx, sock, runner)
	}()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runDaemon did not stop during test cleanup")
		}
	})

	waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(sock)
		return err == nil
	}, "daemon did not bind the trigger socket")

	decInFlight, _ := rawRequest(t, sock)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not enter the pass runner")
	}
	decQueued, _ := rawRequest(t, sock)
	if ev := nextEvent(t, decQueued); ev.Kind != trigger.EventQueued {
		t.Fatalf("queued client's first event = %q, want %q", ev.Kind, trigger.EventQueued)
	}

	cancel()
	release()

	for {
		ev := nextEvent(t, decInFlight)
		if ev.Kind != trigger.EventDone {
			continue
		}
		if !ev.OK {
			t.Errorf("in-flight client's final event = %+v, want done ok=true", ev)
		}
		break
	}
	for {
		ev := nextEvent(t, decQueued)
		if ev.Kind == trigger.EventStarted {
			t.Error("queued client received a started event after shutdown")
		}
		if ev.Kind != trigger.EventDone {
			continue
		}
		if ev.OK || ev.Reason != trigger.CancelledReason {
			t.Errorf("queued client's final event = %+v, want done ok=false reason=%q", ev, trigger.CancelledReason)
		}
		break
	}

	select {
	case <-done:
		if runErr != nil {
			t.Errorf("runDaemon() = %v, want nil", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDaemon did not return after draining in-flight and queued requests")
	}
}
