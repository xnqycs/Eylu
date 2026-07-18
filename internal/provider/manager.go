package provider

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"Eylu/internal/config"
)

type SaveFunc func(string, config.Config) error

type Snapshot struct {
	Name       string
	Config     config.ProviderConfig
	Generation uint64
}

type Manager struct {
	mu         sync.Mutex
	path       string
	save       SaveFunc
	cfg        config.Config
	generation uint64
	active     atomic.Pointer[Snapshot]
}

func NewManager(path string, cfg config.Config, save SaveFunc) (*Manager, error) {
	if save == nil {
		save = config.Save
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	m := &Manager{path: path, save: save, cfg: cfg.Clone(), generation: 1}
	m.publishActive()
	return m, nil
}

func (m *Manager) Config() config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Clone()
}

func (m *Manager) Active() (Snapshot, error) {
	snapshot := m.active.Load()
	if snapshot == nil {
		return Snapshot{}, errors.New("no active provider configured")
	}
	return *snapshot, nil
}

func (m *Manager) Get(name string) (config.ProviderConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.cfg.Providers[name]
	return p, ok
}

func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := config.ProviderNames(m.cfg)
	result := make([]Snapshot, 0, len(names))
	for _, name := range names {
		result = append(result, Snapshot{Name: name, Config: m.cfg.Providers[name], Generation: m.generation})
	}
	return result
}

func (m *Manager) Upsert(name string, provider config.ProviderConfig, activate bool) error {
	if err := config.ValidateProvider(name, provider); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	candidate := m.cfg.Clone()
	candidate.Providers[name] = provider
	if activate || candidate.ActiveProvider == "" {
		candidate.ActiveProvider = name
	}
	return m.commit(candidate)
}

func (m *Manager) Use(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q does not exist", name)
	}
	candidate := m.cfg.Clone()
	candidate.ActiveProvider = name
	return m.commit(candidate)
}

func (m *Manager) Delete(name, replacement string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cfg.Providers[name]; !ok {
		return fmt.Errorf("provider %q does not exist", name)
	}
	candidate := m.cfg.Clone()
	delete(candidate.Providers, name)
	if candidate.ActiveProvider == name {
		if replacement != "" {
			if _, ok := candidate.Providers[replacement]; !ok {
				return fmt.Errorf("replacement provider %q does not exist", replacement)
			}
			candidate.ActiveProvider = replacement
		} else if len(candidate.Providers) == 1 {
			for only := range candidate.Providers {
				candidate.ActiveProvider = only
			}
		} else if len(candidate.Providers) == 0 {
			candidate.ActiveProvider = ""
		} else {
			names := make([]string, 0, len(candidate.Providers))
			for providerName := range candidate.Providers {
				names = append(names, providerName)
			}
			sort.Strings(names)
			return fmt.Errorf("active provider requires a replacement; available: %v", names)
		}
	}
	return m.commit(candidate)
}

func (m *Manager) commit(candidate config.Config) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	if err := m.save(m.path, candidate); err != nil {
		return err
	}
	m.cfg = candidate
	m.generation++
	m.publishActive()
	return nil
}

func (m *Manager) publishActive() {
	if m.cfg.ActiveProvider == "" {
		m.active.Store(nil)
		return
	}
	provider := m.cfg.Providers[m.cfg.ActiveProvider]
	snapshot := &Snapshot{Name: m.cfg.ActiveProvider, Config: provider, Generation: m.generation}
	m.active.Store(snapshot)
}
