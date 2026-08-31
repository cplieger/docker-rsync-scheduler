package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cplieger/scheduler/v4"
)

// --- rsync engine ---

const (
	// outputCapBytes bounds each captured rsync output stream so a chatty
	// subprocess cannot OOM the container.
	outputCapBytes = 1 << 20 // 1 MB

	// logStderrTailBytes bounds the stderr tail attached to terminal job
	// records so one command cannot flood Loki.
	logStderrTailBytes = 2048

	// truncMarker prefixes a cut stderr tail, louder than an ellipsis so a
	// cut record is distinguishable from output that ended there.
	truncMarker = "...(truncated)..."

	// rsyncVanishedExit is rsync's code 24 (a warning, not an error: source
	// files vanished mid-transfer). cleanup.c assigns 24 AFTER the
	// --max-delete status 25, so 24 also masks a capped deletion.
	rsyncVanishedExit = 24

	// rsyncDelLimitWarn is the stderr line rsync prints when --max-delete
	// stops deletions — the only place a capped deletion survives when 24
	// has overwritten 25 in the exit code.
	rsyncDelLimitWarn = "Deletions stopped due to --max-delete limit"

	// rsyncSignalExit is rsync's code 20 (RERR_SIGNAL), reported only from
	// its own signal handlers, so it says nothing about who sent the signal.
	rsyncSignalExit = 20
)

// globalExcludes are applied to every job in addition to per-job excludes:
// Syncthing metadata and OS junk files that should never be mirrored.
var globalExcludes = []string{".stfolder", ".stversions", ".DS_Store", "Thumbs.db"}

// defaultCommandRunner builds rsync subprocess commands with graceful
// shutdown: SIGTERM on context cancellation, then DefaultGrace before
// escalating to SIGKILL.
var defaultCommandRunner = scheduler.NewCommandRunner(scheduler.DefaultGrace)

// syncStats holds the files and bytes transferred, plus the receiver-side
// deletion count, parsed from rsync --stats output.
type syncStats struct {
	files     int64
	bytes     int64
	deletions int64
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
// known_hosts file. A regular file with at least one entry there switches
// ssh from TOFU (accept-new) to strict host-key pinning.
var knownHostsPath = "/config/known_hosts"

// hostKeyMode is the ssh host-key verification posture, decided once at boot
// from the content of knownHostsPath and carried through the pass.
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
// attribute.
func (m hostKeyMode) String() string {
	if m == hostKeyStrict {
		return "strict"
	}
	return "accept-new"
}

// transport carries the boot-decided rsync transport policy. The zero value
// is the shipped default (accept-new TOFU, no attribute flags, no
// compression).
type transport struct {
	compress string // "" = off, otherwise an rsync --compress-choice name or "auto"
	hostKeys hostKeyMode
	acls     bool
	xattrs   bool
}

// classifyKnownHosts decides the host-key posture from what is mounted at
// path. Absent means accept-new. A path that is not a regular file, or a
// file with no entries, can pin nothing — ssh would then fail every pass
// with "Host key verification failed" — so this errors rather than silently
// falling back. Entries are counted, never parsed, so a marker
// (@cert-authority, @revoked) or hashed (|1|) line counts.
func classifyKnownHosts(path string) (hostKeyMode, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- operator-mounted known_hosts path
	switch {
	case errors.Is(err, os.ErrNotExist):
		return hostKeyAcceptNew, nil
	case err != nil:
		return hostKeyAcceptNew, fmt.Errorf("known_hosts: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return hostKeyAcceptNew, fmt.Errorf("known_hosts: %w", err)
	case !info.Mode().IsRegular():
		return hostKeyAcceptNew, fmt.Errorf("known_hosts %q is not a regular file", path)
	}
	br := bufio.NewReader(f)
	inComment := false
	for {
		r, _, rerr := br.ReadRune()
		switch {
		case errors.Is(rerr, io.EOF):
			return hostKeyAcceptNew, fmt.Errorf("known_hosts %q has no entries", path)
		case rerr != nil:
			return hostKeyAcceptNew, fmt.Errorf("known_hosts: %w", rerr)
		case r == '\n':
			inComment = false
		case inComment, unicode.IsSpace(r):
			// Rune-by-rune discard allocates nothing.
		case r == '#':
			inComment = true
		default:
			return hostKeyStrict, nil
		}
	}
}

// sshCommand builds the single -e argument string. rsync splits it
// internally and runs no shell, so the key path (already validated to
// contain no whitespace or quote characters) needs no quoting.
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
// archive-ish flag set is -rlptD (recurse, links, perms, times,
// devices/specials) minus owner/group/ACL/xattr; -A, -X and -z are appended
// only for switches the operator opted into.
func buildRsyncArgs(j *job, tr transport) []string {
	args := []string{"-rlptD"}
	if tr.acls {
		args = append(args, "-A")
	}
	if tr.xattrs {
		args = append(args, "-X")
	}
	if tr.compress != "" {
		args = append(args, "-z")
		if tr.compress != "auto" {
			args = append(args, "--compress-choice="+tr.compress)
		}
	}
	if j.Delete {
		args = append(args, "--delete")
		// The documented backstop for a delete:true job whose excludes can
		// match every top-level source entry. Unset -> uncapped.
		if j.MaxDelete != nil {
			args = append(args, fmt.Sprintf("--max-delete=%d", *j.MaxDelete))
		}
	}
	if j.RemoteUID != nil && j.RemoteGID != nil {
		args = append(args, fmt.Sprintf("--chown=%d:%d", *j.RemoteUID, *j.RemoteGID))
	}
	args = append(args, "--stats", "-e", sshCommand(j.SSHKey, tr.hostKeys))

	// --filter with an explicit "- " prefix, never --exclude: rsync reads an
	// --exclude value in old-prefix mode, where a leading "- "/"+ " flips the
	// rule sense and a value of exactly "!" clears every rule so far,
	// globals included.
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
	// reinterpreted as rsync options.
	args = append(args, "--", j.Local+"/", remoteDest(j))
	return args
}

// sourceIsEmpty reports whether the local source has nothing worth
// mirroring: truly empty, or holding only globally-excluded entries. Such a
// source skips the job, because an empty sender under --delete deletes every
// non-excluded file on the receiver. A missing path is not that state: rsync
// refuses the whole pass for a source it cannot enter (exit 23), so it is
// returned as an error. Per-job excludes are deliberately not applied here:
// they are rsync globs, and approximating their match could falsely skip a
// real source.
func sourceIsEmpty(path string) (bool, error) {
	// O_DIRECTORY makes the kernel refuse non-directories before opening a
	// fifo could block for a writer.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY, 0) // #nosec G304 -- operator-mounted source path
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	for {
		names, rerr := f.Readdirnames(256)
		for _, n := range names {
			if !slices.Contains(globalExcludes, n) {
				return false, nil // a mirrorable entry exists
			}
		}
		if errors.Is(rerr, io.EOF) {
			return true, nil // only globally-excluded entries (or none)
		}
		if rerr != nil {
			return false, rerr
		}
	}
}

// statsRegexes match the relevant lines of rsync --stats output. Numbers
// may carry thousands separators, which parseNum strips.
var (
	reFilesXfer = regexp.MustCompile(`Number of regular files transferred:\s*([\d,]+)`)
	reBytesXfer = regexp.MustCompile(`Total transferred file size:\s*([\d,]+)`)
	reDeletions = regexp.MustCompile(`Number of deleted files:\s*([\d,]+)`)
)

// parseStats extracts files transferred, bytes transferred, and receiver-side
// deletions from rsync --stats output. Missing or malformed values yield 0;
// parsing never fails, so a stats-format change degrades observability without
// failing an otherwise-successful sync.
func parseStats(out string) syncStats {
	var s syncStats
	if m := reFilesXfer.FindStringSubmatch(out); m != nil {
		s.files = parseNum(m[1])
	}
	if m := reBytesXfer.FindStringSubmatch(out); m != nil {
		s.bytes = parseNum(m[1])
	}
	if m := reDeletions.FindStringSubmatch(out); m != nil {
		s.deletions = parseNum(m[1])
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

// tail returns the last n bytes of s, prefixed to indicate truncation. The cut
// advances to the next rune start, so no continuation byte of a split rune
// survives at the head of the retained tail (which would render as
// unattributable noise: the rune it belonged to is gone). Leading continuation
// bytes are skipped whether they came from a split valid rune or malformed
// input; invalid bytes elsewhere in the retained tail are returned unchanged.
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

type drainSignals struct {
	parentErr  error
	jobErr     error
	runErr     error
	ps         *os.ProcessState
	cancelSent bool
}

// interruptedByShutdown reports whether this app interrupted the command as
// part of a parent-context drain. A job deadline is always a job failure.
func interruptedByShutdown(signals drainSignals) bool {
	if errors.Is(signals.jobErr, context.DeadlineExceeded) || signals.parentErr == nil {
		return false
	}
	if signals.ps == nil {
		return errors.Is(signals.runErr, context.Canceled)
	}
	exitCode := signals.ps.ExitCode()
	return (exitCode < 0 || exitCode == rsyncSignalExit) && signals.cancelSent
}

// runJob executes one sync job and returns its result. A source that cannot be
// read fails the job; an empty source is skipped and counts as success.
// Otherwise success is rsync exiting 0, or exiting 24 (vanished source files)
// with no --max-delete diagnostic on stderr.
func runJob(ctx context.Context, j *job, timeout time.Duration, tr transport, newCmd scheduler.CommandRunner) jobResult {
	start := time.Now()
	var res jobResult

	empty, err := sourceIsEmpty(j.Local)
	if err != nil {
		res.duration = time.Since(start)
		slog.Error("sync failed",
			"job", j.Name,
			"path", j.Local,
			"duration_ms", res.duration.Milliseconds(),
			"error", err)
		return res
	}
	if empty {
		slog.Warn("skip empty source", "job", j.Name, "path", j.Local)
		res.skipped = true
		res.success = true
		return res
	}

	jobCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	outBuf := &cappedBuffer{max: outputCapBytes}
	errBuf := &cappedBuffer{max: outputCapBytes}
	cmd := newCmd(jobCtx, "rsync", buildRsyncArgs(j, tr)...)
	var cancelSent atomic.Bool
	if cancelCmd := cmd.Cancel; cancelCmd != nil {
		cmd.Cancel = func() error {
			err := cancelCmd()
			if err == nil {
				cancelSent.Store(true)
			}
			return err
		}
	}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	runErr := cmd.Run()
	res.duration = time.Since(start)

	stats := parseStats(outBuf.String())
	res.files = stats.files
	res.bytes = stats.bytes

	// The child's report decides the outcome. A drain needs positive
	// evidence that this app signalled it (cancelSent, or a Start refused on
	// an already-cancelled context); a job deadline is always a failure.
	ps := cmd.ProcessState
	if ps != nil {
		res.exitCode = ps.ExitCode()
	}
	stderrAll := errBuf.String()
	res.stderrTail = tail(stderrAll, logStderrTailBytes)
	// rsync emits this warning on its own line and escapes control bytes in
	// filenames, so a filename cannot forge the line boundary.
	delLimited := strings.HasPrefix(stderrAll, rsyncDelLimitWarn) ||
		strings.Contains(stderrAll, "\n"+rsyncDelLimitWarn)

	if runErr == nil {
		res.success = true
		slog.Info("sync ok",
			"job", j.Name,
			"files", res.files,
			"bytes", res.bytes,
			"deletions", stats.deletions,
			"duration_ms", res.duration.Milliseconds(),
			"stderr", res.stderrTail)
		return res
	}

	if ps == nil {
		res.exitCode = -1
	}
	drained := interruptedByShutdown(drainSignals{
		parentErr:  ctx.Err(),
		jobErr:     jobCtx.Err(),
		runErr:     runErr,
		ps:         ps,
		cancelSent: cancelSent.Load(),
	})
	switch {
	case ps == nil:
		// No child ran: Start refused on an already-cancelled context, or the
		// exec itself failed. This is the only state with no exit status.
	case ps.Success():
		// Exit 0 with a non-nil error: os/exec's success-only ctx.Err()
		// substitution, or exec.ErrWaitDelay after a clean exit. The transfer
		// completed either way.
		res.success = true
		slog.Warn("sync completed with a residual run error",
			"job", j.Name,
			"files", res.files,
			"bytes", res.bytes,
			"deletions", stats.deletions,
			"duration_ms", res.duration.Milliseconds(),
			"error", runErr,
			"stderr", res.stderrTail)
		return res
	case res.exitCode == rsyncVanishedExit && !delLimited:
		res.success = true
		slog.Warn("sync completed with vanished source files",
			"job", j.Name,
			"files", res.files,
			"bytes", res.bytes,
			"deletions", stats.deletions,
			"duration_ms", res.duration.Milliseconds(),
			"rsync_exit", res.exitCode,
			"stderr", res.stderrTail)
		return res
	}

	if drained {
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

// passResult is the structured outcome of one sync pass. The health
// controller and reporter derive their actions from this single value.
type passResult struct {
	trigger      string        // startup | interval | external
	total        int           // jobs configured
	ok           int           // jobs that succeeded (includes emptySkipped)
	emptySkipped int           // jobs skipped because their source was empty
	failed       int           // jobs that failed
	duration     time.Duration // wall-clock of the pass
	interrupted  bool          // shutdown interruption observed during the pass
}

// healthy reports the marker value this pass implies: healthy iff no job
// failed. Interrupted and unstarted jobs are not failures.
func (r *passResult) healthy() bool { return r.failed == 0 }

// runPass runs every job once and returns their aggregate result. The
// daemon's single executor is the only caller and owns serialization.
func runPass(ctx context.Context, cfg config, timeout time.Duration, tr transport, trig string, newCmd scheduler.CommandRunner) passResult {
	res := passResult{trigger: trig, total: len(cfg.Jobs)}
	start := time.Now()
	for i := range cfg.Jobs {
		if ctx.Err() != nil {
			// Shutdown landed mid-pass: do not start the remaining jobs, and
			// classify as interrupted rather than let each unstarted job log
			// its own outcome.
			res.interrupted = true
			break
		}
		jr := runJob(ctx, &cfg.Jobs[i], timeout, tr, newCmd)
		switch {
		case jr.skipped:
			res.emptySkipped++
			res.ok++
		case jr.success:
			res.ok++
		case jr.interrupted:
			// SIGTERM'd mid-transfer by graceful shutdown; not a failure, so
			// an otherwise clean pass keeps failed==0 and the health
			// controller writes no marker for it.
			res.interrupted = true
		default:
			res.failed++
		}
	}
	res.duration = time.Since(start)
	return res
}

// reportPass emits the single pass-level log line for a pass that ran.
func reportPass(r *passResult) {
	if r.interrupted {
		// Logged at warn (expected, not a failure) and deliberately not the
		// "sync cycle complete" heartbeat, so it never registers as a
		// healthy completion for absence-based staleness alerting.
		slog.Warn("sync cycle interrupted by shutdown",
			"trigger", r.trigger, "jobs", r.total,
			"ok", r.ok, "skipped", r.emptySkipped, "failed", r.failed,
			"duration_ms", r.duration.Milliseconds())
		return
	}
	// Staleness heartbeat, emitted once per pass that ran (clean or with
	// failures) in both scheduling modes so a Loki absence alert catches a
	// scheduler that stopped triggering.
	slog.Info("sync cycle complete",
		"trigger", r.trigger, "jobs", r.total,
		"ok", r.ok, "skipped", r.emptySkipped, "failed", r.failed,
		"duration_ms", r.duration.Milliseconds())
}

// cappedBuffer is an io.Writer that retains at most max bytes of the tail,
// discarding older overflow while still reporting a full write so the
// subprocess is never blocked on a full pipe. Both readers need the end of the
// stream: tail reports the last n stderr bytes, and rsync writes --stats last.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	switch {
	case len(p) >= c.max:
		c.buf.Reset()
		c.buf.Write(p[len(p)-c.max:])
	default:
		if overflow := c.buf.Len() + len(p) - c.max; overflow > 0 {
			c.buf.Next(overflow)
		}
		c.buf.Write(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
