// Package main runs a scheduled rsync-over-ssh daemon that pushes local directories to a remote host.
package main

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/envx/yamlenv/v2"
	"github.com/cplieger/pathinside/v2"
	"github.com/cplieger/scheduler/v4"
	"github.com/cplieger/slogx"
	"go.yaml.in/yaml/v3"
)

// --- Configuration ---

// config is the top-level YAML document: a list of one-way sync jobs.
type config struct {
	Jobs []job `yaml:"jobs"`
}

// job describes a single rsync-over-ssh push of a local directory to a
// remote host.
type job struct {
	RemoteUID  *uint32  `yaml:"remote_uid"`
	RemoteGID  *uint32  `yaml:"remote_gid"`
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

	// socketPath is the daemon's trigger socket. The `sync` subcommand dials
	// it to submit a pass request; the daemon — the single owner of pass
	// execution — serves requests from its queue in order. /tmp is writable
	// by the root-by-design container (same place as the health marker), the
	// socket file is owner-only (0600), and nothing listens on any network
	// port — trigger authority is scoped to the container's own user, the
	// same boundary `docker exec` already enforces.
	socketPath = "/tmp/docker-rsync-scheduler.sock"
)

// userRE matches the optional login name before '@' in a remote_host.
var userRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hostnameRE matches a DNS hostname (alphanumerics, dots, underscores, and
// hyphens). IPv4/IPv6 literals are validated separately via net.ParseIP, so a
// colon is absent here: a colon in a non-literal host is rsync's daemon-mode
// "::" hazard.
var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// shellMetaChars are the characters a shell reads as separators or
// substitutions. Only remote_path is held to them: it becomes rsync's
// destination argument, parsed by the REMOTE login shell. rsync escapes
// these by default, so this refusal is insurance against a layer this app
// does not own. Globs (* ? [ ]) are absent because rsync never escapes them
// — remote_path is refused those separately below.
const shellMetaChars = ";|&$`<>(){}\\\"'"

// hasControl reports whether s contains an ASCII control character (C0 or
// DEL). NUL cannot be represented in an argv element; a newline in an
// exclude makes rsync read the rule as one unmatchable pattern, excluding
// nothing (so --delete pushes the file meant to stay); DEL reaches the log
// stream unescaped. C1 (U+0080-U+009F) is out: those decode from a quoted
// YAML scalar as ordinary text.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// hasShellMeta reports whether s contains a shell metacharacter or an
// ASCII control character. remote_path is the only field held to it.
func hasShellMeta(s string) bool {
	return hasControl(s) || strings.ContainsAny(s, shellMetaChars)
}

// setupLogger installs a slog text handler that emits canonical logfmt to
// stderr for Loki/Alloy collection.
func setupLogger() {
	raw := envx.String("LOG_LEVEL")
	var level slog.Level // LevelInfo is the zero value
	recognized := true
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info": // unset and "info" both take the zero value
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		recognized = false
	}
	slogx.Setup(slogx.Options{Level: level})
	if !recognized {
		slog.Warn("unrecognized LOG_LEVEL, using info", "value", raw)
	}
}

// configPath returns the active config path, honouring CONFIG_PATH.
func configPath() string {
	return cmp.Or(envx.String("CONFIG_PATH"), defaultConfigPath)
}

// loadedConfig is a validated config plus its source document's digest.
type loadedConfig struct {
	cfg    config
	digest [32]byte
}

// loadConfig reads, parses, and validates the YAML config. The stage is
// non-empty only on failure so callers can attach it to their diagnostic.
func loadConfig() (loadedConfig, string, error) {
	path := configPath()

	data, err := readCappedConfig(path)
	if err != nil {
		return loadedConfig{}, "read", err
	}

	cfg, err := parseConfig(data)
	if err != nil {
		return loadedConfig{}, "parse", err
	}

	if err := cfg.validate(); err != nil {
		return loadedConfig{}, "validate", err
	}

	return loadedConfig{cfg: cfg, digest: sha256.Sum256(data)}, "", nil
}

// readCappedConfig reads path through openRegular, refusing any document over
// configCapBytes. The bound is enforced on the bytes read, since a regular
// file can grow after it is opened; the +1 makes "exceeds" decidable.
func readCappedConfig(path string) ([]byte, error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, configCapBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > configCapBytes {
		return nil, fmt.Errorf("config exceeds %d bytes", configCapBytes)
	}
	return data, nil
}

// parseConfig unmarshals raw YAML into a config without validating it. Kept
// separate from validate so fuzz and property tests can drive the parser on
// arbitrary bytes with no ssh key on disk. yamlenv's CheckSingleDocument and
// CheckUnknownKeys read the raw bytes first, since this app expands nothing.
func parseConfig(data []byte) (config, error) {
	if err := yamlenv.CheckSingleDocument(data); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := yamlenv.CheckUnknownKeys(data, &config{}); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
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

// adviseConfig emits inert-setting and shared-destination advisories.
func adviseConfig(cfg config) {
	for i := range cfg.Jobs {
		cfg.Jobs[i].warnInertSettings()
	}
	warnSharedDestinations(cfg.Jobs)
}

// destJob is one job's contribution to the shared-destination advisory: its
// name, its cleaned remote path, the ssh key reported alongside it, and whether
// it deletes.
type destJob struct {
	name string
	raw  string
	path string
	key  string
	del  bool
}

// warnSharedDestinations warns when jobs address one remote tree and any of
// them deletes: --delete then removes whatever the others put there, every
// pass, and the per-job empty-source guard cannot see it since the deletion
// is not per-job. Advisory, not an error: an exclude rule can protect the
// receiver, so an overlapping set can be correct. Identity is host+path: IP
// literals compare as parsed addresses, multi-label DNS names are
// case-folded and normalized across a trailing root dot, and other names use
// their raw case-folded spelling. Ssh keys are reported rather than
// compared, since a path-rooting wrapper on one is what can make a flagged
// set safe.
func warnSharedDestinations(jobs []job) {
	groups := make(map[string][]destJob, len(jobs))
	for i := range jobs {
		j := &jobs[i]
		_, host := splitRemoteHost(j.RemoteHost)
		key := strings.ToLower(host)
		if ip := net.ParseIP(host); ip != nil {
			key = ip.String() // one endpoint, one group: 2001:db8::1 == 2001:0db8:0:0:0:0:0:1
		} else if trimmed := strings.TrimSuffix(key, "."); strings.Contains(trimmed, ".") {
			// At the resolver's default ndots:1 a single label is absolute with the
			// dot and search-list-expanded without it, so only a multi-label name is
			// the same host either way (resolv.conf(5), ndots).
			key = trimmed
		}
		groups[key] = append(groups[key], destJob{
			name: j.Name,
			raw:  j.RemoteHost,
			path: filepath.Clean(j.RemotePath),
			key:  j.SSHKey,
			del:  j.Delete,
		})
	}
	for _, group := range slices.Sorted(maps.Keys(groups)) {
		for _, component := range overlapComponents(groups[group]) {
			warnConflictingTree(component)
		}
	}
}

// overlapComponents partitions one host's jobs into sets closed under "equal
// or nested". It grows each set pairwise rather than a sorted single pass,
// because a sibling prefix sorts between an ancestor and its descendant, so
// {/data, /data-old, /data/x} would hide that /data contains /data/x.
func overlapComponents(group []destJob) [][]destJob {
	var components [][]destJob
	taken := make([]bool, len(group))
	for i := range group {
		if taken[i] {
			continue
		}
		component := []destJob{group[i]}
		taken[i] = true
		for grew := true; grew; {
			grew = false
			for k := range group {
				if taken[k] || !overlapsAny(component, group[k].path) {
					continue
				}
				component = append(component, group[k])
				taken[k] = true
				grew = true
			}
		}
		components = append(components, component)
	}
	return components
}

// overlapsAny reports whether path is the same tree as any member's path, or is
// nested under it, or contains it. Root.Contains admits the root itself, which
// is the answer this predicate wants: equality is overlap.
func overlapsAny(members []destJob, path string) bool {
	for _, m := range members {
		if pathinside.Root(m.path).Contains(path) ||
			pathinside.Root(path).Contains(m.path) {
			return true
		}
	}
	return false
}

// warnConflictingTree emits one advisory per overlap component, listing only
// members whose tree overlaps a deleting job's -- a sibling connected only
// through a non-deleting ancestor is excluded. Deleters and members are
// sampled shortest-path-first and capped at three, so a thousand mutually
// overlapping jobs still cost one bounded record.
func warnConflictingTree(members []destJob) {
	const sampleSize = 3
	if len(members) < 2 {
		return
	}
	if !slices.ContainsFunc(members, func(m destJob) bool { return m.del }) {
		return
	}
	deleting := make([]destJob, 0, len(members))
	for _, m := range members {
		if m.del {
			deleting = append(deleting, m)
		}
	}
	atRisk := make([]destJob, 0, len(members))
	for _, m := range members {
		if !overlapsAny(deleting, m.path) {
			continue
		}
		atRisk = append(atRisk, m)
	}
	slices.SortFunc(atRisk, func(a, b destJob) int {
		return cmp.Or(cmp.Compare(len(a.path), len(b.path)), cmp.Compare(a.path, b.path))
	})
	deleters := make([]string, 0, sampleSize)
	for _, m := range atRisk {
		if len(deleters) == sampleSize {
			break
		}
		if m.del {
			deleters = append(deleters, m.name)
		}
	}
	samples := make([]string, 0, sampleSize)
	for _, m := range atRisk[:min(sampleSize, len(atRisk))] {
		samples = append(samples,
			fmt.Sprintf("%s at %s (ssh_key %s, delete %t)", m.name, m.path, m.key, m.del))
	}
	slog.Warn("jobs share a remote destination tree and one deletes; each pass may delete the others' files",
		"remote_host", atRisk[0].raw, "jobs", len(atRisk), "deleting", len(deleting),
		"outermost_first", strings.Join(samples, "; "),
		"deletes", strings.Join(deleters, ", "),
		"hint", "exclude the sibling's content from the deleting job, or give each job its own remote_path")
}

// validate enforces one job's field contract: required fields present,
// absolute local/remote paths, a sane remote host, no injection characters,
// and a readable ssh key file. Name presence and cross-job uniqueness are
// enforced by (config).validate.
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

	// 0 already means "allow no deletions" (rsync applies -1 the same way),
	// so the config publishes one spelling; unset leaves the pass uncapped.
	if j.MaxDelete != nil && *j.MaxDelete < 0 {
		return fmt.Errorf("job %q: max_delete must be >= 0", j.Name)
	}

	if err := j.checkOwnership(); err != nil {
		return err
	}

	return nil
}

// checkRequiredFields owns the "<field> is required" message for the four
// always-required string fields. (Name presence is enforced by
// (config).validate, which holds the job index for its message.)
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

// checkNoForbiddenChars applies one refusal per interpreter a value
// actually crosses: ASCII controls on every field, shell metacharacters
// and spaces on remote_path -- which the REMOTE shell parses, and which
// rsync escapes by default, so both refusals cover the premise
// shellMetaChars names -- and spaces on ssh_key, which rsync word-splits
// itself.
func (j *job) checkNoForbiddenChars() error {
	if hasShellMeta(j.RemotePath) {
		return fmt.Errorf("job %q: remote_path contains forbidden characters", j.Name)
	}
	for _, f := range []struct{ key, val string }{
		{"name", j.Name},
		{"local", j.Local},
		{"ssh_key", j.SSHKey},
	} {
		if hasControl(f.val) {
			return fmt.Errorf("job %q: %s contains control characters", j.Name, f.key)
		}
	}
	for _, e := range j.Excludes {
		if hasControl(e) {
			return fmt.Errorf("job %q: exclude %q contains control characters", j.Name, e)
		}
	}

	if strings.ContainsRune(j.RemotePath, ' ') {
		return fmt.Errorf("job %q: remote_path %q must not contain spaces",
			j.Name, j.RemotePath)
	}
	// rsync escapes metacharacters in the remote arg but never *?[], which the
	// receiver expands -- a pattern-shaped path makes --delete hit another tree.
	if strings.ContainsAny(j.RemotePath, "*?[]") {
		return fmt.Errorf("job %q: remote_path %q must not contain glob characters (*?[])",
			j.Name, j.RemotePath)
	}

	// sshCommand embeds this path in rsync's single -e string, which rsync
	// word-splits itself: a space ends the argument and a quote opens one
	// rsync never closes ("Missing trailing-' in remote-shell command").
	if strings.ContainsAny(j.SSHKey, " '\"") {
		return fmt.Errorf("job %q: ssh_key %q must not contain spaces or quotes",
			j.Name, j.SSHKey)
	}
	return nil
}

// checkOwnership refuses the one uint32 chown(2) reads as "leave the id
// unchanged", -1. rsync refuses it too ("uid 4294967295 (-1) is impossible to
// set", exit 23), so accepting it only moves the failure into the middle of a
// transfer.
func (j *job) checkOwnership() error {
	for _, f := range []struct {
		key string
		val *uint32
	}{
		{"remote_uid", j.RemoteUID},
		{"remote_gid", j.RemoteGID},
	} {
		if f.val != nil && *f.val == math.MaxUint32 {
			return fmt.Errorf(
				"job %q: %s %d means \"leave unchanged\" to chown (-1 as an unsigned id), "+
					"which rsync refuses; omit the field instead", j.Name, f.key, *f.val)
		}
	}
	return nil
}

// warnInertSettings logs accepted job settings that are inert, or whose
// spelling rsync reads more literally than the operator likely intended.
func (j *job) warnInertSettings() {
	// max_delete only takes effect with delete:true -- buildRsyncArgs emits
	// --max-delete inside the --delete branch.
	if j.MaxDelete != nil && !j.Delete {
		slog.Warn("max_delete set without delete:true; the cap will be ignored",
			"job", j.Name)
	}

	// buildRsyncArgs emits --chown only when BOTH remote_uid and remote_gid
	// are set, so a lone uid or gid is silently dropped.
	if (j.RemoteUID == nil) != (j.RemoteGID == nil) {
		slog.Warn("remote_uid/remote_gid set without its pair; --chown will be skipped",
			"job", j.Name,
			"remote_uid_set", j.RemoteUID != nil,
			"remote_gid_set", j.RemoteGID != nil)
	}

	if slices.Contains(j.Excludes, "") {
		slog.Warn("empty excludes entry is ignored; an empty pattern excludes nothing",
			"job", j.Name)
	}

	// rsync matches surrounding whitespace literally, but a path may
	// legitimately contain it, so advise rather than refuse.
	for _, e := range j.Excludes {
		trimmed := strings.TrimSpace(e)
		if trimmed == e {
			continue
		}
		slog.Warn("exclude pattern has leading or trailing whitespace; rsync matches it literally",
			"job", j.Name, "exclude", e, "without_whitespace", trimmed)
	}
}

// splitRemoteHost separates an optional "user@" prefix from the host and
// strips the surrounding brackets from an IP literal written in rsync's
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
// prefix followed by a DNS name or an IPv4/IPv6 literal, bare or bracketed. A
// colon in a non-IP host is rejected because rsync would read it as its
// daemon-mode separator; remoteDest adds the IPv6 brackets.
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

// checkReadable confirms path names a regular file that can be opened for
// reading. A directory or device opens successfully and can never be a
// private key.
func checkReadable(path string) error {
	f, err := openRegular(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return nil
}

// errNotRegular is returned unwrapped by openRegular so callers can branch on
// it with errors.Is.
var errNotRegular = errors.New("not a regular file")

// openRegular opens path for reading and refuses anything but a regular file
// with errNotRegular. The nonblocking open is load-bearing: opening a FIFO
// without it waits for a writer that may never arrive.
func openRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- trusted, operator-mounted path
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// loadSyncTimeout reads SYNC_TIMEOUT (a Go duration) and falls back to
// defaultSyncTimeout on unset or unparseable values, logging a warning
// rather than refusing to start. The positive-only rule stays app-side: a
// zero or negative timeout would break every rsync context.
func loadSyncTimeout() time.Duration {
	d := envx.Duration("SYNC_TIMEOUT", defaultSyncTimeout)
	if d <= 0 {
		slog.Warn("SYNC_TIMEOUT must be positive, using default",
			"value", d.String(), "default", defaultSyncTimeout)
		return defaultSyncTimeout
	}
	return d
}

// loadTransport reads the opt-in rsync transport switches (SYNC_ACLS,
// SYNC_XATTRS, SYNC_COMPRESS) and combines them with the boot-decided
// host-key posture. Every switch defaults off, so an unset environment
// yields the shipped transport. envx.Bool warns on a malformed boolean;
// an unrecognized SYNC_COMPRESS value warns here and falls back to off.
func loadTransport(hostKeys hostKeyMode) transport {
	tr := transport{
		hostKeys: hostKeys,
		acls:     envx.Bool("SYNC_ACLS", false),
		xattrs:   envx.Bool("SYNC_XATTRS", false),
	}
	raw := envx.String("SYNC_COMPRESS")
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "", "off", "disabled", "no", "false", "0":
	case "on", "yes", "true", "1", "auto":
		tr.compress = "auto"
	case "zstd", "lz4", "zlib":
		tr.compress = v
	default:
		slog.Warn("unrecognized SYNC_COMPRESS, compression stays off",
			"value", raw,
			"accepted", "off|on|auto|zstd|lz4|zlib")
	}
	return tr
}

// loadInterval returns scheduler.ParseInterval's cadence and whether it chose
// built-in scheduling. The cadence is meaningful only when scheduleEnabled is
// true: every other mode carries defaultInterval for reference, so a caller
// that skips the check arms or publishes a cadence nothing observes.
func loadInterval() (interval time.Duration, scheduleEnabled bool) {
	s := scheduler.ParseInterval(envx.String("SYNC_INTERVAL"), defaultInterval,
		scheduler.WithName("SYNC_INTERVAL"))
	return s.Interval, s.Mode == scheduler.ModeBuiltin
}
