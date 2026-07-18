package tool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func (r *pathResolver) forWrite(path string, createParents bool) (string, error) {
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

func inside(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative))
}
