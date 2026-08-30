package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/health"
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
	err := runDaemon(t.Context(), testSocketPath(t), fixedRunner("true"))
	if err == nil {
		t.Fatal("runDaemon() with a missing config = nil, want error")
	}
}

// TestRunDaemon_externalModeReturnsNilOnShutdown pins the SYNC_INTERVAL=off
// dispatch: runDaemon must select external mode (idle until ctx.Done), so an
// already-cancelled parent returns nil cleanly after the drain rather than
// blocking or erroring. That state is reachable: dispatch binds the signal
// context ABOVE the runDaemon call, so a SIGTERM delivered during boot hands
// runDaemon an already-cancelled parent — which is what decides whether a
// `docker stop` shortly after `docker start` exits 0.
// Not parallel: sets env and writes the real health marker.
func TestRunDaemon_externalModeReturnsNilOnShutdown(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	t.Setenv("SYNC_INTERVAL", "off")
	t.Cleanup(func() { _ = os.Remove(healthMarkerPath) })
	// Deliberately pre-cancelled (not t.Context()) to drive the shutdown arm.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDaemon(ctx, testSocketPath(t), fixedRunner("true")); err != nil {
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
	// Deliberately pre-cancelled (not t.Context()) to drive the shutdown arm.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDaemon(ctx, testSocketPath(t), fixedRunner("true")); err != nil {
		t.Fatalf("runDaemon() built-in-mode cancelled = %v, want nil", err)
	}
}

// TestDispatch_unknownSubcommandReturnsTwo pins dispatch's arg routing: any
// argv that names no subcommand exits 2 (distinct from the 0/1 of daemon/sync)
// and logs the valid set — the user-facing CLI-misuse contract. A bare
// invocation is one of them: `daemon` is what the image's CMD passes, not what
// an absent argument selects.
// Not parallel: mutates the process-global os.Args.
func TestDispatch_unknownSubcommandReturnsTwo(t *testing.T) {
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	// An absent config so a row that reached the daemon arm would fail on its
	// own terms rather than on whatever /config/config.yaml the machine holds.
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))

	for _, args := range [][]string{
		{"docker-rsync-scheduler", "bogus"},
		{"docker-rsync-scheduler"},
	} {
		os.Args = args
		if got := dispatch(); got != 2 {
			t.Errorf("dispatch(%v) = %d, want 2", args, got)
		}
	}
}

// TestProbeOptions_builtinArmsMaxAgeFromJobs pins the healthcheck freshness
// policy: built-in mode arms a deadline of 2*interval + jobs*timeout, so a
// wedged interval loop (marker present but never refreshed) eventually probes
// unhealthy. Driven through the public probe decision either side of the
// published 2h10m boundary. Not parallel: sets env.
func TestProbeOptions_builtinArmsMaxAgeFromJobs(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // 1 job
	t.Setenv("SYNC_INTERVAL", "1h")
	t.Setenv("SYNC_TIMEOUT", "10m")

	markerPath := filepath.Join(t.TempDir(), "marker")
	if err := os.WriteFile(markerPath, []byte("healthy\n"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	opts := probeOptions()
	now := time.Now()

	freshTime := now.Add(-125 * time.Minute)
	if err := os.Chtimes(markerPath, freshTime, freshTime); err != nil {
		t.Fatalf("set fresh marker time: %v", err)
	}
	if got := health.ProbeCheck(markerPath, opts...); got != 0 {
		t.Errorf("ProbeCheck(marker age 2h5m) = %d, want 0", got)
	}

	staleTime := now.Add(-135 * time.Minute)
	if err := os.Chtimes(markerPath, staleTime, staleTime); err != nil {
		t.Fatalf("set stale marker time: %v", err)
	}
	if got := health.ProbeCheck(markerPath, opts...); got != 1 {
		t.Errorf("ProbeCheck(marker age 2h15m) = %d, want 1", got)
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

// TestProbeOptions_readableInvalidConfigsDisarm pins the two remaining disarm
// arms, both with a READABLE config: a malformed document (parse error) and a
// valid one over configCapBytes (size refusal). Both must yield a bare probe
// rather than a deadline, since a false-unhealthy would restart-loop the
// container. Not parallel: sets env.
func TestProbeOptions_readableInvalidConfigsDisarm(t *testing.T) {
	t.Setenv("SYNC_INTERVAL", "1h")
	tests := []struct {
		name string
		data []byte
	}{
		{name: "malformed_yaml", data: []byte("jobs: [")},
		// Stays valid YAML past the cap, so it witnesses the size refusal:
		// without that guard the document parses to zero jobs and one option
		// comes back.
		{name: "over_cap", data: []byte("jobs: []\n#" + strings.Repeat("x", configCapBytes))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("CONFIG_PATH", path)
			if opts := probeOptions(); len(opts) != 0 {
				t.Errorf("probeOptions() with %s config = %d options, want 0 (disarmed)", tt.name, len(opts))
			}
		})
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
