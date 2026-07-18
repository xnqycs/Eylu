package provider

import (
	"errors"
	"testing"

	"Eylu/internal/config"
)

func TestManagerPublishesAfterSaveAndRollsBack(t *testing.T) {
	cfg := config.Default(t.TempDir())
	failed := false
	manager, err := NewManager("unused", cfg, func(_ string, _ config.Config) error {
		if failed {
			return errors.New("disk full")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "one",
		Credential: config.CredentialRef{Type: "none"},
	}
	if err := manager.Upsert("work", providerConfig, true); err != nil {
		t.Fatal(err)
	}
	before, _ := manager.Active()
	failed = true
	providerConfig.Model = "two"
	if err := manager.Upsert("work", providerConfig, true); err == nil {
		t.Fatal("expected save failure")
	}
	after, _ := manager.Active()
	if after.Config.Model != "one" || after.Generation != before.Generation {
		t.Fatalf("failed update was published: %#v", after)
	}
}
