package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/scheduler/v4/trigger"
	"github.com/cplieger/slogx/capture"
)

const (
	startupSkippedMsg    = "startup pass skipped: the last scheduled pass succeeded within the interval"
	recordUnwritableMsg  = "cannot rewrite the last-run record; the startup pass runs and the record is ignored"
	recordLostMsg        = "cannot record the pass outcome; the next boot runs a startup pass"
	recordStaleKeptMsg   = "cannot record the pass outcome or remove the stale record; the next boot may trust it"
	tornRecord           = "2026-01-01T00:00:0"
	freshRecordAge       = time.Hour
	staleRecordAge       = 7 * time.Hour
	builtinTestInterval  = "6h"
	daemonStopBudget     = 5 * time.Second
	startupPassWaitLimit = 2 * time.Second
)

func TestStartTicker_PhasesFirstTickFromRecord(t *testing.T) {
	const interval = time.Hour
	tests := []struct {
		name        string
		remaining   time.Duration
		wantWait    time.Duration
		wantTrigger string
	}{
		{name: "record_read_just_before_boundary", remaining: time.Millisecond, wantWait: time.Millisecond, wantTrigger: triggerInterval},
		{name: "record_read_at_or_after_boundary", remaining: 0, wantWait: 0, wantTrigger: triggerStartup},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := &daemon{queue: trigger.NewQueue[struct{}](4)}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				start := time.Now()

				done := startTicker(ctx, d, interval, true, tt.remaining)

				j := <-d.queue.Jobs()
				if waited := time.Since(start); waited != tt.wantWait {
					t.Errorf("startTicker(remaining=%v) submitted the first pass after %v, want %v", tt.remaining, waited, tt.wantWait)
				}
				if j.Trigger != tt.wantTrigger {
					t.Errorf("startTicker(remaining=%v) first trigger = %q, want %q", tt.remaining, j.Trigger, tt.wantTrigger)
				}
				j.Finish(trigger.Outcome{OK: true})
				cancel()
				<-done
			})
		})
	}
}

// Not parallel: it swaps the global slog default and uses the package-global
// healthMarkerPath, stampPath and env.
func TestRunDaemon_PhasesFirstIntervalFromLastSuccessfulPass(t *testing.T) {
	rec := capture.Default(t)
	stampFile := useTempStamp(t)
	writeRecord(t, stampFile, recordLine(9*time.Second, "ok"))
	startedAt := time.Now()
	cancel, done := startBuiltinDaemon(t, "10s", fixedRunner("true"))

	waitFor(t, 5*time.Second, func() bool { return len(heartbeatTriggers(rec)) >= 1 },
		"the phased first interval pass never ran (a first tick one full interval after boot is the composition root passing interval instead of remaining)")
	elapsed := time.Since(startedAt)

	stopDaemon(t, cancel, done)
	if triggers := heartbeatTriggers(rec); triggers[0] != triggerInterval {
		t.Errorf("first pass trigger = %q, want %q (a startup pass means the seeded record aged past the interval before runDaemon read it)", triggers[0], triggerInterval)
	}
	if elapsed >= 5*time.Second {
		t.Errorf("first interval pass ran %v after boot, want under 5s for a record aged 9s of a 10s interval", elapsed)
	}
}

// Not parallel: it swaps the global slog default and uses the package-global
// healthMarkerPath, stampPath and env.
func TestRunDaemon_DueRecordFiresStartupPassAndBootsUnhealthy(t *testing.T) {
	tests := []struct {
		name   string
		record string // "" seeds no file
	}{
		{name: "missing_record"},
		{name: "torn_record", record: tornRecord},
		{name: "fresh_failed_record", record: recordLine(freshRecordAge, "failed")},
		{name: "stale_successful_record", record: recordLine(staleRecordAge, "ok")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			stampFile := useTempStamp(t)
			if tt.record != "" {
				writeRecord(t, stampFile, tt.record)
			}
			assertDueBoot(t, rec)
		})
	}
}

// Not parallel: it swaps the global slog default and uses the package-global
// healthMarkerPath, stampPath, openRecordForWrite and env.
func TestRunDaemon_UnwritableFreshRecordBootsDue(t *testing.T) {
	rec := capture.Default(t)
	stampFile := useTempStamp(t)
	writeRecord(t, stampFile, recordLine(freshRecordAge, "ok"))
	original := openRecordForWrite
	openRecordForWrite = func(string) error { return fs.ErrPermission }
	t.Cleanup(func() { openRecordForWrite = original })

	assertDueBoot(t, rec)

	if got := rec.CountLevel(slog.LevelWarn, recordUnwritableMsg); got != 1 {
		t.Errorf("%q WARN records = %d, want 1; logs = %q", recordUnwritableMsg, got, rec.Messages())
	}
	if !rec.HasAttr(recordUnwritableMsg, "path", stampFile) {
		t.Errorf("%q missing path=%q; logs = %q", recordUnwritableMsg, stampFile, rec.Messages())
	}
}

// Not parallel: it swaps the global slog default and uses the package-global
// healthMarkerPath, stampPath and env.
func TestRunDaemon_FreshSuccessfulRecordSkipsStartupPassAndBootsHealthy(t *testing.T) {
	rec := capture.Default(t)
	stampFile := useTempStamp(t)
	writeRecord(t, stampFile, recordLine(freshRecordAge, "ok"))
	runner := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("a pass ran although the last-run record was fresh")
		return exec.CommandContext(ctx, "true")
	}
	cancel, done := startBuiltinDaemon(t, builtinTestInterval, runner)

	waitFor(t, startupPassWaitLimit, func() bool {
		return rec.CountExact("container started") == 1
	}, "daemon did not emit its startup record")
	if _, err := os.Stat(healthMarkerPath); err != nil {
		t.Errorf("built-in marker with a fresh record: stat error = %v, want healthy (present)", err)
	}
	if !rec.HasAttr("container started", "startup_pass", "false") {
		t.Errorf("startup record lacks startup_pass=false; logs = %q", rec.Messages())
	}
	if got := rec.CountLevel(slog.LevelInfo, startupSkippedMsg); got != 1 {
		t.Errorf("%q INFO records = %d, want 1; logs = %q", startupSkippedMsg, got, rec.Messages())
	}

	stopDaemon(t, cancel, done)
	if got := len(heartbeatTriggers(rec)); got != 0 {
		t.Errorf("%d passes ran with a fresh record, want 0; logs = %q", got, rec.Messages())
	}
}

// assertDueBoot boots the daemon in built-in mode and requires a due boot:
// the startup pass runs while the marker is still absent, the boot record
// says so, and the marker appears once the pass completes.
func assertDueBoot(t *testing.T, rec *capture.Recorder) {
	t.Helper()
	runner, awaitEntered, release := gatedRunner(t)
	cancel, done := startBuiltinDaemon(t, builtinTestInterval, runner)

	awaitEntered()
	if _, err := os.Stat(healthMarkerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker present while the due startup pass is in flight; stat err = %v, want not-exist", err)
	}
	if !rec.HasAttr("container started", "startup_pass", "true") {
		t.Errorf("startup record lacks startup_pass=true; logs = %q", rec.Messages())
	}
	release()
	waitFor(t, daemonStopBudget, func() bool {
		_, err := os.Stat(healthMarkerPath)
		return err == nil
	}, "marker not set healthy after the startup pass completed")

	stopDaemon(t, cancel, done)
	if triggers := heartbeatTriggers(rec); len(triggers) != 1 || triggers[0] != triggerStartup {
		t.Errorf("pass triggers = %q, want exactly one %q", triggers, triggerStartup)
	}
}

// gatedRunner blocks in command construction until release is called, so a
// test can observe the state while a pass is in flight.
func gatedRunner(t *testing.T) (runner scheduler.CommandRunner, awaitEntered, release func()) {
	t.Helper()
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	runner = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		enteredOnce.Do(func() { close(entered) })
		<-proceed
		return exec.CommandContext(ctx, "true")
	}
	release = func() { releaseOnce.Do(func() { close(proceed) }) }
	t.Cleanup(release)
	awaitEntered = func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(startupPassWaitLimit):
			t.Fatal("startup pass did not begin")
		}
	}
	return runner, awaitEntered, release
}

// startBuiltinDaemon runs runDaemon in built-in mode on a one-job config
// whose source is non-empty, so every pass reaches the runner.
func startBuiltinDaemon(t *testing.T, interval string, runner scheduler.CommandRunner) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	writeValidCfg(t, newRunJobSource(t))
	t.Setenv("SYNC_INTERVAL", interval)
	marker := health.NewMarker(healthMarkerPath)
	marker.Cleanup()
	t.Cleanup(marker.Cleanup)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result <- runDaemon(ctx, testSocketPath(t), runner)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	return cancel, result
}

func stopDaemon(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runDaemon() = %v, want nil", err)
		}
	case <-time.After(daemonStopBudget):
		t.Fatal("runDaemon did not return after shutdown")
	}
}

// Not parallel: sets env.
func TestDaemonRun_RecordsOnlyScheduledPasses(t *testing.T) {
	source := newRunJobSource(t)
	writeValidCfg(t, source)

	stamp, stampFile := newTestStamp(t)
	d := &daemon{
		health:    newTestHealth(t),
		stamp:     stamp,
		stampPath: stampFile,
		newCmd:    fixedRunner("true"),
		timeout:   time.Minute,
	}

	if out := d.run(t.Context(), trigger.TriggerExternal, struct{}{}); !out.OK {
		t.Fatalf("external pass ok = false, want true")
	}
	if rec, known := d.stamp.Last(); known {
		t.Errorf("external pass recorded %+v, want no record", rec)
	}

	if out := d.run(t.Context(), triggerStartup, struct{}{}); !out.OK {
		t.Fatalf("startup pass ok = false, want true")
	}
	rec, known := d.stamp.Last()
	if !known || !rec.OK {
		t.Fatalf("startup pass record = %+v known=%v, want a successful record", rec, known)
	}
	successAt := rec.Time

	d.newCmd = fixedRunner("false")
	if out := d.run(t.Context(), triggerInterval, struct{}{}); out.OK {
		t.Fatalf("failed interval pass ok = true, want false")
	}
	rec, known = d.stamp.Last()
	if !known || rec.OK {
		t.Errorf("failed interval pass record = %+v known=%v, want a failed record", rec, known)
	}
	if !rec.Time.After(successAt) {
		t.Errorf("failed record time %v is not after the success record %v", rec.Time, successAt)
	}

	d.newCmd = fixedRunner("true")
	if out := d.run(t.Context(), triggerInterval, struct{}{}); !out.OK {
		t.Fatalf("recovering interval pass ok = false, want true")
	}
	rec, known = d.stamp.Last()
	if !known || !rec.OK {
		t.Fatalf("recovering interval pass record = %+v known=%v, want a successful record", rec, known)
	}
	recoveredAt := rec.Time

	// Interrupted-clean: the shutdown context is cancelled by the runner, so
	// the pass ends before any job fails and must leave the record alone.
	ctx, cancel := context.WithCancel(t.Context())
	d.newCmd = func(cmdCtx context.Context, _ string, _ ...string) *exec.Cmd {
		cancel()
		return exec.CommandContext(cmdCtx, "sleep", "30")
	}
	out := d.run(ctx, triggerInterval, struct{}{})
	if !out.OK || out.Reason == "" {
		t.Fatalf("interrupted-clean pass = ok:%v reason:%q, want ok with the cut-short reason", out.OK, out.Reason)
	}
	rec, known = d.stamp.Last()
	if !known || !rec.OK || !rec.Time.Equal(recoveredAt) {
		t.Errorf("interrupted-clean pass changed the record to %+v known=%v, want the earlier success at %v", rec, known, recoveredAt)
	}
}

// Not parallel: sets env.
func TestDaemonRun_ConfigReloadFailureRecordsScheduledFailure(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))
	stamp, stampFile := newTestStamp(t)
	d := &daemon{
		health:    newTestHealth(t),
		stamp:     stamp,
		stampPath: stampFile,
		newCmd:    fixedRunner("true"),
		timeout:   time.Minute,
	}
	if out := d.run(t.Context(), triggerStartup, struct{}{}); !out.OK {
		t.Fatalf("startup pass ok = false, want true")
	}

	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	if out := d.run(t.Context(), triggerInterval, struct{}{}); out.OK {
		t.Fatal("reload-failure pass ok = true, want false")
	}
	rec, known := d.stamp.Last()
	if !known || rec.OK {
		t.Errorf("reload-failure record = %+v known=%v, want a failed record", rec, known)
	}
}

// A directory in the record's place stands in for a file the daemon cannot
// rewrite: an empty one it can still unlink, a non-empty one it cannot, and
// neither needs a non-root uid. Not parallel: it swaps the global slog
// default and sets env.
func TestDaemonRun_RecordFailureWarnsAndKeepsPassResult(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t))

	runScheduled := func(t *testing.T, stampFile string) *capture.Recorder {
		t.Helper()
		rec := capture.Default(t)
		d := &daemon{
			health:    newTestHealth(t),
			stamp:     scheduler.NewStamp(stampFile),
			stampPath: stampFile,
			newCmd:    fixedRunner("true"),
			timeout:   time.Minute,
		}
		if out := d.run(t.Context(), triggerInterval, struct{}{}); !out.OK {
			t.Errorf("pass ok = false, want true (a failed record write must not fail the pass)")
		}
		return rec
	}

	t.Run("missing_parent_directory", func(t *testing.T) {
		stampFile := filepath.Join(t.TempDir(), "gone", "last-run")
		rec := runScheduled(t, stampFile)
		if got := rec.CountLevel(slog.LevelWarn, recordLostMsg); got != 1 {
			t.Errorf("%q WARN records = %d, want 1; logs = %q", recordLostMsg, got, rec.Messages())
		}
		if !rec.HasAttr(recordLostMsg, "path", stampFile) {
			t.Errorf("%q missing path=%q; logs = %q", recordLostMsg, stampFile, rec.Messages())
		}
	})

	t.Run("stale_record_removed", func(t *testing.T) {
		stampFile := filepath.Join(t.TempDir(), "last-run")
		if err := os.Mkdir(stampFile, 0o700); err != nil {
			t.Fatalf("setup: %v", err)
		}
		rec := runScheduled(t, stampFile)
		if got := rec.CountLevel(slog.LevelWarn, recordLostMsg); got != 1 {
			t.Errorf("%q WARN records = %d, want 1; logs = %q", recordLostMsg, got, rec.Messages())
		}
		if _, err := os.Stat(stampFile); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stale record still present after a failed write; stat err = %v, want not-exist", err)
		}
	})

	t.Run("stale_record_kept", func(t *testing.T) {
		stampFile := filepath.Join(t.TempDir(), "last-run")
		if err := os.MkdirAll(filepath.Join(stampFile, "child"), 0o700); err != nil {
			t.Fatalf("setup: %v", err)
		}
		rec := runScheduled(t, stampFile)
		if got := rec.CountLevel(slog.LevelWarn, recordStaleKeptMsg); got != 1 {
			t.Errorf("%q WARN records = %d, want 1; logs = %q", recordStaleKeptMsg, got, rec.Messages())
		}
		if value, ok := rec.AttrValueExact(recordStaleKeptMsg, "remove_error"); !ok || value == "" {
			t.Errorf("%q remove_error attribute = %q, %v, want non-empty, true", recordStaleKeptMsg, value, ok)
		}
		if _, err := os.Stat(stampFile); err != nil {
			t.Errorf("record path removed although the unlink must have failed; stat err = %v", err)
		}
	})
}

func TestOpenRecordForWrite(t *testing.T) {
	t.Parallel()
	t.Run("writable_record", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "last-run")
		writeRecord(t, path, recordLine(freshRecordAge, "ok"))
		if err := openRecordForWrite(path); err != nil {
			t.Errorf("openRecordForWrite(writable file) = %v, want nil", err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) == "" {
			t.Errorf("record after the probe = %q, %v; want its content intact", got, err)
		}
	})
	t.Run("directory_in_place_of_the_record", func(t *testing.T) {
		t.Parallel()
		if err := openRecordForWrite(t.TempDir()); err == nil {
			t.Error("openRecordForWrite(directory) = nil, want an error")
		}
	})
	t.Run("read_only_record", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root bypasses file write permissions")
		}
		path := filepath.Join(t.TempDir(), "last-run")
		writeRecord(t, path, recordLine(freshRecordAge, "ok"))
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := openRecordForWrite(path); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("openRecordForWrite(0400 file) = %v, want %v", err, fs.ErrPermission)
		}
	})
}
