package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestParseStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		wantFiles int64
		wantBytes int64
	}{
		{
			name: "full stats block",
			in: "Number of files: 12 (reg: 10, dir: 2)\n" +
				"Number of regular files transferred: 5\n" +
				"Total file size: 1,000 bytes\n" +
				"Total transferred file size: 2,048 bytes\n" +
				"sent 3,000 bytes  received 96 bytes\n",
			wantFiles: 5,
			wantBytes: 2048,
		},
		{
			name:      "sent fallback when no transferred line",
			in:        "sent 4,096 bytes  received 96 bytes  total size 4,096\n",
			wantFiles: 0,
			wantBytes: 4096,
		},
		{
			name:      "files with thousands separator",
			in:        "Number of regular files transferred: 1,234,567\n",
			wantFiles: 1234567,
			wantBytes: 0,
		},
		{
			name:      "garbage yields zero",
			in:        "this is not rsync output at all",
			wantFiles: 0,
			wantBytes: 0,
		},
		{
			name:      "empty yields zero",
			in:        "",
			wantFiles: 0,
			wantBytes: 0,
		},
		{
			name:      "transferred preferred over sent",
			in:        "Total transferred file size: 10 bytes\nsent 9999 bytes\n",
			wantFiles: 0,
			wantBytes: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseStats(tt.in)
			if got.files != tt.wantFiles {
				t.Errorf("parseStats files = %d, want %d", got.files, tt.wantFiles)
			}
			if got.bytes != tt.wantBytes {
				t.Errorf("parseStats bytes = %d, want %d", got.bytes, tt.wantBytes)
			}
		})
	}
}

func TestParseNum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"42", 42},
		{"1,234", 1234},
		{"1,234,567", 1234567},
		{"", 0},
		{"abc", 0},
		{"12.5", 0},
	}
	for _, tt := range tests {
		if got := parseNum(tt.in); got != tt.want {
			t.Errorf("parseNum(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestExitCode(t *testing.T) {
	t.Parallel()
	if got := exitCode(nil); got != 0 {
		t.Errorf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("boom")); got != -1 {
		t.Errorf("exitCode(generic) = %d, want -1", got)
	}

	cmd := exec.Command("sh", "-c", "exit 23")
	runErr := cmd.Run()
	if got := exitCode(runErr); got != 23 {
		t.Errorf("exitCode(exit 23) = %d, want 23", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	t.Parallel()

	t.Run("retains within cap", func(t *testing.T) {
		t.Parallel()
		b := &cappedBuffer{max: 10}
		n, err := b.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
		}
		if b.String() != "hello" {
			t.Errorf("String = %q, want hello", b.String())
		}
	})

	t.Run("truncates overflow but reports full length", func(t *testing.T) {
		t.Parallel()
		b := &cappedBuffer{max: 4}
		n, err := b.Write([]byte("abcdefgh"))
		if err != nil || n != 8 {
			t.Fatalf("Write = (%d, %v), want (8, nil)", n, err)
		}
		if b.String() != "abcd" {
			t.Errorf("String = %q, want abcd", b.String())
		}
	})
}

// TestCappedBuffer_capEnforcedAcrossWrites pins the remaining-room arithmetic
// (max - current length). A first write half-fills the buffer; the second
// write must be truncated against the *remaining* room, not the full cap.
// Observing the exact retained bytes (not just the reported count) catches a
// `-` -> `+` mutation of the remaining computation, which would let the second
// write overflow the cap to "abcdef".
func TestCappedBuffer_capEnforcedAcrossWrites(t *testing.T) {
	t.Parallel()
	b := &cappedBuffer{max: 4}

	n1, _ := b.Write([]byte("ab"))
	n2, _ := b.Write([]byte("cdef"))

	if n1 != 2 || n2 != 4 {
		t.Errorf("Write lengths = (%d, %d), want (2, 4)", n1, n2)
	}
	if b.String() != "abcd" {
		t.Errorf("cappedBuffer after writes = %q, want abcd", b.String())
	}
	if len(b.String()) > 4 {
		t.Errorf("cappedBuffer length = %d, want <= cap 4", len(b.String()))
	}
}

func TestTail(t *testing.T) {
	t.Parallel()
	if got := tail("short", 100); got != "short" {
		t.Errorf("tail no truncation = %q, want short", got)
	}
	// len(s) == n is the boundary of the len(s) <= n guard: the string fits
	// exactly and must be returned verbatim with no truncation marker. A
	// `<=` -> `<` off-by-one would prepend the marker here.
	if got := tail("abc", 3); got != "abc" {
		t.Errorf("tail(%q, 3) = %q, want abc (len == n, returned verbatim)", "abc", got)
	}
	got := tail("abcdefghij", 3)
	if !strings.HasSuffix(got, "hij") {
		t.Errorf("tail = %q, want suffix hij", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("tail = %q, want truncation marker", got)
	}
}

func TestProperty_ParseStatsNeverPanics(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		in := rapid.String().Draw(rt, "in")
		got := parseStats(in)
		if got.files < 0 || got.bytes < 0 {
			rt.Fatalf("parseStats(%q) = %+v, want non-negative", in, got)
		}
	})
}

// TestProperty_CappedBufferNeverExceedsMax asserts the two core invariants of
// cappedBuffer across any sequence of writes: the retained bytes never exceed
// max, and they are exactly the first min(total, max) bytes of the
// concatenated input. This is the round-trip/bound counterpart to the
// table-driven cap test and robustly kills arithmetic mutations of the
// remaining-room computation.
func TestProperty_CappedBufferNeverExceedsMax(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		max := rapid.IntRange(0, 64).Draw(rt, "max")
		chunks := rapid.SliceOfN(rapid.SliceOfN(rapid.Byte(), 0, 16), 0, 8).Draw(rt, "chunks")

		b := &cappedBuffer{max: max}
		var concat []byte
		for _, chunk := range chunks {
			n, err := b.Write(chunk)
			if err != nil {
				rt.Fatalf("Write(%q) returned error %v", chunk, err)
			}
			if n != len(chunk) {
				rt.Fatalf("Write(%q) reported n=%d, want %d (full length always reported)", chunk, n, len(chunk))
			}
			concat = append(concat, chunk...)
		}

		got := b.String()
		if len(got) > max {
			rt.Fatalf("buffer length %d exceeds max %d", len(got), max)
		}
		wantLen := min(len(concat), max)
		if want := string(concat[:wantLen]); got != want {
			rt.Fatalf("buffer = %q, want %q (first %d bytes of input)", got, want, wantLen)
		}
	})
}

// assertCappedWrite performs a single cappedBuffer.Write into a fresh buffer
// and asserts both the reported count (Write always reports len(in), even when
// it discards overflow) and the retained content.
func assertCappedWrite(t *testing.T, max int, in, wantBuf string, wantN int) {
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
		t.Errorf("cappedBuffer{max:%d}.Write(%q) = %q, want %q", max, in, got, wantBuf)
	}
}

// TestCappedBuffer_writeBoundaries pins the two cap boundaries the other
// cappedBuffer tests skip: an exact fit (len(p) == remaining room) and a write
// into an already-full buffer (remaining == 0). Together they lock the
// min()-clamp form of cappedBuffer.Write.
//
// These same boundaries are why two CONDITIONALS_BOUNDARY mutants on the clamp
// are equivalent (unkillable): at an exact fit, writing all of p and writing
// p[:remaining] are identical (p[:remaining] == p when remaining == len(p)); at
// an already-full buffer, the remaining>0 guard and its >=0 mutant both write
// nothing. The min() rewrite removed the first comparison outright; the second
// is documented here and deliberately left in place.
func TestCappedBuffer_writeBoundaries(t *testing.T) {
	t.Parallel()

	// len(p) == remaining: exact fit, whole input retained (not truncated).
	assertCappedWrite(t, 4, "abcd", "abcd", 4)
	// len(p) > remaining: truncated to the cap, full length still reported.
	assertCappedWrite(t, 4, "abcde", "abcd", 5)
	// len(p) < remaining: room to spare, whole input retained.
	assertCappedWrite(t, 4, "ab", "ab", 2)

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
