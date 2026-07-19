package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/provider"
	"Eylu/internal/skill"
	"Eylu/internal/ui"
)

type tuiTestKeyring struct{ values map[string]string }

func (k *tuiTestKeyring) Set(service, account, secret string) error {
	if k.values == nil {
		k.values = make(map[string]string)
	}
	k.values[service+"/"+account] = secret
	return nil
}
func (k *tuiTestKeyring) Get(service, account string) (string, error) {
	value, ok := k.values[service+"/"+account]
	if !ok {
		return "", errors.New("missing")
	}
	return value, nil
}
func (k *tuiTestKeyring) Delete(service, account string) error {
	delete(k.values, service+"/"+account)
	return nil
}

func TestTUIBackendStreamsEventsWithoutWritingTerminal(t *testing.T) {
	t.Setenv("EYLU_API_KEY", "tui-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"TUI \"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"works\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tui\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"TUI works\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	cfg := config.Default(workspace)
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "test", Credential: config.CredentialRef{Type: "env", Env: "EYLU_API_KEY"}}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, credentials: provider.NewCredentialStoreWith(&tuiTestKeyring{}, os.LookupEnv), trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	events := make([]ui.Event, 0)
	if err := backend.Submit(context.Background(), "op-1", "hello", func(event ui.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	foundContext := false
	for _, event := range events {
		if event.Kind == ui.EventTextDelta {
			text.WriteString(event.Delta)
		}
		foundContext = foundContext || event.Kind == ui.EventContext && event.Context != nil
	}
	if text.String() != "TUI works" || !foundContext || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("text=%q context=%t stdout=%q stderr=%q events=%#v", text.String(), foundContext, stdout.String(), stderr.String(), events)
	}
}

func TestTUIBackendApprovalProviderAndSecretPersistence(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default(workspace)
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "model", Credential: config.CredentialRef{Type: "env", Env: "EYLU_API_KEY"}}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	manager, err := provider.NewManager(configPath, cfg, config.Save)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	keyring := &tuiTestKeyring{}
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, credentials: provider.NewCredentialStoreWith(keyring, os.LookupEnv), trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	confirm := backend.confirmTools("op", false, func(event ui.Event) {
		if event.Kind != ui.EventApproval || event.Approval == nil {
			t.Fatalf("event = %#v", event)
		}
		event.Approval.Response <- true
	})
	allowed, err := confirm(context.Background(), policy.Request{Tool: "write_file", Input: []byte(`{"path":"x"}`), ConfirmationStep: 1, ConfirmationTotal: 1}, policy.Outcome{Risk: policy.RiskWrite, Reason: "write", Decision: policy.DecisionConfirm})
	if err != nil || !allowed {
		t.Fatalf("allowed=%t err=%v", allowed, err)
	}
	if err := backend.UpsertProvider(context.Background(), ui.ProviderForm{Name: "new", BaseURL: "https://new.example/v1", Model: "new-model", Adapter: "openai_responses", APIKey: "provider-secret", ContextWindow: 64000}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "provider-secret") || keyring.values["eylu/provider:new"] != "provider-secret" {
		t.Fatalf("config=%s keyring=%#v", data, keyring.values)
	}
	if err := backend.SetModel(context.Background(), "work", "work-updated"); err != nil {
		t.Fatal(err)
	}
	work, _ := manager.Get("work")
	newProvider, _ := manager.Get("new")
	if work.Model != "work-updated" || newProvider.Model != "new-model" {
		t.Fatalf("work=%#v new=%#v", work, newProvider)
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil || snapshot.Provider != "new" || snapshot.Model != "new-model" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}
