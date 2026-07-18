package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const MaxIndexedFiles = 100_000

type IndexedFile struct {
	Relative string `json:"relative"`
	Absolute string `json:"absolute"`
	Size     int64  `json:"size"`
}

type IndexSnapshot struct {
	Source     string        `json:"source"`
	Repository string        `json:"repository,omitempty"`
	Workspace  string        `json:"workspace"`
	Files      []IndexedFile `json:"files"`
	Diagnostic string        `json:"diagnostic,omitempty"`
}

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type RepositoryIndex struct {
	mu        sync.RWMutex
	workspace string
	paths     *pathResolver
	run       commandRunner
	snapshot  IndexSnapshot
}

func NewRepositoryIndex(workspace string) (*RepositoryIndex, error) {
	return newRepositoryIndex(workspace, runCommand)
}

func newRepositoryIndex(workspace string, runner commandRunner) (*RepositoryIndex, error) {
	paths, err := newPathResolver(workspace)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		runner = runCommand
	}
	return &RepositoryIndex{workspace: paths.real, paths: paths, run: runner}, nil
}

func (i *RepositoryIndex) Refresh(ctx context.Context) IndexSnapshot {
	snapshot, err := i.fromGit(ctx)
	if err != nil {
		snapshot = i.fromFilesystem(ctx)
		snapshot.Diagnostic = err.Error()
	}
	i.mu.Lock()
	i.snapshot = snapshot
	i.mu.Unlock()
	return cloneSnapshot(snapshot)
}

func (i *RepositoryIndex) Snapshot() IndexSnapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneSnapshot(i.snapshot)
}

func (i *RepositoryIndex) fromGit(ctx context.Context) (IndexSnapshot, error) {
	rootOutput, err := i.run(ctx, "git", "-C", i.workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return IndexSnapshot{}, fmt.Errorf("git root discovery failed: %w", err)
	}
	repository := strings.TrimSpace(string(rootOutput))
	if repository == "" {
		return IndexSnapshot{}, fmt.Errorf("git returned an empty repository root")
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return IndexSnapshot{}, err
	}
	output, err := i.run(ctx, "git", "-C", repository, "ls-files", "--cached", "--others", "--exclude-standard", "--full-name", "-z")
	if err != nil {
		return IndexSnapshot{}, fmt.Errorf("git file index failed: %w", err)
	}
	files := make([]IndexedFile, 0)
	for _, item := range bytes.Split(output, []byte{0}) {
		if len(item) == 0 {
			continue
		}
		absolute := filepath.Join(repository, filepath.FromSlash(string(item)))
		if !inside(i.workspace, absolute) {
			continue
		}
		info, statErr := os.Lstat(absolute)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		relative, relErr := filepath.Rel(i.workspace, absolute)
		if relErr != nil {
			continue
		}
		files = append(files, IndexedFile{Relative: filepath.ToSlash(relative), Absolute: absolute, Size: info.Size()})
		if len(files) >= MaxIndexedFiles {
			break
		}
	}
	sortFiles(files)
	return IndexSnapshot{Source: "git", Repository: repository, Workspace: i.workspace, Files: files}, nil
}

func (i *RepositoryIndex) fromFilesystem(ctx context.Context) IndexSnapshot {
	files := make([]IndexedFile, 0)
	diagnostic := ""
	err := filepath.WalkDir(i.workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == i.workspace {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(i.workspace, path)
		if err != nil {
			return nil
		}
		files = append(files, IndexedFile{Relative: filepath.ToSlash(relative), Absolute: path, Size: info.Size()})
		if len(files) >= MaxIndexedFiles {
			diagnostic = fmt.Sprintf("filesystem index truncated at %d files", MaxIndexedFiles)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && diagnostic == "" {
		diagnostic = err.Error()
	}
	sortFiles(files)
	return IndexSnapshot{Source: "filesystem", Workspace: i.workspace, Files: files, Diagnostic: diagnostic}
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = minimalEnvironment()
	stdout := &cappedBuffer{limit: 64 << 20}
	stderr := &cappedBuffer{limit: 1 << 20}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return nil, fmt.Errorf("%s: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil, err
	}
	if stdout.truncated {
		return nil, fmt.Errorf("command output exceeded 64 MiB")
	}
	return append([]byte(nil), stdout.buffer.Bytes()...), nil
}

func sortFiles(files []IndexedFile) {
	sort.Slice(files, func(a, b int) bool { return files[a].Relative < files[b].Relative })
}

func cloneSnapshot(snapshot IndexSnapshot) IndexSnapshot {
	clone := snapshot
	clone.Files = append([]IndexedFile(nil), snapshot.Files...)
	return clone
}
