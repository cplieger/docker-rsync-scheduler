package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// argJob returns a job with the given knobs set, using a fixed key path
// (buildRsyncArgs does no filesystem access, so the path need not exist).
func argJob(delete bool, uid, gid *uint32, excludes []string) *job {
	return &job{
		Name:       "caddy",
		Local:      "/sources/caddy",
		RemoteHost: "root@192.0.2.87",
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
	t.Parallel()

	t.Run("minimal has no delete or chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, nil, nil), transport{})

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
		if got[len(got)-1] != "root@192.0.2.87:/srv/containers/caddy/" {
			t.Errorf("dest = %q, want root@192.0.2.87:/srv/containers/caddy/", got[len(got)-1])
		}
	})

	t.Run("uid only does not add chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, new(uint32(1000)), nil, nil), transport{})
		if hasChown(got) {
			t.Errorf("--chown present with gid unset in %v", got)
		}
	})

	t.Run("gid only does not add chown", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, new(uint32(1000)), nil), transport{})
		if hasChown(got) {
			t.Errorf("--chown present with uid unset in %v", got)
		}
	})

	t.Run("excludes appended after globals", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, nil, []string{"**/*.lock", "logs"}), transport{})
		if !slices.Contains(got, "--filter=- **/*.lock") {
			t.Errorf("per-job exclude absent in %v", got)
		}
		if !slices.Contains(got, "--filter=- logs") {
			t.Errorf("per-job exclude absent in %v", got)
		}
		globalIdx := slices.Index(got, "--filter=- Thumbs.db")
		jobIdx := slices.Index(got, "--filter=- **/*.lock")
		if globalIdx == -1 || jobIdx == -1 || jobIdx < globalIdx {
			t.Errorf("per-job excludes must follow global excludes: %v", got)
		}
	})

	// A per-job entry occupying rsync's rule-prefix position must stay a
	// literal pattern: bare "!" would clear every accumulated rule (the four
	// built-in globals included, so under --delete the receiver's copies
	// become deletion candidates) and "+ x" would become a first-match
	// include that transfers a file the operator asked to exclude.
	t.Run("rule-prefix and clear patterns stay literal patterns", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(false, nil, nil, []string{"!", "+ real.conf"}), transport{})
		for _, want := range []string{"--filter=- !", "--filter=- + real.conf"} {
			if !slices.Contains(got, want) {
				t.Errorf("buildRsyncArgs() = %v, want it to contain %q", got, want)
			}
		}
		if slices.ContainsFunc(got, func(a string) bool {
			return strings.HasPrefix(a, "--exclude=")
		}) {
			t.Errorf("buildRsyncArgs() = %v, want no --exclude= argument", got)
		}
	})

	t.Run("all knobs together", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(true, new(uint32(0)), new(uint32(0)), []string{"logs"}), transport{})
		want := []string{
			"-rlptD", "--delete", "--chown=0:0", "--stats", "-e", wantSSHAcceptNew,
			"--filter=- .stfolder", "--filter=- .stversions",
			"--filter=- .DS_Store", "--filter=- Thumbs.db",
			"--filter=- logs",
			"--", "/sources/caddy/", "root@192.0.2.87:/srv/containers/caddy/",
		}
		if !slices.Equal(got, want) {
			t.Errorf("buildRsyncArgs =\n  %v\nwant\n  %v", got, want)
		}
	})
}

// TestClassifyKnownHosts pins the boot decision behind the ssh_hostkey_mode
// banner and the -e transport: absent means TOFU, a populated file means
// pinning, and the two states that stat successfully yet pin nothing (a
// directory, and the 0-byte file `ssh-keyscan` leaves behind for an
// unreachable host) are refused so the daemon cannot report pinning it does
// not have.
func TestClassifyKnownHosts(t *testing.T) {
	t.Parallel()

	t.Run("absent is accept-new", func(t *testing.T) {
		t.Parallel()
		mode, err := classifyKnownHosts(filepath.Join(t.TempDir(), "absent"))
		if err != nil || mode != hostKeyAcceptNew {
			t.Errorf("classifyKnownHosts(absent) = (%v, %v), want (accept-new, nil)", mode, err)
		}
	})

	t.Run("populated is strict", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		doc := "# a comment\n\n192.0.2.10 ssh-ed25519 AAAAC3Nz\n"
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		mode, err := classifyKnownHosts(path)
		if err != nil || mode != hostKeyStrict {
			t.Errorf("classifyKnownHosts(populated) = (%v, %v), want (strict, nil)", mode, err)
		}
	})

	t.Run("comments only is refused", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(path, []byte("# nothing here\n\n"), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		_, err := classifyKnownHosts(path)
		if err == nil || !strings.Contains(err.Error(), "no entries") {
			t.Errorf("classifyKnownHosts(comments only) error = %v, want a no-entries refusal", err)
		}
	})

	t.Run("directory is refused", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		_, err := classifyKnownHosts(dir)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("classifyKnownHosts(directory) error = %v, want a not-a-regular-file refusal", err)
		}
	})

	t.Run("over-long comment then a key is strict", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		doc := "# " + strings.Repeat("x", 70<<10) + "\n192.0.2.10 ssh-ed25519 AAAAC3Nz\n"
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		mode, err := classifyKnownHosts(path)
		if err != nil || mode != hostKeyStrict {
			t.Errorf("classifyKnownHosts(long comment + key) = (%v, %v), want (strict, nil)", mode, err)
		}
	})

	t.Run("only an over-long comment is refused as empty", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		doc := "# " + strings.Repeat("x", 70<<10) + "\n"
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		_, err := classifyKnownHosts(path)
		if err == nil || !strings.Contains(err.Error(), "no entries") {
			t.Errorf("classifyKnownHosts(long comment only) error = %v, want a no-entries refusal", err)
		}
	})

	t.Run("indented key is strict", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(path, []byte("  \t192.0.2.10 ssh-ed25519 AAAAC3Nz\n"), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		mode, err := classifyKnownHosts(path)
		if err != nil || mode != hostKeyStrict {
			t.Errorf("classifyKnownHosts(indented key) = (%v, %v), want (strict, nil)", mode, err)
		}
	})

	t.Run("non-ASCII space before a comment is refused as empty", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "known_hosts")
		if err := os.WriteFile(path, []byte("\u00a0# nothing here\n"), 0o600); err != nil {
			t.Fatalf("write known_hosts: %v", err)
		}
		_, err := classifyKnownHosts(path)
		if err == nil || !strings.Contains(err.Error(), "no entries") {
			t.Errorf("classifyKnownHosts(NBSP then comment) error = %v, want a no-entries refusal", err)
		}
	})
}

// TestHostKeyMode_String pins the two operator-visible spellings the startup
// banner's ssh_hostkey_mode attribute publishes.
func TestHostKeyMode_String(t *testing.T) {
	t.Parallel()
	if got := hostKeyAcceptNew.String(); got != "accept-new" {
		t.Errorf("hostKeyAcceptNew.String() = %q, want \"accept-new\"", got)
	}
	if got := hostKeyStrict.String(); got != "strict" {
		t.Errorf("hostKeyStrict.String() = %q, want \"strict\"", got)
	}
}

func TestBuildRsyncArgs_KnownHostsPresent(t *testing.T) {
	t.Parallel()
	got := buildRsyncArgs(argJob(false, nil, nil, nil), transport{hostKeys: hostKeyStrict})
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
	t.Parallel()

	t.Run("delete with max_delete emits the flag", func(t *testing.T) {
		t.Parallel()
		j := argJob(true, nil, nil, nil)
		j.MaxDelete = new(100)
		got := buildRsyncArgs(j, transport{})
		if !slices.Contains(got, "--max-delete=100") {
			t.Errorf("--max-delete=100 absent in %v", got)
		}
	})

	t.Run("delete without max_delete omits the flag", func(t *testing.T) {
		t.Parallel()
		got := buildRsyncArgs(argJob(true, nil, nil, nil), transport{})
		if slices.ContainsFunc(got, func(a string) bool { return strings.HasPrefix(a, "--max-delete=") }) {
			t.Errorf("--max-delete present with max_delete unset in %v", got)
		}
	})

	t.Run("max_delete without delete is a no-op", func(t *testing.T) {
		t.Parallel()
		j := argJob(false, nil, nil, nil)
		j.MaxDelete = new(100)
		got := buildRsyncArgs(j, transport{})
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
		{"ipv4 with user", "root@192.0.2.87", "/srv/x", "root@192.0.2.87:/srv/x/"},
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
		if !slices.Contains(args, "--filter=- "+e) {
			t.Errorf("global exclude --filter=- %s absent in %v", e, args)
		}
	}
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

// TestBuildRsyncArgs_transportSwitches pins the three opt-in transport
// switches. The zero-value row is the important one: transport{} must append
// none of -A/-X/-z, so no existing deployment changes behaviour on an image
// bump. The named-algorithm rows also pin the argument ORDER, since
// --compress-choice is meaningless without the -z that precedes it.
func TestBuildRsyncArgs_transportSwitches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tr      transport
		want    []string
		notWant []string
	}{
		{
			name:    "zero value appends nothing",
			tr:      transport{},
			notWant: []string{"-A", "-X", "-z"},
		},
		{
			name:    "acls appends -A",
			tr:      transport{acls: true},
			want:    []string{"-A"},
			notWant: []string{"-X", "-z"},
		},
		{
			name:    "xattrs appends -X",
			tr:      transport{xattrs: true},
			want:    []string{"-X"},
			notWant: []string{"-A", "-z"},
		},
		{
			name:    "auto appends -z alone",
			tr:      transport{compress: "auto"},
			want:    []string{"-z"},
			notWant: []string{"--compress-choice=auto"},
		},
		{
			name: "zstd appends -z and the choice",
			tr:   transport{compress: "zstd"},
			want: []string{"-z", "--compress-choice=zstd"},
		},
		{
			name: "lz4 appends -z and the choice",
			tr:   transport{compress: "lz4"},
			want: []string{"-z", "--compress-choice=lz4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildRsyncArgs(argJob(false, nil, nil, nil), tt.tr)
			if got[0] != "-rlptD" {
				t.Errorf("buildRsyncArgs(%+v) first arg = %q, want -rlptD", tt.tr, got[0])
			}
			for _, flag := range tt.want {
				if !slices.Contains(got, flag) {
					t.Errorf("buildRsyncArgs(%+v) = %v, want it to contain %q", tt.tr, got, flag)
				}
			}
			for _, flag := range tt.notWant {
				if slices.Contains(got, flag) {
					t.Errorf("buildRsyncArgs(%+v) = %v, want it NOT to contain %q", tt.tr, got, flag)
				}
			}
			if i := slices.Index(got, "--compress-choice="+tt.tr.compress); i > 0 && got[i-1] != "-z" {
				t.Errorf("buildRsyncArgs(%+v) = %v, want --compress-choice preceded by -z", tt.tr, got)
			}
		})
	}
}
