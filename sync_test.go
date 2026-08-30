package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	res := runJob(t.Context(), runJobJob(newRunJobSource(t)), time.Minute, transport{}, newCmd)
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
	res := runJob(t.Context(), runJobJob(newRunJobSource(t)), time.Minute, transport{}, newCmd)
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

// TestRunJob_failureRetainsFinalStderrDiagnostic drives more than
// outputCapBytes through the captured stderr and asserts the terminal
// diagnostic -- normally the actionable summary -- survives the cap, and that
// the same bounded value reaches the published failure record.
func TestRunJob_failureRetainsFinalStderrDiagnostic(t *testing.T) {
	rec := capture.Default(t)
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"dd if=/dev/zero bs=1048576 count=2 1>&2 2>/dev/null; printf 'terminal-diagnostic\\n' >&2; exit 23")
	}

	res := runJob(t.Context(), runJobJob(newRunJobSource(t)), time.Minute, transport{}, newCmd)

	if !strings.HasSuffix(res.stderrTail, "terminal-diagnostic\n") {
		t.Errorf("stderrTail suffix = %q, want terminal diagnostic", res.stderrTail)
	}
	if !strings.HasPrefix(res.stderrTail, truncMarker) {
		t.Errorf("stderrTail = %q, want truncation marker", res.stderrTail)
	}
	if len(res.stderrTail) > len(truncMarker)+logStderrTailBytes {
		t.Errorf("len(stderrTail) = %d, want <= %d", len(res.stderrTail), len(truncMarker)+logStderrTailBytes)
	}
	if !rec.HasAttr("sync failed", "stderr", res.stderrTail) {
		t.Errorf("sync failed record does not carry stderrTail %q; logs = %q", res.stderrTail, rec.Messages())
	}
}

// TestRunJob_vanishedFilesIsSuccessWithWarning pins rsync's exit 24: upstream
// routes it to its warning channel and only reports it when nothing else
// failed, so the transfer succeeded for every file that still existed. The job
// must count as a success and log at WARN, never at ERROR.
func TestRunJob_vanishedFilesIsSuccessWithWarning(t *testing.T) {
	rec := capture.Default(t) // process-global handler: no t.Parallel
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'Number of regular files transferred: 3\\nTotal transferred file size: 1024 bytes\\n'; "+
				"printf 'file has vanished: \"/src/gone\"\\n' >&2; exit 24")
	}
	res := runJob(t.Context(), runJobJob(newRunJobSource(t)), time.Minute, transport{}, newCmd)
	if !res.success || res.exitCode != rsyncVanishedExit {
		t.Errorf("runJob(exit 24) = success:%v exitCode:%d, want true 24", res.success, res.exitCode)
	}
	const message = "sync completed with vanished source files"
	if got := rec.CountLevel(slog.LevelWarn, message); got != 1 {
		t.Errorf("%q WARN records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	if got := rec.CountExact("sync failed"); got != 0 {
		t.Errorf("%q emitted %d time(s), want 0; logs = %q", "sync failed", got, rec.Messages())
	}
	for key, value := range map[string]string{"rsync_exit": "24", "files": "3", "bytes": "1024"} {
		if !rec.HasAttr(message, key, value) {
			t.Errorf("%q missing attr %s=%q; logs = %q", message, key, value, rec.Messages())
		}
	}
	if !rec.AttrContains(message, "stderr", "vanished") {
		t.Errorf("%q stderr attr does not name the vanished file; logs = %q", message, rec.Messages())
	}
}

// TestRunPass_vanishedFilesPassStaysHealthyAndExitsZero pins the two contract
// surfaces a stranger's program observes for the same case: the health marker
// value and the `sync` client's exit code.
func TestRunPass_vanishedFilesPassStaysHealthyAndExitsZero(t *testing.T) {
	t.Parallel()
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 24")
	}
	src := newRunJobSource(t)
	r := runPass(t.Context(), config{Jobs: []job{*runJobJob(src)}}, time.Minute, transport{}, "test", newCmd)
	if r.failed != 0 || r.ok != 1 {
		t.Errorf("runPass(exit 24) = failed:%d ok:%d, want 0 1", r.failed, r.ok)
	}
	if !r.healthy() {
		t.Errorf("healthy() = false, want true")
	}
	if got := r.exitStatus(); got != 0 {
		t.Errorf("exitStatus() = %d, want 0 (a vanished-files pass is a success)", got)
	}
}

func TestRunJob_emptySourceSkipsWithoutRunning(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked for empty source; want skip")
		return exec.CommandContext(ctx, "true")
	}
	res := runJob(t.Context(), runJobJob(t.TempDir()), time.Minute, transport{}, newCmd)
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
	r := runPass(t.Context(), cfg, time.Minute, transport{}, "test", newCmd)
	if r.failed != 1 {
		t.Errorf("failed = %d, want 1", r.failed)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1", r.ok)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if r.healthy() {
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

	// Deliberately pre-cancelled (not t.Context()) to drive the shutdown arm.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	res := runJob(ctx, runJobJob(newRunJobSource(t)), time.Minute, transport{}, newCmd)

	if res.success {
		t.Errorf("runJob success = true, want false when parent context cancelled")
	}
	if res.exitCode != -1 {
		t.Errorf("exitCode = %d, want -1 (no child ran, so there is no exit status)", res.exitCode)
	}
	if !rec.Contains("sync interrupted by shutdown") {
		t.Errorf("runJob logs = %q, want to contain 'sync interrupted by shutdown'", rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, ""); got != 0 {
		t.Errorf("runJob emitted %d ERROR record(s), want none on graceful shutdown; logs = %q", got, rec.Messages())
	}
}

// TestRunJob_jobTimeoutIsReportedAsFailure pins the other side of the same arm
// selection: a per-job deadline expiring under a LIVE parent context is a real
// failure, logged at ERROR with the timeout state and the signal-terminated
// exit code, not a graceful drain.
func TestRunJob_jobTimeoutIsReportedAsFailure(t *testing.T) {
	rec := capture.Default(t)
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}

	res := runJob(t.Context(), runJobJob(newRunJobSource(t)), 20*time.Millisecond, transport{}, newCmd)

	if res.success || res.interrupted {
		t.Errorf("runJob timeout = success:%v interrupted:%v, want false false", res.success, res.interrupted)
	}
	const message = "sync failed"
	if got := rec.CountExact(message); got != 1 {
		t.Fatalf("%q emitted %d time(s), want 1; logs = %q", message, got, rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelError, message); got != 1 {
		t.Errorf("%q ERROR records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	for key, value := range map[string]string{
		"timed_out":  "true",
		"rsync_exit": "-1",
		"stderr":     "",
	} {
		if !rec.HasAttr(message, key, value) {
			t.Errorf("%q missing attr %s=%q; logs = %q", message, key, value, rec.Messages())
		}
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

	r := runPass(t.Context(), cfg, time.Minute, transport{}, "test", newCmd)

	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (an empty-source skip is not a failure)", r.failed)
	}
	if r.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", r.emptySkipped)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1 (a skip counts toward ok)", r.ok)
	}
	if !r.healthy() {
		t.Error("healthy = false, want true (an all-skip pass is healthy)")
	}
}

// TestRunPass_shutdownInterruptedJobIsNotCountedAsFailure pins the completion of
// the l-f8 fix: when graceful shutdown cancels the context mid-pass, the
// interrupted in-flight job must NOT count as a failure (runJob treats it as
// "not a real failure"), the remaining jobs must NOT be started under the dead
// context, and the resulting interrupted-clean pass (failed==0) must take
// the health controller's no-write carve-out and exit 0 — so no false-unhealthy marker
// outlives the drain.
func TestRunPass_shutdownInterruptedJobIsNotCountedAsFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())

	var calls int
	newCmd := func(cmdCtx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		cancel() // graceful shutdown lands while this first job is in flight
		return exec.CommandContext(cmdCtx, "sleep", "30")
	}
	src := newRunJobSource(t)
	cfg := config{Jobs: []job{*runJobJob(src), *runJobJob(src)}}

	r := runPass(ctx, cfg, time.Minute, transport{}, "test", newCmd)

	if calls != 1 {
		t.Errorf("commandRunner calls = %d, want 1 (the second job must be skipped under the cancelled context)", calls)
	}
	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (a shutdown-interrupted job is a graceful drain, not a failure)", r.failed)
	}
	if !r.interrupted {
		t.Error("interrupted = false, want true")
	}
	if !r.healthy() {
		t.Error("healthy() = false, want true (interrupted-clean is not a failure)")
	}
	if got := r.exitStatus(); got != 0 {
		t.Errorf("exitStatus() = %d, want 0 (interrupted-clean exits success)", got)
	}
}

// TestDefaultCommandRunner_cancelSignalsProcess exercises the Cancel closure
// body at sync.go:44: a real subprocess is started, its context cancelled, and
// Wait must return a termination error proving the SIGTERM closure ran (the
// closure is otherwise invisible to the fake-injecting unit tests).
func TestDefaultCommandRunner_cancelSignalsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
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
// the interrupted carve-out in apply, at the runPass integration level.
func TestRunPass_realFailureDuringShutdownStillUnhealthy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
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
	r := runPass(ctx, cfg, time.Minute, transport{}, "test", newCmd)
	if r.failed != 1 {
		t.Errorf("failed = %d, want 1", r.failed)
	}
	if !r.interrupted {
		t.Error("interrupted = false, want true")
	}
	if r.healthy() {
		t.Errorf("healthy() = true, want false")
	}
	if got := r.exitStatus(); got != 1 {
		t.Errorf("exitStatus() = %d, want 1", got)
	}
}


// TestRunPass_childFailureUnderCancelledParentStillFails pins the race the
// classifier exists for: a child that reports a concrete non-zero exit of its
// own while the parent context is ALREADY cancelled. os/exec substitutes
// ctx.Err() only for a nil error, so that report reaches runJob intact and must
// be classified as a failure, not swallowed as a drain. A ctx-first read makes
// this row go red. The runner deliberately builds a context-free command so the
// child runs to completion under the cancelled parent, which is the state the
// census measured.
func TestRunPass_childFailureUnderCancelledParentStillFails(t *testing.T) {
	rec := capture.Default(t) // process-global handler: no t.Parallel
	ctx, cancel := context.WithCancel(t.Context())

	newCmd := func(context.Context, string, ...string) *exec.Cmd {
		cancel() // the drain lands while this child is already exiting
		return exec.Command("sh", "-c", "printf 'rsync error: some files could not be transferred\\n' >&2; exit 23")
	}
	src := newRunJobSource(t)

	r := runPass(ctx, config{Jobs: []job{*runJobJob(src)}}, time.Minute, transport{}, "test", newCmd)

	if r.failed != 1 {
		t.Errorf("failed = %d, want 1 (a concrete child failure is not a drain)", r.failed)
	}
	if got := r.exitStatus(); got != 1 {
		t.Errorf("exitStatus() = %d, want 1", got)
	}
	const message = "sync failed"
	if got := rec.CountLevel(slog.LevelError, message); got != 1 {
		t.Errorf("%q ERROR records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	if !rec.HasAttr(message, "rsync_exit", "23") {
		t.Errorf("%q missing rsync_exit=23; logs = %q", message, rec.Messages())
	}
	if got := rec.CountExact("sync interrupted by shutdown"); got != 0 {
		t.Errorf("%q emitted %d time(s), want 0; logs = %q", "sync interrupted by shutdown", got, rec.Messages())
	}
}

// TestRunPass_signalExitUnderCancelledParentIsInterruptedClean pins the
// complement: rsync exits 20 (RERR_SIGNAL) when it is signalled, which is what
// a graceful drain of a mid-transfer pass looks like. Under a cancelled parent
// that stays interrupted-clean — exit 0 for the triggering client and no marker
// write at all — which is the drain contract a bare ctx-first-to-child-first
// swap would destroy.
func TestRunPass_signalExitUnderCancelledParentIsInterruptedClean(t *testing.T) {
	rec := capture.Default(t) // process-global handler: no t.Parallel
	ctx, cancel := context.WithCancel(t.Context())

	newCmd := func(context.Context, string, ...string) *exec.Cmd {
		cancel()
		return exec.Command("sh", "-c", "exit 20")
	}
	src := newRunJobSource(t)

	r := runPass(ctx, config{Jobs: []job{*runJobJob(src)}}, time.Minute, transport{}, "test", newCmd)

	if !r.interrupted {
		t.Error("interrupted = false, want true")
	}
	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (exit 20 under a cancelled parent is a drain)", r.failed)
	}
	if got := r.exitStatus(); got != 0 {
		t.Errorf("exitStatus() = %d, want 0", got)
	}
	m := &fakeMarker{}
	newHealthController(m).apply(&r)
	if _, writes := m.state(); writes != 0 {
		t.Errorf("marker writes = %d, want 0 (an interrupted-clean drain writes nothing)", writes)
	}
	if got := rec.CountExact("sync failed"); got != 0 {
		t.Errorf("%q emitted %d time(s), want 0; logs = %q", "sync failed", got, rec.Messages())
	}
	if !rec.Contains("sync interrupted by shutdown") {
		t.Errorf("logs = %q, want to contain 'sync interrupted by shutdown'", rec.Messages())
	}
}

// TestRunPass_vanishedWithDeleteLimitIsFailure is the oracle for the vanished
// arm's second witness. rsync assigns 24 AFTER the --max-delete status 25, so
// exit 24 alone cannot distinguish "only files vanished" from "the deletion cap
// also tripped"; the del-limit line on stderr is the only surviving evidence.
// Deleting that witness makes this row go red while
// TestRunJob_vanishedFilesIsSuccessWithWarning (exit 24, no such line) stays
// green — the two together pin the split.
func TestRunPass_vanishedWithDeleteLimitIsFailure(t *testing.T) {
	rec := capture.Default(t) // process-global handler: no t.Parallel
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'file has vanished: \"/src/gone\"\\n' >&2; "+
				"printf '"+rsyncDelLimitWarn+" (4 skipped)\\n' >&2; exit 24")
	}
	src := newRunJobSource(t)

	r := runPass(t.Context(), config{Jobs: []job{*runJobJob(src)}}, time.Minute, transport{}, "test", newCmd)

	if r.failed != 1 {
		t.Errorf("failed = %d, want 1 (a capped deletion is a failed pass)", r.failed)
	}
	if got := r.exitStatus(); got != 1 {
		t.Errorf("exitStatus() = %d, want 1", got)
	}
	const message = "sync failed"
	if got := rec.CountLevel(slog.LevelError, message); got != 1 {
		t.Errorf("%q ERROR records = %d, want 1; logs = %q", message, got, rec.Messages())
	}
	if got := rec.CountExact("sync completed with vanished source files"); got != 0 {
		t.Errorf("%q emitted %d time(s), want 0; logs = %q",
			"sync completed with vanished source files", got, rec.Messages())
	}
}
