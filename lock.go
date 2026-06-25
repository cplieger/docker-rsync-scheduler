package main

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
)

// --- Sync Overlap Lock ---

// fileLock is an advisory exclusive lock backed by flock(2). It is the single
// overlap-prevention mechanism for both scheduling modes: the built-in
// scheduler (a startup pass vs an interval pass, though the sequential loop
// already serialises those in-process) and the external `sync` subcommand (a
// scheduled run vs a manual docker exec, cross-process). flock associates the
// lock with the open file description, so two independent os.OpenFile calls
// contend even within one process.
type fileLock struct {
	f *os.File
}

// lockHolder describes the process holding the overlap lock, read from the
// lock file on a failed acquisition. since is the zero time when the holder's
// start time could not be read — a holder that has not yet written it, a torn
// mid-write read, or a file predating this format. Callers then report the
// holder age as unknown; the field is observability-only and never affects
// locking correctness.
type lockHolder struct {
	since time.Time
}

// age reports how long the holder has held the lock, or 0 when unknown.
func (h lockHolder) age() time.Duration {
	if h.since.IsZero() {
		return 0
	}
	return time.Since(h.since)
}

// known reports whether the holder's acquisition time was readable. It
// disambiguates age()'s 0 return: known()==false means the timestamp could not
// be read (unknown), whereas known()==true with a ~0 age is a just-acquired
// holder.
func (h lockHolder) known() bool { return !h.since.IsZero() }

// tryLock attempts a non-blocking exclusive flock on path. On success it
// records the acquisition time in the file (for a later failed acquirer to
// read) and returns ok=true; the caller must release with unlock. When another
// holder owns the lock, ok is false and holder carries that holder's metadata
// (best-effort, zero-valued when unreadable).
func tryLock(path string) (l *fileLock, ok bool, holder lockHolder, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644) // #nosec G304 -- fixed in-container lock path
	if err != nil {
		return nil, false, lockHolder{}, err
	}
	if lockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); lockErr != nil {
		// Read the current holder's metadata before closing — best-effort, used
		// only for the deferred-pass log line, never for correctness.
		holder = readHolder(f)
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, false, holder, nil
		}
		return nil, false, lockHolder{}, lockErr
	}
	writeHolder(f)
	return &fileLock{f: f}, true, lockHolder{}, nil
}

// unlock releases the lock and closes the underlying file. The lockfile is
// left on disk; its only content is the last holder's acquisition timestamp,
// reused across runs.
func (l *fileLock) unlock() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// writeHolder records the current time as the lock-acquisition timestamp.
// Best-effort: a failure only degrades the holder age reported to a later
// deferred pass, never correctness. Truncate-then-write keeps a shorter line
// from leaving a stale tail.
func writeHolder(f *os.File) {
	line := time.Now().UTC().Format(time.RFC3339Nano) + "\n"
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.WriteAt([]byte(line), 0)
}

// readHolder parses the acquisition timestamp from the lock file. It returns a
// zero lockHolder on any read or parse failure (the holder may not have
// written it yet, or the line may be torn mid-write); the age is then unknown.
func readHolder(f *os.File) lockHolder {
	buf := make([]byte, 64)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return lockHolder{}
	}
	since, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(buf[:n])))
	if perr != nil {
		return lockHolder{}
	}
	return lockHolder{since: since}
}
