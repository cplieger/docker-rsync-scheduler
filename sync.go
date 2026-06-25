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
	"syscall"
	"time"
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

// commandRunner constructs a configured *exec.Cmd. It decouples
// orchestration from subprocess construction so tests can inject a fake.
type commandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd

// defaultCommandRunner returns an exec.Cmd with graceful shutdown:
// SIGTERM on context cancellation with a 5s grace period before SIGKILL.
func defaultCommandRunner(ctx context.Context, name string, cmdArgs ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, cmdArgs...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// syncStats holds the figures parsed from rsync --stats output.
type syncStats struct {
	files int64
	bytes int64
}

// jobResult captures the outcome of a single job for logging and health
// aggregation. Fields are ordered largest-first for fieldalignment.
type jobResult struct {
	err         error
	name        string
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
// known_hosts file. When this file is present (mounted read-only by
// the operator), ssh switches from TOFU (accept-new) to strict host-key
// pinning — a stronger security posture for environments where the remote
// host key is pre-known.
const knownHostsPath = "/config/known_hosts"

// knownHostsExists reports whether a known_hosts file is present at the
// conventional path. Extracted as a package-level variable so tests can
// override it without touching the filesystem.
var knownHostsExists = func() bool {
	_, err := os.Stat(knownHostsPath)
	return err == nil
}

// sshHostKeyMode reports the active SSH host-key verification posture for
// startup logging: "strict" when a known_hosts file is present, "accept-new"
// (TOFU) otherwise. Lets an operator confirm pinning is active and catch a
// mis-mounted known_hosts that silently fell back to TOFU.
func sshHostKeyMode() string {
	if knownHostsExists() {
		return "strict"
	}
	return "accept-new"
}

// sshCommand builds the single -e argument string. rsync splits it
// internally; there is no shell, so the key path (already validated to
// contain no whitespace or metacharacters) needs no quoting.
//
// When a known_hosts file exists at /config/known_hosts, the command uses
// StrictHostKeyChecking=yes with an explicit UserKnownHostsFile. When
// absent, it uses accept-new (TOFU) so first-run works without
// pre-provisioning host keys.
func sshCommand(key string) string {
	if knownHostsExists() {
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
func buildRsyncArgs(j *job) []string {
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
	args = append(args, "--stats", "-e", sshCommand(j.SSHKey))

	for _, e := range globalExcludes {
		args = append(args, "--exclude="+e)
	}
	for _, e := range j.Excludes {
		args = append(args, "--exclude="+e)
	}

	args = append(args, j.Local+"/", remoteDest(j))
	return args
}

// sourceIsEmpty reports whether the local source directory has nothing worth
// mirroring: it is missing, unreadable, truly empty, or contains ONLY
// globally-excluded entries (.stfolder, .DS_Store, …). Such a source skips the
// job so that a vanished, unmounted, or junk-only directory cannot cause
// --delete to wipe the remote: an excludes-only source would otherwise pass a
// naive "any entry present" check yet transfer nothing, and --delete would then
// delete every non-excluded file on the receiver. Any read error is treated as
// empty for the same safety reason.
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
		if len(names) == 0 {
			// Defensive: no error and no names cannot normally occur with a
			// positive count, but guard against an unexpected zero-progress read
			// rather than spin forever.
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

// tail returns the last n bytes of s, prefixed to indicate truncation.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)..." + s[len(s)-n:]
}

// runJob executes one sync job and returns its result. An empty source is
// skipped and counts as success. Otherwise success is defined as rsync
// exiting 0.
func runJob(ctx context.Context, j *job, timeout time.Duration, newCmd commandRunner) jobResult {
	start := time.Now()
	res := jobResult{name: j.Name}

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
	cmd := newCmd(jobCtx, "rsync", buildRsyncArgs(j)...)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	runErr := cmd.Run()
	res.duration = time.Since(start)
	res.exitCode = exitCode(runErr)

	stats := parseStats(outBuf.String())
	res.files = stats.files
	res.bytes = stats.bytes

	if runErr != nil {
		res.err = runErr
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

// passDisposition is what a whole sync pass did. It distinguishes the three
// outcomes a bare failure count conflated: a pass that ran its jobs, a pass
// deferred because another holder owns the overlap lock, and a pass that could
// not acquire the lock at all.
type passDisposition int

const (
	passRan      passDisposition = iota // jobs executed; see failed for clean vs failed
	passDeferred                        // overlap: another holder owns the lock; no jobs ran
	passLockErr                         // could not acquire the lock (environment failure)
)

// passResult is the structured outcome of one sync pass. The health
// controller, the reporter, and the sync exit code each derive their action
// from this single value, so the three dispositions can never be re-conflated
// by a caller reading a bare int. Fields are ordered largest-first for
// fieldalignment.
type passResult struct {
	err            error           // non-nil only for passLockErr
	trigger        string          // startup | interval | external
	disposition    passDisposition // what the pass did
	total          int             // jobs configured
	ok             int             // jobs that succeeded (includes emptySkipped)
	emptySkipped   int             // jobs skipped because their source was missing/empty
	failed         int             // jobs that failed
	duration       time.Duration   // wall-clock of a pass that ran
	holderAge      time.Duration   // how long the current holder has run (passDeferred)
	interrupted    bool            // ctx cancelled mid-pass (graceful shutdown drain)
	holderAgeKnown bool            // holder's acquisition time was readable (passDeferred)
}

// healthSignal reports whether this result should write the health marker
// (set) and to what value (healthy). A lock error is unhealthy; a deferred pass
// carries no signal of its own (set is false) because the running holder owns
// health; a pass that ran writes its health value — EXCEPT an interrupted-clean
// pass (every job succeeded, failed==0, but a shutdown signal coincided with
// pass-end), which also carries no signal: it leaves the marker at its last
// real value rather than writing a false-unhealthy. A pass with a real job
// failure still writes unhealthy even when interrupted, and built-in shutdown
// marks unhealthy via beginDrain (the drain watcher), not through this path —
// so the interrupted-clean no-write only matters in external mode, where
// nothing else resets a false-unhealthy until the next sync (up to a full
// interval away).
func (r *passResult) healthSignal() (set, healthy bool) {
	switch r.disposition {
	case passRan:
		if r.interrupted && r.failed == 0 {
			return false, false // interrupted-clean: no job failed; don't downgrade the marker
		}
		return true, r.failed == 0
	case passDeferred:
		return false, false // the running holder owns health
	case passLockErr:
		return true, false
	}
	// An unhandled disposition is a programming error; fail safe by writing
	// unhealthy rather than silently leaving the marker stale.
	return true, false
}

// exitStatus is the process exit code for the `sync` subcommand: 1 on any job
// failure or a lock error, 0 when the pass ran clean or deferred to an
// in-flight holder (the holder, not this invocation, owns that outcome).
//
// An interrupted-clean pass (every job succeeded but graceful shutdown cut the
// pass short) exits 0, since no configured job failed. healthSignal treats the
// same case as no signal (it declines to write the marker), so the two agree:
// a gracefully-interrupted clean `sync` exits 0 AND leaves no false-unhealthy
// marker behind. Both answer "did a configured job fail?" — an interrupted pass
// with a real failure still exits 1 and writes unhealthy.
func (r *passResult) exitStatus() int {
	switch r.disposition {
	case passRan:
		if r.failed > 0 {
			return 1
		}
		return 0
	case passDeferred:
		return 0
	case passLockErr:
		return 1
	}
	// An unhandled disposition is a programming error; fail safe
	// (non-zero) rather than reporting success for an outcome no
	// branch understood.
	return 1
}

// runPass runs every job once under the advisory overlap lock and returns a
// structured result. It performs NO pass-level logging and never touches the
// health marker: reportPass owns the pass-level log line and the health
// controller owns the marker, each deriving its action from the returned
// result. Keeping execution separate from interpretation is what prevents an
// early return (an overlap defer, a lock error) from silently omitting a
// signal. The lock guards a concurrent pass: an external `sync` exec racing
// the daemon, or a manual docker exec racing a scheduled run.
func runPass(ctx context.Context, cfg config, timeout time.Duration, trigger string, newCmd commandRunner) passResult {
	res := passResult{trigger: trigger, total: len(cfg.Jobs)}

	lock, ok, holder, lockErr := tryLock(lockFilePath)
	switch {
	case lockErr != nil:
		res.disposition = passLockErr
		res.err = lockErr
		return res
	case !ok:
		res.disposition = passDeferred
		res.holderAge = holder.age()
		res.holderAgeKnown = holder.known()
		return res
	}
	defer lock.unlock()

	res.disposition = passRan
	start := time.Now()
	for i := range cfg.Jobs {
		if ctx.Err() != nil {
			// Graceful shutdown landed mid-pass: do not start the remaining jobs
			// under an already-cancelled context (they would fail-fast and
			// inflate the failure count). res.interrupted is recorded after the
			// loop so healthSignal/reportPass see the drain.
			break
		}
		jr := runJob(ctx, &cfg.Jobs[i], timeout, newCmd)
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

// reportPass emits the single pass-level log line for a pass. Every
// disposition produces exactly one structured line, so no path can return
// from a pass without a signal — the gap that previously let an overlap defer
// emit no heartbeat.
func reportPass(r *passResult) {
	switch r.disposition {
	case passRan:
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
		// scheduler that has stopped triggering.
		slog.Info("sync cycle complete",
			"trigger", r.trigger, "jobs", r.total,
			"ok", r.ok, "skipped", r.emptySkipped, "failed", r.failed,
			"duration_ms", r.duration.Milliseconds())
	case passDeferred:
		// Liveness without a completion: the scheduler is alive and tried, but a
		// prior pass still holds the lock. Deliberately NOT the heartbeat (a stuck
		// holder must still trip the absence alert); holder_age_ms lets an
		// operator alert on a holder that has run too long.
		slog.Info("sync deferred, prior pass still running",
			"trigger", r.trigger, "holder_age_ms", r.holderAge.Milliseconds(),
			"holder_age_known", r.holderAgeKnown)
	case passLockErr:
		// A real environment failure (cannot even acquire the lock). level=error
		// trips the documented error alert; the marker goes unhealthy too.
		slog.Error("cannot acquire sync lock",
			"trigger", r.trigger, "path", lockFilePath, "error", r.err)
	default:
		// An unhandled disposition is a programming error. Emit a line anyway so
		// the "exactly one structured line per pass" invariant -- and any
		// absence-based alert that depends on it -- survives a future disposition
		// whose case was forgotten here. Mirrors exitStatus's fail-safe arm.
		slog.Error("sync pass completed with unknown disposition",
			"trigger", r.trigger, "disposition", int(r.disposition))
	}
}

// cappedBuffer is an io.Writer that retains at most max bytes, discarding
// the overflow while still reporting a full write so the subprocess is
// never blocked on a full pipe.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.max - c.buf.Len(); remaining > 0 {
		// Append at most `remaining` bytes: min() clamps the slice to whichever
		// of the input length or the leftover room is smaller.
		c.buf.Write(p[:min(len(p), remaining)])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
