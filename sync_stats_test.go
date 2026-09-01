package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"pgregory.net/rapid"
)

func TestParseStats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		in            string
		wantFiles     int64
		wantBytes     int64
		wantDeletions int64
	}{
		{
			name: "full stats block",
			in: "Number of files: 12 (reg: 10, dir: 2)\n" +
				"Number of deleted files: 1 (reg: 1)\n" +
				"Number of regular files transferred: 5\n" +
				"Total file size: 1,000 bytes\n" +
				"Total transferred file size: 2,048 bytes\n" +
				"sent 3,000 bytes  received 96 bytes\n",
			wantFiles:     5,
			wantBytes:     2048,
			wantDeletions: 1,
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
			// A capture of only separators reduces to the empty string, which
			// ParseInt refuses: the matched-but-unparseable arm parseStats
			// publishes as "malformed values yield 0".
			name:      "matched but unparseable yields zero",
			in:        "Number of regular files transferred: ,,,\n",
			wantFiles: 0,
			wantBytes: 0,
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
			if got.deletions != tt.wantDeletions {
				t.Errorf("parseStats deletions = %d, want %d", got.deletions, tt.wantDeletions)
			}
		})
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
	// A cut that splits a valid multi-byte rune must leave no continuation
	// byte at the head of the retained tail: the rune it belonged to is gone,
	// so the bytes are unattributable noise. Cutting 3000 'a' + U+4E16 + 2046
	// 'b' at 2048 lands inside the three-byte rune.
	split := tail(strings.Repeat("a", 3000)+"\u4e16"+strings.Repeat("b", 2046), 2048)
	head := strings.TrimPrefix(split, truncMarker)
	if head == "" || !utf8.RuneStart(head[0]) {
		t.Errorf("tail() retained tail starts with %q, want a rune start", head[:min(len(head), 4)])
	}
}

func TestDelLimitCapture_matchesPrefixAcrossWritesBeforeTailEviction(t *testing.T) {
	t.Parallel()

	tail := &cappedBuffer{max: 8}
	capture := &delLimitCapture{dst: tail}
	split := len(rsyncDelLimitWarn) / 2
	if _, err := capture.Write([]byte(rsyncDelLimitWarn[:split])); err != nil {
		t.Fatalf("first Write() error = %v, want nil", err)
	}
	if capture.limited {
		t.Error("limited = true after a partial prefix, want false")
	}
	if _, err := capture.Write([]byte(rsyncDelLimitWarn[split:] + " discarded tail")); err != nil {
		t.Fatalf("second Write() error = %v, want nil", err)
	}
	if !capture.limited {
		t.Error("limited = false after a split warning prefix, want true")
	}
	if strings.Contains(tail.String(), rsyncDelLimitWarn) {
		t.Errorf("bounded tail = %q, want the matched warning evicted", tail.String())
	}
}

// TestProperty_CappedBufferNeverExceedsMax asserts the two core invariants of
// cappedBuffer across any sequence of writes: the retained bytes never exceed
// max, and they are exactly the last min(total, max) bytes of the
// concatenated input. This robustly kills arithmetic mutations of the
// overflow computation.
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
		if want := string(concat[len(concat)-wantLen:]); got != want {
			rt.Fatalf("buffer = %q, want %q (last %d bytes of input)", got, want, wantLen)
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
// cappedBuffer tests skip: an exact fit (len(p) == max) and a write into an
// already-full buffer. Together they lock the sliding-window form of
// cappedBuffer.Write.
//
// Both boundaries in that form are mutation-equivalent, which is why they are
// documented rather than asserted separately: `len(p) >= c.max` -> `>` reaches
// the default arm, where `overflow` is then `c.buf.Len()`, so Next drops the
// whole retained prefix and the result is identical; and `overflow > 0` ->
// `>= 0` calls Next(0), which is a no-op.
func TestCappedBuffer_writeBoundaries(t *testing.T) {
	t.Parallel()

	// len(p) == max: exact fit, whole input retained.
	assertCappedWrite(t, 4, "abcd", "abcd", 4)
	// len(p) >= max: only the newest max bytes are retained, full length still reported.
	assertCappedWrite(t, 4, "abcde", "bcde", 5)
	// len(p) < max: room to spare, whole input retained.
	assertCappedWrite(t, 4, "ab", "ab", 2)

	// A write into an already-full buffer slides the window forward: the
	// newest bytes are kept and the consumed length is still reported.
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
	if got := b.String(); got != "ore" {
		t.Errorf("full-buffer Write left buffer = %q, want ore (newest bytes)", got)
	}
}
