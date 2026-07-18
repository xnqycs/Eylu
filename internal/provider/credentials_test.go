package provider

import (
	"errors"
	"testing"

	"Eylu/internal/config"
)

type fakeKeyring struct {
	values map[string]string
	err    error
}

func (f *fakeKeyring) Set(service, account, secret string) error {
	if f.err != nil {
		return f.err
	}
	f.values[service+"/"+account] = secret
	return nil
}
func (f *fakeKeyring) Get(service, account string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	value, ok := f.values[service+"/"+account]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}
func (f *fakeKeyring) Delete(service, account string) error {
	delete(f.values, service+"/"+account)
	return nil
}

func TestCredentialStoreBackends(t *testing.T) {
	keyring := &fakeKeyring{values: make(map[string]string)}
	store := NewCredentialStoreWith(keyring, func(name string) (string, bool) {
		return map[string]string{"TEST_KEY": "from-env"}[name], name == "TEST_KEY"
	})
	keyringRef := config.CredentialRef{Type: "keyring", Service: "eylu", Account: "provider:test"}
	if err := store.Save(keyringRef, "from-keyring"); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Resolve(keyringRef); err != nil || value != "from-keyring" {
		t.Fatalf("keyring resolve = %q, %v", value, err)
	}
	memoryRef := config.CredentialRef{Type: "memory", Service: "eylu", Account: "provider:memory"}
	if err := store.Save(memoryRef, "from-memory"); err != nil {
		t.Fatal(err)
	}
	if value, _ := store.Resolve(memoryRef); value != "from-memory" {
		t.Fatalf("memory resolve = %q", value)
	}
	if value, _ := store.Resolve(config.CredentialRef{Type: "env", Env: "TEST_KEY"}); value != "from-env" {
		t.Fatalf("env resolve = %q", value)
	}
}
