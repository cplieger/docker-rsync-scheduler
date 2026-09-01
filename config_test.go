package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cplieger/envx/yamlenv/v2"
	"github.com/cplieger/slogx/capture"
)

// writeKey creates a readable dummy private key inside a temp dir and
// returns its path, so validation's ssh_key readability check passes for
// the cases that are meant to be valid.
func writeKey(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, []byte("dummy-key\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// validJob returns a job that passes validation, using key as ssh_key.
func validJob(name, key string) job {
	return job{
		Name:       name,
		Local:      "/sources/" + name,
		RemoteHost: "root@192.0.2.87",
		RemotePath: "/srv/containers/" + name,
		SSHKey:     key,
	}
}

func TestValidate(t *testing.T) {
	key := writeKey(t)

	tests := []struct {
		name    string
		wantErr string
		cfg     config
	}{
		{
			name: "valid minimal",
			cfg:  config{Jobs: []job{validJob("caddy", key)}},
		},
		{
			name: "valid with chown delete and excludes",
			cfg: config{Jobs: []job{{
				Name:       "caddy",
				Local:      "/sources/caddy",
				RemoteHost: "root@192.0.2.87",
				RemotePath: "/srv/containers/caddy",
				SSHKey:     key,
				RemoteUID:  new(uint32(1000)),
				RemoteGID:  new(uint32(1000)),
				Delete:     true,
				Excludes:   []string{"**/locks", "**/*.lock", "logs"},
			}}},
		},
		{
			name: "valid ipv6 host",
			cfg: config{Jobs: []job{{
				Name:       "v6",
				Local:      "/sources/v6",
				RemoteHost: "user@2001:db8::1",
				RemotePath: "/srv/v6",
				SSHKey:     key,
			}}},
		},
		{
			name: "valid bare ipv6 host",
			cfg: config{Jobs: []job{{
				Name:       "v6bare",
				Local:      "/sources/v6bare",
				RemoteHost: "2001:db8::1",
				RemotePath: "/srv/v6",
				SSHKey:     key,
			}}},
		},
		{
			name: "valid bracketed ipv6 host",
			cfg: config{Jobs: []job{{
				Name:       "v6br",
				Local:      "/sources/v6br",
				RemoteHost: "user@[2001:db8::1]",
				RemotePath: "/srv/v6",
				SSHKey:     key,
			}}},
		},
		{
			name: "valid bare ipv4 host",
			cfg: config{Jobs: []job{{
				Name:       "v4",
				Local:      "/sources/v4",
				RemoteHost: "192.0.2.10",
				RemotePath: "/srv/v4",
				SSHKey:     key,
			}}},
		},
		{
			name:    "empty jobs",
			cfg:     config{Jobs: nil},
			wantErr: "jobs list is empty",
		},
		{
			name:    "missing name",
			cfg:     config{Jobs: []job{{Local: "/a", RemoteHost: "h", RemotePath: "/b", SSHKey: key}}},
			wantErr: "name is required",
		},
		{
			name:    "missing local",
			cfg:     config{Jobs: []job{{Name: "j", RemoteHost: "h", RemotePath: "/b", SSHKey: key}}},
			wantErr: "local is required",
		},
		{
			name:    "missing remote_host",
			cfg:     config{Jobs: []job{{Name: "j", Local: "/a", RemotePath: "/b", SSHKey: key}}},
			wantErr: "remote_host is required",
		},
		{
			name:    "missing remote_path",
			cfg:     config{Jobs: []job{{Name: "j", Local: "/a", RemoteHost: "h", SSHKey: key}}},
			wantErr: "remote_path is required",
		},
		{
			name:    "missing ssh_key",
			cfg:     config{Jobs: []job{{Name: "j", Local: "/a", RemoteHost: "h", RemotePath: "/b"}}},
			wantErr: "ssh_key is required",
		},
		{
			name: "duplicate names",
			cfg: config{Jobs: []job{
				validJob("dup", key),
				validJob("dup", key),
			}},
			wantErr: "duplicate name",
		},
		{
			name: "local not absolute",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "relative/path", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "must be absolute",
		},
		{
			name: "remote_path not absolute",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "relative", SSHKey: key,
			}}},
			wantErr: "must be absolute",
		},
		{
			name: "remote_host with space",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "bad host",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "remote_host with semicolon",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host;rm -rf /",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "remote_host with leading dash",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "-eevil",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "remote_host trailing colon rejected",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host:",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "remote_host incomplete ipv6 rejected",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "2001:db8",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "remote_host ipv6 zone id rejected",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "fe80::1%eth0",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "remote_host",
		},
		{
			name: "semicolon in local is accepted",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a;rm", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
			}}},
		},
		{
			name: "dollar in remote_path",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b/$(whoami)", SSHKey: key,
			}}},
			wantErr: "forbidden characters",
		},
		{
			name: "newline in local",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a\nrm", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "control characters",
		},
		{
			name: "semicolon in exclude is accepted",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
				Excludes: []string{"good", "bad;evil"},
			}}},
		},
		{
			name: "glob exclude is allowed",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
				Excludes: []string{"**/*.lock", "**/locks"},
			}}},
		},
		{
			name: "ssh_key is a directory",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b", SSHKey: t.TempDir(),
			}}},
			wantErr: "not a regular file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validate() error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidate_excludeControlsRejected(t *testing.T) {
	key := writeKey(t)
	tests := []struct {
		name    string
		exclude string
		wantErr bool
	}{
		{name: "newline", exclude: "keep\nme", wantErr: true},
		{name: "del", exclude: "keep\x7fme", wantErr: true},
		{name: "printable", exclude: "keepme", wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := validJob("excluded", key)
			j.Excludes = []string{tt.exclude}
			err := (config{Jobs: []job{j}}).validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validate() with exclude %q = nil, want error", tt.exclude)
				}
				if !strings.Contains(err.Error(), "exclude") || !strings.Contains(err.Error(), "control characters") {
					t.Errorf("validate() with exclude %q error = %q, want an exclude control-character refusal", tt.exclude, err)
				}
				return
			}
			if err != nil {
				t.Errorf("validate() with exclude %q = %v, want nil", tt.exclude, err)
			}
		})
	}
}

// TestValidate_sshKeyMissingIsNotExist pins the missing-key CAUSE at the frame
// an operator's `config validation failed` record carries. job.validate wraps
// checkReadable's error with %w, and errors.Is traverses that chain, so this is
// the same sentinel test one frame out — and unlike a direct test of the private
// helper it survives a rename, an extraction or a replacement of checkReadable.
// An edit that rewraps with %v keeps the "not readable" substring green while
// this assertion goes red, which is the contract break no linter catches.
func TestValidate_sshKeyMissingIsNotExist(t *testing.T) {
	t.Parallel()
	cfg := config{Jobs: []job{{
		Name: "j", Local: "/a", RemoteHost: "host",
		RemotePath: "/b", SSHKey: filepath.Join(t.TempDir(), "absent"),
	}}}

	err := cfg.validate()

	if err == nil {
		t.Fatalf("validate() with a missing ssh_key = nil, want error")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("validate() error = %q, want to contain 'not readable'", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(validate() error, os.ErrNotExist) = false, want true; error = %q", err)
	}
}

func TestValidate_sshKeyWithSpaceRejected(t *testing.T) {
	cfg := config{Jobs: []job{{
		Name:       "spaced",
		Local:      "/sources/spaced",
		RemoteHost: "root@192.0.2.87",
		RemotePath: "/srv/spaced",
		SSHKey:     "/keys/id ed25519",
	}}}

	err := cfg.validate()

	if err == nil {
		t.Fatalf("validate() with spaced ssh_key = nil, want error")
	}
	if !strings.Contains(err.Error(), "must not contain spaces") {
		t.Errorf("validate() error = %q, want to contain 'must not contain spaces'", err)
	}
}

func TestValidate_sshKeyQuotesRejected(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"id'ed25519", `id"ed25519`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(key, []byte("dummy-key\n"), 0o600); err != nil {
				t.Fatalf("write key: %v", err)
			}
			j := validJob("quoted", key)
			if err := (config{Jobs: []job{j}}).validate(); err == nil {
				t.Errorf("validate() with ssh_key %q = nil error, want refusal", key)
			}
		})
	}
}

func TestHasShellMeta(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"plain path", "/sources/caddy", false},
		{"user at host", "root@192.0.2.87", false},
		{"glob exclude", "**/*.lock", false},
		{"ipv6 host", "2001:db8::1", false},
		{"dash and dot", "host-1.example.com", false},
		// 0x20 (space) is the boundary of the r < 0x20 control-char guard:
		// it must be treated as a printable, allowed character. A `<` -> `<=`
		// off-by-one would wrongly reject it.
		{"space is printable", "a b", false},
		// 0x1f (unit separator) is the last control char below the boundary.
		{"unit separator is control", "a\x1fb", true},
		// 0x7f (DEL) is the explicit second control-char branch.
		{"del is control", "a\x7fb", true},
		{"semicolon", "a;b", true},
		{"pipe", "a|b", true},
		{"ampersand", "a&b", true},
		{"dollar", "$(x)", true},
		{"backtick", "a`b`", true},
		{"newline", "a\nb", true},
		{"carriage return", "a\rb", true},
		{"tab", "a\tb", true},
		{"null", "a\x00b", true},
		{"redirect", "a>b", true},
		{"backslash", "a\\b", true},
		{"double quote", "a\"b", true},
		{"single quote", "a'b", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hasShellMeta(tt.in); got != tt.want {
				t.Errorf("hasShellMeta(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseConfig(t *testing.T) {
	t.Parallel()
	doc := `
jobs:
  - name: caddy
    local: /sources/caddy
    remote_host: root@192.0.2.87
    remote_path: /srv/containers/caddy
    remote_uid: 1000
    remote_gid: 1000
    ssh_key: /keys/id_ed25519
    delete: true
    excludes: ["**/locks", "logs"]
`
	cfg, err := parseConfig([]byte(doc))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Jobs) != 1 {
		t.Fatalf("len(Jobs) = %d, want 1", len(cfg.Jobs))
	}
	j := cfg.Jobs[0]
	if j.Name != "caddy" {
		t.Errorf("Name = %q, want caddy", j.Name)
	}
	if j.RemoteUID == nil || *j.RemoteUID != 1000 {
		t.Errorf("RemoteUID = %v, want 1000", j.RemoteUID)
	}
	if !j.Delete {
		t.Error("Delete = false, want true")
	}
	if len(j.Excludes) != 2 {
		t.Errorf("Excludes = %v, want 2 entries", j.Excludes)
	}
}

func TestParseConfig_rejectsRemoteOwnershipOutsideUint32(t *testing.T) {
	key := writeKey(t)
	for _, value := range []string{"-1", "4294967296"} {
		t.Run(value, func(t *testing.T) {
			doc := fmt.Sprintf(`
jobs:
  - name: caddy
    local: /sources/caddy
    remote_host: root@192.0.2.87
    remote_path: /srv/containers/caddy
    remote_uid: %s
    remote_gid: 1000
    ssh_key: %s
`, value, key)

			_, err := parseConfig([]byte(doc))
			if err == nil {
				t.Fatalf("parseConfig(remote_uid=%s) = nil, want decode error", value)
			}
			if !strings.Contains(err.Error(), "cannot unmarshal") || !strings.Contains(err.Error(), "uint32") {
				t.Errorf("parseConfig(remote_uid=%s) error = %q, want uint32 decode error", value, err)
			}
		})
	}
}

func TestParseConfigInvalidYAML(t *testing.T) {
	t.Parallel()
	_, err := parseConfig([]byte("jobs: [unterminated"))
	if err == nil {
		t.Fatal("parseConfig on malformed YAML: want error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want to contain 'parse config'", err)
	}
}

func TestParseConfig_rejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	tests := map[string]byte{
		"raw_0x80": 0x80,
		"raw_0xff": 0xff,
	}
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := append([]byte("jobs:\n  - name: invalid"), invalid)
			doc = append(doc, '\n')
			if _, err := parseConfig(doc); err == nil {
				t.Errorf("parseConfig(%s) = nil error, want invalid UTF-8 refusal", name)
			}
		})
	}
}

// TestParseConfigUnknownKeyRejected pins the fail-loud unknown-key contract
// from yamlenv.CheckUnknownKeys: a misspelled optional job key (max_delet
// for max_delete) is a parse error naming the key, not a silently ignored
// setting that leaves the intended cap unset. The parse fails and the message
// names the operator's key and its line, in yaml.v3's own words.
func TestParseConfigUnknownKeyRejected(t *testing.T) {
	t.Parallel()
	doc := `
jobs:
  - name: caddy
    local: /sources/caddy
    remote_host: root@192.0.2.87
    remote_path: /srv/containers/caddy
    ssh_key: /keys/id_ed25519
    delete: true
    max_delet: 100
`
	_, err := parseConfig([]byte(doc))
	if err == nil {
		t.Fatal("parseConfig with misspelled key 'max_delet' = nil, want error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want to contain 'parse config'", err)
	}
	if !strings.Contains(err.Error(), "max_delet") {
		t.Errorf("error = %q, want it to name the unknown key 'max_delet'", err)
	}
}

// TestParseConfigMultiDocumentRejected pins the fail-loud multi-document
// contract from yamlenv.CheckSingleDocument: everything below a stray "---"
// separator would be silently dropped by the single-document Unmarshal, so
// the parse rejects it with the typed ErrMultipleDocuments instead.
func TestParseConfigMultiDocumentRejected(t *testing.T) {
	t.Parallel()
	doc := `jobs:
  - name: caddy
    local: /sources/caddy
    remote_host: root@192.0.2.87
    remote_path: /srv/containers/caddy
    ssh_key: /keys/id_ed25519
---
jobs:
  - name: shadowed
`
	_, err := parseConfig([]byte(doc))
	if err == nil {
		t.Fatal("parseConfig with a second YAML document = nil, want error")
	}
	if !errors.Is(err, yamlenv.ErrMultipleDocuments) {
		t.Errorf("error = %q, want errors.Is ErrMultipleDocuments", err)
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("error = %q, want to contain 'parse config'", err)
	}
}

func TestParseConfig_rejectsAliasExpansionDocuments(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"aliases_in_unknown_top_level_fields": `
a0: &a0 [x,x,x,x,x,x,x,x,x]
a1: &a1 [*a0,*a0,*a0,*a0,*a0,*a0,*a0,*a0,*a0]
a2: &a2 [*a1,*a1,*a1,*a1,*a1,*a1,*a1,*a1,*a1]
a3: &a3 [*a2,*a2,*a2,*a2,*a2,*a2,*a2,*a2,*a2]
a4: &a4 [*a3,*a3,*a3,*a3,*a3,*a3,*a3,*a3,*a3]
jobs: []
`,
		"nested_aliases_in_known_field": `
jobs:
  - name: a0
    excludes: &a0 [x,x,x,x,x,x,x,x,x]
  - name: a1
    excludes: &a1 [*a0,*a0,*a0,*a0,*a0,*a0,*a0,*a0,*a0]
  - name: a2
    excludes: &a2 [*a1,*a1,*a1,*a1,*a1,*a1,*a1,*a1,*a1]
  - name: a3
    excludes: &a3 [*a2,*a2,*a2,*a2,*a2,*a2,*a2,*a2,*a2]
  - name: a4
    excludes: &a4 [*a3,*a3,*a3,*a3,*a3,*a3,*a3,*a3,*a3]
`,
	}

	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte(doc)); err == nil {
				t.Error("parseConfig(alias expansion document) = nil, want error")
			}
		})
	}
}

func TestLoadInterval(t *testing.T) {
	tests := []struct {
		name         string
		env          string
		wantInterval time.Duration
		wantEnabled  bool
	}{
		{"duration", "30m", 30 * time.Minute, true},
		{"hour duration", "1h", time.Hour, true},
		{"off", "off", defaultInterval, false},
		{"off uppercase", "OFF", defaultInterval, false},
		{"disabled", "disabled", defaultInterval, false},
		{"disabled mixed case", "Disabled", defaultInterval, false},
		{"zero", "0", defaultInterval, false},
		{"zero seconds", "0s", defaultInterval, false},
		{"unset defaults to enabled", "", defaultInterval, true},
		{"unparseable falls back enabled", "not-a-duration", defaultInterval, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SYNC_INTERVAL", tt.env)
			interval, enabled := loadInterval()
			if enabled != tt.wantEnabled {
				t.Errorf("loadInterval() enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if interval != tt.wantInterval {
				t.Errorf("loadInterval() interval = %v, want %v", interval, tt.wantInterval)
			}
		})
	}
}

// TestLoadInterval_negativeDurationFallsBackToDefaultEnabled pins the
// negative-duration arm of loadInterval's inner switch: a parseable but
// negative SYNC_INTERVAL ("-30m") is neither a disable sentinel nor a valid
// cadence, so the built-in scheduler stays ENABLED at the default interval.
// This is a distinct path from the unparseable case and guards against a
// `d == 0` -> `d <= 0` regression that would wrongly disable scheduling.
func TestLoadInterval_negativeDurationFallsBackToDefaultEnabled(t *testing.T) {
	t.Setenv("SYNC_INTERVAL", "-30m")

	interval, enabled := loadInterval()

	if !enabled {
		t.Errorf("loadInterval() with -30m enabled = false, want true (negative is not a disable sentinel)")
	}
	if interval != defaultInterval {
		t.Errorf("loadInterval() with -30m interval = %v, want default %v", interval, defaultInterval)
	}
}

// TestRunDaemon_ConfigFailureLogsOneActionableRecord pins the boot boundary:
// each load stage produces one error record with the failed path and the mount hint.
func TestRunDaemon_ConfigFailureLogsOneActionableRecord(t *testing.T) {
	tests := []struct {
		name      string
		doc       string
		stage     string
		directory bool
	}{
		{name: "missing", stage: "read"},
		{name: "not_regular", stage: "read", directory: true},
		{name: "unparseable", doc: "jobs: [\n", stage: "parse"},
		{name: "invalid", doc: "jobs: []\n", stage: "validate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.directory {
				path = t.TempDir()
			} else if tt.doc != "" {
				if err := os.WriteFile(path, []byte(tt.doc), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			t.Setenv("CONFIG_PATH", path)

			if err := runDaemon(t.Context(), testSocketPath(t), fixedRunner("true")); err == nil {
				t.Fatal("runDaemon() with broken config = nil, want error")
			}

			const message = "cannot load config"
			if got := rec.CountLevel(slog.LevelError, ""); got != 1 {
				t.Errorf("runDaemon() ERROR records = %d, want 1; logs = %q", got, rec.Messages())
			}
			if !rec.HasAttr(message, "path", path) {
				t.Errorf("%q missing path=%q; logs = %q", message, path, rec.Messages())
			}
			if !rec.HasAttr(message, "stage", tt.stage) {
				t.Errorf("%q missing stage=%q; logs = %q", message, tt.stage, rec.Messages())
			}
			const hint = "mount a config.yaml at this path — see config.example.yaml in the repo"
			if !rec.HasAttr(message, "hint", hint) {
				t.Errorf("%q missing hint=%q; logs = %q", message, hint, rec.Messages())
			}
		})
	}
}

// TestDaemonRun_ConfigFailureLogsOneActionableRecord pins the reload boundary:
// the same load stages retain their path and identify the request that triggered them.
func TestDaemonRun_ConfigFailureLogsOneActionableRecord(t *testing.T) {
	tests := []struct {
		name      string
		doc       string
		stage     string
		directory bool
	}{
		{name: "missing", stage: "read"},
		{name: "not_regular", stage: "read", directory: true},
		{name: "unparseable", doc: "jobs: [\n", stage: "parse"},
		{name: "invalid", doc: "jobs: []\n", stage: "validate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.directory {
				path = t.TempDir()
			} else if tt.doc != "" {
				if err := os.WriteFile(path, []byte(tt.doc), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			t.Setenv("CONFIG_PATH", path)
			d := &daemon{health: newTestHealth(t)}

			out := d.run(t.Context(), "external", struct{}{})
			if out.OK || out.Reason != "config reload failed" {
				t.Errorf("daemon.run() = ok:%v reason:%q, want false %q", out.OK, out.Reason, "config reload failed")
			}

			const message = "config reload failed"
			if got := rec.CountLevel(slog.LevelError, ""); got != 1 {
				t.Errorf("daemon.run() ERROR records = %d, want 1; logs = %q", got, rec.Messages())
			}
			for key, value := range map[string]string{"path": path, "stage": tt.stage, "trigger": "external"} {
				if !rec.HasAttr(message, key, value) {
					t.Errorf("%q missing %s=%q; logs = %q", message, key, value, rec.Messages())
				}
			}
		})
	}
}

// TestLoadConfig_acceptsExactlyCapBytes pins the upper boundary of the size
// guard: a config of exactly configCapBytes must be accepted, because the
// guard rejects only files strictly larger than the cap. A `>` -> `>=`
// off-by-one would reject a file sitting precisely on the limit.
func TestLoadConfig_acceptsExactlyCapBytes(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")

	doc := "jobs:\n  - name: caddy\n    local: /sources/caddy\n" +
		"    remote_host: root@host\n    remote_path: /srv/caddy\n" +
		"    ssh_key: " + key + "\n"
	// Pad to exactly the cap with a trailing full-line YAML comment so the
	// document stays valid while landing on the boundary byte count.
	pad := configCapBytes - len(doc)
	if pad < 1 {
		t.Fatalf("base doc is %d bytes, already >= cap %d", len(doc), configCapBytes)
	}
	doc += "#" + strings.Repeat("x", pad-1)
	if len(doc) != configCapBytes {
		t.Fatalf("padded doc = %d bytes, want exactly %d", len(doc), configCapBytes)
	}
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	t.Setenv("SYNC_INTERVAL", "")

	lc, _, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig at exactly cap (%d bytes) = %v, want success", configCapBytes, err)
	}
	if len(lc.cfg.Jobs) != 1 || lc.cfg.Jobs[0].Name != "caddy" {
		t.Errorf("loadConfig = %+v, want one caddy job", lc.cfg.Jobs)
	}
}

// TestLoadConfig_rejectsOverCapBytes covers the other side of the boundary:
// one byte over the cap must be rejected by the size guard.
func TestLoadConfig_rejectsOverCapBytes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	doc := strings.Repeat("#", configCapBytes+1)
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)

	_, _, err := loadConfig()

	if err == nil {
		t.Fatal("loadConfig one byte over cap = nil, want error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("loadConfig over cap error = %q, want to contain 'exceeds'", err)
	}
}

// TestConfigReaders_refuseNonRegularConfig pins the kind check both config
// readers run on the opened object: a nonblocking open cannot wait on a FIFO,
// and fstat ensures neither reader consumes a non-regular stream. The byte
// bound also covers a regular file that grows while it is being read.
func TestConfigReaders_refuseNonRegularConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	t.Setenv("CONFIG_PATH", path)
	t.Setenv("SYNC_INTERVAL", "1h")
	if _, _, err := loadConfig(); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("loadConfig() error = %v, want a not-a-regular-file refusal", err)
	}
	if opts := probeOptions(); len(opts) != 0 {
		t.Errorf("probeOptions() = %d options, want 0 (disarmed)", len(opts))
	}
}

func TestLoadSyncTimeout(t *testing.T) {
	// The default is a documented part of the container's contract (10m, in
	// the README's SYNC_TIMEOUT row), so this arm asserts the duration itself
	// rather than the constant the function returns: an expectation of
	// defaultSyncTimeout holds for whatever that constant says, including a
	// zero that would expire every job's context before rsync starts.
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "")
		if got := loadSyncTimeout(); got != 10*time.Minute {
			t.Errorf("loadSyncTimeout() with SYNC_TIMEOUT unset = %v, want 10m0s", got)
		}
	})
	t.Run("parsed value", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "30m")
		if got := loadSyncTimeout(); got.String() != "30m0s" {
			t.Errorf("loadSyncTimeout() = %v, want 30m0s", got)
		}
	})
	t.Run("default on garbage", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "not-a-duration")
		if got := loadSyncTimeout(); got != defaultSyncTimeout {
			t.Errorf("loadSyncTimeout() = %v, want %v", got, defaultSyncTimeout)
		}
	})
	t.Run("default on non-positive", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "-5m")
		if got := loadSyncTimeout(); got != defaultSyncTimeout {
			t.Errorf("loadSyncTimeout() = %v, want %v", got, defaultSyncTimeout)
		}
	})
	// Exactly zero is the boundary of the d <= 0 guard: a parseable "0"
	// duration must fall back to the default, not be used as a 0s timeout.
	// A `<=` -> `<` off-by-one would let the zero through.
	t.Run("default on zero", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "0")
		if got := loadSyncTimeout(); got != defaultSyncTimeout {
			t.Errorf("loadSyncTimeout() = %v, want %v", got, defaultSyncTimeout)
		}
	})
	t.Run("default on zero seconds", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "0s")
		if got := loadSyncTimeout(); got != defaultSyncTimeout {
			t.Errorf("loadSyncTimeout() = %v, want %v", got, defaultSyncTimeout)
		}
	})
}

// TestLoadSyncTimeout_NonPositiveWarnsWithFallback pins the app-owned half of
// the SYNC_TIMEOUT contract: a syntactically valid but non-positive duration is
// parsed by envx and then rejected here, so the WARN naming the rejected value
// and the applied default is the operator's only record of the substitution.
// Not parallel: capture.Default swaps the global slog default.
func TestLoadSyncTimeout_NonPositiveWarnsWithFallback(t *testing.T) {
	const message = "SYNC_TIMEOUT must be positive, using default"
	tests := []struct {
		name, value, wantRejected string
	}{
		{name: "negative", value: "-5m", wantRejected: "-5m0s"},
		{name: "zero", value: "0", wantRejected: "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			t.Setenv("SYNC_TIMEOUT", tt.value)

			if got := loadSyncTimeout(); got != 10*time.Minute {
				t.Errorf("loadSyncTimeout() with SYNC_TIMEOUT=%q = %v, want 10m0s", tt.value, got)
			}
			if got := rec.CountLevel(slog.LevelWarn, message); got != 1 {
				t.Errorf("%q WARN records for SYNC_TIMEOUT=%q = %d, want 1; logs = %q", message, tt.value, got, rec.Messages())
			}
			if !rec.HasAttr(message, "value", tt.wantRejected) {
				t.Errorf("%q missing value=%q; logs = %q", message, tt.wantRejected, rec.Messages())
			}
			if !rec.HasAttr(message, "default", "10m0s") {
				t.Errorf("%q missing default=10m0s; logs = %q", message, rec.Messages())
			}
		})
	}
}

func TestSetupLogger_levelMapping(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	tests := []struct {
		name string
		env  string
		want slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"uppercase is lowercased", "DEBUG", slog.LevelDebug},
		{"surrounding whitespace trimmed", "  warn  ", slog.LevelWarn},
		{"unset defaults to info", "", slog.LevelInfo},
		{"set-but-empty defaults to info", "   ", slog.LevelInfo},
		{"unrecognized defaults to info", "bogus", slog.LevelInfo},
		{"offset syntax is not published, so info", "info+4", slog.LevelInfo},
		{"negative offset syntax is not published either", "error-1", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tt.env)

			setupLogger()

			if !slog.Default().Enabled(t.Context(), tt.want) {
				t.Errorf("setupLogger() LOG_LEVEL=%q: level %v not enabled, want enabled", tt.env, tt.want)
			}
			if slog.Default().Enabled(t.Context(), tt.want-1) {
				t.Errorf("setupLogger() LOG_LEVEL=%q: level %v enabled, want disabled", tt.env, tt.want-1)
			}
		})
	}
}

// captureSetupLoggerStderr installs the logger for the given LOG_LEVEL value on
// the REAL production handler and returns what it wrote to stderr. setupLogger
// installs its own handler over slog.Default(), so capture.Default cannot see
// its records — the operator reads them off the container's stderr, which is
// what this redirects. Callers must not be parallel: this sets env and swaps
// os.Stderr plus the global slog default.
func captureSetupLoggerStderr(t *testing.T, level string) string {
	t.Helper()
	originalLogger := slog.Default()
	originalStderr := os.Stderr
	stderrPath := filepath.Join(t.TempDir(), "stderr")
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	os.Stderr = stderr
	t.Cleanup(func() {
		os.Stderr = originalStderr
		slog.SetDefault(originalLogger)
		_ = stderr.Close()
	})
	t.Setenv("LOG_LEVEL", level)

	setupLogger()

	if err := stderr.Close(); err != nil {
		t.Fatalf("close stderr capture: %v", err)
	}
	os.Stderr = originalStderr
	slog.SetDefault(originalLogger)
	out, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr capture: %v", err)
	}
	return string(out)
}

// TestSetupLogger_warnsOnUnrecognizedLevel pins the rejected-value warning's
// attributes, including that the record echoes the operator's RAW value.
// Not parallel: sets env and swaps os.Stderr plus the global slog default.
func TestSetupLogger_warnsOnUnrecognizedLevel(t *testing.T) {
	logs := captureSetupLoggerStderr(t, " bogus ")

	const message = `msg="unrecognized LOG_LEVEL, using info"`
	if got := strings.Count(logs, message); got != 1 {
		t.Errorf("setupLogger() warning count = %d, want 1; stderr = %q", got, logs)
	}
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("setupLogger() stderr = %q, want level=WARN", logs)
	}
	if !strings.Contains(logs, `value=" bogus "`) {
		t.Errorf("setupLogger() stderr = %q, want the raw value echoed as value=\" bogus \"", logs)
	}
}

// TestSetupLogger_warningFiresOnlyForUnpublishedValues pins WHICH values the
// refusal covers. The offset rows are the ones that matter: slog's levels are
// four apart, so "info+4" parses to slog.LevelWarn — filtering the parsed level
// against the four published constants would accept it silently and raise the
// threshold to WARN, which disarms the shipped staleness alert. Testing the
// INPUT over the four published names makes the claim true by construction.
// Not parallel: each case sets env and swaps os.Stderr plus the global default.
func TestSetupLogger_warningFiresOnlyForUnpublishedValues(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  int
	}{
		{name: "debug", level: "debug", want: 0},
		{name: "info", level: "info", want: 0},
		{name: "warn", level: "warn", want: 0},
		{name: "error", level: "error", want: 0},
		{name: "warning alias", level: "warning", want: 0},
		{name: "padded warn", level: "  warn  ", want: 0},
		{name: "unset", level: "", want: 0},
		{name: "set but whitespace only", level: "   ", want: 0},
		{name: "positive offset", level: "info+4", want: 1},
		{name: "negative offset", level: "error-1", want: 1},
	}

	const message = `msg="unrecognized LOG_LEVEL, using info"`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureSetupLoggerStderr(t, tt.level)
			if got := strings.Count(logs, message); got != tt.want {
				t.Errorf("setupLogger() LOG_LEVEL=%q warning count = %d, want %d; stderr = %q",
					tt.level, got, tt.want, logs)
			}
		})
	}
}

func TestValidateRemoteHost(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, host, wantErr string }{
		{"plain hostname accepted", "host", ""},
		{"user on hostname accepted", "root@host", ""},
		{"bare ipv4 accepted", "192.0.2.10", ""},
		{"bare ipv6 accepted", "2001:db8::1", ""},
		{"bracketed ipv6 accepted", "user@[2001:db8::1]", ""},
		{"bracketed ipv4 accepted", "[192.0.2.10]", ""},
		{"user on bracketed ipv4 accepted", "user@[192.0.2.10]", ""},
		{"invalid login prefix rejected", "-bad@host", "invalid login prefix"},
		{"empty login prefix rejected", "@host", "invalid login prefix"},
		{"malformed bracket name rejected", "[name]", "not a valid hostname or IP"},
		{"malformed bracket incomplete ipv6", "[2001:db8]", "not a valid hostname or IP"},
		{"trailing colon rejected", "host:", "not a valid hostname or IP"},
		{"zone id rejected", "fe80::1%eth0", "not a valid hostname or IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateRemoteHost(&job{Name: "j", RemoteHost: tt.host})
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validateRemoteHost(%q) = %v, want nil", tt.host, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateRemoteHost(%q) = nil, want error containing %q", tt.host, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateRemoteHost(%q) error = %q, want to contain %q", tt.host, err, tt.wantErr)
			}
		})
	}
}

func TestValidate_maxDelete(t *testing.T) {
	key := writeKey(t)
	tests := []struct {
		name      string
		maxDelete *int
		wantErr   string
	}{
		{"unset is accepted", nil, ""},
		{"zero is accepted", new(0), ""},
		{"positive is accepted", new(100), ""},
		{"negative is rejected", new(-1), "max_delete must be >= 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := validJob("caddy", key)
			j.Delete = true
			j.MaxDelete = tt.maxDelete
			err := config{Jobs: []job{j}}.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() with max_delete=%v = %v, want nil", tt.maxDelete, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() with max_delete=%v = nil, want error containing %q", tt.maxDelete, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validate() error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidate_warnsMaxDeleteWithoutDelete pins warnInertSettings' advisory for
// an inert max_delete cap. max_delete only takes effect under delete:true
// (buildRsyncArgs emits --max-delete inside the --delete branch), so a cap set
// without delete is silently ignored; the warning is how an operator learns the
// cap is dead, part of the logs-only observability contract. The cap-without-
// delete case must emit the warning, while a cap paired with delete:true (the
// cap is live) and an unset cap must stay silent.
func TestValidate_warnsMaxDeleteWithoutDelete(t *testing.T) {
	// Not parallel: captureLogs mutates the global slog default.
	key := writeKey(t)
	const warning = "max_delete set without delete:true"

	tests := []struct {
		name      string
		maxDelete *int
		delete    bool
		wantWarn  bool
	}{
		{"cap set without delete warns", new(100), false, true},
		{"cap set with delete is silent", new(100), true, false},
		{"unset cap without delete is silent", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			j := validJob("caddy", key)
			j.MaxDelete = tt.maxDelete
			j.Delete = tt.delete
			cfg := config{Jobs: []job{j}}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			adviseConfig(cfg)
			if got := rec.Contains(warning); got != tt.wantWarn {
				t.Errorf("adviseConfig(max_delete=%v, delete=%v) warned=%v, want %v; logs=%q",
					tt.maxDelete, tt.delete, got, tt.wantWarn, rec.Messages())
			}
		})
	}
}

// TestValidate_warnsLoneRemoteOwnership pins warnInertSettings' advisory for an
// unpaired remote_uid/remote_gid. buildRsyncArgs emits --chown only when BOTH
// are set, so a lone uid or gid is silently dropped and the remote keeps the
// ssh user's ownership; the warning surfaces that. The guard is an exclusive-or
// (warn iff exactly one of the pair is set), so the full truth table is
// asserted -- both-set and neither-set stay silent, each lone field warns --
// which both documents the contract and keeps the check deterministic.
func TestValidate_warnsLoneRemoteOwnership(t *testing.T) {
	// Not parallel: captureLogs mutates the global slog default.
	key := writeKey(t)
	const warning = "remote_uid/remote_gid set without its pair"

	tests := []struct {
		name       string
		uid, gid   *uint32
		wantWarn   bool
		wantFields map[string]string // structured attrs the advisory must carry when it fires
	}{
		{"both set is silent", new(uint32(1000)), new(uint32(1000)), false, nil},
		{"uid only warns", new(uint32(1000)), nil, true, map[string]string{"remote_uid_set": "true", "remote_gid_set": "false"}},
		{"gid only warns", nil, new(uint32(1000)), true, map[string]string{"remote_uid_set": "false", "remote_gid_set": "true"}},
		{"neither set is silent", nil, nil, false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			j := validJob("caddy", key)
			j.RemoteUID = tt.uid
			j.RemoteGID = tt.gid
			cfg := config{Jobs: []job{j}}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			adviseConfig(cfg)
			if got := rec.Contains(warning); got != tt.wantWarn {
				t.Errorf("adviseConfig(uid_set=%v, gid_set=%v) warned=%v, want %v; logs=%q",
					tt.uid != nil, tt.gid != nil, got, tt.wantWarn, rec.Messages())
			}
			// When the advisory fires it must report WHICH side is set, so an
			// operator can tell which field to add. Expected values are fixed
			// literals (not derived from the inputs) to keep the check honest.
			for k, v := range tt.wantFields {
				if !rec.HasAttr(warning, k, v) {
					t.Errorf("validate(uid_set=%v, gid_set=%v) advisory missing attr %s=%s",
						tt.uid != nil, tt.gid != nil, k, v)
				}
			}
		})
	}
}

func TestAdviseConfig_warnsEmptyExclude(t *testing.T) {
	rec := capture.Default(t)
	j := validJob("caddy", writeKey(t))
	j.Excludes = []string{""}
	cfg := config{Jobs: []job{j}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}

	adviseConfig(cfg)

	const warning = "empty excludes entry is ignored; an empty pattern excludes nothing"
	if got := rec.CountExact(warning); got != 1 {
		t.Errorf("adviseConfig() empty-exclude warnings = %d, want 1; logs=%q", got, rec.Messages())
	}
	if !rec.HasAttr(warning, "job", "caddy") {
		t.Errorf("adviseConfig() empty-exclude warning missing job=caddy; logs=%q", rec.Messages())
	}
}

func TestAdviseConfig_warnsExcludeWhitespace(t *testing.T) {
	rec := capture.Default(t)
	j := validJob("caddy", writeKey(t))
	j.Excludes = []string{" *.log ", "   "}
	cfg := config{Jobs: []job{j}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}

	adviseConfig(cfg)

	const warning = "exclude pattern has leading or trailing whitespace; rsync matches it literally"
	if !rec.HasAttr(warning, "without_whitespace", "*.log") {
		t.Errorf("adviseConfig() whitespace warning missing trimmed pattern; logs=%q", rec.Messages())
	}
	if !rec.HasAttr(warning, "without_whitespace", "") {
		t.Errorf("adviseConfig() whitespace-only warning missing trimmed pattern; logs=%q", rec.Messages())
	}
}

// TestValidate_remotePathWithSpaceRejected pins the remote_path space guard
// in checkNoForbiddenChars: a space is not a shell metacharacter (hasShellMeta
// deliberately permits spaces in path fields), so remote_path "/srv/my path"
// passes the metachar loop yet must still be rejected -- a remote-side login
// shell word-splits it into several rsync args, mis-targeting the dest (and,
// under --delete, the wrong remote tree). The sibling ssh_key-space branch is
// pinned by TestValidate_sshKeyWithSpaceRejected; this closes the untested
// remote_path arm.
func TestValidate_remotePathWithSpaceRejected(t *testing.T) {
	key := writeKey(t)
	cfg := config{Jobs: []job{{
		Name:       "spaced",
		Local:      "/sources/spaced",
		RemoteHost: "root@192.0.2.87",
		RemotePath: "/srv/my path",
		SSHKey:     key,
	}}}

	err := cfg.validate()

	if err == nil {
		t.Fatalf("validate() with spaced remote_path = nil, want error")
	}
	if !strings.Contains(err.Error(), "must not contain spaces") {
		t.Errorf("validate() error = %q, want to contain 'must not contain spaces'", err)
	}
}

// TestValidate_remotePathWithGlobRejected pins the remote_path glob guard:
// rsync leaves *?[] alone in a filename argument and the receiving side expands
// them, so a pattern-shaped path silently resolves to whichever sibling tree
// matches — and under --delete that deletes in a tree the operator never named.
// The trigger is time rather than typing: a literal directory named `lit[2026]`
// delivers correctly until a sibling named `lit2` appears.
func TestValidate_remotePathWithGlobRejected(t *testing.T) {
	key := writeKey(t)
	for _, path := range []string{"/srv/cont*", "/srv/c?nt", "/srv/lit[2026]", "/srv/lit]"} {
		j := validJob("globbed", key)
		j.RemotePath = path
		err := config{Jobs: []job{j}}.validate()
		if err == nil {
			t.Errorf("validate() with remote_path %q = nil, want error", path)
			continue
		}
		if !strings.Contains(err.Error(), "must not contain glob characters") {
			t.Errorf("validate() with remote_path %q error = %q, want a glob refusal", path, err)
		}
	}
}

// TestValidate_warnsSharedDestinations pins the cross-job advisory: two jobs
// addressing one remote tree with either deleting is accepted (an rsync exclude
// can protect the receiver, so the pair CAN be correct) but warned about,
// because otherwise each pass silently deletes the other's files while the
// heartbeat still reports failed=0. Identity is host+path, so a different host,
// a disjoint path, or neither job deleting stays silent.
func TestValidate_warnsSharedDestinations(t *testing.T) {
	// Not parallel: capture.Default swaps the global slog default.
	key := writeKey(t)
	const warning = "jobs share a remote destination tree and one deletes"

	tests := []struct {
		name             string
		pathA, pathB     string
		hostB            string
		deleteA, deleteB bool
		wantWarn         bool
	}{
		{"same path with delete warns", "/srv/x", "/srv/x", "root@192.0.2.87", true, false, true},
		{"nested path with delete warns", "/srv", "/srv/x", "root@192.0.2.87", true, false, true},
		{"rrsync-rooted parent with delete warns", "/", "/srv/x", "root@192.0.2.87", true, false, true},
		{"same path without delete is silent", "/srv/x", "/srv/x", "root@192.0.2.87", false, false, false},
		{"sibling paths are silent", "/srv/x", "/srv/xy", "root@192.0.2.87", true, true, false},
		{"different host is silent", "/srv/x", "/srv/x", "root@192.0.2.88", true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			a, b := validJob("a", key), validJob("b", key)
			a.RemotePath, a.Delete = tt.pathA, tt.deleteA
			b.RemotePath, b.Delete, b.RemoteHost = tt.pathB, tt.deleteB, tt.hostB
			cfg := config{Jobs: []job{a, b}}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			adviseConfig(cfg)
			if got := rec.Contains(warning); got != tt.wantWarn {
				t.Errorf("adviseConfig(%q vs %q on %q) warned=%v, want %v; logs=%q",
					tt.pathA, tt.pathB, tt.hostB, got, tt.wantWarn, rec.Messages())
			}
		})
	}
}

// TestValidate_sharedDestinationsIdentityIsTheHostComponent pins the identity
// the advisory compares. A bare host and the same host with a login prefix
// address one tree (the container runs as root by design, so an omitted user IS
// root), and RFC 4343 makes DNS case insignificant — so both pairs must warn.
// Under the old raw-string guard both were silent, which suppressed the advisory
// for exactly the pairs it exists to describe.
func TestValidate_sharedDestinationsIdentityIsTheHostComponent(t *testing.T) {
	// Not parallel: capture.Default swaps the global slog default.
	key := writeKey(t)
	const warning = "jobs share a remote destination tree and one deletes"

	tests := []struct{ name, hostA, hostB string }{
		{"bare host and root@host are one host", "defiant", "root@defiant"},
		{"DNS case is insignificant", "Defiant", "defiant"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := capture.Default(t)
			a, b := validJob("a", key), validJob("b", key)
			a.RemoteHost, a.RemotePath, a.Delete = tt.hostA, "/srv/x", true
			b.RemoteHost, b.RemotePath = tt.hostB, "/srv/x"
			cfg := config{Jobs: []job{a, b}}
			if err := cfg.validate(); err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
			adviseConfig(cfg)
			if !rec.Contains(warning) {
				t.Errorf("adviseConfig(%q vs %q) did not warn; logs=%q", tt.hostA, tt.hostB, rec.Messages())
			}
		})
	}
}

func TestValidate_sharedDestinationsNormalizesIPv6Identity(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	a, b := validJob("a", key), validJob("b", key)
	a.RemoteHost, a.RemotePath, a.Delete = "2001:db8::1", "/srv/x", true
	b.RemoteHost, b.RemotePath = "2001:0db8:0:0:0:0:0:1", "/srv/x"

	cfg := config{Jobs: []job{a, b}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree"
	if !rec.Contains(warning) {
		t.Errorf("adviseConfig() with equivalent IPv6 spellings did not warn; logs=%q", rec.Messages())
	}
}

// TestValidate_sharedDestinationsSuppressionByKeyIsGone pins the other half of
// the identity change: two jobs reaching one tree with DIFFERENT ssh keys must
// still warn, because two keys can reach one tree. The keys are reported in the
// record instead of compared, since a path-rooting rrsync wrapper on one of them
// is what makes a flagged pair safe.
func TestValidate_sharedDestinationsSuppressionByKeyIsGone(t *testing.T) {
	rec := capture.Default(t)
	keyA, keyB := writeKey(t), writeKey(t)
	a, b := validJob("a", keyA), validJob("b", keyB)
	a.RemotePath, a.Delete = "/srv/x", true
	b.RemotePath = "/srv/x"

	cfg := config{Jobs: []job{a, b}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree and one deletes"
	if !rec.Contains(warning) {
		t.Errorf("adviseConfig() with two ssh keys on one tree did not warn; logs=%q", rec.Messages())
	}
}

// TestValidate_sharedDestinationsOutputIsBounded pins the amplification fix: the
// pairwise emitter produced one record per unordered pair, so 100 mutually
// overlapping deleting jobs cost 4,950 records and 1,000 cost 499,500 — paid
// twice before the first rsync in built-in mode. Grouping by conflicting tree
// makes it ONE. The assertion is a bounded record count, never serialized bytes
// or any other implementation detail.
func TestValidate_sharedDestinationsOutputIsBounded(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	jobs := make([]job, 0, 40)
	for i := range 40 {
		j := validJob(fmt.Sprintf("job-%02d", i), key)
		j.RemotePath, j.Delete = "/srv/x", true
		jobs = append(jobs, j)
	}

	cfg := config{Jobs: jobs}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree and one deletes; each pass may delete the others' files"
	if got := rec.CountExact(warning); got != 1 {
		t.Errorf("validate() with 40 identical deleting destinations emitted %d advisory records, want 1", got)
	}
}

func TestValidate_sharedDestinationsSamplesAreCappedAtThree(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	root := validJob("root", key)
	first := validJob("first", key)
	second := validJob("second", key)
	omitted := validJob("omitted", key)
	root.RemotePath, root.Delete = "/a", true
	first.RemotePath, first.Delete = "/a/b", true
	second.RemotePath, second.Delete = "/a/c", true
	omitted.RemotePath, omitted.Delete = "/a/d", true
	cfg := config{Jobs: []job{root, first, second, omitted}}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree"
	if !rec.HasAttr(warning, "jobs", "4") {
		t.Errorf("adviseConfig() advisory missing jobs=4; logs=%q", rec.Messages())
	}
	if !rec.HasAttr(warning, "deletes", "root, first, second") {
		t.Errorf("adviseConfig() pruning-name sample is not capped at three; logs=%q", rec.Messages())
	}
	if rec.AttrContains(warning, "outermost_first", "omitted at /a/d (") {
		t.Errorf("adviseConfig() member sample includes the fourth job; logs=%q", rec.Messages())
	}
}

func TestValidate_sharedDestinationsSampleIsOutermostFirst(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	deepest := validJob("deepest", key)
	lexicalSecond := validJob("lexical-second", key)
	lexicalFirst := validJob("lexical-first", key)
	root := validJob("root", key)
	deepest.RemotePath = "/a/deep/path"
	lexicalSecond.RemotePath = "/a/cc"
	lexicalFirst.RemotePath = "/a/bb"
	root.RemotePath, root.Delete = "/a", true
	cfg := config{Jobs: []job{deepest, lexicalSecond, lexicalFirst, root}}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree"
	got, ok := rec.AttrValue(warning, "outermost_first")
	if !ok {
		t.Fatalf("adviseConfig() advisory has no outermost_first attribute; logs=%q", rec.Messages())
	}
	rootIndex := strings.Index(got, "root at /a (")
	firstIndex := strings.Index(got, "lexical-first at /a/bb (")
	secondIndex := strings.Index(got, "lexical-second at /a/cc (")
	if rootIndex < 0 || firstIndex < 0 || secondIndex < 0 {
		t.Fatalf("adviseConfig() outermost_first = %q, want root, lexical-first, and lexical-second", got)
	}
	if rootIndex >= firstIndex || firstIndex >= secondIndex {
		t.Errorf("adviseConfig() outermost_first = %q, want root then lexical-first then lexical-second", got)
	}
	if strings.Contains(got, "deepest at /a/deep/path (") {
		t.Errorf("adviseConfig() outermost_first = %q, want the deeper fourth member outside the sample", got)
	}
}

// TestValidate_sharedDestinationsOneRecordPerTree pins that the grouping is per
// conflicting TREE and not per host: one host holding two independent overlaps
// (/a with /a/x, /b with /b/y) must produce two records, so neither conflict is
// conflated with the other or dropped past a per-host sample budget.
func TestValidate_sharedDestinationsOneRecordPerTree(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	paths := []string{"/a", "/a/x", "/b", "/b/y"}
	jobs := make([]job, 0, len(paths))
	for i, p := range paths {
		j := validJob(fmt.Sprintf("job-%d", i), key)
		j.RemotePath, j.Delete = p, true
		jobs = append(jobs, j)
	}

	cfg := config{Jobs: jobs}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree and one deletes; each pass may delete the others' files"
	if got := rec.CountExact(warning); got != 2 {
		t.Errorf("validate() with two independent overlaps on one host emitted %d records, want 2; logs=%q",
			got, rec.Messages())
	}
}

// TestValidate_sharedDestinationsFindsInterleavedSiblingPrefix is the row a
// sorted single-pass scan would silently pass. Plain lexicographic order does
// not make an ancestor adjacent to its descendants: both '-' (0x2d) and '.'
// (0x2e) are legal in a remote_path and sort below '/' (0x2f), so {/data,
// /data-old, /data/x} orders with the sibling BETWEEN the pair that overlaps.
// A maintain-root scan then tests /data/x against /data-old and misses that
// /data contains /data/x, which is the exact data-loss pair the advisory is for.
func TestValidate_sharedDestinationsFindsInterleavedSiblingPrefix(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	paths := []string{"/data", "/data-old", "/data/x"}
	jobs := make([]job, 0, len(paths))
	for i, p := range paths {
		j := validJob(fmt.Sprintf("job-%d", i), key)
		j.RemotePath, j.Delete = p, true
		jobs = append(jobs, j)
	}

	cfg := config{Jobs: jobs}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree and one deletes; each pass may delete the others' files"
	if got := rec.CountExact(warning); got != 1 {
		t.Fatalf("validate() with {/data, /data-old, /data/x} emitted %d records, want 1; logs=%q",
			got, rec.Messages())
	}
	if !rec.AttrContains(warning, "outermost_first", "/data/x") {
		t.Errorf("advisory does not report /data/x with /data; logs=%q", rec.Messages())
	}
	if !rec.HasAttr(warning, "jobs", "2") {
		t.Errorf("advisory jobs count is not 2, so /data-old was folded in; logs=%q", rec.Messages())
	}
}

func TestValidate_invalidJobDoesNotEmitSharedDestinationAdvisory(t *testing.T) {
	rec := capture.Default(t)
	source := t.TempDir()
	cfgPath := writeValidCfg(t, source)
	key := filepath.Join(filepath.Dir(cfgPath), "id_ed25519")
	doc := "jobs:\n" +
		"  - name: \"bad\\x7fname\"\n    local: " + source + "\n" +
		"    remote_host: root@192.0.2.10\n    remote_path: /srv/shared\n" +
		"    ssh_key: " + key + "\n    delete: true\n" +
		"  - name: other\n    local: " + source + "\n" +
		"    remote_host: root@192.0.2.10\n    remote_path: /srv/shared\n" +
		"    ssh_key: " + key + "\n"
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write invalid overlapping config: %v", err)
	}
	d := &daemon{health: newTestHealth(t)}

	out := d.run(t.Context(), "external", struct{}{})

	if out.OK {
		t.Error("daemon.run() with an invalid overlapping config ok = true, want false")
	}
	if !rec.AttrContains("config reload failed", "error", "control characters") {
		t.Errorf("daemon.run() error does not report the control-character refusal; logs=%q", rec.Messages())
	}
	const warning = "jobs share a remote destination tree and one deletes"
	if rec.Contains(warning) {
		t.Errorf("daemon.run() on a refused config emitted the shared-destination advisory; logs=%q", rec.Messages())
	}
}

func TestValidate_sharedDestinationsReportsOnlyTreesAtRiskFromDelete(t *testing.T) {
	rec := capture.Default(t)
	key := writeKey(t)
	ancestor := validJob("ancestor", key)
	pruningChild := validJob("pruning-child", key)
	sibling := validJob("sibling", key)
	ancestor.RemotePath = "/a"
	pruningChild.RemotePath, pruningChild.Delete = "/a/b", true
	sibling.RemotePath = "/a/c"
	cfg := config{Jobs: []job{ancestor, pruningChild, sibling}}

	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil", err)
	}
	adviseConfig(cfg)

	const warning = "jobs share a remote destination tree"
	if !rec.HasAttr(warning, "jobs", "2") {
		t.Errorf("adviseConfig() advisory missing jobs=2; logs=%q", rec.Messages())
	}
	if !rec.AttrContains(warning, "outermost_first", "ancestor at /a (") {
		t.Errorf("adviseConfig() advisory missing the non-pruning ancestor; logs=%q", rec.Messages())
	}
	if !rec.AttrContains(warning, "outermost_first", "pruning-child at /a/b (") {
		t.Errorf("adviseConfig() advisory missing the pruning child; logs=%q", rec.Messages())
	}
	if rec.AttrContains(warning, "outermost_first", "sibling at /a/c (") {
		t.Errorf("adviseConfig() advisory includes an unaffected sibling; logs=%q", rec.Messages())
	}
}

// TestLoadTransport pins the opt-in switches' env contract. The unset row is
// the one that matters: an unset environment must yield the shipped transport,
// so no existing deployment changes behaviour on an image bump. An unrecognized
// SYNC_COMPRESS falls back to off, which is this app's settled posture for a
// malformed env value and the safe direction for an optimisation.
// Not parallel: t.Setenv is incompatible with t.Parallel.
func TestLoadTransport(t *testing.T) {
	tests := []struct {
		name     string
		acls     string
		xattrs   string
		compress string
		want     transport
	}{
		{name: "unset is the shipped transport", want: transport{}},
		{name: "off", compress: "off", want: transport{}},
		{name: "disabled", compress: "disabled", want: transport{}},
		{name: "no", compress: "no", want: transport{}},
		{name: "false", compress: "false", want: transport{}},
		{name: "zero", compress: "0", want: transport{}},
		{name: "on", compress: "on", want: transport{compress: "auto"}},
		{name: "yes", compress: "yes", want: transport{compress: "auto"}},
		{name: "true", compress: "true", want: transport{compress: "auto"}},
		{name: "one", compress: "1", want: transport{compress: "auto"}},
		{name: "auto", compress: "auto", want: transport{compress: "auto"}},
		{name: "zstd", compress: "zstd", want: transport{compress: "zstd"}},
		{name: "lz4", compress: "lz4", want: transport{compress: "lz4"}},
		{name: "zlib", compress: "zlib", want: transport{compress: "zlib"}},
		{name: "padded and mixed case", compress: "  ZSTD ", want: transport{compress: "zstd"}},
		{name: "unrecognized falls back to off", compress: "brotli", want: transport{}},
		{name: "acls", acls: "true", want: transport{acls: true}},
		{name: "xattrs", xattrs: "true", want: transport{xattrs: true}},
		{
			name: "all three",
			acls: "true", xattrs: "true", compress: "lz4",
			want: transport{compress: "lz4", acls: true, xattrs: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SYNC_ACLS", tt.acls)
			t.Setenv("SYNC_XATTRS", tt.xattrs)
			t.Setenv("SYNC_COMPRESS", tt.compress)

			if got := loadTransport(hostKeyAcceptNew); got != tt.want {
				t.Errorf("loadTransport() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestLoadTransport_UnrecognizedCompressionWarns(t *testing.T) {
	rec := capture.Default(t)
	t.Setenv("SYNC_ACLS", "")
	t.Setenv("SYNC_XATTRS", "")
	t.Setenv("SYNC_COMPRESS", "brotli")

	got := loadTransport(hostKeyAcceptNew)

	if got.compress != "" {
		t.Errorf("loadTransport() compress = %q, want off", got.compress)
	}
	const message = "unrecognized SYNC_COMPRESS, compression stays off"
	if count := rec.CountLevel(slog.LevelWarn, message); count != 1 {
		t.Errorf("%q WARN records = %d, want 1; logs = %q", message, count, rec.Messages())
	}
	if !rec.HasAttr(message, "value", "brotli") {
		t.Errorf("%q missing value=brotli; logs = %q", message, rec.Messages())
	}
	if !rec.HasAttr(message, "accepted", "off|on|auto|zstd|lz4|zlib") {
		t.Errorf("%q missing accepted choices; logs = %q", message, rec.Messages())
	}
}

// TestLoadTransport_carriesTheHostKeyPosture pins the field the reshape absorbed:
// the boot-decided posture is threaded through the transport, so the -e argument
// still pins when a known_hosts file is mounted.
func TestLoadTransport_carriesTheHostKeyPosture(t *testing.T) {
	t.Setenv("SYNC_COMPRESS", "")
	if got := loadTransport(hostKeyStrict); got.hostKeys != hostKeyStrict {
		t.Errorf("loadTransport(strict).hostKeys = %v, want strict", got.hostKeys)
	}
}
