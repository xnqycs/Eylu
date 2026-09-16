//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The atomic replace is why a snapshot write can never be observed half-finished:
// on POSIX it is a rename, and a rename onto a directory fails rather than
// replacing it. This runs on the Linux and macOS CI legs; it cannot run on a
// Windows host, where the other implementation is selected.
func TestReplaceFileReplacesAtomicallyAndRefusesADirectory(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "snapshot.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "snapshot.tmp")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(source, target); err != nil {
		t.Fatalf("replace = %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "new" {
		t.Fatalf("target = %q err = %v", data, err)
	}
	// The rename consumes the source, so no temporary file is left behind.
	if _, err := os.Stat(source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the temporary file survived the replace")
	}

	// A directory cannot be replaced by a rename. The failure is reported rather
	// than leaving a half-written target, and the directory is untouched.
	blocked := filepath.Join(directory, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(directory, "staged.tmp")
	if err := os.WriteFile(staged, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(staged, blocked); err == nil {
		t.Fatal("replacing a directory succeeded")
	}
	entries, err := os.ReadDir(blocked)
	if err != nil || len(entries) != 0 {
		t.Fatalf("the directory was modified: entries=%v err=%v", entries, err)
	}
	if data, err := os.ReadFile(staged); err != nil || string(data) != "staged" {
		t.Fatalf("the staged file was lost: %q err=%v", data, err)
	}
}

// syncDirectory makes the rename durable, which is what closes the window in which
// the file exists but the directory entry does not.
func TestSyncDirectoryAcceptsADirectoryAndRefusesAMissingPath(t *testing.T) {
	directory := t.TempDir()
	if err := syncDirectory(directory); err != nil {
		t.Fatalf("syncing a directory = %v", err)
	}
	if err := syncDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("syncing a path that does not exist succeeded")
	}
}
