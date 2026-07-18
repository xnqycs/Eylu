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

func TestManagerInFlightSnapshotAndNextGeneration(t *testing.T) {
	cfg := config.Default(t.TempDir())
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://one.example/v1", Model: "one", Credential: config.CredentialRef{Type: "none"}}
	manager, err := NewManager("unused", cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	inFlight, _ := manager.Active()
	updated := inFlight.Config
	updated.BaseURL = "https://two.example/v1"
	updated.Model = "two"
	if err := manager.Upsert("work", updated, true); err != nil {
		t.Fatal(err)
	}
	next, _ := manager.Active()
	if inFlight.Config.Model != "one" || inFlight.Config.BaseURL != "https://one.example/v1" {
		t.Fatalf("in-flight snapshot changed: %#v", inFlight)
	}
	if next.Config.Model != "two" || next.Generation <= inFlight.Generation {
		t.Fatalf("next snapshot = %#v", next)
	}
}
