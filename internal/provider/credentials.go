package provider

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/zalando/go-keyring"

	"Eylu/internal/config"
)

var ErrCredentialNotFound = errors.New("credential not found")

type Keyring interface {
	Set(service, account, secret string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

type systemKeyring struct{}

func (systemKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}
func (systemKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}
func (systemKeyring) Delete(service, account string) error { return keyring.Delete(service, account) }

type CredentialStore struct {
	mu      sync.RWMutex
	memory  map[string]string
	keyring Keyring
	lookup  func(string) (string, bool)
}

func NewCredentialStore() *CredentialStore {
	return NewCredentialStoreWith(systemKeyring{}, os.LookupEnv)
}

func NewCredentialStoreWith(k Keyring, lookup func(string) (string, bool)) *CredentialStore {
	return &CredentialStore{memory: make(map[string]string), keyring: k, lookup: lookup}
}

func (s *CredentialStore) Save(ref config.CredentialRef, secret string) error {
	if secret == "" {
		return errors.New("credential value is empty")
	}
	switch ref.Type {
	case "keyring", "":
		service, account := keyringNames(ref)
		return s.keyring.Set(service, account, secret)
	case "memory":
		s.mu.Lock()
		s.memory[memoryKey(ref)] = secret
		s.mu.Unlock()
		return nil
	case "env":
		return errors.New("environment credentials are supplied by the process environment")
	case "none":
		return errors.New("credential type none cannot store a secret")
	default:
		return fmt.Errorf("unknown credential type %q", ref.Type)
	}
}

func (s *CredentialStore) Resolve(ref config.CredentialRef) (string, error) {
	switch ref.Type {
	case "keyring", "":
		service, account := keyringNames(ref)
		value, err := s.keyring.Get(service, account)
		if err != nil {
			return "", fmt.Errorf("%w: keyring %s/%s", ErrCredentialNotFound, service, account)
		}
		return value, nil
	case "memory":
		s.mu.RLock()
		value, ok := s.memory[memoryKey(ref)]
		s.mu.RUnlock()
		if !ok {
			return "", ErrCredentialNotFound
		}
		return value, nil
	case "env":
		value, ok := s.lookup(ref.Env)
		if !ok || value == "" {
			return "", fmt.Errorf("%w: environment variable %s", ErrCredentialNotFound, ref.Env)
		}
		return value, nil
	case "none":
		return "", nil
	default:
		return "", fmt.Errorf("unknown credential type %q", ref.Type)
	}
}

func (s *CredentialStore) Delete(ref config.CredentialRef) error {
	switch ref.Type {
	case "keyring", "":
		service, account := keyringNames(ref)
		return s.keyring.Delete(service, account)
	case "memory":
		s.mu.Lock()
		delete(s.memory, memoryKey(ref))
		s.mu.Unlock()
		return nil
	case "env", "none":
		return nil
	default:
		return fmt.Errorf("unknown credential type %q", ref.Type)
	}
}

func keyringNames(ref config.CredentialRef) (string, string) {
	service, account := ref.Service, ref.Account
	if service == "" {
		service = "eylu"
	}
	if account == "" {
		account = "default"
	}
	return service, account
}

func memoryKey(ref config.CredentialRef) string {
	return ref.Service + "\x00" + ref.Account
}
