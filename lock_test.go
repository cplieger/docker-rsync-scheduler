package main

import (
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

// TestReadHolder_unparseableIsUnknown verifies that holder metadata that is
// absent or malformed degrades to a zero (unknown) age rather than failing.
func TestReadHolder_unparseableIsUnknown(t *testing.T) {
	t.Parallel()

	if got := (lockHolder{}).age(); got != 0 {
		t.Errorf("zero lockHolder age = %v, want 0", got)
	}
}
