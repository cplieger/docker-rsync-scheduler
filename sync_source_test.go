package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/slogx/capture"
)

func TestSourceIsEmpty(t *testing.T) {
	t.Parallel()

	t.Run("empty dir is empty", func(t *testing.T) {
		t.Parallel()
		if !sourceIsEmpty(t.TempDir()) {
			t.Error("sourceIsEmpty on empty dir = false, want true")
		}
	})

	t.Run("dir with file is not empty", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if sourceIsEmpty(dir) {
			t.Error("sourceIsEmpty on populated dir = true, want false")
		}
	})

	t.Run("missing path is empty", func(t *testing.T) {
		t.Parallel()
		if !sourceIsEmpty(filepath.Join(t.TempDir(), "nope")) {
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
		if !sourceIsEmpty(dir) {
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
		if sourceIsEmpty(dir) {
			t.Error("sourceIsEmpty on a dir with a real file = true, want false (must mirror)")
		}
	})
}

// TestSourceIsEmpty_unreadableDirSurfacesWarnAndSkips covers the cycle-2
// error-surfacing arm where os.Open succeeds but Readdirnames returns a
// non-EOF error: a source path that is a regular file (not a directory).
// sourceIsEmpty must still report empty (true) so a broken source cannot let
// --delete wipe the remote, AND it must emit a WARN so the breakage is not
// masked as a benign empty source. Asserting the WARN is what distinguishes
// this arm from the silent missing-dir case.
func TestSourceIsEmpty_unreadableDirSurfacesWarnAndSkips(t *testing.T) {
	rec := capture.Default(t)

	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got := sourceIsEmpty(path)

	if !got {
		t.Errorf("sourceIsEmpty(regular file) = false, want true (skip to protect remote)")
	}
	if rec.CountLevel(slog.LevelWarn, "source unreadable") == 0 {
		t.Errorf("sourceIsEmpty(regular file) logs = %q, want a 'source unreadable' WARN", rec.Messages())
	}
}

// TestSourceIsEmpty_openErrorSurfacesWarnAndSkips covers the other cycle-2
// arm: os.Open itself fails with a non-ErrNotExist error. A path whose parent
// component is a regular file yields ENOTDIR (not ENOENT), independent of uid
// (so it is reliable under the root-by-design container, unlike a chmod-0
// dir). The expected missing-dir (ENOENT) case stays silent; this asserts the
// non-silent arm so the two are not collapsed.
func TestSourceIsEmpty_openErrorSurfacesWarnAndSkips(t *testing.T) {
	rec := capture.Default(t)

	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	path := filepath.Join(parent, "child")

	got := sourceIsEmpty(path)

	if !got {
		t.Errorf("sourceIsEmpty(path under a file) = false, want true (skip to protect remote)")
	}
	if rec.CountLevel(slog.LevelWarn, "source unreadable") == 0 {
		t.Errorf("sourceIsEmpty(path under a file) logs = %q, want a 'source unreadable' WARN", rec.Messages())
	}
}
