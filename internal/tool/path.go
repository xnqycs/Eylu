package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type pathResolver struct {
	root string
	real string
}

func newPathResolver(workspace string) (*pathResolver, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	return &pathResolver{root: filepath.Clean(root), real: filepath.Clean(real)}, nil
}

func (r *pathResolver) existing(path string) (string, error) {
	candidate, err := r.lexical(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !inside(r.real, real) {
		return "", errors.New("path resolves outside workspace")
	}
	return real, nil
}

// forWrite resolves a path for writing and optionally creates its parents. The
// cancellation check runs before the directory creation, which is the first side
// effect of a write.
func (r *pathResolver) forWrite(ctx context.Context, path string, createParents bool) (string, error) {
	candidate, err := r.lexical(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(candidate)
	if createParents {
		ancestor := parent
		for {
			if _, statErr := os.Lstat(ancestor); statErr == nil {
				break
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", statErr
			}
			next := filepath.Dir(ancestor)
			if next == ancestor {
				return "", errors.New("cannot find an existing parent inside workspace")
			}
			ancestor = next
		}
		realParent, err := filepath.EvalSymlinks(ancestor)
		if err != nil || !inside(r.real, realParent) {
			return "", errors.New("parent resolves outside workspace")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", err
		}
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve parent: %w", err)
	}
	if !inside(r.real, realParent) {
		return "", errors.New("parent resolves outside workspace")
	}
	return filepath.Join(realParent, filepath.Base(candidate)), nil
}

// writeTarget resolves the path a write would produce without changing anything:
// no directory is created and no file is touched.
//
// It exists because describing an intent and performing the write are two
// different phases, and only the second one may modify the filesystem. The path
// an intent records therefore comes from here, while the write itself still goes
// through forWrite, which creates the parents - after the intent is durable.
//
// The resolution matches forWrite whenever forWrite can succeed: the deepest
// existing ancestor is resolved through the real filesystem and checked to stay
// inside the workspace, and the missing tail is rebuilt on top of it. A path
// whose parent does not exist yet and whose call did not ask for it to be created
// still resolves here; the intent then names the target the call aimed at, and the
// call itself fails at execution and records that failure.
func (r *pathResolver) writeTarget(path string) (string, error) {
	candidate, err := r.lexical(path)
	if err != nil {
		return "", err
	}
	dir, err := r.resolvedDirectory(filepath.Dir(candidate))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(candidate)), nil
}

// resolvedDirectory rebuilds a directory path from its deepest existing ancestor
// without creating anything, and verifies through the real filesystem that the
// result stays inside the workspace.
func (r *pathResolver) resolvedDirectory(dir string) (string, error) {
	ancestor := dir
	tail := make([]string, 0)
	for {
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if !info.IsDir() {
				// Lstat does not follow the link, so a symlink may still name a
				// directory: the resolved form is what decides.
				if resolved, err := os.Stat(ancestor); err != nil || !resolved.IsDir() {
					return "", fmt.Errorf("%s is not a directory", ancestor)
				}
			}
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("cannot find an existing parent inside workspace")
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
	real, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	if !inside(r.real, real) {
		return "", errors.New("parent resolves outside workspace")
	}
	resolved := real
	for index := len(tail) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, tail[index])
	}
	return resolved, nil
}

func (r *pathResolver) lexical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.root, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if !inside(r.root, candidate) {
		return "", errors.New("path is outside workspace")
	}
	return filepath.Clean(candidate), nil
}

// resourcePath resolves the existing portion of a path without creating it.
// It gives the scheduler one stable key for aliases, symlinks and new files.
func (r *pathResolver) resourcePath(path string) (string, error) {
	candidate, err := r.lexical(path)
	if err != nil {
		return "", err
	}
	ancestor := candidate
	tail := make([]string, 0)
	for {
		if _, statErr := os.Lstat(ancestor); statErr == nil {
			break
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("cannot find an existing parent inside workspace")
		}
		tail = append(tail, filepath.Base(ancestor))
		ancestor = parent
	}
	realAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", err
	}
	if !inside(r.real, realAncestor) {
		return "", errors.New("path resolves outside workspace")
	}
	resolved := realAncestor
	for index := len(tail) - 1; index >= 0; index-- {
		resolved = filepath.Join(resolved, tail[index])
	}
	return resourceKey(resolved), nil
}

// rootResourceKey is the canonical key of the workspace tree itself.
func (r *pathResolver) rootResourceKey() string {
	return resourceKey(r.real)
}

// resourceKey is the single entry point that turns a path into the canonical key
// used to detect conflicting work. Every tool and the coordinator build their
// claims through it, so two aliases of the same file always produce one key.
func resourceKey(path string) string {
	return resourceKeyFor(runtime.GOOS, path)
}

// resourceKeyFor implements the platform policy of resourceKey and is exported to
// tests so both policies can be verified from any platform.
//
// The key is cleaned, separator-normalized and stripped of a trailing separator
// (except for a volume root). On Windows the key is additionally lowercased,
// because the filesystem is case-insensitive there: this may serialize two names
// that Windows would treat as distinct, but it never misses a conflict, and a
// missed conflict is the dangerous direction.
//
// Known boundaries, which deliberately fall back to serialization rather than to
// a guess: hard links and other path aliases that resolve to the same file
// through different paths, a UNC path versus the drive letter it is mapped to,
// and per-directory case sensitivity on Windows. A path whose identity cannot be
// established must stay exclusive.
//
// One more boundary is checked rather than assumed. Cleaning is not a normal form
// for every Windows volume-relative spelling - fuzzing found a three-character
// path that cleans to one value and cleans again to another - and a key that
// changes when it is passed through a second time is not a key: two spellings of
// one path could produce two of them, which is the missed conflict this function
// exists to prevent. A value that is not its own cleaning result is therefore
// refused as an unknown identity.
func resourceKeyFor(goos, path string) string {
	key := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if key == "." || key == "" {
		return ""
	}
	if goos == "windows" {
		key = strings.ToLower(key)
	}
	if key != "/" && !strings.HasSuffix(key, ":/") {
		key = strings.TrimSuffix(key, "/")
	}
	if filepath.ToSlash(filepath.Clean(key)) != key {
		return ""
	}
	return key
}

func inside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}
