package main

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v2"
)

// Log capture goes through slogx/capture (capture.Default(t)): the Recorder
// is concurrency-safe, so it also covers the tests that poll logs while a
// daemon goroutine is still writing them. Tests using it must NOT be
// parallel: it swaps the global slog default.

// fixedRunner returns a CommandRunner whose child is the given binary
// regardless of the requested rsync args — the standard fake for pass-level
// tests ("true" = a job that succeeds, "false" = a job that fails).
func fixedRunner(bin string) scheduler.CommandRunner {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, bin)
	}
}

// recordingRunner is fixedRunner with a test handle, for call sites that read
// better with the t argument (parity with the sibling scheduler's suite).
func recordingRunner(t *testing.T, bin string) scheduler.CommandRunner {
	t.Helper()
	return fixedRunner(bin)
}

// newTestDaemon builds a daemon wired to a temp health marker and the given
// runner, with the executor started. Returns the daemon, the shutdown cancel,
// a channel closed when the executor has drained, and the marker path.
func newTestDaemon(t *testing.T, runner scheduler.CommandRunner) (d *daemon, cancel context.CancelFunc, done <-chan struct{}, markerPath string) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	markerPath = filepath.Join(t.TempDir(), "marker")
	d = &daemon{
		queue:   newRunQueue(queueCapacity),
		hc:      newHealthController(health.NewMarker(markerPath)),
		newCmd:  runner,
		timeout: time.Minute,
	}
	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		d.runPasses(ctx)
	}()
	t.Cleanup(func() {
		cancelCtx()
		d.queue.close()
		<-executorDone
	})
	return d, cancelCtx, executorDone, markerPath
}

// submitWait submits a request and returns its outcome.
func submitWait(t *testing.T, d *daemon, r *request) runOutcome {
	t.Helper()
	if err := d.queue.submit(r); err != nil {
		t.Fatalf("submit() = %v, want nil", err)
	}
	select {
	case out := <-r.result:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("request result not delivered within 5s")
		return runOutcome{}
	}
}

// startTestServer wires a queue + executor + trigger server on a temp socket
// and returns the socket path plus the daemon. The caller is responsible for
// CONFIG_PATH (the executor reloads the config per pass). Everything is torn
// down via t.Cleanup.
func startTestServer(t *testing.T, runner scheduler.CommandRunner) (sock string, d *daemon) {
	t.Helper()
	sock = filepath.Join(t.TempDir(), "s.sock")

	ctx, cancel := context.WithCancel(context.Background())
	d = &daemon{
		queue:   newRunQueue(queueCapacity),
		hc:      newHealthController(health.NewMarker(filepath.Join(t.TempDir(), "marker"))),
		newCmd:  runner,
		timeout: time.Minute,
	}
	execDone := make(chan struct{})
	go func() { defer close(execDone); d.runPasses(ctx) }()

	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() = %v", err)
	}
	srv := &triggerServer{queue: d.queue}
	go srv.serve(ln)

	t.Cleanup(func() {
		_ = ln.Close()
		cancel()
		d.queue.close()
		<-execDone
		srv.handlers.Wait()
	})
	return sock, d
}

// rawRequest dials the socket, sends a request, and returns the decoder over
// the event stream plus the connection for cleanup.
func rawRequest(t *testing.T, sock string) (*json.Decoder, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(wireRequest{}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	return json.NewDecoder(conn), conn
}

// nextEvent decodes one event with a test deadline.
func nextEvent(t *testing.T, dec *json.Decoder) wireEvent {
	t.Helper()
	var ev wireEvent
	done := make(chan error, 1)
	go func() { done <- dec.Decode(&ev) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("decode event: %v", err)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return wireEvent{}
	}
}

// TestWireEvent_OKIsExplicitOnTheWire pins the protocol regression guard: a
// done event always carries "ok" (a failed pass must be explicit, not an
// omitted field a lenient decoder defaults).
func TestWireEvent_OKIsExplicitOnTheWire(t *testing.T) {
	t.Parallel()
	for _, ok := range []bool{true, false} {
		raw, err := json.Marshal(wireEvent{Event: eventDone, OK: ok})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"ok":`) {
			t.Errorf("wire form %s omits the ok field (ok=%v), want it explicit", raw, ok)
		}
	}
}

// TestWireEvent_RoundTrip pins event symmetry through JSON.
func TestWireEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	ev := wireEvent{Event: eventDone, Reason: "r", DurationMs: 42, OK: true}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got wireEvent
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != ev {
		t.Errorf("round trip = %+v, want %+v", got, ev)
	}
}
