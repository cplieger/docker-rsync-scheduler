package main

import "testing"

// gk_rsync_r2_writeOnce performs a single cappedBuffer.Write into a fresh
// buffer and asserts both the reported count (Write always reports len(in))
// and the retained content, with direct hardcoded expectations.
func gk_rsync_r2_writeOnce(t *testing.T, max int, in, wantBuf string, wantN int) {
	t.Helper()
	b := &cappedBuffer{max: max}
	n, err := b.Write([]byte(in))
	if err != nil {
		t.Fatalf("Write(%q) err = %v, want nil", in, err)
	}
	if n != wantN {
		t.Errorf("Write(%q) n = %d, want %d", in, n, wantN)
	}
	if got := b.String(); got != wantBuf {
		t.Errorf("cappedBuffer{max:%d}.Write(%q) -> %q, want %q", max, in, got, wantBuf)
	}
}

// TestCappedBuffer_boundaries_gk_rsync_r2 pins the two cap boundaries the
// existing TestCappedBuffer cases skip, locking the min()-clamp form of
// cappedBuffer.Write (sync.go).
//
// These two boundaries are also why the surviving CONDITIONALS_BOUNDARY
// mutants at sync.go:299/300 are equivalent and not test-killable:
//   - len(p) == remaining (exact fit): the former `len(p) > remaining` branch
//     and its `>=` mutant both wrote all of p (p[:remaining] == p when
//     remaining == len(p)), so no test distinguished them. The min() rewrite
//     removed that comparison outright, which is the real kill.
//   - remaining == 0 (already full): the `remaining > 0` guard's `>= 0` mutant
//     writes min(len(p), 0) == 0 bytes, i.e. nothing — identical observable
//     output. Documented here, deliberately left in place as equivalent.
func TestCappedBuffer_boundaries_gk_rsync_r2(t *testing.T) {
	t.Parallel()

	// len(p) == remaining: exact fit, whole input retained (not truncated).
	gk_rsync_r2_writeOnce(t, 4, "abcd", "abcd", 4)
	// len(p) > remaining: truncated to the cap, full length still reported.
	gk_rsync_r2_writeOnce(t, 4, "abcde", "abcd", 5)
	// len(p) < remaining: room to spare, whole input retained.
	gk_rsync_r2_writeOnce(t, 4, "ab", "ab", 2)

	// remaining == 0: a write into an already-full buffer adds nothing but
	// still reports the consumed length.
	b := &cappedBuffer{max: 3}
	if _, _ = b.Write([]byte("xyz")); b.String() != "xyz" {
		t.Fatalf("setup write left buffer = %q, want xyz", b.String())
	}
	n, err := b.Write([]byte("more"))
	if err != nil {
		t.Fatalf("full-buffer Write err = %v, want nil", err)
	}
	if n != 4 {
		t.Errorf("full-buffer Write n = %d, want 4", n)
	}
	if got := b.String(); got != "xyz" {
		t.Errorf("full-buffer Write left buffer = %q, want xyz (unchanged)", got)
	}
}
