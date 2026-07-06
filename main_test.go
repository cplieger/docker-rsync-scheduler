package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeValidCfg writes a minimal valid config (one job whose source is the
// given local dir) plus a readable dummy ssh key, points CONFIG_PATH at it, and
// returns the config path. It is the fixture for the composition-root tests
// below, which drive run/runSync end-to-end with a cancelled context or an
// empty source so no real rsync ever executes.
func writeValidCfg(t *testing.T, local string) string {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	doc := "jobs:\n  - name: caddy\n    local: " + local + "\n" +
		"    remote_host: root@192.0.2.10\n    remote_path: /srv/caddy\n" +
		"    ssh_key: " + key + "\n"
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// TestRun_badConfigReturnsError pins run's composition-root error arm: an
// unreadable/missing config must propagate a non-nil error (which main turns
// into a non-zero exit) rather than starting a daemon on empty config.
// Not parallel: sets env and writes the real health marker at /tmp/.healthy.
func TestRun_badConfigReturnsError(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	if err := run(context.Background()); err == nil {
		t.Fatal("run() with a missing config = nil, want error")
	}
}

// TestRun_externalModeReturnsNilOnShutdown pins the SYNC_INTERVAL=off dispatch:
// run must select runExternal (idle until ctx.Done), so a pre-cancelled context
// returns nil cleanly after the drain rather than blocking or erroring.
// Not parallel: sets env and writes the real health marker.
func TestRun_externalModeReturnsNilOnShutdown(t *testing.T) {
	t.Setenv("CONFIG_PATH", writeValidCfg(t, t.TempDir()))
	t.Setenv("SYNC_INTERVAL", "off")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx); err != nil {
		t.Fatalf("run() external-mode cancelled = %v, want nil", err)
	}
}

// TestRun_builtinModeReturnsNilOnShutdown pins the built-in-scheduler dispatch:
// run must select runBuiltin, run its startup pass, and return nil when the
// context is already cancelled (the startup pass is interrupted before any job
// starts, the drain watcher fires, and the select loop exits on ctx.Done).
// Not parallel: contends on the real lockFilePath and writes the marker.
func TestRun_builtinModeReturnsNilOnShutdown(t *testing.T) {
	t.Setenv("CONFIG_PATH", writeValidCfg(t, t.TempDir()))
	t.Setenv("SYNC_INTERVAL", "6h")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx); err != nil {
		t.Fatalf("run() built-in-mode cancelled = %v, want nil", err)
	}
}

// TestRunSync_badConfigReturnsOne pins runSync's error exit: an unreadable
// config makes the sync subcommand exit 1.
// Not parallel: sets env and writes the real health marker.
func TestRunSync_badConfigReturnsOne(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	if got := runSync(context.Background()); got != 1 {
		t.Fatalf("runSync() with a missing config = %d, want 1", got)
	}
}

// TestRunSync_cleanPassReturnsZero pins runSync's success path: a valid config
// whose single job has an empty source is skipped (a skip counts as success),
// so the pass runs clean and the sync subcommand exits 0. No real rsync runs --
// the empty source short-circuits runJob before the command is built.
// Not parallel: contends on the real lockFilePath and writes the marker.
func TestRunSync_cleanPassReturnsZero(t *testing.T) {
	t.Setenv("CONFIG_PATH", writeValidCfg(t, t.TempDir()))
	if got := runSync(context.Background()); got != 0 {
		t.Fatalf("runSync() over an empty source = %d, want 0", got)
	}
}

// TestDispatch_unknownSubcommandReturnsTwo pins dispatch's arg routing: an
// unrecognized subcommand exits 2 (distinct from the 0/1 of daemon/sync) and
// logs the valid set -- the user-facing CLI-misuse contract.
// Not parallel: mutates the process-global os.Args.
func TestDispatch_unknownSubcommandReturnsTwo(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = []string{"docker-rsync-scheduler", "bogus"}
	if got := dispatch(); got != 2 {
		t.Fatalf("dispatch(bogus) = %d, want 2", got)
	}
}
