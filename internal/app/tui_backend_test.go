package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/environment"
	"Eylu/internal/metrics"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
	"Eylu/internal/ui"
)

func TestFormatRequestCompletionUsesInterruptedLabelAndScaledDurations(t *testing.T) {
	metric := metrics.RequestMetric{DurationMS: 18023, FirstTokenMS: 3053, GenerationMS: 2000, TokensPerSecond: 42.64, Usage: protocol.Usage{OutputTokens: 85, Exact: true}}
	got := formatRequestCompletion(metric, true)
	want := "Interrupted after 18.023s; TTFT 3.053s; TPS 42.6 t/s."
	if got != want {
		t.Fatalf("formatRequestCompletion() = %q, want %q", got, want)
	}
	estimated := formatRequestCompletion(metrics.RequestMetric{DurationMS: 1000, TokensPerSecond: 8, Usage: protocol.Usage{OutputTokens: 8}}, false)
	if estimated != "Completed in 1s; TTFT n/a; TPS ~8.0 t/s." {
		t.Fatalf("estimated completion = %q", estimated)
	}
	empty := formatRequestCompletion(metrics.RequestMetric{DurationMS: 10}, false)
	if empty != "Completed in 10ms; TTFT n/a; TPS n/a." {
		t.Fatalf("empty completion = %q", empty)
	}
}

func TestFormatCompactionCompletionAndNoopBackendEvents(t *testing.T) {
	formatted := formatCompactionCompletion(contextledger.CompressionEvent{DurationMS: 1234, BeforeTokens: 42_100, AfterTokens: 20_300, OmittedTurns: 18})
	if formatted != "Context compacted in 1.234s; 42.1K → 20.3K tokens; 18 turns summarized." {
		t.Fatalf("formatted=%q", formatted)
	}
	workspace := t.TempDir()
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "test", ContextWindow: 10_000}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	conversation := agent.NewConversation()
	backend := &tuiBackend{
		runtime:      &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, workspace: workspace, trustPrompted: make(map[string]bool)},
		conversation: conversation, manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil),
	}
	events := make([]ui.Event, 0)
	if err := backend.Compact(context.Background(), "compact-op", func(event ui.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	foundState, foundNoop, foundContext := false, false, false
	for _, event := range events {
		foundState = foundState || event.Kind == ui.EventState && event.State == ui.StateCompacting
		foundNoop = foundNoop || event.Kind == ui.EventNotice && event.Notice == "Context is already compact."
		foundContext = foundContext || event.Kind == ui.EventContext && event.Context != nil
	}
	if !foundState || !foundNoop || !foundContext || len(conversation.Transcript()) != 0 || len(conversation.ExportState().PromptHistory) != 0 {
		t.Fatalf("events=%#v state=%#v", events, conversation.ExportState())
	}
}

func TestTUIBackendStreamsEventsWithoutWritingTerminal(t *testing.T) {
	t.Setenv("EYLU_API_KEY", "tui-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("Here is useful information about the environment")) || !bytes.Contains(body, []byte("Your model ID is test.")) {
			t.Fatalf("environment prompt missing from TUI request: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"TUI \"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"works\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tui\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"TUI works\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "test"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, workspace: workspace, trustPrompted: make(map[string]bool)}
	conversation := agent.NewConversationWithEnvironment(environment.Context{WorkingDirectory: workspace, Platform: "windows", Today: "2026-07-19"})
	backend := &tuiBackend{runtime: runtime, conversation: conversation, manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	events := make([]ui.Event, 0)
	if err := backend.Submit(context.Background(), "op-1", ui.Submission{Text: "hello", HistoryText: "hello"}, func(event ui.Event) { events = append(events, event) }); err != nil {
		t.Fatal(err)
	}
	if history := conversation.ExportState().PromptHistory; len(history) != 1 || history[0] != "hello" {
		t.Fatalf("prompt history = %#v", history)
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
	cfg := testAppConfig()
	cfg.PermissionMode = "full"
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "test"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, workspace: workspace, trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	events := make([]ui.Event, 0)
	if err := backend.Submit(context.Background(), "op-write", ui.Submission{Text: "create live.txt"}, func(event ui.Event) { events = append(events, event) }); err != nil {
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
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "model"}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	manager, err := provider.NewManager(configPath, cfg, config.Save)
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, workspace: workspace, trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	confirm := backend.confirmTools("op", false, func(event ui.Event) {
		if event.Kind != ui.EventApproval || event.Approval == nil {
			t.Fatalf("event = %#v", event)
		}
		if event.Approval.Reason != "Update x for the requested change" {
			t.Fatalf("approval reason = %q", event.Approval.Reason)
		}
		event.Approval.Response <- ui.ApprovalDecision{Approved: true}
	})
	decision, err := confirm(context.Background(), policy.Request{Tool: "write_file", Input: []byte(`{"path":"x","reason":"Update x for the requested change"}`), ConfirmationStep: 1, ConfirmationTotal: 1}, policy.Outcome{Risk: policy.RiskWrite, Reason: "write", Decision: policy.DecisionConfirm})
	if err != nil || !decision.Approved {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
	if _, err := backend.UpsertProvider(context.Background(), ui.ProviderForm{Name: "new", BaseURL: "https://new.example/v1", Model: "new-model", Adapter: "openai_responses", APIKey: "provider-secret", ContextWindow: 64000}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api_key = 'provider-secret'") {
		t.Fatalf("config=%s", data)
	}
	if _, err := backend.SetModel(context.Background(), "work", "work-updated"); err != nil {
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
	if _, err := backend.UpsertProvider(context.Background(), ui.ProviderForm{OriginalName: "new", Name: "renamed", BaseURL: "https://new.example/v1", Model: "new-model", Adapter: "openai_responses", ContextWindow: 64000}); err != nil {
		t.Fatal(err)
	}
	renamed, exists := manager.Get("renamed")
	if _, oldExists := manager.Get("new"); !exists || oldExists || renamed.APIKey != "provider-secret" || renamed.ContextWindow != 64000 {
		t.Fatalf("renamed=%#v exists=%t old=%t", renamed, exists, oldExists)
	}
}

func TestTUIBackendReasoningEffortAndModelReset(t *testing.T) {
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "gpt-5.6-sol", ReasoningEffort: "high",
	}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	initial, _ := manager.Active()
	conversation, err := agent.RestoreConversation(agent.ConversationState{
		SessionID: "effort-session", PermissionMode: "manual",
		Provider: agent.ProviderState{Name: initial.Name, Generation: initial.Generation, Adapter: initial.Config.Adapter, BaseURL: initial.Config.BaseURL, Model: initial.Config.Model, ReasoningEffort: initial.Config.ReasoningEffort},
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &tuiBackend{runtime: &runtime{metadataCachePath: filepath.Join(t.TempDir(), "metadata.json")}, conversation: conversation, manager: manager}

	message, err := backend.Command(context.Background(), "/effort max")
	if err != nil || !strings.Contains(message, "max") {
		t.Fatalf("command message=%q error=%v", message, err)
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil || snapshot.ReasoningEffort != "max" || strings.Join(snapshot.SupportedReasoningEfforts, ",") != "auto,low,medium,high,xhigh,max,ultra" {
		t.Fatalf("snapshot=%#v error=%v", snapshot, err)
	}

	selection, err := backend.SetModel(context.Background(), "work", "qwen3")
	if err != nil {
		t.Fatal(err)
	}
	updated, _ := manager.Get("work")
	if updated.Model != "qwen3" || updated.ReasoningEffort != "auto" || selection.EffortResetFrom != "max" {
		t.Fatalf("updated=%#v selection=%#v", updated, selection)
	}
	snapshot, err = backend.Snapshot(context.Background())
	if err != nil || snapshot.Model != "qwen3" || snapshot.ReasoningEffort != "auto" {
		t.Fatalf("refreshed snapshot=%#v error=%v", snapshot, err)
	}
	if _, err := backend.Command(context.Background(), "/effort high"); err == nil || !strings.Contains(err.Error(), "available: auto") {
		t.Fatalf("incompatible command error=%v", err)
	}
}

func TestTUIBackendEffortUpdatesLastRoutedProvider(t *testing.T) {
	cfg := testAppConfig()
	cfg.ActiveProvider = "active"
	cfg.Providers["active"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://active.example/v1", Model: "gpt-5.6-sol", ReasoningEffort: "low"}
	cfg.Providers["routed"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://routed.example/v1", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	routed, _ := manager.Snapshot("routed")
	conversation := agent.NewConversation()
	conversation.ApplyProviderSnapshot(routed)
	backend := &tuiBackend{runtime: &runtime{}, conversation: conversation, manager: manager}

	if _, err := backend.Command(context.Background(), "/effort max"); err != nil {
		t.Fatal(err)
	}
	active, _ := manager.Get("active")
	updated, _ := manager.Get("routed")
	managerActive, _ := manager.Active()
	if active.ReasoningEffort != "low" || updated.ReasoningEffort != "max" || managerActive.Name != "active" {
		t.Fatalf("active=%#v routed=%#v manager_active=%s", active, updated, managerActive.Name)
	}
}

func TestTUIBackendGradientCommandUpdatesSnapshotAndSavedConfig(t *testing.T) {
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "model"}
	var saved config.Config
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(_ string, value config.Config) error {
		saved = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := &tuiBackend{runtime: &runtime{}, conversation: agent.NewConversation(), manager: manager}

	message, err := backend.Command(context.Background(), "/gradient")
	if err != nil || message != "Gradient: Off; available: On, Off" {
		t.Fatalf("default message=%q error=%v", message, err)
	}
	message, err = backend.Command(context.Background(), "/gradient on")
	if err != nil || message != "Gradient: On" || !saved.GradientEnabled {
		t.Fatalf("enabled message=%q config=%#v error=%v", message, saved, err)
	}
	snapshot, err := backend.Snapshot(context.Background())
	if err != nil || !snapshot.GradientEnabled {
		t.Fatalf("snapshot=%#v error=%v", snapshot, err)
	}
	if _, err := backend.Command(context.Background(), "/gradient maybe"); err == nil || !strings.Contains(err.Error(), "usage: /gradient on|off") {
		t.Fatalf("invalid gradient error=%v", err)
	}
}

func TestTUIBackendSetModelProbesImmediately(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"next-model","context_length":131072,"max_completion_tokens":16384}]}`)
	}))
	defer server.Close()

	cfg := testAppConfig()
	cfg.ModelMetadata.Enabled = true
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "old-model"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	conversation := agent.NewConversation()
	appRuntime := &runtime{metadataCachePath: filepath.Join(t.TempDir(), "metadata.json")}
	backend := &tuiBackend{runtime: appRuntime, conversation: conversation, manager: manager}
	selection, err := backend.SetModel(context.Background(), "work", "next-model")
	if err != nil {
		t.Fatal(err)
	}
	report := conversation.ContextReport()
	if requests != 1 || selection.DetectedContextWindow != 131072 || report.Provider != "work" || report.Model != "next-model" || report.DetectedContextWindow != 131072 || report.ContextWindow != 131072 {
		t.Fatalf("requests=%d selection=%#v report=%#v", requests, selection, report)
	}
}

func TestTUIBackendAskAnswersCancellationAndContextRelease(t *testing.T) {
	backend := &tuiBackend{}
	request := protocol.AskRequest{Questions: []protocol.AskQuestion{{ID: "scope", Header: "Scope", Question: "Choose scope", Options: []protocol.AskOption{{Label: "Small", Description: "Focused"}, {Label: "Full", Description: "Complete"}}}}}
	ask := backend.askUser("op-ask", func(event ui.Event) {
		if event.Kind != ui.EventAsk || event.Ask == nil {
			t.Fatalf("event=%#v", event)
		}
		event.Ask.Response <- ui.AskDecision{Answers: map[string][]string{"scope": {"Full"}}}
	})
	response, err := ask(context.Background(), request)
	if err != nil || len(response.Answers["scope"]) != 1 || response.Answers["scope"][0] != "Full" {
		t.Fatalf("response=%#v err=%v", response, err)
	}

	cancelled := backend.askUser("op-ask", func(event ui.Event) {
		event.Ask.Response <- ui.AskDecision{Cancelled: true}
	})
	if _, err := cancelled(context.Background(), request); !errors.Is(err, tool.ErrAskDismissed) {
		t.Fatalf("cancel error=%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	released := backend.askUser("op-ask", func(ui.Event) {})
	done := make(chan error, 1)
	go func() { _, err := released(ctx, request); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("context error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ask callback did not release after context cancellation")
	}
}

func TestTUIAskTodoAndSessionRestoreSmoke(t *testing.T) {
	isolateUserState(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		input := body["input"].([]any)
		switch requests {
		case 1:
			writeResponsesCompleted(w, `{"id":"resp_ask","output":[{"type":"function_call","id":"fc_ask","call_id":"call-ask","name":"ask","arguments":"{\"questions\":[{\"id\":\"scope\",\"header\":\"Scope\",\"question\":\"Choose scope\",\"options\":[{\"label\":\"Small\",\"description\":\"Focused change\"},{\"label\":\"Full\",\"description\":\"Complete change\"}]}]}"}]}`)
		case 2:
			if !containsFunctionOutput(input, "call-ask", `"Full"`) {
				t.Fatalf("ask result missing: %#v", input)
			}
			writeResponsesCompleted(w, `{"id":"resp_todo","output":[{"type":"function_call","id":"fc_todo","call_id":"call-todo","name":"todolist","arguments":"{\"items\":[{\"id\":\"implement\",\"content\":\"Implement the full flow\",\"status\":\"in_progress\"}]}"}]}`)
		case 3:
			if !containsFunctionOutput(input, "call-todo", `"remaining": 1`) {
				t.Fatalf("todo result missing: %#v", input)
			}
			writeResponsesCompleted(w, `{"id":"resp_done","output":[{"type":"message","content":[{"type":"output_text","text":"Ready"}]}]}`)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	workspace := t.TempDir()
	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL, Model: "model"}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	appRuntime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "text", workspace: workspace, trustPrompted: make(map[string]bool)}
	opts := chatOptions{}
	conversation, err := appRuntime.openConversation(context.Background(), manager, &opts)
	if err != nil {
		t.Fatal(err)
	}
	registry, skillSession, err := appRuntime.loadSkillRuntime(context.Background(), cfg, opts, conversation, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := &tuiBackend{runtime: appRuntime, conversation: conversation, manager: manager, opts: opts, skills: registry, skillSession: skillSession}
	seenTypedTodo := false
	err = backend.Submit(context.Background(), "op-smoke", ui.Submission{Text: "Implement the selected scope"}, func(event ui.Event) {
		if event.Kind == ui.EventAsk && event.Ask != nil {
			event.Ask.Response <- ui.AskDecision{Answers: map[string][]string{"scope": {"Full"}}}
		}
		if event.Kind == ui.EventToolResult && event.ToolResult != nil && event.ToolResult.TodoList != nil {
			seenTypedTodo = true
		}
	})
	if err != nil || requests != 3 || !seenTypedTodo || len(conversation.TodoList().Items) != 1 {
		t.Fatalf("err=%v requests=%d typed=%t todos=%#v", err, requests, seenTypedTodo, conversation.TodoList())
	}

	restoredRuntime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "text", workspace: workspace, trustPrompted: make(map[string]bool)}
	restoredOpts := chatOptions{sessionID: conversation.SessionID()}
	restored, err := restoredRuntime.openConversation(context.Background(), manager, &restoredOpts)
	if err != nil || len(restored.TodoList().Items) != 1 || restored.TodoList().Items[0].ID != "implement" {
		t.Fatalf("restored todos=%#v err=%v", restored.TodoList(), err)
	}
}

func TestTUIBackendPreparesGitAwareFileAndSkillReferences(t *testing.T) {
	workspace := t.TempDir()
	runAppGit(t, workspace, "init")
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored.txt\nbuild/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "visible.go"), []byte("package visible\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "space name.go"), []byte("package spaced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "ignored.txt"), []byte("secret ignored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "build", "index.html"), []byte("<main>ignored build</main>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	skillRoot := filepath.Join(home, ".eylu", "skills", "review")
	if err := os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := "---\nname: review\ndescription: Review repository changes\n---\nReview the referenced implementation carefully.\n"
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte(skillFile), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testAppConfig()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "test"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, workspace: workspace, trustPrompted: make(map[string]bool)}
	backend := &tuiBackend{runtime: runtime, conversation: agent.NewConversation(), manager: manager, skills: registry, skillSession: skill.NewSession(registry, nil)}
	files, err := backend.ListFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file.Path)
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "visible.go") || strings.Contains(joined, "ignored.txt") {
		t.Fatalf("indexed files = %v", names)
	}

	prompt, err := backend.prepareSubmission(context.Background(), ui.Submission{
		Text: "review @skill:review and @file:\"space name.go\"",
		References: []ui.Reference{
			{Kind: ui.ReferenceSkill, Value: "review"},
			{Kind: ui.ReferenceFile, Value: "space name.go"},
		},
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `<referenced_file path="space name.go"`) || !strings.Contains(prompt, "package spaced") || backend.conversation.ActivatedSkillDigests()["review"] == "" {
		t.Fatalf("prompt=%q skills=%v", prompt, backend.conversation.ActivatedSkillDigests())
	}
	ignoredPrompt, err := backend.prepareSubmission(context.Background(), ui.Submission{
		Text: "inspect @index.html and @build/index.html",
		References: []ui.Reference{
			{Kind: ui.ReferenceFile, Value: "index.html"},
			{Kind: ui.ReferenceFile, Value: "build/index.html"},
		},
	}, cfg)
	if err != nil || !strings.Contains(ignoredPrompt, "<main>ignored build</main>") || strings.Count(ignoredPrompt, `<referenced_file path="build/index.html"`) != 1 {
		t.Fatalf("ignored prompt=%q err=%v", ignoredPrompt, err)
	}
}

func TestTruncateReferenceContentHonorsUTF8ByteLimit(t *testing.T) {
	value := strings.Repeat("你好abc", 20)
	for _, limit := range []int{1, 2, 8, 35, 64} {
		truncated, clipped := truncateReferenceContent(value, limit)
		if !clipped || len([]byte(truncated)) > limit || !utf8.ValidString(truncated) {
			t.Fatalf("limit=%d bytes=%d clipped=%t valid=%t value=%q", limit, len([]byte(truncated)), clipped, utf8.ValidString(truncated), truncated)
		}
	}
}

type planRecordingDriver struct {
	mu       sync.Mutex
	requests []driver.Request
	planCall int
}

func (d *planRecordingDriver) Name() string { return "plan-recording" }
func (d *planRecordingDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *planRecordingDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, request)
	isPlan := len(request.Model.Turns) > 0 && strings.Contains(request.Model.Turns[0].Parts[0].Text, "software architecture planner")
	if isPlan {
		d.planCall++
		if d.planCall == 1 {
			call := protocol.ToolCall{ID: "plan-read", Name: "plan_read", Arguments: json.RawMessage(`{"path":"main.go"}`)}
			return protocol.ModelResponse{Turn: protocol.Turn{ID: "plan-tool", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}, nil
		}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "plan-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "final isolated plan"}}}, Stop: protocol.StopCompleted, DriverState: json.RawMessage(`{"response_id":"plan"}`)}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "parent-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "parent context"}}}, Stop: protocol.StopCompleted, DriverState: json.RawMessage(`{"response_id":"parent"}`)}, nil
}

type planReadTool struct{}

func (planReadTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "plan_read", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (planReadTool) Risk() policy.Risk { return policy.RiskRead }
func (planReadTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	return protocol.ToolResult{Content: "main.go content"}
}

type cancelledPlanDriver struct{}

func (cancelledPlanDriver) Name() string { return "cancelled-plan" }
func (cancelledPlanDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (cancelledPlanDriver) Generate(context.Context, driver.Request, driver.EmitFunc) (protocol.ModelResponse, error) {
	return protocol.ModelResponse{}, context.Canceled
}

func TestPlanRunnerKeepsToolSidechainOutOfParent(t *testing.T) {
	driver := &planRecordingDriver{}
	modelRuntime := agent.Runtime{
		Provider: provider.Snapshot{Name: "work", Generation: 1, Config: config.ProviderConfig{Adapter: driver.Name(), BaseURL: "https://example.com/v1", Model: "reasoning-model", ContextWindow: 32000}},
		Driver:   driver, PermissionMode: "manual", Workspace: t.TempDir(), Timeout: time.Second,
	}
	conversation := agent.NewConversation()
	if _, err := conversation.Send(context.Background(), "parent request", modelRuntime, false, nil); err != nil {
		t.Fatal(err)
	}
	before := len(conversation.Transcript())
	modelRuntime.PermissionMode = "plan"
	executor := &tool.Executor{Registry: tool.NewRegistry(planReadTool{}), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModePlan))}
	response, err := runConversationWithProfile(context.Background(), conversation, "design it", modelRuntime, executor, agent.LoopOptions{MaxTurns: 4}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	turns := conversation.Transcript()
	state := conversation.ExportState()
	if response.Turn.ID != "plan-final" || len(turns) != before+2 || turns[len(turns)-1].ID != "plan-final" || len(state.DriverState) != 0 {
		t.Fatalf("response=%#v turns=%#v state=%#v", response, turns, state)
	}
	for _, turn := range turns {
		if turn.ID == "plan-tool" || turn.Role == protocol.RoleTool {
			t.Fatalf("plan sidechain leaked into parent: %#v", turns)
		}
	}
}

func TestCancelledPlanDoesNotMutateParentTranscript(t *testing.T) {
	modelDriver := cancelledPlanDriver{}
	modelRuntime := agent.Runtime{
		Provider:       provider.Snapshot{Name: "work", Generation: 1, Config: config.ProviderConfig{Adapter: modelDriver.Name(), BaseURL: "https://example.com/v1", Model: "reasoning-model"}},
		Driver:         modelDriver,
		PermissionMode: "plan",
		Workspace:      t.TempDir(),
		Timeout:        time.Second,
	}
	conversation := agent.NewConversation()
	before := conversation.Transcript()
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModePlan))}
	_, err := runConversationWithProfile(context.Background(), conversation, "design it", modelRuntime, executor, agent.LoopOptions{MaxTurns: 4}, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if after := conversation.Transcript(); len(after) != len(before) {
		t.Fatalf("parent transcript changed: before=%#v after=%#v", before, after)
	}
}

func TestPlanExecutorPublishesOnlyReadToolsAndClassifiedBash(t *testing.T) {
	workspace := t.TempDir()
	cfg := testAppConfig()
	runtime := &runtime{workspace: workspace}
	ask := func(context.Context, protocol.AskRequest) (protocol.AskResponse, error) {
		return protocol.AskResponse{}, nil
	}
	executor, err := runtime.toolExecutorWith(cfg, chatOptions{mode: "plan"}, nil, nil, nil, ask, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, definition := range executor.Definitions() {
		names[definition.Name] = true
	}
	if !names["read_file"] || !names["search_code"] || !names["list_directory"] || !names["bash"] || !names["ask"] {
		t.Fatalf("missing plan tools: %v", names)
	}
	if names["write_file"] || names["edit_file"] || names["todolist"] {
		t.Fatalf("write tools leaked into plan profile: %v", names)
	}
}

func testAppConfig() config.Config {
	cfg := config.Default()
	cfg.ModelMetadata.Enabled = false
	return cfg
}

func runAppGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
