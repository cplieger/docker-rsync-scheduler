package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeValidCfg writes a minimal valid config (one job whose source is the
// given local dir) plus a readable dummy ssh key, points CONFIG_PATH at it, and
// returns the config path. It is the fixture for the composition-root tests
// below, which drive runDaemon end-to-end with a cancelled context or an
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
	t.Setenv("CONFIG_PATH", cfgPath)
	return cfgPath
}

// testSocketPath returns a short unix-socket path (unix socket paths are
// length-limited, so t.TempDir() alone can be too deep on some runners).
func testSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "s.sock")
}

// TestRunDaemon_badConfigReturnsError pins the composition-root error arm: an
// unreadable/missing config must propagate a non-nil error (which main turns
// into a non-zero exit) rather than starting a daemon on empty config.
// Not parallel: sets env.
func TestRunDaemon_badConfigReturnsError(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	err := runDaemon(context.Background(), testSocketPath(t), recordingRunner(t, "true"))
	if err == nil {
		t.Fatal("runDaemon() with a missing config = nil, want error")
	}
}

// TestRunDaemon_externalModeReturnsNilOnShutdown pins the SYNC_INTERVAL=off
// dispatch: runDaemon must select external mode (idle until ctx.Done), so a
// pre-cancelled context returns nil cleanly after the drain rather than
// blocking or erroring.
// Not parallel: sets env and writes the real health marker.
func TestRunDaemon_externalModeReturnsNilOnShutdown(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	t.Setenv("SYNC_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDaemon(ctx, testSocketPath(t), recordingRunner(t, "true")); err != nil {
		t.Fatalf("runDaemon() external-mode cancelled = %v, want nil", err)
	}
}

// TestRunDaemon_builtinModeReturnsNilOnShutdown pins the built-in-scheduler
// dispatch: runDaemon must select built-in mode and return nil when the
// context is already cancelled (the ticker submits nothing under a cancelled
// context, the executor drains, and the shutdown sequence completes).
// Not parallel: sets env and writes the real health marker.
func TestRunDaemon_builtinModeReturnsNilOnShutdown(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	t.Setenv("SYNC_INTERVAL", "6h")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDaemon(ctx, testSocketPath(t), recordingRunner(t, "true")); err != nil {
		t.Fatalf("runDaemon() built-in-mode cancelled = %v, want nil", err)
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

// TestProbeOptions_builtinArmsMaxAgeFromJobs pins the healthcheck freshness
// policy: built-in mode arms a deadline of 2*interval + jobs*timeout, so a
// wedged interval loop (marker present but never refreshed) eventually probes
// unhealthy. Not parallel: sets env.
func TestProbeOptions_builtinArmsMaxAgeFromJobs(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // 1 job
	t.Setenv("SYNC_INTERVAL", "1h")
	t.Setenv("SYNC_TIMEOUT", "10m")
	opts := probeOptions()
	if len(opts) != 1 {
		t.Fatalf("probeOptions() built-in = %d options, want 1 (WithMaxAge)", len(opts))
	}
}

// TestProbeOptions_externalAndBrokenConfigDisarm pins the two no-deadline
// arms: external mode never arms a deadline, and an unreadable config in
// built-in mode disarms it (bare probe) rather than risking a false-unhealthy
// restart loop. Not parallel: sets env.
func TestProbeOptions_externalAndBrokenConfigDisarm(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	t.Setenv("SYNC_INTERVAL", "off")
	if opts := probeOptions(); len(opts) != 0 {
		t.Errorf("probeOptions() external = %d options, want 0", len(opts))
	}

	t.Setenv("SYNC_INTERVAL", "1h")
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	if opts := probeOptions(); len(opts) != 0 {
		t.Errorf("probeOptions() with unreadable config = %d options, want 0 (disarmed)", len(opts))
	}
}

// waitFor polls cond until true or the deadline, failing the test with msg.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
