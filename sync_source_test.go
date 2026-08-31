package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

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

	t.Run("missing path returns error", func(t *testing.T) {
		t.Parallel()
		empty, err := sourceIsEmpty(filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Errorf("sourceIsEmpty(missing path) = %v, nil, want an error", empty)
		}
		if empty {
			t.Error("sourceIsEmpty(missing path) = true, want false")
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

// TestSourceIsEmpty_readErrorIsReturnedNotSkipped covers a source path that
// is a regular file rather than a directory. A broken source must be returned
// as an error rather than reported as a benign empty one, and the classifier
// must emit nothing because runJob owns the diagnostic.
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

// TestSourceIsEmpty_fifoReturnsErrorWithoutBlocking pins the directory-only
// open. An ordinary read-only fifo open waits for a writer before runJob has a
// timeout context.
func TestSourceIsEmpty_fifoReturnsErrorWithoutBlocking(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "source.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		empty, err := sourceIsEmpty(path)
		if err == nil {
			done <- fmt.Errorf("sourceIsEmpty(fifo) = %v, nil, want an error", empty)
			return
		}
		if empty {
			done <- errors.New("sourceIsEmpty(fifo) = true, want false")
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(fd)
		}
		t.Fatal("sourceIsEmpty(fifo) blocked for a writer")
	}
}
