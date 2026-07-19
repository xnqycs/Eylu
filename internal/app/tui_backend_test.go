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
	textEvents := 0
	inputActivities := 0
	foundContext, foundActivity, foundUsage := false, false, false
	for _, event := range events {
		if event.Kind == ui.EventTextDelta {
			textEvents++
			text.WriteString(event.Delta)
		}
		foundContext = foundContext || event.Kind == ui.EventContext && event.Context != nil
		foundActivity = foundActivity || event.Kind == ui.EventActivity && event.Activity != nil && event.Activity.Reasoning && event.Activity.ReasoningKnown && event.Activity.TokenBytesPerToken == cfg.TokenBytesPerToken && event.Activity.InputTokens > 0
		if event.Kind == ui.EventActivity && event.Activity != nil && event.Activity.InputTokens > 0 {
			inputActivities++
		}
		foundUsage = foundUsage || event.Kind == ui.EventUsage && event.Usage != nil && event.Usage.OutputTokens == 2 && event.Usage.Exact
	}
	if text.String() != "TUI works" || textEvents != 1 || inputActivities < 2 || !foundContext || !foundActivity || !foundUsage || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("text=%q textEvents=%d inputActivities=%d context=%t activity=%t usage=%t stdout=%q stderr=%q events=%#v", text.String(), textEvents, inputActivities, foundContext, foundActivity, foundUsage, stdout.String(), stderr.String(), events)
	}
}

func TestTUIBackendStreamsFileToolArgumentsBeforeExecution(t *testing.T) {
	t.Setenv("EYLU_API_KEY", "tui-secret")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_write\",\"call_id\":\"call-write\",\"name\":\"write_file\",\"arguments\":\"\"}}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"path\\\":\\\"live.txt\\\",\\\"content\\\":\\\"hel\"}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"lo\\\"}\"}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.function_call_arguments.done\",\"output_index\":0,\"arguments\":\"{\\\"path\\\":\\\"live.txt\\\",\\\"content\\\":\\\"hello\\\"}\"}\n\n"))
			writeResponsesCompleted(w, `{"id":"resp_write","output":[{"type":"function_call","id":"fc_write","call_id":"call-write","name":"write_file","arguments":"{\"path\":\"live.txt\",\"content\":\"hello\"}"}]}`)
		case 2:
			writeResponsesCompleted(w, `{"id":"resp_done","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	cfg := config.Default(workspace)
	cfg.PermissionMode = "full"
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
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, credentials: provider.NewCredentialStoreWith(&tuiTestKeyring{}, os.LookupEnv), trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	events := make([]ui.Event, 0)
	if err := backend.Submit(context.Background(), "op-write", "create live.txt", func(event ui.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	firstDelta, firstStart := -1, -1
	var arguments strings.Builder
	for index, event := range events {
		if event.Kind == ui.EventToolCallDelta && event.ToolCallDelta != nil {
			if firstDelta < 0 {
				firstDelta = index
			}
			arguments.WriteString(event.ToolCallDelta.Delta)
		}
		if event.Kind == ui.EventToolStart && firstStart < 0 {
			firstStart = index
		}
	}
	data, readErr := os.ReadFile(filepath.Join(workspace, "live.txt"))
	if readErr != nil || string(data) != "hello" || firstDelta < 0 || firstStart <= firstDelta || arguments.String() != `{"path":"live.txt","content":"hello"}` {
		t.Fatalf("file=%q readErr=%v firstDelta=%d firstStart=%d arguments=%q events=%#v", data, readErr, firstDelta, firstStart, arguments.String(), events)
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
	if err != nil || snapshot.Workspace != workspace || snapshot.Provider != "new" || snapshot.Model != "new-model" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}
