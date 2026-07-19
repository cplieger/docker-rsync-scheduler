package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestListenTrigger_RemovesStaleSocketAndSetsOwnerOnly pins the boot hygiene:
// a stale socket file from a SIGKILLed predecessor is replaced, and the live
// socket is owner-only (triggering scoped to the container's user).
func TestListenTrigger_RemovesStaleSocketAndSetsOwnerOnly(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "s.sock")

	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("setup stale socket: %v", err)
	}
	// Simulate a SIGKILL: the file stays, nobody listens. Closing the
	// listener would remove the file, so disable unlink-on-close first.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket file missing after setup: %v", err)
	}

	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() over a stale socket = %v, want nil", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat live socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket permissions = %o, want 0600 (owner-only trigger authority)", perm)
	}
}

// TestServer_EventSequenceForCleanPass pins the wire contract: queued →
// started → done{ok:true}, in that order, one done exactly.
// Not parallel: sets env.
func TestServer_EventSequenceForCleanPass(t *testing.T) {
	writeValidCfg(t, t.TempDir()) // empty source: clean skip pass
	sock, _ := startTestServer(t, fixedRunner("true"))
	dec, _ := rawRequest(t, sock)

	if ev := nextEvent(t, dec); ev.Event != eventQueued {
		t.Fatalf("first event = %q, want %q", ev.Event, eventQueued)
	}
	if ev := nextEvent(t, dec); ev.Event != eventStarted {
		t.Fatalf("second event = %q, want %q", ev.Event, eventStarted)
	}
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || !ev.OK {
		t.Fatalf("final event = %+v, want done ok=true", ev)
	}
}

// TestServer_FailedPassReportsNotOK pins the exit-code half of the trigger
// contract at the wire level. Not parallel: sets env.
func TestServer_FailedPassReportsNotOK(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t)) // non-empty source: the failing runner executes
	sock, _ := startTestServer(t, fixedRunner("false"))
	dec, _ := rawRequest(t, sock)
	for {
		ev := nextEvent(t, dec)
		if ev.Event != eventDone {
			continue
		}
		if ev.OK {
			t.Error("done ok=true for a failing pass, want false")
		}
		return
	}
}

// TestServer_RejectsWhenQueueFull pins honest backpressure: a full queue
// answers immediately with done{ok:false, reason} instead of queueing
// unboundedly or blocking the trigger.
func TestServer_RejectsWhenQueueFull(t *testing.T) {
	t.Parallel()
	sock := filepath.Join(t.TempDir(), "s.sock")

	// No executor: requests sit in the queue. Capacity 1, pre-filled.
	q := newRunQueue(1)
	if err := q.submit(newRequest("external")); err != nil {
		t.Fatalf("pre-fill submit: %v", err)
	}
	ln, err := listenTrigger(sock)
	if err != nil {
		t.Fatalf("listenTrigger() = %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &triggerServer{queue: q}
	go srv.serve(ln)

	dec, _ := rawRequest(t, sock)
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || ev.OK {
		t.Fatalf("event = %+v, want immediate done ok=false", ev)
	}
	if !strings.Contains(ev.Reason, "full") {
		t.Errorf("reason = %q, want a queue-full explanation", ev.Reason)
	}
}

// TestServer_UndecodableRequestAnswersDone pins the protocol's failure mode
// for a malformed client: an explicit done with a reason, never a hang.
// Not parallel: sets env.
func TestServer_UndecodableRequestAnswersDone(t *testing.T) {
	writeValidCfg(t, t.TempDir())
	sock, _ := startTestServer(t, fixedRunner("true"))
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	dec := json.NewDecoder(conn)
	ev := nextEvent(t, dec)
	if ev.Event != eventDone || ev.OK {
		t.Fatalf("event = %+v, want done ok=false for an undecodable request", ev)
	}
}
