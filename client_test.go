package main

import (
	"log/slog"
	"path/filepath"
	"testing"
)

// TestRunClient_ExitCodesOverRealSocket pins the trigger contract end-to-end
// (the same `sync` → exit 0/1 surface Ofelia consumes): a clean pass exits 0,
// a failing pass exits 1. Not parallel: sets env and installs the production
// logger.
func TestRunClient_ExitCodesOverRealSocket(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Run("clean pass exits zero", func(t *testing.T) {
		writeValidCfg(t, t.TempDir()) // empty source: clean skip pass
		sock, _ := startTestServer(t, fixedRunner("true"))
		if code := runClient(sock); code != 0 {
			t.Errorf("runClient() = %d, want 0", code)
		}
	})
	t.Run("failed pass exits one", func(t *testing.T) {
		writeValidCfg(t, newRunJobSource(t)) // non-empty source: the failing runner executes
		sock, _ := startTestServer(t, fixedRunner("false"))
		if code := runClient(sock); code != 1 {
			t.Errorf("runClient() = %d, want 1", code)
		}
	})
}

// TestRunClient_DaemonUnreachableExitsOne pins the no-daemon failure mode:
// an immediate exit 1 (the trigger reports a failed job), never a hang.
// Exit code only: runClient installs the production logger (setupLogger), so
// its output goes to the real stderr, not a capturable test handler.
func TestRunClient_DaemonUnreachableExitsOne(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if code := runClient(sock); code != 1 {
		t.Errorf("runClient() = %d with no daemon, want 1", code)
	}
}
