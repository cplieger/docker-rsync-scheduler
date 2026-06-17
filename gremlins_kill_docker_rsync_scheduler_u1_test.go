package main

import (
	"os"
	"testing"
)

// gk_docker_rsync_scheduler_u1_statExists reports whether knownHostsPath
// currently exists on disk. It is used ONLY to detect the test precondition
// (which branch to assert); the asserted expectations below are hardcoded
// boolean literals, never derived from this helper. The mutation under test
// lives in the production knownHostsExists closure (sync.go), not in this
// test-file helper, so there is no tautology.
func gk_docker_rsync_scheduler_u1_statExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(knownHostsPath)
	return err == nil
}

// gk_docker_rsync_scheduler_u1_snapshotKnownHosts captures the current state of
// knownHostsPath (existence + bytes) and registers a t.Cleanup that restores
// exactly that state, so the test never leaves /config/known_hosts altered.
func gk_docker_rsync_scheduler_u1_snapshotKnownHosts(t *testing.T) {
	t.Helper()
	_, statErr := os.Stat(knownHostsPath)
	existed := statErr == nil
	data, readErr := os.ReadFile(knownHostsPath)
	t.Cleanup(func() {
		switch {
		case existed && readErr == nil:
			_ = os.WriteFile(knownHostsPath, data, 0o600)
		case !existed:
			_ = os.Remove(knownHostsPath)
		default:
			// Existed but was unreadable: leave whatever is on disk untouched.
		}
	})
}

// TestKnownHostsExistsDefaultClosure_gk_docker_rsync_scheduler_u1 kills the
// CONDITIONALS_NEGATION mutant at sync.go:80 (the default knownHostsExists
// closure's `return err == nil`, mutated to `return err != nil`).
//
// Every sibling test in sync_test.go OVERRIDES the package-level
// knownHostsExists var (each after calling t.Parallel()), so the default
// closure body is never executed by them — which is exactly why this mutant
// survives. This test is deliberately NON-parallel: it runs in the serial
// phase, before any parallel sibling resumes and reassigns the var, so
// knownHostsExists() here invokes the pristine default closure that stats
// knownHostsPath and returns (err == nil).
//
// Asserting the closure's boolean against the file's true presence flips with
// the mutation in both directions, so either branch is a real kill:
//   - known_hosts absent  -> default returns false ; mutant returns true
//   - known_hosts present -> default returns true  ; mutant returns false
func TestKnownHostsExistsDefaultClosure_gk_docker_rsync_scheduler_u1(t *testing.T) {
	// No t.Parallel(): must observe the default closure during the serial phase.
	gk_docker_rsync_scheduler_u1_snapshotKnownHosts(t)

	asserted := false

	// Branch 1: known_hosts ABSENT -> default closure must report false.
	if gk_docker_rsync_scheduler_u1_statExists(t) {
		_ = os.Remove(knownHostsPath)
	}
	if !gk_docker_rsync_scheduler_u1_statExists(t) {
		asserted = true
		if got := knownHostsExists(); got != false {
			t.Errorf("knownHostsExists() with %s absent = %v, want false", knownHostsPath, got)
		}
	}

	// Branch 2: known_hosts PRESENT -> default closure must report true.
	if err := os.WriteFile(knownHostsPath, []byte("gk-u1 placeholder\n"), 0o600); err == nil {
		asserted = true
		if got := knownHostsExists(); got != true {
			t.Errorf("knownHostsExists() with %s present = %v, want true", knownHostsPath, got)
		}
	} else {
		t.Logf("present-case skipped: cannot create %s: %v", knownHostsPath, err)
	}

	if !asserted {
		t.Fatalf("could not establish a controllable %s state; mutant not exercised", knownHostsPath)
	}
}
