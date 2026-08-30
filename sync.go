package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cplieger/scheduler/v4"
)

// --- rsync engine ---

const (
	// outputCapBytes bounds each captured rsync output stream (stdout for
	// --stats parsing, stderr for the failure tail) so a chatty or misbehaving
	// subprocess cannot OOM the container.
	outputCapBytes = 1 << 20 // 1 MB

	// logStderrTailBytes bounds the stderr tail attached to a failure log
	// line so a single failure cannot flood Loki.
	logStderrTailBytes = 2048

	// truncMarker prefixes a cut stderr tail. Deliberately louder than a bare
	// ellipsis so an operator can tell a cut record from output that ended
	// there.
	truncMarker = "...(truncated)..."

	// rsyncVanishedExit is rsync's code 24. Upstream classifies it as a warning
	// rather than an error (log.c: "VANISHED is not an error, only a warning") and
	// only reports it when nothing else failed (a transfer error overwrites it with
	// 23), so the transfer succeeded for every file that still existed.
	rsyncVanishedExit = 24
)

// globalExcludes are applied to every job in addition to the per-job
// excludes. They cover Syncthing metadata and OS junk files that should
// never be mirrored to a remote.
var globalExcludes = []string{".stfolder", ".stversions", ".DS_Store", "Thumbs.db"}

// globalExcludeSet is globalExcludes as a set for O(1) membership tests in
// sourceIsEmpty. Derived from globalExcludes so the two cannot drift.
var globalExcludeSet = func() map[string]bool {
	m := make(map[string]bool, len(globalExcludes))
	for _, e := range globalExcludes {
		m[e] = true
	}
	return m
}()

// defaultCommandRunner builds the rsync subprocess commands with graceful
// shutdown — SIGTERM on context cancellation, then a DefaultGrace (5s) window
// before os/exec escalates to SIGKILL — via the shared scheduler library
// (identical to the hand-rolled runner it replaces). The caller wires
// Stdout/Stderr on the returned command before Run.
var defaultCommandRunner = scheduler.NewCommandRunner(scheduler.DefaultGrace)

// syncStats holds the figures parsed from rsync --stats output.
type syncStats struct {
	files int64
	bytes int64
}

// jobResult captures the outcome of a single job for logging and health
// aggregation.
type jobResult struct {
	stderrTail  string
	files       int64
	bytes       int64
	duration    time.Duration
	exitCode    int
	skipped     bool
	success     bool
	interrupted bool
}

// knownHostsPath is the conventional location for a user-supplied
// known_hosts file. A regular file with at least one entry there (mounted
// read-only by the operator) switches ssh from TOFU (accept-new) to strict
// host-key pinning — a stronger security posture for environments where the
// remote host key is pre-known. classifyKnownHosts decides that once at boot.
const knownHostsPath = "/config/known_hosts"

// hostKeyMode is the ssh host-key verification posture, decided once at boot
// from the content of knownHostsPath and then carried through the pass.
type hostKeyMode int

const (
	// hostKeyAcceptNew is TOFU: no known_hosts is mounted, so ssh accepts and
	// records the first key it sees.
	hostKeyAcceptNew hostKeyMode = iota
	// hostKeyStrict pins every connection against the mounted known_hosts.
	hostKeyStrict
)

var _ fmt.Stringer = hostKeyAcceptNew

// String renders the posture for the startup banner's ssh_hostkey_mode
// attribute; the two spellings are an operator-visible log value.
func (m hostKeyMode) String() string {
	if m == hostKeyStrict {
		return "strict"
	}
	return "accept-new"
}

// classifyKnownHosts decides the host-key posture from what is mounted at
// path. Absent means accept-new (TOFU). A path that is not a regular file, or
// a file carrying no entries, can pin nothing: ssh then fails every pass with
// "Host key verification failed", so this returns an error naming the path and
// the daemon refuses to start rather than reporting pinning it does not have.
// Entries are counted, never parsed -- validating one would re-implement ssh's
// hostfile parser -- so a marker (@cert-authority, @revoked) or hashed (|1|)
// line counts.
func classifyKnownHosts(path string) (hostKeyMode, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return hostKeyAcceptNew, nil
	case err != nil:
		return hostKeyAcceptNew, fmt.Errorf("known_hosts %q: %w", path, err)
	case !info.Mode().IsRegular():
		return hostKeyAcceptNew, fmt.Errorf("known_hosts %q is not a regular file", path)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- operator-mounted known_hosts path
	if err != nil {
		return hostKeyAcceptNew, fmt.Errorf("known_hosts %q: %w", path, err)
	}
	for line := range strings.Lines(string(data)) {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			return hostKeyStrict, nil
		}
	}
	return hostKeyAcceptNew, fmt.Errorf("known_hosts %q has no entries", path)
}

// sshCommand builds the single -e argument string. rsync splits it
// internally; there is no shell, so the key path (already validated to
// contain no whitespace or metacharacters) needs no quoting.
//
// Under hostKeyStrict the command pins against /config/known_hosts with
// StrictHostKeyChecking=yes; under accept-new it is TOFU, so a first run works
// without pre-provisioning host keys.
func sshCommand(key string, mode hostKeyMode) string {
	if mode == hostKeyStrict {
		return fmt.Sprintf(
			"ssh -i %s -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -o BatchMode=yes -o ConnectTimeout=10",
			key, knownHostsPath,
		)
	}
	return fmt.Sprintf(
		"ssh -i %s -o StrictHostKeyChecking=accept-new -o BatchMode=yes -o ConnectTimeout=10",
		key,
	)
}

// buildRsyncArgs assembles the explicit argument slice for a job. The
// archive-ish flag set is -rlptD (recurse, links, perms, times, devices/
// specials) minus owner/group/ACL/xattr, matching a logs-only one-way push.
func buildRsyncArgs(j *job, mode hostKeyMode) []string {
	args := []string{"-rlptD"}
	if j.Delete {
		args = append(args, "--delete")
		// --max-delete (when configured) caps the deletions a single pass may
		// perform: the documented backstop for a delete:true job whose per-job
		// excludes can match every top-level source entry. Unset -> uncapped.
		if j.MaxDelete != nil {
			args = append(args, fmt.Sprintf("--max-delete=%d", *j.MaxDelete))
		}
	}
	if j.RemoteUID != nil && j.RemoteGID != nil {
		args = append(args, fmt.Sprintf("--chown=%d:%d", *j.RemoteUID, *j.RemoteGID))
	}
	args = append(args, "--stats", "-e", sshCommand(j.SSHKey, mode))

	// Every pattern goes through --filter with an explicit "- " rule prefix rather
	// than --exclude: rsync reads an --exclude VALUE in old-prefix mode, where a
	// leading "- "/"+ " flips the rule sense and a value of exactly "!" clears every
	// rule accumulated so far, globals included. Emitting the prefix here keeps an
	// operator pattern a pattern.
	for _, e := range globalExcludes {
		args = append(args, "--filter=- "+e)
	}
	for _, e := range j.Excludes {
		if e == "" {
			continue // "--filter=- " with no pattern is an rsync syntax error
		}
		args = append(args, "--filter=- "+e)
	}

	// "--" terminates option parsing so the positional path args can never be
	// reinterpreted as rsync options, even if config validation is later relaxed.
	args = append(args, "--", j.Local+"/", remoteDest(j))
	return args
}

// sourceIsEmpty reports whether the local source directory has nothing worth
// mirroring: it is missing, unreadable, truly empty, or contains ONLY
// globally-excluded entries (.stfolder, .DS_Store, …). Such a source skips the
// job, because an empty sender under --delete deletes every non-excluded file on
// the receiver — and an excludes-only source would pass a naive "any entry
// present" check while transferring nothing. Any read error is treated as empty
// for the same reason. This is a pre-flight check, not a guarantee: rsync builds
// its own file list after the process starts, so a source that empties in that
// window still reaches --delete, and --max-delete is the only backstop inside the
// transfer.
//
// Only globalExcludes are considered here, not per-job excludes. The globals
// are exact filenames, so membership is exact and cheap; per-job excludes are
// rsync glob patterns whose matching semantics are not safely replicated with a
// simple name comparison (a wrong guess would falsely skip a real source). A
// source reduced to only per-job-excluded content is therefore still mirrored;
// bound that residual case with an rsync --max-delete backstop if needed.
func sourceIsEmpty(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- operator-mounted source path
	if err != nil {
		// A missing dir is the expected "not yet mounted / empty" case and
		// stays silent. Any other open error (permission denied, broken mount)
		// is surfaced -- still skip to protect the remote, but do not mask the
		// breakage as a benign empty source.
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("source unreadable, skipping to protect remote", "path", path, "error", err)
		}
		return true
	}
	defer func() { _ = f.Close() }()

	// Scan all top-level entries in batches, short-circuiting on the first
	// entry rsync would actually mirror (i.e. not in globalExcludeSet). io.EOF
	// is the normal end-of-directory signal; any other read error (I/O failure,
	// not-a-directory) is a broken source -- skip for safety but surface it.
	for {
		names, rerr := f.Readdirnames(256)
		for _, n := range names {
			if !globalExcludeSet[n] {
				return false // a mirrorable entry exists
			}
		}
		if errors.Is(rerr, io.EOF) {
			return true // only globally-excluded entries (or none)
		}
		if rerr != nil {
			slog.Warn("source unreadable, skipping to protect remote", "path", path, "error", rerr)
			return true
		}
	}
}

// statsRegexes match the relevant lines of rsync --stats output. Numbers
// may carry thousands separators, which parseNum strips.
var (
	reFilesXfer = regexp.MustCompile(`Number of regular files transferred:\s*([\d,]+)`)
	reBytesXfer = regexp.MustCompile(`Total transferred file size:\s*([\d,]+)`)
	reBytesSent = regexp.MustCompile(`sent\s+([\d,]+)\s+bytes`)
)

// parseStats extracts files-transferred and bytes-transferred from rsync
// --stats output. Missing or malformed values yield 0; parsing never
// fails, so a stats-format change degrades observability without failing
// an otherwise-successful sync.
func parseStats(out string) syncStats {
	var s syncStats
	if m := reFilesXfer.FindStringSubmatch(out); m != nil {
		s.files = parseNum(m[1])
	}
	if m := reBytesXfer.FindStringSubmatch(out); m != nil {
		s.bytes = parseNum(m[1])
	} else if m := reBytesSent.FindStringSubmatch(out); m != nil {
		s.bytes = parseNum(m[1])
	}
	return s
}

// parseNum parses a possibly comma-grouped integer, returning 0 on error.
func parseNum(s string) int64 {
	n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// exitCode extracts a process exit code from a run error: 0 for success,
// the real code for a non-zero exit, and -1 for failures that never
// produced an exit status (e.g. binary not found, context cancellation).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

// tail returns the last n bytes of s, prefixed to indicate truncation. The cut
// advances to the next rune start, so no continuation byte of a split rune
// survives at the head of the retained tail (which would render as
// unattributable noise: the rune it belonged to is gone). It re-encodes
// nothing, so a byte that was never part of a valid rune is returned as it
// stands.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return truncMarker + s[i:]
}

// runJob executes one sync job and returns its result. An empty source is
// skipped and counts as success. Otherwise success is rsync exiting 0, or
// exiting with the vanished-files warning code.
func runJob(ctx context.Context, j *job, timeout time.Duration, mode hostKeyMode, newCmd scheduler.CommandRunner) jobResult {
	start := time.Now()
	var res jobResult

	if sourceIsEmpty(j.Local) {
		slog.Warn("skip empty source", "job", j.Name, "path", j.Local)
		res.skipped = true
		res.success = true
		res.duration = time.Since(start)
		return res
	}

	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outBuf := &cappedBuffer{max: outputCapBytes}
	errBuf := &cappedBuffer{max: outputCapBytes}
	cmd := newCmd(jobCtx, "rsync", buildRsyncArgs(j, mode)...)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	runErr := cmd.Run()
	res.duration = time.Since(start)
	res.exitCode = exitCode(runErr)

	stats := parseStats(outBuf.String())
	res.files = stats.files
	res.bytes = stats.bytes

	if runErr != nil {
		res.stderrTail = tail(errBuf.String(), logStderrTailBytes)
		// A cancelled parent context means graceful shutdown SIGTERM'd this
		// in-flight rsync -- not a real failure; real timeouts cancel only
		// jobCtx (not ctx) so they still reach the error branch below.
		if ctx.Err() != nil {
			res.interrupted = true
			slog.Info("sync interrupted by shutdown",
				"job", j.Name,
				"duration_ms", res.duration.Milliseconds())
			return res
		}
		if res.exitCode == rsyncVanishedExit {
			res.success = true
			slog.Warn("sync completed with vanished source files",
				"job", j.Name,
				"files", res.files,
				"bytes", res.bytes,
				"duration_ms", res.duration.Milliseconds(),
				"rsync_exit", res.exitCode,
				"stderr", res.stderrTail)
			return res
		}
		slog.Error("sync failed",
			"job", j.Name,
			"duration_ms", res.duration.Milliseconds(),
			"timed_out", errors.Is(jobCtx.Err(), context.DeadlineExceeded),
			"error", runErr,
			"rsync_exit", res.exitCode,
			"stderr", res.stderrTail)
		return res
	}

	res.success = true
	slog.Info("sync ok",
		"job", j.Name,
		"files", res.files,
		"bytes", res.bytes,
		"duration_ms", res.duration.Milliseconds())
	return res
}

// passResult is the structured outcome of one sync pass. The health
// controller, the reporter, and the pass exit status each derive their action
// from this single value, so the outcomes can never be re-conflated by a
// caller reading a bare int.
type passResult struct {
	trigger      string        // startup | interval | external
	total        int           // jobs configured
	ok           int           // jobs that succeeded (includes emptySkipped)
	emptySkipped int           // jobs skipped because their source was missing/empty
	failed       int           // jobs that failed
	duration     time.Duration // wall-clock of the pass
	interrupted  bool          // ctx cancelled mid-pass (graceful shutdown drain)
}

// healthSignal reports whether this result should write the health marker
// (set) and to what value (healthy). Every pass writes its value EXCEPT an
// interrupted-clean one — no job failed (interrupted and unstarted jobs are
// not counted as failures) and a shutdown signal coincided with pass-end —
// which writes nothing, so a graceful drain cannot stamp a false-unhealthy
// marker that would outlive it. A real failure still writes unhealthy when
// interrupted, and shutdown itself marks unhealthy via beginDrain.
func (r *passResult) healthSignal() (set, healthy bool) {
	if r.interrupted && r.failed == 0 {
		return false, false // interrupted-clean: no job failed; don't downgrade the marker
	}
	return true, r.failed == 0
}

// exitStatus is the pass's process-level status, delivered to a triggering
// `sync` client as its exit code: 1 on any job failure, 0 on a clean pass.
//
// An interrupted-clean pass exits 0 — no job failed, and interrupted or
// unstarted jobs are not counted as failures. healthSignal declines to write
// the marker in the same case, so the two agree: exit 0 and no
// false-unhealthy marker. An interrupted pass with a real failure exits 1.
func (r *passResult) exitStatus() int {
	if r.failed > 0 {
		return 1
	}
	return 0
}

// runPass runs every job once and returns a structured result. It performs NO
// pass-level logging and never touches the health marker: reportPass owns the
// pass-level log line and the health controller owns the marker, each
// deriving its action from the returned result. Keeping execution separate
// from interpretation is what prevents an early return from silently omitting
// a signal. Concurrency control is not this function's job: the daemon's
// single executor goroutine is the only caller, and the queue in front of it
// serializes every trigger source (ticker, socket clients) — the flock that
// used to guard a cross-process `sync` exec is gone because there is no
// second executing process anymore.
func runPass(ctx context.Context, cfg config, timeout time.Duration, mode hostKeyMode, trigger string, newCmd scheduler.CommandRunner) passResult {
	res := passResult{trigger: trigger, total: len(cfg.Jobs)}
	start := time.Now()
	for i := range cfg.Jobs {
		if ctx.Err() != nil {
			// Graceful shutdown landed mid-pass: do not start the remaining jobs
			// under an already-cancelled context (they would fail-fast and
			// inflate the failure count). res.interrupted is recorded after the
			// loop so healthSignal/reportPass see the drain.
			break
		}
		jr := runJob(ctx, &cfg.Jobs[i], timeout, mode, newCmd)
		switch {
		case jr.skipped:
			res.emptySkipped++
			res.ok++
		case jr.success:
			res.ok++
		case jr.interrupted:
			// SIGTERM'd mid-transfer by graceful shutdown. runJob classifies this
			// as "not a real failure"; do NOT count it as failed, so an otherwise
			// clean pass keeps failed==0 and healthSignal's interrupted-clean
			// carve-out can fire (no false-unhealthy marker, exit 0). A genuine
			// rsync failure still lands in the default arm and sets failed>0.
		default:
			res.failed++
		}
	}
	res.duration = time.Since(start)
	res.interrupted = ctx.Err() != nil
	return res
}

// reportPass emits the single pass-level log line for a pass. Every pass
// produces exactly one structured line, so no path can return from a pass
// without a signal.
func reportPass(r *passResult) {
	if r.interrupted {
		// A real pass began but was cut short by graceful shutdown. Logged at
		// warn (the drain is expected, not a failure) and deliberately NOT the
		// "sync cycle complete" heartbeat, so it never registers as a healthy
		// completion for absence-based staleness alerting.
		slog.Warn("sync cycle interrupted by shutdown",
			"trigger", r.trigger, "jobs", r.total,
			"ok", r.ok, "skipped", r.emptySkipped, "failed", r.failed,
			"duration_ms", r.duration.Milliseconds())
		return
	}
	// The staleness heartbeat: emitted once per pass that actually ran
	// (clean or with failures). A Loki absence alert on this line catches a
	// scheduler that has stopped triggering — and since every pass (built-in
	// or externally triggered) runs in the daemon, the line reaches the
	// container log stream in both scheduling modes.
	slog.Info("sync cycle complete",
		"trigger", r.trigger, "jobs", r.total,
		"ok", r.ok, "skipped", r.emptySkipped, "failed", r.failed,
		"duration_ms", r.duration.Milliseconds())
}

// cappedBuffer is an io.Writer that retains at most max bytes, discarding
// the overflow while still reporting a full write so the subprocess is
// never blocked on a full pipe.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	switch {
	case c.max <= 0:
		// sink: retain nothing
	case len(p) >= c.max:
		c.buf.Reset()
		c.buf.Write(p[len(p)-c.max:])
	default:
		// Drop the oldest bytes first so the retained window is a true tail:
		// both readers (the stderr tail and rsync's trailing --stats block)
		// want the end of the stream, never the beginning.
		if overflow := c.buf.Len() + len(p) - c.max; overflow > 0 {
			c.buf.Next(overflow)
		}
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
