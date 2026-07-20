package tool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestRepositoryIndexUsesGitIgnoreSemantics(t *testing.T) {
	repository := t.TempDir()
	git(t, repository, "init")
	git(t, repository, "config", "user.email", "test@example.com")
	git(t, repository, "config", "user.name", "Test")
	writeTestFile(t, repository, ".gitignore", "ignored/\n*.log\n!keep.log\nnested/*.tmp\n")
	writeTestFile(t, repository, "main.go", "package main\n")
	writeTestFile(t, repository, "tracked.log", "tracked despite ignore\n")
	writeTestFile(t, repository, "keep.log", "kept\n")
	writeTestFile(t, repository, "ignored/hidden.go", "ignored\n")
	writeTestFile(t, repository, "nested/visible.go", "package nested\n")
	writeTestFile(t, repository, "nested/skip.tmp", "ignored\n")
	writeTestFile(t, repository, "info.txt", "ignored by info exclude\n")
	writeTestFile(t, repository, "global.txt", "ignored globally\n")
	writeTestFile(t, repository, "deleted.go", "package deleted\n")
	writeTestFile(t, repository, "space name.go", "package main\n")
	writeTestFile(t, repository, "中文.go", "package main\n")
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte("info.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalExclude := filepath.Join(repository, "global-excludes")
	if err := os.WriteFile(globalExclude, []byte("global.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repository, "config", "core.excludesFile", globalExclude)
	git(t, repository, "add", ".gitignore", "main.go", "keep.log", "nested/visible.go", "deleted.go", "space name.go", "中文.go")
	git(t, repository, "add", "-f", "tracked.log")
	if err := os.Remove(filepath.Join(repository, "deleted.go")); err != nil {
		t.Fatal(err)
	}
	index, err := NewRepositoryIndex(repository)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := index.Refresh(context.Background())
	if snapshot.Source != "git" || snapshot.Diagnostic != "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	got := relativeNames(snapshot.Files)
	want := []string{".gitignore", "global-excludes", "keep.log", "main.go", "nested/visible.go", "space name.go", "tracked.log", "中文.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}

	subIndex, err := NewRepositoryIndex(filepath.Join(repository, "nested"))
	if err != nil {
		t.Fatal(err)
	}
	subSnapshot := subIndex.Refresh(context.Background())
	if got := relativeNames(subSnapshot.Files); !reflect.DeepEqual(got, []string{"visible.go"}) {
		t.Fatalf("subdirectory files = %#v", got)
	}
}

func TestRepositoryIndexNULSpecialNameAndFallback(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, workspace, "normal.txt", "normal")
	writeTestFile(t, workspace, ".hidden.txt", "hidden")
	if runtime.GOOS != "windows" {
		writeTestFile(t, workspace, "line\nbreak.txt", "newline")
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, workspace, ".git/config", "metadata")
	index, err := newRepositoryIndex(workspace, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("git unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := index.Refresh(context.Background())
	if snapshot.Source != "filesystem" || !strings.Contains(snapshot.Diagnostic, "git unavailable") {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	got := relativeNames(snapshot.Files)
	want := []string{".hidden.txt", "normal.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "line\nbreak.txt")
		sort.Strings(want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %#v, want %#v", got, want)
	}
}

func TestRepositoryIndexResolvesExactAndUniqueIgnoredReferences(t *testing.T) {
	workspace := t.TempDir()
	git(t, workspace, "init")
	writeTestFile(t, workspace, ".gitignore", "build/\n")
	writeTestFile(t, workspace, "build/index.html", "<main>Eylu</main>\n")
	writeTestFile(t, workspace, "src/main.go", "package main\n")
	index, err := NewRepositoryIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}

	exact, err := index.ResolveFileReference(context.Background(), "build/index.html")
	if err != nil || exact.Relative != "build/index.html" {
		t.Fatalf("exact=%#v err=%v", exact, err)
	}
	unique, err := index.ResolveFileReference(context.Background(), "index.html")
	if err != nil || unique.Relative != "build/index.html" {
		t.Fatalf("unique=%#v err=%v", unique, err)
	}
}

func TestRepositoryIndexRejectsAmbiguousAndEscapingReferences(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, workspace, "a/index.html", "a")
	writeTestFile(t, workspace, "b/index.html", "b")
	index, err := NewRepositoryIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := index.ResolveFileReference(context.Background(), "index.html"); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "a/index.html") || !strings.Contains(err.Error(), "b/index.html") {
		t.Fatalf("ambiguous err=%v", err)
	}
	if _, err := index.ResolveFileReference(context.Background(), "../outside.txt"); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("escape err=%v", err)
	}
}

func git(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func relativeNames(files []IndexedFile) []string {
	result := make([]string, len(files))
	for index, file := range files {
		result[index] = file.Relative
	}
	return result
}
