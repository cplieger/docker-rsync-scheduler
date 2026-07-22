package main

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/health"
	"github.com/cplieger/scheduler/v3"
	"github.com/cplieger/scheduler/v3/trigger"
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
		queue:   trigger.NewQueue[struct{}](queueCapacity),
		hc:      newHealthController(health.NewMarker(markerPath)),
		newCmd:  runner,
		timeout: time.Minute,
	}
	executorDone := make(chan struct{})
	go func() {
		defer close(executorDone)
		trigger.Execute(ctx, d.queue, d.run)
	}()
	t.Cleanup(func() {
		cancelCtx()
		d.queue.Close()
		<-executorDone
	})
	return d, cancelCtx, executorDone, markerPath
}

// submitWait submits a request and returns its outcome.
func submitWait(t *testing.T, d *daemon, r *trigger.Job[struct{}]) trigger.Outcome {
	t.Helper()
	if err := d.queue.Submit(r); err != nil {
		t.Fatalf("Submit() = %v, want nil", err)
	}
	select {
	case out := <-r.Result():
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("request result not delivered within 5s")
		return trigger.Outcome{}
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
		queue:   trigger.NewQueue[struct{}](queueCapacity),
		hc:      newHealthController(health.NewMarker(filepath.Join(t.TempDir(), "marker"))),
		newCmd:  runner,
		timeout: time.Minute,
	}
	execDone := make(chan struct{})
	go func() { defer close(execDone); trigger.Execute(ctx, d.queue, d.run) }()

	ln, err := trigger.Listen(sock)
	if err != nil {
		t.Fatalf("trigger.Listen() = %v", err)
	}
	srv := &trigger.Server[struct{}]{Queue: d.queue}
	srv.Serve(ln)

	t.Cleanup(func() {
		_ = ln.Close()
		cancel()
		d.queue.Close()
		<-execDone
		srv.Wait()
	})
	return sock, d
}

// rawRequest dials the socket, sends a request (the empty `{}` frame), and
// returns the decoder over the event stream plus the connection for cleanup.
func rawRequest(t *testing.T, sock string) (*json.Decoder, net.Conn) {
	t.Helper()
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(struct{}{}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	return json.NewDecoder(conn), conn
}

// nextEvent decodes one event with a test deadline.
func nextEvent(t *testing.T, dec *json.Decoder) trigger.Event {
	t.Helper()
	var ev trigger.Event
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
		return trigger.Event{}
	}
}
