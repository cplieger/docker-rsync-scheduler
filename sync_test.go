package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestParseStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		wantFiles int64
		wantBytes int64
	}{
		{
			name: "full stats block",
			in: "Number of files: 12 (reg: 10, dir: 2)\n" +
				"Number of regular files transferred: 5\n" +
				"Total file size: 1,000 bytes\n" +
				"Total transferred file size: 2,048 bytes\n" +
				"sent 3,000 bytes  received 96 bytes\n",
			wantFiles: 5,
			wantBytes: 2048,
		},
		{
			name:      "sent fallback when no transferred line",
			in:        "sent 4,096 bytes  received 96 bytes  total size 4,096\n",
			wantFiles: 0,
			wantBytes: 4096,
		},
		{
			name:      "files with thousands separator",
			in:        "Number of regular files transferred: 1,234,567\n",
			wantFiles: 1234567,
			wantBytes: 0,
		},
		{
			name:      "garbage yields zero",
			in:        "this is not rsync output at all",
			wantFiles: 0,
			wantBytes: 0,
		},
		{
			name:      "empty yields zero",
			in:        "",
			wantFiles: 0,
			wantBytes: 0,
		},
		{
			name:      "transferred preferred over sent",
			in:        "Total transferred file size: 10 bytes\nsent 9999 bytes\n",
			wantFiles: 0,
			wantBytes: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseStats(tt.in)
			if got.files != tt.wantFiles {
				t.Errorf("parseStats files = %d, want %d", got.files, tt.wantFiles)
			}
			if got.bytes != tt.wantBytes {
				t.Errorf("parseStats bytes = %d, want %d", got.bytes, tt.wantBytes)
			}
		})
	}
}

func TestParseNum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"42", 42},
		{"1,234", 1234},
		{"1,234,567", 1234567},
		{"", 0},
		{"abc", 0},
		{"12.5", 0},
	}
	for _, tt := range tests {
		if got := parseNum(tt.in); got != tt.want {
			t.Errorf("parseNum(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestSourceIsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("empty dir is empty", func(t *testing.T) {
		t.Parallel()
		if !sourceIsEmpty(t.TempDir()) {
			t.Error("sourceIsEmpty on empty dir = false, want true")
		}
	})

	t.Run("dir with file is not empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if sourceIsEmpty(dir) {
			t.Error("sourceIsEmpty on populated dir = true, want false")
		}
	})

	t.Run("missing path is empty", func(t *testing.T) {
		t.Parallel()
		if !sourceIsEmpty(filepath.Join(t.TempDir(), "nope")) {
			t.Error("sourceIsEmpty on missing path = false, want true")
		}
	})
}

// TestSourceIsEmpty_onlyGloballyExcludedEntriesIsEmpty pins the h-f1 fix: a
// source whose top-level holds ONLY globally-excluded entries (e.g. a Syncthing
// folder reduced to just .stfolder, or a macOS dir holding only .DS_Store) must
// report empty, so a delete:true job skips it instead of letting rsync --delete
// wipe the remote after the post-exclude sender list comes up empty. A real
// file alongside an excluded entry must still mirror.
func TestSourceIsEmpty_onlyGloballyExcludedEntriesIsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("only globally-excluded entries is empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range globalExcludes {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
		if !sourceIsEmpty(dir) {
			t.Error("sourceIsEmpty on an excludes-only dir = false, want true (must skip to protect the remote)")
		}
	})

	t.Run("an excluded entry plus a real file is not empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".stfolder"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "real.conf"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if sourceIsEmpty(dir) {
			t.Error("sourceIsEmpty on a dir with a real file = true, want false (must mirror)")
		}
	})
}

// TestSourceIsEmpty_unreadableDirSurfacesWarnAndSkips covers the cycle-2
// error-surfacing arm where os.Open succeeds but Readdirnames returns a
// non-EOF error: a source path that is a regular file (not a directory).
// sourceIsEmpty must still report empty (true) so a broken source cannot let
// --delete wipe the remote, AND it must emit a WARN so the breakage is not
// masked as a benign empty source. Asserting the WARN is what distinguishes
// this arm from the silent missing-dir case.
func TestSourceIsEmpty_unreadableDirSurfacesWarnAndSkips(t *testing.T) {
	buf := captureLogs(t, slog.LevelWarn)

	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := sourceIsEmpty(path)

	if !got {
		t.Errorf("sourceIsEmpty(regular file) = false, want true (skip to protect remote)")
	}
	if !strings.Contains(buf.String(), "source unreadable") {
		t.Errorf("sourceIsEmpty(regular file) log = %q, want a 'source unreadable' WARN", buf.String())
	}
}

// TestSourceIsEmpty_openErrorSurfacesWarnAndSkips covers the other cycle-2
// arm: os.Open itself fails with a non-ErrNotExist error. A path whose parent
// component is a regular file yields ENOTDIR (not ENOENT), independent of uid
// (so it is reliable under the root-by-design container, unlike a chmod-0
// dir). The expected missing-dir (ENOENT) case stays silent; this asserts the
// non-silent arm so the two are not collapsed.
func TestSourceIsEmpty_openErrorSurfacesWarnAndSkips(t *testing.T) {
	buf := captureLogs(t, slog.LevelWarn)

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := filepath.Join(parent, "child")

	got := sourceIsEmpty(path)

	if !got {
		t.Errorf("sourceIsEmpty(path under a file) = false, want true (skip to protect remote)")
	}
	if !strings.Contains(buf.String(), "source unreadable") {
		t.Errorf("sourceIsEmpty(path under a file) log = %q, want a 'source unreadable' WARN", buf.String())
	}
}

func TestExitCode(t *testing.T) {
	t.Parallel()
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("boom")); got != -1 {
		t.Errorf("exitCode(generic) = %d, want -1", got)
	}

	cmd := exec.Command("sh", "-c", "exit 23")
	runErr := cmd.Run()
	if got := exitCode(runErr); got != 23 {
		t.Errorf("exitCode(exit 23) = %d, want 23", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	t.Parallel()

	t.Run("retains within cap", func(t *testing.T) {
		t.Parallel()
		b := &cappedBuffer{max: 10}
		n, err := b.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
		}
		if b.String() != "hello" {
			t.Errorf("String = %q, want hello", b.String())
		}
	})

	t.Run("truncates overflow but reports full length", func(t *testing.T) {
		t.Parallel()
		b := &cappedBuffer{max: 4}
		n, err := b.Write([]byte("abcdefgh"))
		if err != nil || n != 8 {
			t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
		}
		if b.String() != "abcd" {
			t.Errorf("String = %q, want abcd", b.String())
		}
	})
}

// TestCappedBuffer_capEnforcedAcrossWrites pins the remaining-room arithmetic
// (max - current length). A first write half-fills the buffer; the second
// write must be truncated against the *remaining* room, not the full cap.
// Observing the exact retained bytes (not just the reported count) catches a
// `-` -> `+` mutation of the remaining computation, which would let the second
// write overflow the cap to "abcdef".
func TestCappedBuffer_capEnforcedAcrossWrites(t *testing.T) {
	t.Parallel()
	b := &cappedBuffer{max: 4}

	n1, _ := b.Write([]byte("ab"))
	n2, _ := b.Write([]byte("cdef"))

	if n1 != 2 || n2 != 4 {
		t.Errorf("Write lengths = (%d, %d), want (2, 4)", n1, n2)
	}
	if b.String() != "abcd" {
		t.Errorf("cappedBuffer after writes = %q, want abcd", b.String())
	}
	if len(b.String()) > 4 {
		t.Errorf("cappedBuffer length = %d, want <= cap 4", len(b.String()))
	}
}

func TestTail(t *testing.T) {
	t.Parallel()
	if got := tail("short", 100); got != "short" {
		t.Errorf("tail no truncation = %q, want short", got)
	}
	// len(s) == n is the boundary of the len(s) <= n guard: the string fits
	// exactly and must be returned verbatim with no truncation marker. A
	// `<=` -> `<` off-by-one would prepend the marker here.
	if got := tail("abc", 3); got != "abc" {
		t.Errorf("tail(%q, 3) = %q, want abc (len == n, returned verbatim)", "abc", got)
	}
	got := tail("abcdefghij", 3)
	if !strings.HasSuffix(got, "hij") {
		t.Errorf("tail = %q, want suffix hij", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("tail = %q, want truncation marker", got)
	}
}

// --- Property-based tests ---

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

func TestProperty_ParseStatsNeverPanics(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := rapid.String().Draw(rt, "in")
		got := parseStats(in)
		if got.files < 0 || got.bytes < 0 {
			rt.Fatalf("parseStats(%q) = %+v, want non-negative", in, got)
		}
	})
}

// TestProperty_CappedBufferNeverExceedsMax asserts the two core invariants of
// cappedBuffer across any sequence of writes: the retained bytes never exceed
// max, and they are exactly the first min(total, max) bytes of the
// concatenated input. This is the round-trip/bound counterpart to the
// table-driven cap test and robustly kills arithmetic mutations of the
// remaining-room computation.
func TestProperty_CappedBufferNeverExceedsMax(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		max := rapid.IntRange(0, 64).Draw(rt, "max")
		chunks := rapid.SliceOfN(rapid.SliceOfN(rapid.Byte(), 0, 16), 0, 8).Draw(rt, "chunks")

		b := &cappedBuffer{max: max}
		var concat []byte
		for _, chunk := range chunks {
			n, err := b.Write(chunk)
			if err != nil {
				rt.Fatalf("Write(%q) returned error %v", chunk, err)
			}
			if n != len(chunk) {
				rt.Fatalf("Write(%q) reported n=%d, want %d (full length always reported)", chunk, n, len(chunk))
			}
			concat = append(concat, chunk...)
		}

		got := b.String()
		if len(got) > max {
			rt.Fatalf("buffer length %d exceeds max %d", len(got), max)
		}
		wantLen := min(len(concat), max)
		if want := string(concat[:wantLen]); got != want {
			rt.Fatalf("buffer = %q, want %q (first %d bytes of input)", got, want, wantLen)
		}
	})
}

// newRunJobSource creates a non-empty temp source dir so runJob does not
// take the empty-source skip path.
func newRunJobSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatalf("setup source: %v", err)
	}
	return dir
}

// runJobJob returns a minimal job rooted at local.
func runJobJob(local string) *job {
	return &job{
		Name:       "caddy",
		Local:      local,
		RemoteHost: "root@192.168.1.87",
		RemotePath: "/srv/containers/caddy",
		SSHKey:     "/keys/id_ed25519",
	}
}

func TestRunJob_successParsesStatsAndMarksSuccess(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'Number of regular files transferred: 5\\nTotal transferred file size: 2048 bytes\\n'; exit 0")
	}
	res := runJob(context.Background(), runJobJob(newRunJobSource(t)), time.Minute, newCmd)
	if !res.success {
		t.Errorf("success = false, want true")
	}
	if res.skipped {
		t.Errorf("skipped = true, want false")
	}
	if res.exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", res.exitCode)
	}
	if res.files != 5 {
		t.Errorf("files = %d, want 5", res.files)
	}
	if res.bytes != 2048 {
		t.Errorf("bytes = %d, want 2048", res.bytes)
	}
}

func TestRunJob_failureCapturesExitCodeAndStderr(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c",
			"printf 'rsync error: link_stat failed\\n' >&2; exit 23")
	}
	res := runJob(context.Background(), runJobJob(newRunJobSource(t)), time.Minute, newCmd)
	if res.success {
		t.Errorf("success = true, want false")
	}
	if res.exitCode != 23 {
		t.Errorf("exitCode = %d, want 23", res.exitCode)
	}
	if !strings.Contains(res.stderrTail, "rsync error") {
		t.Errorf("stderrTail missing rsync error")
	}
}

func TestRunJob_emptySourceSkipsWithoutRunning(t *testing.T) {
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked for empty source; want skip")
		return exec.CommandContext(ctx, "true")
	}
	res := runJob(context.Background(), runJobJob(t.TempDir()), time.Minute, newCmd)
	if !res.skipped {
		t.Errorf("skipped = false, want true")
	}
	if !res.success {
		t.Errorf("success = false, want true (skip counts as success)")
	}
}

func TestRunPass_deferredWhenLockHeld(t *testing.T) {
	// Not parallel: contends on the real lockFilePath.
	held, ok, _, err := tryLock(lockFilePath)
	if err != nil || !ok {
		t.Fatalf("could not acquire lock: ok=%v err=%v", ok, err)
	}
	defer held.unlock()
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked with held lock; want deferred")
		return exec.CommandContext(ctx, "true")
	}
	cfg := config{Jobs: []job{*runJobJob(newRunJobSource(t))}}
	r := runPass(context.Background(), cfg, time.Minute, "test", newCmd)
	if r.disposition != passDeferred {
		t.Errorf("disposition = %v, want passDeferred (%v)", r.disposition, passDeferred)
	}
	if r.exitStatus() != 0 {
		t.Errorf("exitStatus = %d, want 0 (deferred is success)", r.exitStatus())
	}
	if set, _ := r.healthSignal(); set {
		t.Error("deferred pass wrote a health signal; want none (the running holder owns health)")
	}
}

func TestRunPass_aggregatesFailures(t *testing.T) {
	// Not parallel: contends on the real lockFilePath.
	var calls int
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		calls++
		if calls == 1 {
			return exec.CommandContext(ctx, "sh", "-c", "exit 0")
		}
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	src := newRunJobSource(t)
	cfg := config{Jobs: []job{*runJobJob(src), *runJobJob(src)}}
	r := runPass(context.Background(), cfg, time.Minute, "test", newCmd)
	if r.disposition != passRan {
		t.Fatalf("disposition = %v, want passRan (%v)", r.disposition, passRan)
	}
	if r.failed != 1 {
		t.Errorf("failed = %d, want 1", r.failed)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1", r.ok)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
	if _, healthy := r.healthSignal(); healthy {
		t.Error("healthy = true, want false (a job failed)")
	}
	if r.exitStatus() != 1 {
		t.Errorf("exitStatus = %d, want 1 (a job failed)", r.exitStatus())
	}
}

// TestRunJob_parentCancellationLogsShutdownNotFailure verifies the
// graceful-shutdown arm of runJob (the `if ctx.Err() != nil` branch): when the
// PARENT context is cancelled (container shutdown SIGTERM'd the in-flight
// rsync), the interrupted job must log at INFO ("sync interrupted by shutdown")
// and never at ERROR ("sync failed"). The shutdown and failure arms return
// identical jobResult values, so only the emitted log distinguishes them; this
// protects the no-false-page contract (Loki alerts on level=error).
func TestRunJob_parentCancellationLogsShutdownNotFailure(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	res := runJob(ctx, runJobJob(newRunJobSource(t)), time.Minute, newCmd)

	if res.success {
		t.Errorf("runJob success = true, want false when parent context cancelled")
	}
	logs := buf.String()
	if !strings.Contains(logs, "sync interrupted by shutdown") {
		t.Errorf("runJob log = %q, want to contain 'sync interrupted by shutdown'", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("runJob log = %q, want no ERROR-level line on graceful shutdown", logs)
	}
}

// TestRunPass_emptySourceSkippedNotCountedAsFailure verifies the
// `case jr.skipped` arm of runPass's tally: an empty-source job is skipped
// (its runner is never invoked) and counts toward ok+emptySkipped, never
// failed, so the pass is healthy. (The heartbeat wording is asserted
// separately in TestReportPass_ranEmitsHeartbeat.)
func TestRunPass_emptySourceSkippedNotCountedAsFailure(t *testing.T) {
	// Not parallel: contends on the real lockFilePath.
	newCmd := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		t.Error("runner invoked for empty-source job; want skip, not exec")
		return exec.CommandContext(ctx, "true")
	}
	cfg := config{Jobs: []job{*runJobJob(t.TempDir())}}

	r := runPass(context.Background(), cfg, time.Minute, "test", newCmd)

	if r.disposition != passRan {
		t.Fatalf("disposition = %v, want passRan (%v)", r.disposition, passRan)
	}
	if r.failed != 0 {
		t.Errorf("failed = %d, want 0 (an empty-source skip is not a failure)", r.failed)
	}
	if r.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", r.emptySkipped)
	}
	if r.ok != 1 {
		t.Errorf("ok = %d, want 1 (a skip counts toward ok)", r.ok)
	}
	if _, healthy := r.healthSignal(); !healthy {
		t.Error("healthy = false, want true (an all-skip pass is healthy)")
	}
}

// captureLogs installs a text slog handler over a buffer for the duration of
// the test and returns the buffer. The caller must NOT be parallel: it mutates
// the global slog default.
func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// TestReportPass_ranEmitsHeartbeat verifies the staleness heartbeat carries the
// job tally that Loki absence/skip alerts parse.
func TestReportPass_ranEmitsHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{
		disposition: passRan, trigger: "interval",
		total: 2, ok: 2, emptySkipped: 1, failed: 0, duration: 5 * time.Millisecond,
	})
	logs := buf.String()
	for _, want := range []string{"sync cycle complete", "trigger=interval", "ok=2", "skipped=1", "failed=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("heartbeat log = %q, want substring %q", logs, want)
		}
	}
}

// TestReportPass_interruptedDoesNotEmitHeartbeat verifies a shutdown-interrupted
// pass logs a distinct warn line and NOT the "sync cycle complete" heartbeat
// (so a drain never registers as a healthy completion) and never at error.
func TestReportPass_interruptedDoesNotEmitHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{
		disposition: passRan, trigger: "interval", interrupted: true,
		total: 1, ok: 0, failed: 1,
	})
	logs := buf.String()
	if !strings.Contains(logs, "sync cycle interrupted by shutdown") {
		t.Errorf("log = %q, want 'sync cycle interrupted by shutdown'", logs)
	}
	if strings.Contains(logs, "sync cycle complete") {
		t.Errorf("log = %q, want NO 'sync cycle complete' heartbeat on an interrupted pass", logs)
	}
	if strings.Contains(logs, "level=ERROR") {
		t.Errorf("log = %q, want no ERROR on a shutdown interruption", logs)
	}
}

// TestReportPass_deferredEmitsLivenessNotHeartbeat verifies an overlap defer
// logs a liveness line with holder_age_ms and NOT the heartbeat (a stuck
// holder must still trip the absence alert).
func TestReportPass_deferredEmitsLivenessNotHeartbeat(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{disposition: passDeferred, trigger: "interval", holderAge: 90 * time.Second})
	logs := buf.String()
	if !strings.Contains(logs, "sync deferred, prior pass still running") {
		t.Errorf("log = %q, want the deferred liveness line", logs)
	}
	if !strings.Contains(logs, "holder_age_ms=90000") {
		t.Errorf("log = %q, want holder_age_ms=90000", logs)
	}
	if strings.Contains(logs, "sync cycle complete") {
		t.Errorf("log = %q, want NO heartbeat for a deferred pass", logs)
	}
}

// TestReportPass_lockErrorEmitsError verifies a lock-acquisition failure logs
// at error level so it trips the documented error alert.
func TestReportPass_lockErrorEmitsError(t *testing.T) {
	buf := captureLogs(t, slog.LevelInfo)
	reportPass(&passResult{disposition: passLockErr, trigger: "interval", err: errors.New("flock boom")})
	logs := buf.String()
	if !strings.Contains(logs, "level=ERROR") {
		t.Errorf("log = %q, want level=ERROR for a lock error", logs)
	}
	if !strings.Contains(logs, "cannot acquire sync lock") {
		t.Errorf("log = %q, want 'cannot acquire sync lock'", logs)
	}
}

// TestPassResult_exitStatus pins the process exit code each disposition maps to:
// a clean pass and a deferred pass exit 0, a pass with job failures and a lock
// error exit 1. The clean-passRan and passLockErr arms are otherwise unexercised
// by the runPass-level tests, so this is the direct guard on exitStatus.
func TestPassResult_exitStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		res  passResult
		want int
	}{
		{"ran clean is zero", passResult{disposition: passRan, failed: 0}, 0},
		{"ran with failures is one", passResult{disposition: passRan, failed: 2}, 1},
		{"lock error is one", passResult{disposition: passLockErr, err: errors.New("flock boom")}, 1},
		{"deferred is zero", passResult{disposition: passDeferred, holderAge: time.Second}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := tt.res
			if got := r.exitStatus(); got != tt.want {
				t.Errorf("passResult{disposition:%d, failed:%d}.exitStatus() = %d, want %d",
					tt.res.disposition, tt.res.failed, got, tt.want)
			}
		})
	}
}

// TestDefaultCommandRunner_structural pins the graceful-shutdown construction
// of the real commandRunner that every runJob/runPass test bypasses via a fake:
// a 5s WaitDelay before SIGKILL, a non-nil SIGTERM Cancel closure, and the
// verbatim arg slice. Mutating WaitDelay (e.g. 5s -> 9s) fails this test.
func TestDefaultCommandRunner_structural(t *testing.T) {
	cmd := defaultCommandRunner(context.Background(), "echo", "hi", "there")
	if cmd.WaitDelay != 5*time.Second {
		t.Errorf("WaitDelay = %v, want 5s", cmd.WaitDelay)
	}
	if cmd.Cancel == nil {
		t.Error("Cancel = nil, want a SIGTERM closure")
	}
	if want := []string{"echo", "hi", "there"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("Args = %v, want %v", cmd.Args, want)
	}
}

// TestDefaultCommandRunner_cancelSignalsProcess exercises the Cancel closure
// body at sync.go:44: a real subprocess is started, its context cancelled, and
// Wait must return a termination error proving the SIGTERM closure ran (the
// closure is otherwise invisible to the fake-injecting unit tests).
func TestDefaultCommandRunner_cancelSignalsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := defaultCommandRunner(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := cmd.Wait(); err == nil {
		t.Errorf("Wait() = nil, want a termination error from the cancelled process")
	}
}
