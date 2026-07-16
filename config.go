// Package main runs a scheduled rsync-over-ssh daemon that pushes local directories to a remote host.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/scheduler"
	"github.com/cplieger/slogx"
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
	MaxDelete  *int     `yaml:"max_delete"`
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

// userRE matches the optional login name before '@' in a remote_host.
var userRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hostnameRE matches a DNS hostname (alphanumerics, dots, underscores, and
// hyphens). IPv4/IPv6 literals are validated separately via net.ParseIP, so a
// colon is deliberately absent here: a colon in a non-literal host is the
// daemon-mode "::" hazard that rsync's host:path parser would misread.
var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	levelStr := strings.TrimSpace(envx.String("LOG_LEVEL", "info"))
	level, recognized := slogx.ParseLevel(levelStr, slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: level})
	if !recognized {
		slog.Warn("unrecognized LOG_LEVEL, using info", "value", levelStr)
	}
}

// configPath returns the active config path, honouring CONFIG_PATH.
func configPath() string {
	return envx.String("CONFIG_PATH", defaultConfigPath)
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
// are enforced by (config).validate. The per-concern checks live in helpers
// (checkRequiredFields, checkNoForbiddenChars) so this stays readable and
// under the complexity threshold.
func (j *job) validate() error {
	if err := j.checkRequiredFields(); err != nil {
		return err
	}

	if !filepath.IsAbs(j.Local) {
		return fmt.Errorf("job %q: local %q must be absolute", j.Name, j.Local)
	}
	if !filepath.IsAbs(j.RemotePath) {
		return fmt.Errorf("job %q: remote_path %q must be absolute", j.Name, j.RemotePath)
	}
	if err := validateRemoteHost(j); err != nil {
		return err
	}

	if err := j.checkNoForbiddenChars(); err != nil {
		return err
	}

	if err := checkReadable(j.SSHKey); err != nil {
		return fmt.Errorf("job %q: ssh_key %q not readable: %w", j.Name, j.SSHKey, err)
	}

	// max_delete, when set, caps how many deletions a single --delete pass may
	// perform (rsync --max-delete); a negative cap is meaningless. Unset leaves
	// the pass uncapped, preserving the prior behavior.
	if j.MaxDelete != nil && *j.MaxDelete < 0 {
		return fmt.Errorf("job %q: max_delete must be >= 0", j.Name)
	}

	j.warnInertSettings()

	return nil
}

// checkRequiredFields confirms the four always-required string fields are
// present. (Name presence is enforced by (config).validate, which holds the
// job index needed for its error message.)
func (j *job) checkRequiredFields() error {
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
	return nil
}

// checkNoForbiddenChars rejects shell metacharacters and control characters in
// every job string field and exclude pattern (defense-in-depth: jobs run with
// an explicit argument slice and no shell, so these can never be interpreted),
// then applies the stricter no-space rule to the two fields that are word-split
// downstream.
func (j *job) checkNoForbiddenChars() error {
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

	// remote_path is sent to the remote host as an rsync argument that the
	// remote login shell word-splits (rsync runs without --secluded-args/-s).
	// A space splits one path into several remote args -- the dest is then
	// wrong, and under --delete that can target the wrong remote tree.
	if strings.ContainsRune(j.RemotePath, ' ') {
		return fmt.Errorf("job %q: remote_path %q must not contain spaces",
			j.Name, j.RemotePath)
	}

	// ssh_key is embedded in the word-split `-e "ssh -i <key> ..."` string
	// (sshCommand); a space would split it into separate argv elements and
	// break the job. hasShellMeta deliberately allows spaces for path
	// fields, so the key path needs this stricter check of its own.
	if strings.ContainsRune(j.SSHKey, ' ') {
		return fmt.Errorf("job %q: ssh_key %q must not contain spaces",
			j.Name, j.SSHKey)
	}
	return nil
}

// warnInertSettings logs advisory warnings for job fields that are accepted
// but silently inert in the current configuration: a max_delete cap without
// delete:true, or a lone remote_uid/remote_gid. Neither is an error -- the job
// still runs -- so these stay out of validate's error path (and keep validate
// under the gocyclo complexity threshold).
func (j *job) warnInertSettings() {
	// max_delete only takes effect with delete:true -- buildRsyncArgs emits
	// --max-delete inside the --delete branch, so a cap set without delete is
	// silently inert. Warn so the operator notices, mirroring the
	// remote_uid/remote_gid pairing warning below.
	if j.MaxDelete != nil && !j.Delete {
		slog.Warn("max_delete set without delete:true; the cap will be ignored",
			"job", j.Name)
	}

	// buildRsyncArgs emits --chown only when BOTH remote_uid and remote_gid
	// are set, so a lone uid or gid is silently dropped and the remote keeps
	// the ssh user's ownership. Warn so the operator notices.
	if (j.RemoteUID == nil) != (j.RemoteGID == nil) {
		slog.Warn("remote_uid/remote_gid set without its pair; --chown will be skipped",
			"job", j.Name,
			"remote_uid_set", j.RemoteUID != nil,
			"remote_gid_set", j.RemoteGID != nil)
	}
}

// splitRemoteHost separates an optional "user@" prefix from the host and
// strips the surrounding brackets from an IPv6 literal written in rsync's
// [addr] form. It performs no validation; user is "" when no prefix is
// present. Brackets are stripped only when the inner text is a valid IP, so a
// stray "[name]" is left intact for validateRemoteHost to reject.
func splitRemoteHost(raw string) (user, host string) {
	host = raw
	if u, h, found := strings.Cut(raw, "@"); found {
		user, host = u, h
	}
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		if inner := host[1 : len(host)-1]; net.ParseIP(inner) != nil {
			host = inner
		}
	}
	return user, host
}

// validateRemoteHost enforces the remote_host contract: an optional "user@"
// prefix followed by either an IPv4/IPv6 literal or a DNS hostname. IPv6
// literals are accepted bare ("2001:db8::1") or bracketed ("[2001:db8::1]");
// the brackets rsync's host:path syntax needs are added automatically when the
// destination is built (see remoteDest). A bare host that merely contains a
// colon but is not a valid IP (e.g. "host:" or the incomplete "2001:db8") is
// rejected, because that colon would otherwise be misread by rsync as the
// daemon-mode "::" separator. Link-local IPv6 with a zone id ("fe80::1%eth0")
// is not supported; use a global/ULA address or an ssh_config Host alias.
func validateRemoteHost(j *job) error {
	user, host := splitRemoteHost(j.RemoteHost)
	if strings.Contains(j.RemoteHost, "@") && !userRE.MatchString(user) {
		return fmt.Errorf("job %q: remote_host %q has an invalid login prefix", j.Name, j.RemoteHost)
	}
	if net.ParseIP(host) != nil {
		return nil // a valid IPv4 or IPv6 literal
	}
	if !hostnameRE.MatchString(host) {
		return fmt.Errorf("job %q: remote_host %q is not a valid hostname or IP address "+
			"(for an IPv6 literal use the bare address, e.g. 2001:db8::1)", j.Name, j.RemoteHost)
	}
	return nil
}

// remoteDest builds rsync's destination argument for a job:
// "[user@]host:/remote/path/". An IPv6-literal host is wrapped in brackets so
// rsync's host:path parser reads the address colons as part of the host rather
// than the daemon-mode "::" separator.
func remoteDest(j *job) string {
	user, host := splitRemoteHost(j.RemoteHost)
	// A colon in a validated host can only come from an IPv6 literal
	// (hostnameRE and IPv4 dotted-quads never contain one), including the
	// IPv4-mapped form ::ffff:192.0.2.1 that net.IP.To4 reports as IPv4.
	// Bracket on the colon so the validated host and the emitted dest agree.
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	if user != "" {
		host = user + "@" + host
	}
	return host + ":" + j.RemotePath + "/"
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
// defaultSyncTimeout on unset or unparseable values (envx.Duration warns on
// malformed input), logging a warning rather than refusing to start. The
// positive-only rule stays app-side: a zero or negative timeout would break
// every rsync context.
func loadSyncTimeout() time.Duration {
	d := envx.Duration("SYNC_TIMEOUT", defaultSyncTimeout)
	if d <= 0 {
		slog.Warn("SYNC_TIMEOUT must be positive, using default",
			"value", d.String(), "default", defaultSyncTimeout)
		return defaultSyncTimeout
	}
	return d
}

// loadInterval parses SYNC_INTERVAL and reports the built-in scheduler
// cadence and whether the built-in scheduler runs at all. It delegates to
// scheduler.ParseInterval, the fleet-standard *_INTERVAL parser: a Go
// duration ("1h", "30m") sets the interval; the sentinels "off"/"disabled"
// (case-insensitive) or a zero duration ("0", "0s") select external mode
// (the container idles and syncs are triggered out-of-band via the `sync`
// subcommand); unset, negative, or unparseable falls back to defaultInterval
// with the scheduler enabled (a warning is logged for the negative and
// unparseable cases). scheduleEnabled is true only in built-in mode.
func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	s := scheduler.ParseInterval(os.Getenv("SYNC_INTERVAL"), defaultInterval,
		scheduler.WithName("SYNC_INTERVAL"))
	return s.Interval, s.Mode == scheduler.ModeBuiltin
}
