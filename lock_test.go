package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileLockMutualExclusion verifies the advisory lock contract: the first
// tryLock acquires, a second tryLock on the same path fails while the lock is
// held, and unlock releases it so it can be re-acquired.
func TestFileLockMutualExclusion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sync.lock")

	first, ok, _, err := tryLock(path)
	if err != nil {
		t.Fatalf("first tryLock: unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("first tryLock should acquire the lock")
	}

	if _, ok, _, err := tryLock(path); err != nil {
		t.Fatalf("second tryLock: unexpected error: %v", err)
	} else if ok {
		t.Error("second tryLock should fail while the lock is held")
	}

	first.unlock()

	again, ok, _, err := tryLock(path)
	if err != nil {
		t.Fatalf("third tryLock: unexpected error: %v", err)
	}
	if !ok {
		t.Error("tryLock should re-acquire after unlock")
	}
	again.unlock()
}

// TestTryLock_reportsHolderAge verifies that a failed acquisition reads the
// current holder's acquisition timestamp, so a deferred pass can report a
// non-zero holder age. The holder writes its timestamp on acquire; the
// contender reads it back on the EWOULDBLOCK path.
func TestTryLock_reportsHolderAge(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sync.lock")

	held, ok, _, err := tryLock(path)
	if err != nil || !ok {
		t.Fatalf("first tryLock: ok=%v err=%v", ok, err)
	}
	defer held.unlock()

	// Let a measurable amount of wall-clock elapse so age is unambiguously > 0.
	time.Sleep(5 * time.Millisecond)

	_, ok, holder, err := tryLock(path)
	if err != nil {
		t.Fatalf("contending tryLock: unexpected error: %v", err)
	}
	if ok {
		t.Fatal("contending tryLock should fail while the lock is held")
	}
	if holder.age() <= 0 {
		t.Errorf("holder.age() = %v, want > 0 (the holder's timestamp should have been read)", holder.age())
	}
}

// TestLockHolder_zeroValueAgeIsZero verifies that a zero-value lockHolder
// (the value readHolder returns when the acquisition timestamp is absent or
// unparseable) reports an unknown age of 0 rather than a bogus duration.
func TestLockHolder_zeroValueAgeIsZero(t *testing.T) {
	t.Parallel()
	if got := (lockHolder{}).age(); got != 0 {
		t.Errorf("zero lockHolder age = %v, want 0", got)
	}
}

// TestReadHolder_parsesValidAndRejectsMalformed exercises readHolder's
// parse-failure arm (a torn/absent/pre-format timestamp line -> zero holder),
// which the named-but-unrelated TestLockHolder_zeroValueAgeIsZero never
// reached. The round-trip subtest pins the writeHolder->readHolder pair.
func TestReadHolder_parsesValidAndRejectsMalformed(t *testing.T) {
	t.Parallel()
	t.Run("round-trips a timestamp written by writeHolder", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "sync.lock")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = f.Close() }()
		before := time.Now().UTC()
		writeHolder(f)
		holder := readHolder(f)
		if holder.since.IsZero() {
			t.Fatalf("readHolder after writeHolder = zero")
		}
		if holder.since.Before(before.Add(-time.Second)) || holder.since.After(time.Now().Add(time.Second)) {
			t.Errorf("readHolder since = %v, want within ~1s of %v", holder.since, before)
		}
	})
	t.Run("malformed line is unknown", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "sync.lock")
		if err := os.WriteFile(path, []byte("not-a-timestamp\n"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = f.Close() }()
		if got := readHolder(f); !got.since.IsZero() {
			t.Errorf("readHolder(malformed) since = %v, want zero", got.since)
		}
	})
	t.Run("empty file is unknown", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "sync.lock")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() { _ = f.Close() }()
		if got := readHolder(f); !got.since.IsZero() {
			t.Errorf("readHolder(empty) since = %v, want zero", got.since)
		}
	})
}

// TestTryLock_openErrorIsReturned exercises tryLock's os.OpenFile-error arm: a
// lock path whose parent directory does not exist makes OpenFile fail with
// ENOENT (O_CREATE creates the file, not parent dirs), so acquisition must
// surface a non-nil error rather than a silent ok=false.
func TestTryLock_openErrorIsReturned(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "nonexistent-subdir", "sync.lock")
	l, ok, holder, err := tryLock(path)
	if err == nil {
		t.Fatalf("tryLock(%q) err = nil, want a non-nil open error", path)
	}
	if ok {
		t.Errorf("tryLock(%q) ok = true, want false on open error", path)
	}
	if l != nil {
		t.Errorf("tryLock(%q) lock = %v, want nil on open error", path, l)
	}
	if !holder.since.IsZero() {
		t.Errorf("tryLock(%q) holder = %+v, want zero on open error", path, holder)
	}
}
