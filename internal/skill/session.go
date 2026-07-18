package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Session struct {
	mu       sync.RWMutex
	registry *Registry
	active   map[string]Activation
	digests  map[string]struct{}
}

func NewSession(registry *Registry, initial map[string]string) *Session {
	session := &Session{registry: registry, active: make(map[string]Activation), digests: make(map[string]struct{})}
	for name, digest := range initial {
		item, ok := registry.Get(name)
		if !ok || item.Digest != digest {
			continue
		}
		activation := Activation{Name: item.Name, Source: item.Source, Entry: item.Entry, Root: item.Root, Digest: item.Digest, AllowedTools: item.AllowedTools}
		session.active[name] = activation
		session.digests[digest] = struct{}{}
	}
	return session
}

func (s *Session) Activate(name, trigger string) (Activation, error) {
	item, ok := s.registry.Get(name)
	if !ok {
		return Activation{}, fmt.Errorf("unknown skill %q", name)
	}
	current, err := ParseDirectory(item.Root, item.Source, item.Trusted)
	if err != nil {
		return Activation{}, fmt.Errorf("reload skill %q: %w", name, err)
	}
	resources, err := listResources(current.Root)
	if err != nil {
		return Activation{}, err
	}
	activation := Activation{
		Name: current.Name, Source: current.Source, Entry: current.Entry, Root: current.Root, Digest: current.Digest,
		AllowedTools: current.AllowedTools, Resources: resources, Body: current.Body, Trigger: trigger, ActivatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.digests[activation.Digest]; exists {
		activation.Duplicate = true
		activation.Body = ""
	}
	s.active[name] = activation
	s.digests[activation.Digest] = struct{}{}
	return activation, nil
}

func (s *Session) ActiveDigests() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string, len(s.active))
	for name, activation := range s.active {
		result[name] = activation.Digest
	}
	return result
}

func (s *Session) IsActive(name string) (Activation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	activation, ok := s.active[name]
	return activation, ok
}

func (s *Session) ReadResource(name, relative string) (string, map[string]any, error) {
	activation, ok := s.IsActive(name)
	if !ok {
		return "", nil, fmt.Errorf("skill %q is not activated", name)
	}
	if strings.TrimSpace(relative) == "" || filepath.IsAbs(relative) {
		return "", nil, fmt.Errorf("resource path must be relative to the skill root")
	}
	rootReal, err := filepath.EvalSymlinks(activation.Root)
	if err != nil {
		return "", nil, err
	}
	candidate := filepath.Join(rootReal, filepath.Clean(relative))
	if !insidePath(rootReal, candidate) {
		return "", nil, fmt.Errorf("resource path is outside the skill root")
	}
	entryInfo, err := os.Lstat(candidate)
	if err != nil {
		return "", nil, err
	}
	if entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.Mode().IsRegular() {
		return "", nil, fmt.Errorf("resource must be a regular non-symlink file")
	}
	if !supportedTextResource(candidate) {
		return "", nil, fmt.Errorf("resource type is not supported as text")
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil || !insidePath(rootReal, real) {
		return "", nil, fmt.Errorf("resource resolves outside the skill root")
	}
	if entryInfo.Size() > MaxResourceBytes {
		return "", nil, fmt.Errorf("resource exceeds %d bytes", MaxResourceBytes)
	}
	file, err := os.Open(real)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil || !os.SameFile(entryInfo, openInfo) {
		return "", nil, fmt.Errorf("resource changed while opening")
	}
	data := make([]byte, openInfo.Size())
	read := 0
	for read < len(data) {
		count, readErr := file.Read(data[read:])
		read += count
		if readErr != nil {
			return "", nil, readErr
		}
	}
	if !utf8.Valid(data) {
		return "", nil, fmt.Errorf("resource is not UTF-8 text")
	}
	return string(data), map[string]any{"skill_name": name, "skill_digest": activation.Digest, "resource": filepath.ToSlash(relative), "bytes": len(data)}, nil
}

func supportedTextResource(path string) bool {
	extension := strings.ToLower(filepath.Ext(path))
	supported := map[string]struct{}{
		".md": {}, ".txt": {}, ".json": {}, ".yaml": {}, ".yml": {}, ".toml": {}, ".xml": {}, ".csv": {}, ".tsv": {},
		".go": {}, ".py": {}, ".js": {}, ".mjs": {}, ".cjs": {}, ".ts": {}, ".tsx": {}, ".jsx": {}, ".sh": {}, ".bash": {}, ".ps1": {}, ".rb": {}, ".pl": {},
		".html": {}, ".css": {}, ".sql": {}, ".ini": {}, ".cfg": {}, ".conf": {}, ".properties": {},
	}
	if _, ok := supported[extension]; ok {
		return true
	}
	if extension == "" {
		name := strings.ToLower(filepath.Base(path))
		return name == "license" || name == "readme" || name == "makefile" || name == "dockerfile"
	}
	return false
}

func listResources(root string) ([]string, error) {
	resources := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		depth := len(strings.Split(filepath.ToSlash(relative), "/"))
		if depth > MaxResourceDepth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() && ignoredSkillDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() || relative == "SKILL.md" {
			return nil
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() {
			resources = append(resources, filepath.ToSlash(relative))
		}
		if len(resources) >= MaxResourcesPerSkill {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(resources)
	return resources, err
}

func insidePath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
