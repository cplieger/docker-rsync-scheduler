// Package main runs a scheduled rsync-over-ssh daemon that pushes local directories to a remote host.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// --- Configuration ---

// config is the top-level YAML document: a list of one-way sync jobs.
// Interval and ScheduleEnabled are populated from SYNC_INTERVAL after the
// YAML is parsed (hence the yaml:"-" tags); they are not part of the
// on-disk schema. Fields are ordered largest-first for fieldalignment.
type config struct {
	Jobs []job `yaml:"jobs"`

	// Interval is the built-in scheduler cadence (the startup pass fires
	// immediately, then every Interval). Only consulted when
	// ScheduleEnabled is true.
	Interval time.Duration `yaml:"-"`

	// ScheduleEnabled reports whether the built-in interval scheduler runs.
	// When false (SYNC_INTERVAL=off/disabled/0), the container idles and
	// syncs are triggered out-of-band via the `sync` subcommand (e.g. an
	// external scheduler such as Ofelia running `docker exec`).
	ScheduleEnabled bool `yaml:"-"`
}

// job describes a single rsync-over-ssh push of a local directory to a
// remote host. Fields are ordered largest-first to satisfy the
// fieldalignment vet check.
type job struct {
	RemoteUID  *int     `yaml:"remote_uid"`
	RemoteGID  *int     `yaml:"remote_gid"`
	Name       string   `yaml:"name"`
	Local      string   `yaml:"local"`
	RemoteHost string   `yaml:"remote_host"`
	RemotePath string   `yaml:"remote_path"`
	SSHKey     string   `yaml:"ssh_key"`
	Excludes   []string `yaml:"excludes"`
	Delete     bool     `yaml:"delete"`
}

const (
	// defaultConfigPath is where the YAML config is mounted by default.
	// Override with the CONFIG_PATH environment variable.
	defaultConfigPath = "/config/config.yaml"

	// configCapBytes bounds the config read so a runaway mount cannot OOM
	// the container during startup. A sync config is a few KB at most.
	configCapBytes = 1 << 20 // 1 MB

	// defaultSyncTimeout caps a single job's rsync invocation. Override
	// with the SYNC_TIMEOUT environment variable (a Go duration).
	defaultSyncTimeout = 10 * time.Minute

	// defaultInterval is the fallback built-in scheduler cadence when
	// SYNC_INTERVAL is unset or unparseable (non-sentinel). Six hours keeps
	// mirrors fresh without thrashing a slow remote.
	defaultInterval = 6 * time.Hour

	// lockFilePath guards against overlapping sync passes. flock(2) on this
	// file serialises runs both in-process (the built-in ticker racing the
	// startup pass) and cross-process (an external `sync` invocation racing
	// the built-in ticker or a manual docker exec). /tmp is writable by the
	// root-by-design container, same place as the health marker.
	lockFilePath = "/tmp/.docker-rsync-scheduler.lock"
)

// remoteHostRE accepts an optional [user@] prefix followed by a host that
// may contain IPv6-style colons, but rejects shell metacharacters and
// whitespace by construction.
var remoteHostRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*@)?[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// shellMetaChars are characters that enable command injection or argument
// splitting in a shell. Jobs are executed with an explicit argument slice
// (no shell), so these can never be interpreted; rejecting them is
// defense-in-depth. Glob characters (* ? [ ]) are deliberately absent so
// rsync exclude patterns remain expressible.
const shellMetaChars = ";|&$`<>(){}\\\"'"

// hasShellMeta reports whether s contains a shell metacharacter or any
// control character (newline, carriage return, tab, NUL, etc.).
func hasShellMeta(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
		if strings.ContainsRune(shellMetaChars, r) {
			return true
		}
	}
	return false
}

// setupLogger installs a slog text handler that emits canonical logfmt
// (`time=... level=... msg=... k=v`) to stderr for Loki/Alloy collection.
func setupLogger() {
	levelStr := strings.ToLower(strings.TrimSpace(getEnv("LOG_LEVEL", "info")))
	level := slog.LevelInfo
	unknown := false
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		unknown = true
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
	if unknown {
		slog.Warn("unrecognized LOG_LEVEL, using info", "value", levelStr)
	}
}

// configPath returns the active config path, honouring CONFIG_PATH.
func configPath() string {
	return getEnv("CONFIG_PATH", defaultConfigPath)
}

// loadConfig reads, parses, and validates the YAML config. On any
// failure it logs a structured error and returns it so the caller
// (daemon/sync) can exit non-zero.
func loadConfig() (config, error) {
	path := configPath()

	info, statErr := os.Stat(path)
	if statErr != nil {
		slog.Error("config not found", "path", path, "error", statErr,
			"hint", "mount a config.yaml at this path — see config.example.yaml in the repo")
		return config{}, fmt.Errorf("stat config %q: %w", path, statErr)
	}
	if info.Size() > configCapBytes {
		slog.Error("config too large", "path", path, "size", info.Size(), "cap", configCapBytes)
		return config{}, fmt.Errorf("config %q exceeds %d bytes", path, configCapBytes)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- trusted, operator-mounted config path
	if err != nil {
		slog.Error("config read failed", "path", path, "error", err)
		return config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	cfg, err := parseConfig(data)
	if err != nil {
		slog.Error("config parse failed", "path", path, "error", err)
		return config{}, err
	}

	if err := cfg.validate(); err != nil {
		slog.Error("config validation failed", "path", path, "error", err)
		return config{}, err
	}

	// SYNC_INTERVAL selects the scheduling mode and built-in cadence. It is
	// read after parse+validate because it comes from the environment, not
	// the YAML document.
	cfg.Interval, cfg.ScheduleEnabled = loadInterval()

	return cfg, nil
}

// parseConfig unmarshals raw YAML into a config without validating it.
// Kept separate from validate so fuzz/property tests can exercise the
// parser on arbitrary bytes without needing a real ssh key on disk.
func parseConfig(data []byte) (config, error) {
	var cfg config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// validate enforces the config contract: a non-empty job list with unique
// names, delegating each job's per-field contract to (job).validate.
func (c config) validate() error {
	if len(c.Jobs) == 0 {
		return errors.New("config: jobs list is empty")
	}

	seen := make(map[string]bool, len(c.Jobs))
	for i := range c.Jobs {
		j := &c.Jobs[i]

		if j.Name == "" {
			return fmt.Errorf("job %d: name is required", i)
		}
		if seen[j.Name] {
			return fmt.Errorf("job %q: duplicate name", j.Name)
		}
		seen[j.Name] = true

		if err := j.validate(); err != nil {
			return err
		}
	}

	return nil
}

// validate enforces one job's field contract: required fields present,
// absolute local/remote paths, a sane remote host, no injection characters
// anywhere, and a readable ssh key. Name presence and cross-job uniqueness
// are enforced by (config).validate.
func (j *job) validate() error {
	if j.Local == "" {
		return fmt.Errorf("job %q: local is required", j.Name)
	}
	if j.RemoteHost == "" {
		return fmt.Errorf("job %q: remote_host is required", j.Name)
	}
	if j.RemotePath == "" {
		return fmt.Errorf("job %q: remote_path is required", j.Name)
	}
	if j.SSHKey == "" {
		return fmt.Errorf("job %q: ssh_key is required", j.Name)
	}

	if !filepath.IsAbs(j.Local) {
		return fmt.Errorf("job %q: local %q must be absolute", j.Name, j.Local)
	}
	if !filepath.IsAbs(j.RemotePath) {
		return fmt.Errorf("job %q: remote_path %q must be absolute", j.Name, j.RemotePath)
	}
	if !remoteHostRE.MatchString(j.RemoteHost) {
		return fmt.Errorf("job %q: remote_host %q is invalid", j.Name, j.RemoteHost)
	}

	for _, f := range []struct{ key, val string }{
		{"name", j.Name},
		{"local", j.Local},
		{"remote_host", j.RemoteHost},
		{"remote_path", j.RemotePath},
		{"ssh_key", j.SSHKey},
	} {
		if hasShellMeta(f.val) {
			return fmt.Errorf("job %q: %s contains forbidden characters", j.Name, f.key)
		}
	}
	for _, e := range j.Excludes {
		if hasShellMeta(e) {
			return fmt.Errorf("job %q: exclude %q contains forbidden characters", j.Name, e)
		}
	}

	// ssh_key is embedded in the word-split `-e "ssh -i <key> ..."` string
	// (sshCommand); a space would split it into separate argv elements and
	// break the job. hasShellMeta deliberately allows spaces for path
	// fields, so the key path needs this stricter check of its own.
	if strings.ContainsRune(j.SSHKey, ' ') {
		return fmt.Errorf("job %q: ssh_key %q must not contain spaces",
			j.Name, j.SSHKey)
	}

	if err := checkReadable(j.SSHKey); err != nil {
		return fmt.Errorf("job %q: ssh_key %q not readable: %w", j.Name, j.SSHKey, err)
	}

	return nil
}

// checkReadable confirms a file exists and can be opened for reading.
func checkReadable(path string) error {
	f, err := os.Open(path) // #nosec G304 -- trusted, operator-mounted key path
	if err != nil {
		return err
	}
	return f.Close()
}

// loadSyncTimeout reads SYNC_TIMEOUT (a Go duration) and falls back to
// defaultSyncTimeout on unset or unparseable values, logging a warning
// rather than refusing to start.
func loadSyncTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("SYNC_TIMEOUT"))
	if raw == "" {
		return defaultSyncTimeout
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		slog.Warn("cannot parse SYNC_TIMEOUT, using default",
			"value", raw, "default", defaultSyncTimeout)
		return defaultSyncTimeout
	case d <= 0:
		slog.Warn("SYNC_TIMEOUT must be positive, using default",
			"value", raw, "default", defaultSyncTimeout)
		return defaultSyncTimeout
	}
	return d
}

// loadInterval parses SYNC_INTERVAL and reports the built-in scheduler
// cadence and whether the built-in scheduler runs at all. SYNC_INTERVAL is
// a Go duration ("1h", "30m") that sets the interval. The sentinels "off"
// and "disabled" (case-insensitive) or any zero duration ("0", "0s")
// disable the built-in scheduler: the container idles and syncs are
// triggered out-of-band via the `sync` subcommand. Unset defaults to
// defaultInterval with the scheduler enabled. Any other parse failure
// falls back to defaultInterval and logs a warning rather than refusing to
// start, keeping the container syncing on a reasonable cadence even with a
// malformed env block.
func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	interval = defaultInterval
	scheduleEnabled = true
	raw := strings.TrimSpace(os.Getenv("SYNC_INTERVAL"))
	if raw == "" {
		return interval, scheduleEnabled
	}
	switch strings.ToLower(raw) {
	case "off", "disabled":
		scheduleEnabled = false
	default:
		d, perr := time.ParseDuration(raw)
		switch {
		case perr != nil:
			slog.Warn("cannot parse SYNC_INTERVAL, using default",
				"value", raw, "default", defaultInterval)
		case d > 0:
			interval = d
		case d == 0:
			// Zero duration ("0", "0s") disables built-in scheduling.
			scheduleEnabled = false
		default:
			// A negative duration is not a valid interval and not a
			// documented disable sentinel; warn and fall back to default.
			slog.Warn("SYNC_INTERVAL is negative, using default",
				"value", raw, "default", defaultInterval)
		}
	}
	return interval, scheduleEnabled
}

// getEnv returns the environment value for key, or fallback when unset
// or empty.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
