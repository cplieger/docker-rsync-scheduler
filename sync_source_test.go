package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/slogx/capture"
)

// emptyOf drives sourceIsEmpty for a case that must not error, failing the
// test if it does.
func emptyOf(t *testing.T, path string) bool {
	t.Helper()
	empty, err := sourceIsEmpty(path)
	if err != nil {
		t.Fatalf("sourceIsEmpty(%q) = _, %v, want no error", path, err)
	}
	return empty
}

func TestSourceIsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("empty dir is empty", func(t *testing.T) {
		t.Parallel()
		if !emptyOf(t, t.TempDir()) {
			t.Error("sourceIsEmpty on empty dir = false, want true")
		}
	})

	t.Run("dir with file is not empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if emptyOf(t, dir) {
			t.Error("sourceIsEmpty on populated dir = true, want false")
		}
	})

	t.Run("missing path is empty", func(t *testing.T) {
		t.Parallel()
		if !emptyOf(t, filepath.Join(t.TempDir(), "nope")) {
			t.Error("sourceIsEmpty on missing path = false, want true")
		}
	})
}

// TestSourceIsEmpty_onlyGloballyExcludedEntriesIsEmpty pins the h-f1 fix: a
// source whose top-level holds ONLY globally-excluded entries (e.g. a Syncthing
// folder reduced to just .stfolder, or a macOS dir holding only .DS_Store) must
// report empty, so a delete:true job skips it instead of letting rsync --delete
// wipe the remote after the post-exclude sender list comes up empty. A real
// file alongside an excluded entry must still mirror.
func TestSourceIsEmpty_onlyGloballyExcludedEntriesIsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("only globally-excluded entries is empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for _, name := range globalExcludes {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
				t.Fatalf("setup: %v", err)
			}
		}
		if !emptyOf(t, dir) {
			t.Error("sourceIsEmpty on an excludes-only dir = false, want true (must skip to protect the remote)")
		}
	})

	t.Run("an excluded entry plus a real file is not empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".stfolder"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "real.conf"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if emptyOf(t, dir) {
			t.Error("sourceIsEmpty on a dir with a real file = true, want false (must mirror)")
		}
	})
}

// TestSourceIsEmpty_readErrorIsReturnedNotSkipped covers the arm where os.Open
// succeeds but Readdirnames returns a non-EOF error: a source path that is a
// regular file, not a directory. A broken source must be returned as an error
// rather than reported as a benign empty one, and the classifier must emit
// nothing -- runJob owns the diagnostic.
func TestSourceIsEmpty_readErrorIsReturnedNotSkipped(t *testing.T) {
	rec := capture.Default(t)

	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	empty, err := sourceIsEmpty(path)

	if err == nil {
		t.Errorf("sourceIsEmpty(regular file) = %v, nil, want an error", empty)
	}
	if empty {
		t.Error("sourceIsEmpty(regular file) = true, want false (a broken source is not an empty one)")
	}
	if got := rec.Messages(); len(got) != 0 {
		t.Errorf("sourceIsEmpty(regular file) logs = %q, want none (the caller reports)", got)
	}
}

// TestSourceIsEmpty_openErrorIsReturnedNotSkipped covers the other arm:
// os.Open itself fails with a non-ErrNotExist error. A path whose parent
// component is a regular file yields ENOTDIR (not ENOENT), independent of uid
// (so it is reliable under the root-by-design container, unlike a chmod-0
// dir). The expected missing-dir (ENOENT) case stays a silent empty; this
// asserts the error arm so the two are not collapsed.
func TestSourceIsEmpty_openErrorIsReturnedNotSkipped(t *testing.T) {
	rec := capture.Default(t)

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := filepath.Join(parent, "child")

	empty, err := sourceIsEmpty(path)

	if err == nil {
		t.Errorf("sourceIsEmpty(path under a file) = %v, nil, want an error", empty)
	}
	if empty {
		t.Error("sourceIsEmpty(path under a file) = true, want false (a broken source is not an empty one)")
	}
	if got := rec.Messages(); len(got) != 0 {
		t.Errorf("sourceIsEmpty(path under a file) logs = %q, want none (the caller reports)", got)
	}
}
