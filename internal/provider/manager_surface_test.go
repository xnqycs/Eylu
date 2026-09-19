package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"Eylu/internal/config"
)

// managerFixture is a manager with two providers and no store, so the surface can
// be exercised without a filesystem.
func managerFixture(t *testing.T, save SaveFunc) *Manager {
	t.Helper()
	cfg := config.Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://one.example/v1", Model: "one"}
	cfg.Providers["personal"] = config.ProviderConfig{Adapter: "openai_chat", BaseURL: "https://two.example/v1", Model: "two"}
	if save == nil {
		save = func(string, config.Config) error { return nil }
	}
	manager, err := NewManager("unused", cfg, save)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// The read side of the manager answers from the configuration it holds, and every
// answer is a copy: a caller that edits what it was given must not change what the
// manager holds.
func TestManagerReadsItsConfigurationByName(t *testing.T) {
	manager := managerFixture(t, nil)

	work, ok := manager.Get("work")
	if !ok || work.Model != "one" {
		t.Fatalf("Get(work) = %#v, %t", work, ok)
	}
	if _, ok := manager.Get("missing"); ok {
		t.Fatal("Get resolved a provider that does not exist")
	}

	snapshot, ok := manager.Snapshot("personal")
	if !ok || snapshot.Name != "personal" || snapshot.Config.Model != "two" || snapshot.Generation == 0 {
		t.Fatalf("Snapshot(personal) = %#v, %t", snapshot, ok)
	}
	if _, ok := manager.Snapshot("missing"); ok {
		t.Fatal("Snapshot resolved a provider that does not exist")
	}

	list := manager.List()
	if len(list) != 2 || list[0].Name != "personal" || list[1].Name != "work" {
		t.Fatalf("List() = %#v, want the providers in a stable order", list)
	}
	if list[0].Generation != snapshot.Generation {
		t.Fatalf("two answers disagree about the generation: %d / %d", list[0].Generation, snapshot.Generation)
	}

	active, err := manager.Active()
	if err != nil || active.Name != "work" || active.Config.Model != "one" {
		t.Fatalf("Active() = %#v, %v", active, err)
	}
	// The active snapshot is a copy: editing the configuration the manager hands
	// out does not edit the manager.
	cfg := manager.Config()
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://elsewhere/v1", Model: "changed"}
	cfg.Providers["added"] = config.ProviderConfig{Adapter: "openai_chat", BaseURL: "https://three/v1", Model: "three"}
	after, _ := manager.Active()
	if after.Config.Model != "one" {
		t.Fatalf("the configuration handed out aliases the manager's own: %#v", after)
	}
	if _, ok := manager.Get("added"); ok {
		t.Fatal("a provider added to the handed-out configuration reached the manager")
	}
}

// A manager with no active provider says so rather than returning an empty one,
// because "no provider" and "a provider with an empty name" are different states.
func TestManagerWithoutAnActiveProviderReportsNone(t *testing.T) {
	cfg := config.Default()
	cfg.ActiveProvider = ""
	cfg.Providers = map[string]config.ProviderConfig{}
	manager, err := NewManager("unused", cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Active(); err == nil || !strings.Contains(err.Error(), "no active provider") {
		t.Fatalf("Active() = %v, want a missing provider", err)
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("List() = %#v, want none", list)
	}
	if snapshot, ok := manager.Snapshot("work"); ok || snapshot.Name != "" {
		t.Fatalf("Snapshot() = %#v, %t, want a missing provider", snapshot, ok)
	}
}

// Switching the active provider, and refusing to switch to one that does not
// exist, is what stops a request from being routed to nothing.
func TestManagerUseActivatesAndRefusesAnUnknownProvider(t *testing.T) {
	manager := managerFixture(t, nil)
	if err := manager.Use("personal"); err != nil {
		t.Fatal(err)
	}
	active, _ := manager.Active()
	if active.Name != "personal" || active.Config.Model != "two" {
		t.Fatalf("the active provider did not change: %#v", active)
	}
	if err := manager.Use("missing"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Use(missing) = %v", err)
	}
	after, _ := manager.Active()
	if after.Name != "personal" {
		t.Fatalf("a refused switch changed the active provider: %#v", after)
	}
}

// A save that fails leaves the manager exactly as it was, for every write, not only
// for an upsert.
func TestManagerRollsBackEveryWriteWhenTheSaveFails(t *testing.T) {
	failing := false
	manager := managerFixture(t, func(string, config.Config) error {
		if failing {
			return errors.New("disk full")
		}
		return nil
	})
	before, _ := manager.Active()
	failing = true

	if err := manager.Use("personal"); err == nil {
		t.Fatal("a switch was accepted although the save failed")
	}
	if err := manager.SetGradientEnabled(true); err == nil {
		t.Fatal("a gradient change was accepted although the save failed")
	}
	if err := manager.Upsert("work", config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://one.example/v1", Model: "changed"}, true); err == nil {
		t.Fatal("an update was accepted although the save failed")
	}
	if err := manager.Delete("personal", "work"); err == nil {
		t.Fatal("a deletion was accepted although the save failed")
	}
	after, _ := manager.Active()
	if after.Name != before.Name || after.Config.Model != before.Config.Model || after.Generation != before.Generation {
		t.Fatalf("a failed write was published: %#v (was %#v)", after, before)
	}
	if _, ok := manager.Get("personal"); !ok {
		t.Fatal("a refused deletion removed the provider")
	}
}

// A gradient change is validated and saved like any other write, and the value it
// was given is what is stored.
func TestManagerSetGradientEnabledWritesTheConfiguration(t *testing.T) {
	manager := managerFixture(t, nil)
	if manager.Config().GradientEnabled {
		t.Fatal("the fixture starts with the gradient enabled")
	}
	if err := manager.SetGradientEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !manager.Config().GradientEnabled {
		t.Fatal("the gradient change was not stored")
	}
	if err := manager.SetGradientEnabled(false); err != nil {
		t.Fatal(err)
	}
	if manager.Config().GradientEnabled {
		t.Fatal("the gradient change was not reversible")
	}
}

// Deleting the active provider needs somewhere to point afterwards. The manager
// chooses for itself when only one provider is left, and says which ones could be
// the replacement when there is a real choice.
func TestManagerDeleteRequiresAReplacementForTheActiveProvider(t *testing.T) {
	manager := managerFixture(t, nil)
	third := config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://three.example/v1", Model: "three"}
	if err := manager.Upsert("third", third, false); err != nil {
		t.Fatal(err)
	}
	// Two providers remain after the deletion, so a choice has to be made.
	if err := manager.Delete("work", ""); err == nil || !strings.Contains(err.Error(), "personal") {
		t.Fatalf("deleting the active provider = %v, want the available replacements named", err)
	}
	if _, ok := manager.Get("work"); !ok {
		t.Fatal("a refused deletion removed the provider")
	}
	if err := manager.Delete("work", "missing"); err == nil || !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("an unknown replacement = %v", err)
	}
	if err := manager.Delete("missing", ""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("deleting an unknown provider = %v", err)
	}
	if err := manager.Delete("work", "personal"); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Get("work"); ok {
		t.Fatal("the provider was not deleted")
	}
	if active, _ := manager.Active(); active.Name != "personal" {
		t.Fatalf("the replacement was not activated: %#v", active)
	}

	// Deleting a provider that is not the active one needs no replacement, and the
	// last provider leaves no active one, which is a state the manager reports
	// rather than hiding.
	if err := manager.Delete("third", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.Delete("personal", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Active(); err == nil {
		t.Fatal("the manager still reports an active provider")
	}
	if list := manager.List(); len(list) != 0 {
		t.Fatalf("List() = %#v", list)
	}
}

// An update that would leave an invalid configuration is refused before anything
// is written, so a broken provider never becomes the active one.
func TestManagerRefusesAnInvalidUpdate(t *testing.T) {
	writes := 0
	manager := managerFixture(t, func(string, config.Config) error {
		writes++
		return nil
	})
	before, _ := manager.Active()
	// A provider with no adapter, endpoint or model cannot serve a request, so it is
	// refused before it can become the active one.
	if err := manager.Upsert("broken", config.ProviderConfig{}, true); err == nil {
		t.Fatal("an empty provider was accepted")
	}
	if err := manager.UpsertPatch("work", config.CompleteProviderPatch(config.ProviderConfig{}), true); err == nil {
		t.Fatal("an update that emptied the active provider was accepted")
	}
	after, _ := manager.Active()
	if after.Name != before.Name || after.Generation != before.Generation {
		t.Fatalf("a refused update was published: %#v", after)
	}
	if writes != 0 {
		t.Fatalf("a refused update wrote %d times", writes)
	}
	if _, ok := manager.Get("broken"); ok {
		t.Fatal("a refused provider was stored")
	}
}

// The store-backed constructor refuses what it cannot use rather than returning a
// manager that would fail on its first write.
func TestNewManagerWithStoreRefusesAMissingStore(t *testing.T) {
	if _, err := NewManagerWithStore(nil); err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("NewManagerWithStore(nil) = %v", err)
	}
}

// The numbers a provider reports arrive in several encodings, and the helpers that
// read them are the boundary between a provider's own vocabulary and the context
// window Eylu budgets against.
func TestProviderMetadataNumberHelpers(t *testing.T) {
	if got := parameterContext("first num_ctx 8192 last"); got != 8192 {
		t.Fatalf("parameterContext(string) = %d", got)
	}
	if got := parameterContext("num_ctx missing"); got != 0 {
		t.Fatalf("parameterContext without a value = %d", got)
	}
	if got := parameterContext(map[string]any{"num_ctx": float64(4096)}); got != 4096 {
		t.Fatalf("parameterContext(map) = %d", got)
	}
	if got := parameterContext(map[string]any{}); got != 0 {
		t.Fatalf("parameterContext of an empty map = %d", got)
	}
	if got := parameterContext(7); got != 0 {
		t.Fatalf("parameterContext(int) = %d, want zero for a shape it does not read", got)
	}

	for value, want := range map[any]int{
		float64(12):       12,
		json.Number("13"): 13,
		14:                14,
		"15":              15,
		"not a number":    0,
		json.Number("x"):  0,
		nil:               0,
		true:              0,
	} {
		if got := numberAsInt(value); got != want {
			t.Fatalf("numberAsInt(%#v) = %d, want %d", value, got, want)
		}
	}

	if got := firstPositive(0, -1, 3, 4); got != 3 {
		t.Fatalf("firstPositive = %d", got)
	}
	if got := firstPositive(0, -1); got != 0 {
		t.Fatalf("firstPositive without a positive = %d", got)
	}
	for _, testCase := range []struct{ left, right, want int }{
		{0, 5, 5}, {-1, 5, 5}, {5, 0, 5}, {5, -1, 5}, {3, 5, 3}, {5, 3, 3}, {0, 0, 0},
	} {
		if got := minPositive(testCase.left, testCase.right); got != testCase.want {
			t.Fatalf("minPositive(%d, %d) = %d, want %d", testCase.left, testCase.right, got, testCase.want)
		}
	}
	// The next tier is the largest tier below the current one, and a request below
	// the smallest tier has nowhere to go.
	if got := nextTier(nil, 100); got != 0 {
		t.Fatalf("nextTier(nil) = %d", got)
	}
	if got := nextTier([]int{8000, 4000, 2000}, 0); got != 8000 {
		t.Fatalf("nextTier from nothing = %d", got)
	}
	if got := nextTier([]int{8000, 4000, 2000}, 4000); got != 2000 {
		t.Fatalf("nextTier(4000) = %d", got)
	}
	if got := nextTier([]int{8000, 4000, 2000}, 2000); got != 0 {
		t.Fatalf("nextTier below the smallest = %d, want no smaller tier", got)
	}
}

// A model name is compared with and without its tag, because two providers spell
// the same model differently.
func TestModelNamesMatchAcrossTagSpellings(t *testing.T) {
	for _, testCase := range []struct {
		want, got string
		matches   bool
	}{
		{"llama3", "llama3", true},
		{"llama3:latest", "llama3", true},
		{"llama3", "llama3:latest", true},
		{"llama3", "llama4", false},
		{"", "", true},
	} {
		if got := modelMatches(testCase.want, testCase.got); got != testCase.matches {
			t.Fatalf("modelMatches(%q, %q) = %t, want %t", testCase.want, testCase.got, got, testCase.matches)
		}
	}
}
