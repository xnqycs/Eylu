package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type trustFile struct {
	Version    int             `json:"version"`
	Workspaces map[string]bool `json:"workspaces"`
}

type TrustStore struct {
	mu   sync.RWMutex
	path string
	data trustFile
}

func DefaultTrustPath() string {
	if state := os.Getenv("EYLU_STATE_DIR"); state != "" {
		return filepath.Join(state, "trust.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".eylu", "state", "trust.json")
}

func OpenTrustStore(path string) (*TrustStore, error) {
	if path == "" {
		path = DefaultTrustPath()
	}
	store := &TrustStore{path: path, data: trustFile{Version: 1, Workspaces: make(map[string]bool)}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &store.data); err != nil {
		return nil, fmt.Errorf("parse trust store: %w", err)
	}
	if store.data.Version != 1 || store.data.Workspaces == nil {
		return nil, fmt.Errorf("unsupported trust store version")
	}
	return store, nil
}

func (s *TrustStore) IsTrusted(workspace string) bool {
	normalized, err := normalizeWorkspace(workspace)
	if err != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Workspaces[normalized]
}

func (s *TrustStore) Trust(workspace string) error {
	return s.set(workspace, true)
}

func (s *TrustStore) Revoke(workspace string) error {
	return s.set(workspace, false)
}

func (s *TrustStore) set(workspace string, trusted bool) error {
	normalized, err := normalizeWorkspace(workspace)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if trusted {
		s.data.Workspaces[normalized] = true
	} else {
		delete(s.data.Workspaces, normalized)
	}
	return s.save()
}

func (s *TrustStore) save() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".trust-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceTrustFile(name, s.path)
}

func normalizeWorkspace(workspace string) (string, error) {
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	if real, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		absolute = real
	}
	absolute = filepath.Clean(absolute)
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
}
