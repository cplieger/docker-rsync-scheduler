package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		RemoteHost: "root@192.168.1.87",
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
				RemoteHost: "root@192.168.1.87",
				RemotePath: "/srv/containers/caddy",
				SSHKey:     key,
				RemoteUID:  new(1000),
				RemoteGID:  new(1000),
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
			name: "dangerous char in local",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a;rm", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
			}}},
			wantErr: "forbidden characters",
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
			wantErr: "forbidden characters",
		},
		{
			name: "dangerous char in exclude",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b", SSHKey: key,
				Excludes: []string{"good", "bad;evil"},
			}}},
			wantErr: "forbidden characters",
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
			name: "ssh_key missing file",
			cfg: config{Jobs: []job{{
				Name: "j", Local: "/a", RemoteHost: "host",
				RemotePath: "/b", SSHKey: "/nonexistent/key",
			}}},
			wantErr: "not readable",
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

func TestValidate_sshKeyWithSpaceRejected(t *testing.T) {
	cfg := config{Jobs: []job{{
		Name:       "spaced",
		Local:      "/sources/spaced",
		RemoteHost: "root@192.168.1.87",
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

func TestHasShellMeta(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"plain path", "/sources/caddy", false},
		{"user at host", "root@192.168.1.87", false},
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
    remote_host: root@192.168.1.87
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

func TestLoadConfigEndToEnd(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(key, []byte("k\n"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	doc := "jobs:\n  - name: caddy\n    local: /sources/caddy\n" +
		"    remote_host: root@host\n    remote_path: /srv/caddy\n" +
		"    ssh_key: " + key + "\n"
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "caddy" {
		t.Errorf("loadConfig = %+v, want one caddy job", cfg.Jobs)
	}
	// With SYNC_INTERVAL unset the built-in scheduler is enabled at the
	// default cadence.
	t.Setenv("SYNC_INTERVAL", "")
	cfg, err = loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.ScheduleEnabled {
		t.Error("ScheduleEnabled = false, want true by default")
	}
	if cfg.Interval != defaultInterval {
		t.Errorf("Interval = %v, want default %v", cfg.Interval, defaultInterval)
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

func TestLoadConfig_parseErrorMsg(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("jobs: [unterminated"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig with malformed YAML = nil, want error")
	}
	if !strings.Contains(err.Error(), "parse config") {
		t.Errorf("loadConfig error = %q, want to contain 'parse config'", err)
	}
}

func TestLoadConfig_validateErrorMsg(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("jobs:\n  - name: x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("CONFIG_PATH", cfgPath)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig with invalid config = nil, want error")
	}
	if !strings.Contains(err.Error(), "local is required") {
		t.Errorf("loadConfig error = %q, want to contain 'local is required'", err)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "absent.yaml"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig on missing file: want error")
	}
}

func TestCheckReadable(t *testing.T) {
	t.Run("readable file returns nil", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "key")
		if err := os.WriteFile(path, []byte("k"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}

		if err := checkReadable(path); err != nil {
			t.Errorf("checkReadable(%q) = %v, want nil", path, err)
		}
	})

	// A missing file must surface a genuine not-exist error. If the err != nil
	// guard is negated, the function skips the early return and instead closes
	// a nil *os.File, which reports os.ErrInvalid ("invalid argument") rather
	// than the real not-exist cause — a regression this assertion catches.
	t.Run("missing file returns a not-exist error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent")

		err := checkReadable(path)

		if err == nil {
			t.Fatalf("checkReadable(%q) = nil, want error", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("checkReadable(%q) = %v, want an os.ErrNotExist", path, err)
		}
	})
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

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig at exactly cap (%d bytes) = %v, want success", configCapBytes, err)
	}
	if len(cfg.Jobs) != 1 || cfg.Jobs[0].Name != "caddy" {
		t.Errorf("loadConfig = %+v, want one caddy job", cfg.Jobs)
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

	_, err := loadConfig()

	if err == nil {
		t.Fatal("loadConfig one byte over cap = nil, want error")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("loadConfig over cap error = %q, want to contain 'exceeds'", err)
	}
}

func TestLoadSyncTimeout(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv("SYNC_TIMEOUT", "")
		if got := loadSyncTimeout(); got != defaultSyncTimeout {
			t.Errorf("loadSyncTimeout() = %v, want %v", got, defaultSyncTimeout)
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

func TestGetEnv(t *testing.T) {
	t.Setenv("TEST_RSYNC_ENV", "value")
	if got := getEnv("TEST_RSYNC_ENV", "fallback"); got != "value" {
		t.Errorf("getEnv = %q, want value", got)
	}
	t.Setenv("TEST_RSYNC_ENV", "")
	if got := getEnv("TEST_RSYNC_ENV", "fallback"); got != "fallback" {
		t.Errorf("getEnv = %q, want fallback", got)
	}
}
