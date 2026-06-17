package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// stderrCapBytes bounds captured rsync stderr so a chatty or
	// misbehaving subprocess cannot OOM the container.
	stderrCapBytes = 1 << 20 // 1 MB

	// logStderrTailBytes bounds the stderr tail attached to a failure log
	// line so a single failure cannot flood Loki.
	logStderrTailBytes = 2048
)

// globalExcludes are applied to every job in addition to the per-job
// excludes. They cover Syncthing metadata and OS junk files that should
// never be mirrored to a remote.
var globalExcludes = []string{".stfolder", ".stversions", ".DS_Store", "Thumbs.db"}

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
	err        error
	name       string
	stderrTail string
	files      int64
	bytes      int64
	duration   time.Duration
	exitCode   int
	skipped    bool
	success    bool
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

	args = append(args, j.Local+"/", j.RemoteHost+":"+j.RemotePath+"/")
	return args
}

// sourceIsEmpty reports whether the local source directory is missing or
// has no entries. An empty source skips the job so that a vanished or
// unmounted directory cannot cause --delete to wipe the remote. Any read
// error is treated as empty for the same safety reason.
func sourceIsEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return true
	}
	return len(entries) == 0
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
	var ee *exec.ExitError
	if errors.As(err, &ee) {
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

	outBuf := &cappedBuffer{max: stderrCapBytes}
	errBuf := &cappedBuffer{max: stderrCapBytes}
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
		slog.Error("sync failed",
			"job", j.Name,
			"duration_ms", res.duration.Milliseconds(),
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

// runSyncPass runs every job once and returns the failure count.
// It is the shared pass used by both the built-in scheduler (startup and
// interval triggers) and the `sync` subcommand (external trigger). The
// whole pass is guarded by an advisory file lock: if another pass is
// already running (the built-in ticker racing the startup pass in-process,
// or an external `sync` exec racing the ticker cross-process) this call
// skips as a no-op success rather than running a second concurrent pass.
func runSyncPass(ctx context.Context, cfg config, timeout time.Duration, trigger string, newCmd commandRunner) (failCount int) {
	lock, ok, lockErr := tryLock(lockFilePath)
	if lockErr != nil {
		slog.Error("cannot acquire sync lock",
			"trigger", trigger, "path", lockFilePath, "error", lockErr)
		// A lock error is a real environment failure; report it as a failed
		// pass so the health marker and exit code reflect the problem.
		return 1
	}
	if !ok {
		slog.Info("sync already running, skipping overlapping request", "trigger", trigger)
		return 0
	}
	defer lock.unlock()

	start := time.Now()
	var okCount int
	for i := range cfg.Jobs {
		res := runJob(ctx, &cfg.Jobs[i], timeout, newCmd)
		if res.success {
			okCount++
		} else {
			failCount++
		}
	}
	slog.Info("sync cycle complete",
		"trigger", trigger,
		"jobs", len(cfg.Jobs),
		"ok", okCount,
		"failed", failCount,
		"duration_ms", time.Since(start).Milliseconds())
	return failCount
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
