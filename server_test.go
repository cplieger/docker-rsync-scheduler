package main

import (
	"testing"

	"github.com/cplieger/scheduler/v2/trigger"
)

// The broker mechanics (queue semantics, socket hygiene, wire ordering,
// backpressure rejection, undecodable requests, departed clients) are the
// scheduler library's and are tested in scheduler/v2/trigger. The tests here
// pin what stays THIS app's: the executor's pass-outcome policy as observed
// over the real socket. The exit-code half of the same contract is pinned
// end-to-end in client_test.go; the drain-versus-cancel split in
// daemon_test.go.

// TestServer_FailedPassReportsNotOK pins the failure half of the trigger
// contract at the wire level: a failing pass answers done{ok:false}.
// Not parallel: sets env.
func TestServer_FailedPassReportsNotOK(t *testing.T) {
	writeValidCfg(t, newRunJobSource(t)) // non-empty source: the failing runner executes
	sock, _ := startTestServer(t, fixedRunner("false"))
	dec, _ := rawRequest(t, sock)
	for {
		ev := nextEvent(t, dec)
		if ev.Kind != trigger.EventDone {
			continue
		}
		if ev.OK {
			t.Error("done ok=true for a failing pass, want false")
		}
		return
	}
}
