package tool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Below the size bound the intent records the hash of the content, which is strong
// evidence: a match proves the operation did not change the target. Above it the
// intent records a marker instead, so writing a large file does not charge a full
// read to every side effect.
func TestIntentEvidenceIsAHashForSmallTargetsAndAMarkerForLargeOnes(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	small := filepath.Join(workspace, "small.txt")
	if err := os.WriteFile(small, []byte("small content"), 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(workspace, "large.txt")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", previousHashMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}

	smallPath, smallEvidence := write.ReportIntent(json.RawMessage(`{"path":"small.txt"}`))
	// The resolver reports the path with symlinks and short names resolved, so the
	// assertion is about identity and not about the string: on a runner whose temp
	// directory is reached through a symlink the two legitimately differ.
	if !sameFile(t, smallPath, small) {
		t.Fatalf("path = %q, want the same file as %q", smallPath, small)
	}
	if strings.HasPrefix(smallEvidence, weakEvidencePrefix) || smallEvidence != fileContentHash(small) {
		t.Fatalf("a small target should be hashed: %q", smallEvidence)
	}

	largePath, largeEvidence := write.ReportIntent(json.RawMessage(`{"path":"large.txt"}`))
	if !sameFile(t, largePath, large) {
		t.Fatalf("path = %q, want the same file as %q", largePath, large)
	}
	if !strings.HasPrefix(largeEvidence, weakEvidencePrefix) {
		t.Fatalf("a large target should carry a marked weak marker: %q", largeEvidence)
	}
	if largeEvidence == fileContentHash(large) {
		t.Fatal("a large target was hashed anyway")
	}
}

// sameFile reports whether two paths name the same file.
func sameFile(t *testing.T, left, right string) bool {
	t.Helper()
	leftInfo, err := os.Stat(left)
	if err != nil {
		t.Fatalf("stat %q: %v", left, err)
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		t.Fatalf("stat %q: %v", right, err)
	}
	return os.SameFile(leftInfo, rightInfo)
}

// The weakness is real and the marker says so: a rewrite that preserves size and
// modification time leaves the same evidence, so a match is a hint rather than
// proof. This test pins that limitation instead of hiding it.
func TestWeakEvidenceCannotTellARewriteFromTheOriginal(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "large.txt")
	content := []byte(strings.Repeat("a", previousHashMaxBytes+1))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	before := previousContentEvidence(path)

	// Same length, and the timestamp is restored to what it was.
	rewritten := []byte(strings.Repeat("b", len(content)))
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if after := previousContentEvidence(path); after != before {
		t.Fatalf("weak evidence changed for a rewrite that preserved size and time: %q -> %q", before, after)
	}
	// The content really is different, so the marker agreeing means the marker is
	// weak - which is exactly what a reader has to be told.
	if fileContentHash(path) == "" {
		t.Fatal("the fixture is unreadable")
	}
}

// A missing target records no evidence at all, which recovery reports as an
// absence of evidence rather than as proof.
func TestIntentEvidenceForAMissingTargetIsEmpty(t *testing.T) {
	workspace := t.TempDir()
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, evidence := write.ReportIntent(json.RawMessage(`{"path":"missing.txt"}`)); evidence != "" {
		t.Fatalf("evidence = %q, want none", evidence)
	}
}

// A target that is a directory is not evidence either.
func TestIntentEvidenceForADirectoryIsEmpty(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if evidence := previousContentEvidence(filepath.Join(workspace, "sub")); evidence != "" {
		t.Fatalf("evidence = %q, want none", evidence)
	}
	// A timestamp-based marker carries the size it observed, so a reader can see
	// what was measured.
	path := filepath.Join(workspace, "large.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", previousHashMaxBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := previousContentEvidence(path)
	if !strings.Contains(marker, "size=") || !strings.Contains(marker, "mtime=") {
		t.Fatalf("weak marker = %q", marker)
	}
}
