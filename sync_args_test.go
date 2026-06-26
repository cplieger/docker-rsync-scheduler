package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// argJob returns a job with the given knobs set, using a fixed key path
// (buildRsyncArgs does no filesystem access, so the path need not exist).
func argJob(delete bool, uid, gid *int, excludes []string) *job {
	return &job{
		Name:       "caddy",
		Local:      "/sources/caddy",
		RemoteHost: "root@192.168.1.87",
		RemotePath: "/srv/containers/caddy",
		SSHKey:     "/keys/id_ed25519",
		RemoteUID:  uid,
		RemoteGID:  gid,
		Delete:     delete,
		Excludes:   excludes,
	}
}

const (
	wantSSHAcceptNew = "ssh -i /keys/id_ed25519 -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10"
	wantSSHStrict    = "ssh -i /keys/id_ed25519 -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/config/known_hosts -o BatchMode=yes -o ConnectTimeout=10"
)

func TestBuildRsyncArgs(t *testing.T) {
	// Not parallel: this test overrides the shared package-level knownHostsExists
	// var; running it concurrently with the other knownHostsExists-overriding
	// tests is a data race on that var (go test -race). The subtests below only
	// read the var (via buildRsyncArgs) and stay parallel.

	// Ensure baseline tests run with known_hosts absent (accept-new mode).
	origKH := knownHostsExists
	knownHostsExists = func() bool { return false }
	t.Cleanup(func() { knownHostsExists = origKH })

	t.Run("minimal has no delete or chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, nil, nil))

		if got[0] != "-rlptD" {
			t.Errorf("first arg = %q, want -rlptD", got[0])
		}
		if slices.Contains(got, "--delete") {
			t.Error("--delete present, want absent")
		}
		if hasChown(got) {
			t.Error("--chown present, want absent")
		}
		if !slices.Contains(got, "--stats") {
			t.Error("--stats absent")
		}
		assertSSHArg(t, got)
		assertGlobalExcludes(t, got)
		if got[len(got)-2] != "/sources/caddy/" {
			t.Errorf("source = %q, want /sources/caddy/", got[len(got)-2])
		}
		if got[len(got)-1] != "root@192.168.1.87:/srv/containers/caddy/" {
			t.Errorf("dest = %q, want root@192.168.1.87:/srv/containers/caddy/", got[len(got)-1])
		}
	})

	t.Run("delete adds --delete", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(true, nil, nil, nil))
		if !slices.Contains(got, "--delete") {
			t.Errorf("--delete absent in %v", got)
		}
	})

	t.Run("uid and gid add chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, new(1000), new(1000), nil))
		if !slices.Contains(got, "--chown=1000:1000") {
			t.Errorf("--chown=1000:1000 absent in %v", got)
		}
	})

	t.Run("uid only does not add chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, new(1000), nil, nil))
		if hasChown(got) {
			t.Errorf("--chown present with gid unset in %v", got)
		}
	})

	t.Run("gid only does not add chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, new(1000), nil))
		if hasChown(got) {
			t.Errorf("--chown present with uid unset in %v", got)
		}
	})

	t.Run("excludes appended after globals", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, nil, []string{"**/*.lock", "logs"}))
		if !slices.Contains(got, "--exclude=**/*.lock") {
			t.Errorf("per-job exclude absent in %v", got)
		}
		if !slices.Contains(got, "--exclude=logs") {
			t.Errorf("per-job exclude absent in %v", got)
		}
		globalIdx := slices.Index(got, "--exclude=Thumbs.db")
		jobIdx := slices.Index(got, "--exclude=**/*.lock")
		if globalIdx == -1 || jobIdx == -1 || jobIdx < globalIdx {
			t.Errorf("per-job excludes must follow global excludes: %v", got)
		}
	})

	t.Run("all knobs together", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(true, new(0), new(0), []string{"logs"}))
		want := []string{
			"-rlptD", "--delete", "--chown=0:0", "--stats", "-e", wantSSHAcceptNew,
			"--exclude=.stfolder", "--exclude=.stversions",
			"--exclude=.DS_Store", "--exclude=Thumbs.db",
			"--exclude=logs",
			"/sources/caddy/", "root@192.168.1.87:/srv/containers/caddy/",
		}
		if !slices.Equal(got, want) {
			t.Errorf("buildRsyncArgs =\n  %v\nwant\n  %v", got, want)
		}
	})
}

func TestSSHCommand_KnownHostsAbsent(t *testing.T) {
	// Not parallel: overrides the shared package-level knownHostsExists var.
	orig := knownHostsExists
	knownHostsExists = func() bool { return false }
	t.Cleanup(func() { knownHostsExists = orig })

	got := sshCommand("/keys/id_ed25519")
	if got != wantSSHAcceptNew {
		t.Errorf("sshCommand (no known_hosts) = %q, want %q", got, wantSSHAcceptNew)
	}
	if !strings.Contains(got, "StrictHostKeyChecking=accept-new") {
		t.Error("expected accept-new when known_hosts is absent")
	}
	if strings.Contains(got, "UserKnownHostsFile") {
		t.Error("UserKnownHostsFile must not be present when known_hosts is absent")
	}
}

func TestSSHCommand_KnownHostsPresent(t *testing.T) {
	// Not parallel: overrides the shared package-level knownHostsExists var.
	orig := knownHostsExists
	knownHostsExists = func() bool { return true }
	t.Cleanup(func() { knownHostsExists = orig })

	got := sshCommand("/keys/id_ed25519")
	if got != wantSSHStrict {
		t.Errorf("sshCommand (known_hosts present) = %q, want %q", got, wantSSHStrict)
	}
	if !strings.Contains(got, "StrictHostKeyChecking=yes") {
		t.Error("expected StrictHostKeyChecking=yes when known_hosts is present")
	}
	if !strings.Contains(got, "UserKnownHostsFile=/config/known_hosts") {
		t.Error("expected UserKnownHostsFile=/config/known_hosts when known_hosts is present")
	}
	if !strings.Contains(got, "BatchMode=yes") {
		t.Error("BatchMode=yes must always be present")
	}
	if !strings.Contains(got, "ConnectTimeout=10") {
		t.Error("ConnectTimeout=10 must always be present")
	}
}

func TestSSHHostKeyMode(t *testing.T) {
	// Not parallel: overrides the shared package-level knownHostsExists var,
	// matching the TestSSHCommand_* convention.
	orig := knownHostsExists
	t.Cleanup(func() { knownHostsExists = orig })

	t.Run("known_hosts present reports strict", func(t *testing.T) {
		knownHostsExists = func() bool { return true }
		if got := sshHostKeyMode(); got != "strict" {
			t.Errorf("sshHostKeyMode() with known_hosts present = %q, want \"strict\"", got)
		}
	})

	t.Run("known_hosts absent reports accept-new", func(t *testing.T) {
		knownHostsExists = func() bool { return false }
		if got := sshHostKeyMode(); got != "accept-new" {
			t.Errorf("sshHostKeyMode() with known_hosts absent = %q, want \"accept-new\"", got)
		}
	})
}

func TestBuildRsyncArgs_KnownHostsPresent(t *testing.T) {
	// Not parallel: overrides the shared package-level knownHostsExists var.
	orig := knownHostsExists
	knownHostsExists = func() bool { return true }
	t.Cleanup(func() { knownHostsExists = orig })

	got := buildRsyncArgs(argJob(false, nil, nil, nil))
	i := slices.Index(got, "-e")
	if i == -1 || i+1 >= len(got) {
		t.Fatalf("-e argument missing in %v", got)
	}
	if got[i+1] != wantSSHStrict {
		t.Errorf("ssh arg (known_hosts) = %q, want %q", got[i+1], wantSSHStrict)
	}
}

// TestBuildRsyncArgs_maxDelete pins the cycle-1 max_delete feature: the
// --max-delete=N append is nested inside the `if j.Delete` block, so it must
// surface only when BOTH delete and max_delete are set, be omitted when
// max_delete is unset, and be a no-op when max_delete is set without delete
// (--max-delete is meaningless without --delete).
func TestBuildRsyncArgs_maxDelete(t *testing.T) {
	// Not parallel: overrides the shared package-level knownHostsExists var,
	// matching the other buildRsyncArgs tests.
	orig := knownHostsExists
	knownHostsExists = func() bool { return false }
	t.Cleanup(func() { knownHostsExists = orig })

	t.Run("delete with max_delete emits the flag", func(t *testing.T) {
		j := argJob(true, nil, nil, nil)
		j.MaxDelete = new(100)
		got := buildRsyncArgs(j)
		if !slices.Contains(got, "--max-delete=100") {
			t.Errorf("--max-delete=100 absent in %v", got)
		}
	})

	t.Run("delete without max_delete omits the flag", func(t *testing.T) {
		got := buildRsyncArgs(argJob(true, nil, nil, nil))
		if slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "--max-delete=") }) {
			t.Errorf("--max-delete present with max_delete unset in %v", got)
		}
	})

	t.Run("max_delete without delete is a no-op", func(t *testing.T) {
		j := argJob(false, nil, nil, nil)
		j.MaxDelete = new(100)
		got := buildRsyncArgs(j)
		if slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "--max-delete=") }) {
			t.Errorf("--max-delete present without delete in %v", got)
		}
		if slices.Contains(got, "--delete") {
			t.Errorf("--delete present with delete=false in %v", got)
		}
	})
}

// TestRemoteDest pins rsync's destination construction, especially the IPv6
// bracketing: an IPv6-literal host must be wrapped in [brackets] so rsync reads
// the address colons as the host, not the daemon-mode "::" separator. IPv4 and
// hostnames are left unbracketed, and an already-bracketed IPv6 input is
// normalized to a single bracket pair.
func TestRemoteDest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, host, path, want string
	}{
		{"ipv4 with user", "root@192.168.1.87", "/srv/x", "root@192.168.1.87:/srv/x/"},
		{"hostname no user", "example.com", "/srv/x", "example.com:/srv/x/"},
		{"bare ipv6 gets brackets", "2001:db8::1", "/srv/x", "[2001:db8::1]:/srv/x/"},
		{"ipv6 with user gets brackets", "user@2001:db8::1", "/srv/x", "user@[2001:db8::1]:/srv/x/"},
		{"already-bracketed ipv6 normalized", "user@[2001:db8::1]", "/srv/x", "user@[2001:db8::1]:/srv/x/"},
		{"ipv6 loopback", "::1", "/srv/x", "[::1]:/srv/x/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &job{RemoteHost: tt.host, RemotePath: tt.path}
			if got := remoteDest(j); got != tt.want {
				t.Errorf("remoteDest(host=%q, path=%q) = %q, want %q", tt.host, tt.path, got, tt.want)
			}
		})
	}
}

// TestRemoteDest_ipv4MappedIPv6Bracketed pins the cycle-1 h-f1 colon-presence
// fix for the IPv4-mapped IPv6 literal ::ffff:192.0.2.1. The old
// net.ParseIP(host).To4()!=nil predicate classified it as IPv4 and left it
// unbracketed, so rsync misread the leading "::" as the daemon-mode separator;
// the colon-presence predicate brackets it. validateRemoteHost accepts the
// host, so this is a reachable, accepted case the existing TestRemoteDest table
// omits.
func TestRemoteDest_ipv4MappedIPv6Bracketed(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, host, want string }{
		{"ipv4-mapped ipv6 bare gets brackets", "::ffff:192.0.2.1", "[::ffff:192.0.2.1]:/srv/x/"},
		{"ipv4-mapped ipv6 with user gets brackets", "user@::ffff:192.0.2.1", "user@[::ffff:192.0.2.1]:/srv/x/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &job{RemoteHost: tt.host, RemotePath: "/srv/x"}
			if got := remoteDest(j); got != tt.want {
				t.Errorf("remoteDest(host=%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func hasChown(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return strings.HasPrefix(a, "--chown=")
	})
}

func assertSSHArg(t *testing.T, args []string) {
	t.Helper()
	i := slices.Index(args, "-e")
	if i == -1 || i+1 >= len(args) {
		t.Fatalf("-e argument missing in %v", args)
	}
	if args[i+1] != wantSSHAcceptNew {
		t.Errorf("ssh arg = %q, want %q", args[i+1], wantSSHAcceptNew)
	}
}

func assertGlobalExcludes(t *testing.T, args []string) {
	t.Helper()
	for _, e := range globalExcludes {
		if !slices.Contains(args, "--exclude="+e) {
			t.Errorf("global exclude --exclude=%s absent in %v", e, args)
		}
	}
}

func TestProperty_BuildRsyncArgsInvariants(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		del := rapid.Bool().Draw(rt, "delete")
		hasUID := rapid.Bool().Draw(rt, "hasUID")
		hasGID := rapid.Bool().Draw(rt, "hasGID")

		var uid, gid *int
		if hasUID {
			uid = new(rapid.IntRange(0, 70000).Draw(rt, "uid"))
		}
		if hasGID {
			gid = new(rapid.IntRange(0, 70000).Draw(rt, "gid"))
		}
		n := rapid.IntRange(0, 4).Draw(rt, "nExcludes")
		excludes := make([]string, n)
		for i := range n {
			excludes[i] = rapid.StringMatching(`[a-z*/.]{1,8}`).Draw(rt, "exclude")
		}

		got := buildRsyncArgs(argJob(del, uid, gid, excludes))

		if got[0] != "-rlptD" {
			rt.Fatalf("first arg = %q, want -rlptD", got[0])
		}
		if !strings.HasSuffix(got[len(got)-2], "/") {
			rt.Fatalf("source %q must end in /", got[len(got)-2])
		}
		if !strings.HasSuffix(got[len(got)-1], "/") {
			rt.Fatalf("dest %q must end in /", got[len(got)-1])
		}
		// chown present iff both uid and gid set.
		wantChown := uid != nil && gid != nil
		if hasChown(got) != wantChown {
			rt.Fatalf("chown present = %v, want %v", hasChown(got), wantChown)
		}
		// global excludes always present.
		for _, e := range globalExcludes {
			if !slices.Contains(got, "--exclude="+e) {
				rt.Fatalf("global exclude %q missing", e)
			}
		}
	})
}

func TestRemoteDest_bracketedIPv4Normalized(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, host, want string }{
		{"bracketed ipv4 normalized to bare", "[192.0.2.10]", "192.0.2.10:/srv/x/"},
		{"user on bracketed ipv4 normalized", "user@[192.0.2.10]", "user@192.0.2.10:/srv/x/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := &job{RemoteHost: tt.host, RemotePath: "/srv/x"}
			if got := remoteDest(j); got != tt.want {
				t.Errorf("remoteDest(host=%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

// knownHostsFileExists reports whether knownHostsPath currently exists on disk.
// It detects the test precondition (which branch to assert); the asserted
// expectations are hardcoded boolean literals, never derived from it, so there
// is no tautology. The mutation under test lives in the production
// knownHostsExists closure (sync.go), not in this helper.
func knownHostsFileExists(t *testing.T) bool {
	t.Helper()
	_, err := os.Stat(knownHostsPath)
	return err == nil
}

// snapshotKnownHosts captures the current state of knownHostsPath (existence +
// bytes) and registers a t.Cleanup that restores exactly that state, so the
// test never leaves /config/known_hosts altered.
func snapshotKnownHosts(t *testing.T) {
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

// TestKnownHostsExists_defaultClosureStatsRealPath exercises the default
// knownHostsExists closure (sync.go), whose body `return err == nil` every
// sibling test bypasses by OVERRIDING the package-level knownHostsExists var.
// It is deliberately NON-parallel so it runs in the serial phase, before any
// parallel sibling resumes and reassigns the var: knownHostsExists() here
// invokes the pristine default closure that stats knownHostsPath.
//
// Asserting the closure's boolean against the file's true presence flips with a
// CONDITIONALS_NEGATION (err == nil -> err != nil) in both directions, so
// either branch is a real kill:
//   - known_hosts absent  -> default returns false ; mutant returns true
//   - known_hosts present -> default returns true  ; mutant returns false
func TestKnownHostsExists_defaultClosureStatsRealPath(t *testing.T) {
	// No t.Parallel(): must observe the default closure during the serial phase.
	snapshotKnownHosts(t)

	asserted := false

	// Branch 1: known_hosts ABSENT -> default closure must report false.
	if knownHostsFileExists(t) {
		_ = os.Remove(knownHostsPath)
	}
	if !knownHostsFileExists(t) {
		asserted = true
		if got := knownHostsExists(); got != false {
			t.Errorf("knownHostsExists() with %s absent = %v, want false", knownHostsPath, got)
		}
	}

	// Branch 2: known_hosts PRESENT -> default closure must report true.
	if err := os.WriteFile(knownHostsPath, []byte("placeholder\n"), 0o600); err == nil {
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
