package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/slogx/capture"
)

// newRunJobSource creates a non-empty temp source dir so runJob does not
// take the empty-source skip path.
func newRunJobSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup source: %v", err)
	}
	return dir
}

// runJobJob returns a minimal job rooted at local.
func runJobJob(local string) *job {
	return &job{
		Name:       "caddy",
		Local:      local,
		RemoteHost: "root@192.0.2.87",
		RemotePath: "/srv/containers/caddy",
		SSHKey:     "/keys/id_ed25519",
	}
}

func TestRunJob_successParsesStatsAndMarksSuccess(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'Number of regular files transferred: 5\\nTotal transferred file size: 2048 bytes\\n'; exit 0")
	}
	res := runJob(context.Background(), runJobJob(newRunJobSource(t)), time.Minute, newCmd)
	if !res.success {
		t.Errorf("success = false, want true")
	}
	if res.skipped {
		t.Errorf("skipped = true, want false")
	}
	if res.exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", res.exitCode)
	}
	if res.files != 5 {
		t.Errorf("files = %d, want 5", res.files)
	}
	if res.bytes != 2048 {
		t.Errorf("bytes = %d, want 2048", res.bytes)
	}
}

func TestRunJob_failureCapturesExitCodeAndStderr(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'rsync error: link_stat failed\\n' >&2; exit 23")
	}
	res := runJob(context.Background(), runJobJob(newRunJobSource(t)), time.Minute, newCmd)
	if res.success {
		t.Errorf("success = true, want false")
	}
	if res.exitCode != 23 {
		t.Errorf("exitCode = %d, want 23", res.exitCode)
	}
	if !strings.Contains(res.stderrTail, "rsync error") {
		t.Errorf("stderrTail missing rsync error")
	}
}

func TestRunJob_emptySourceSkipsWithoutRunning(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked for empty source; want skip")
		return exec.CommandContext(ctx, "true")
	}
	res := runJob(context.Background(), runJobJob(t.TempDir()), time.Minute, newCmd)
	if !res.skipped {
		t.Errorf("skipped = false, want true")
	}
	if !res.success {
		t.Errorf("success = false, want true (skip counts as success)")
	}
}

func TestRunPass_aggregatesFailures(t *testing.T) {
	t.Parallel()
	var calls int
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return exec.CommandContext(ctx, "sh", "-c", "exit 0")
		}
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	src := newRunJobSource(t)
	cfg := config{Jobs: []job{*runJobJob(src), *runJobJob(src)}}
	r := runPass(context.Background(), cfg, time.Minute, "test", newCmd)
	if r.failed != 1 {
		t.Errorf("failed = %d, want 1", r.failed)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1", r.ok)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if _, healthy := r.healthSignal(); healthy {
		t.Error("healthy = true, want false (a job failed)")
	}
	if r.exitStatus() != 1 {
		t.Errorf("exitStatus = %d, want 1 (a job failed)", r.exitStatus())
	}
}

// TestRunJob_parentCancellationLogsShutdownNotFailure verifies the
// graceful-shutdown arm of runJob (the `if ctx.Err() != nil` branch): when the
// PARENT context is cancelled (container shutdown SIGTERM'd the in-flight
// rsync), the interrupted job must log at INFO ("sync interrupted by shutdown")
// and never at ERROR ("sync failed"). The shutdown and failure arms return
// identical jobResult values, so only the emitted log distinguishes them; this
// protects the no-false-page contract (Loki alerts on level=error).
func TestRunJob_parentCancellationLogsShutdownNotFailure(t *testing.T) {
	rec := capture.Default(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	res := runJob(ctx, runJobJob(newRunJobSource(t)), time.Minute, newCmd)

	if res.success {
		t.Errorf("runJob success = true, want false when parent context cancelled")
	}
	if !rec.Contains("sync interrupted by shutdown") {
		t.Errorf("runJob logs = %q, want to contain 'sync interrupted by shutdown'", rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, ""); got != 0 {
		t.Errorf("runJob emitted %d ERROR record(s), want none on graceful shutdown; logs = %q", got, rec.Messages())
	}
}

// TestRunPass_emptySourceSkippedNotCountedAsFailure verifies the
// `case jr.skipped` arm of runPass's tally: an empty-source job is skipped
// (its runner is never invoked) and counts toward ok+emptySkipped, never
// failed, so the pass is healthy. (The heartbeat wording is asserted
// separately in TestReportPass_ranEmitsHeartbeat.)
func TestRunPass_emptySourceSkippedNotCountedAsFailure(t *testing.T) {
	t.Parallel()
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked for empty-source job; want skip, not exec")
		return exec.CommandContext(ctx, "true")
	}
	cfg := config{Jobs: []job{*runJobJob(t.TempDir())}}

	r := runPass(context.Background(), cfg, time.Minute, "test", newCmd)

	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (an empty-source skip is not a failure)", r.failed)
	}
	if r.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", r.emptySkipped)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1 (a skip counts toward ok)", r.ok)
	}
	if _, healthy := r.healthSignal(); !healthy {
		t.Error("healthy = false, want true (an all-skip pass is healthy)")
	}
}

// TestRunPass_shutdownInterruptedJobIsNotCountedAsFailure pins the completion of
// the l-f8 fix: when graceful shutdown cancels the context mid-pass, the
// interrupted in-flight job must NOT count as a failure (runJob treats it as
// "not a real failure"), the remaining jobs must NOT be started under the dead
// context, and the resulting interrupted-clean pass (failed==0) must take
// healthSignal's no-write carve-out and exit 0 — so no false-unhealthy marker
// outlives the drain.
func TestRunPass_shutdownInterruptedJobIsNotCountedAsFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	var calls int
	newCmd := func(cmdCtx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		cancel() // graceful shutdown lands while this first job is in flight
		return exec.CommandContext(cmdCtx, "sleep", "30")
	}
	src := newRunJobSource(t)
	cfg := config{Jobs: []job{*runJobJob(src), *runJobJob(src)}}

	r := runPass(ctx, cfg, time.Minute, "test", newCmd)

	if calls != 1 {
		t.Errorf("commandRunner calls = %d, want 1 (the second job must be skipped under the cancelled context)", calls)
	}
	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (a shutdown-interrupted job is a graceful drain, not a failure)", r.failed)
	}
	if !r.interrupted {
		t.Error("interrupted = false, want true")
	}
	if set, _ := r.healthSignal(); set {
		t.Error("healthSignal set = true, want false (interrupted-clean must not write a false-unhealthy marker)")
	}
	if got := r.exitStatus(); got != 0 {
		t.Errorf("exitStatus() = %d, want 0 (interrupted-clean exits success)", got)
	}
}

// TestDefaultCommandRunner_structural pins the graceful-shutdown construction
// of the real commandRunner that every runJob/runPass test bypasses via a fake:
// a 5s WaitDelay before SIGKILL, a non-nil SIGTERM Cancel closure, and the
// verbatim arg slice. Mutating WaitDelay (e.g. 5s -> 9s) fails this test.
func TestDefaultCommandRunner_structural(t *testing.T) {
	cmd := defaultCommandRunner(context.Background(), "echo", "hi", "there")
	if cmd.WaitDelay != 5*time.Second {
		t.Errorf("WaitDelay = %v, want 5s", cmd.WaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil, want a SIGTERM closure")
	}
	if want := []string{"echo", "hi", "there"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestDefaultCommandRunner_cancelSignalsProcess exercises the Cancel closure
// body at sync.go:44: a real subprocess is started, its context cancelled, and
// Wait must return a termination error proving the SIGTERM closure ran (the
// closure is otherwise invisible to the fake-injecting unit tests).
func TestDefaultCommandRunner_cancelSignalsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := defaultCommandRunner(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Errorf("Wait() = nil, want a termination error from the cancelled process")
	}
}

// TestRunPass_realFailureDuringShutdownStillUnhealthy pins the failed>0 half of
// healthSignal's interrupted carve-out at the runPass integration level.
func TestRunPass_realFailureDuringShutdownStillUnhealthy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	newCmd := func(cmdCtx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return exec.CommandContext(cmdCtx, "sh", "-c", "exit 1")
		}
		cancel()
		return exec.CommandContext(cmdCtx, "sleep", "30")
	}
	src := newRunJobSource(t)
	cfg := config{Jobs: []job{*runJobJob(src), *runJobJob(src)}}
	r := runPass(ctx, cfg, time.Minute, "test", newCmd)
	if r.failed != 1 {
		t.Errorf("failed = %d, want 1", r.failed)
	}
	if !r.interrupted {
		t.Error("interrupted = false, want true")
	}
	set, healthy := r.healthSignal()
	if !set || healthy {
		t.Errorf("healthSignal() = (set=%v, healthy=%v), want (true, false)", set, healthy)
	}
	if got := r.exitStatus(); got != 1 {
		t.Errorf("exitStatus() = %d, want 1", got)
	}
}
