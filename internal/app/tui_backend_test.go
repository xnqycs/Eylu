package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	metric := metrics.RequestMetric{DurationMS: 18023, FirstTokenMS: 3053, ToolSuccessRate: 1}
	got := formatRequestCompletion(metric, true)
	want := "Interrupted after 18.023s; first token 3.053s; tool success 100%."
	if got != want {
		t.Fatalf("formatRequestCompletion() = %q, want %q", got, want)
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
	cfg := config.Default()
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
	if err := backend.Submit(context.Background(), "op-1", ui.Submission{Text: "hello"}, func(event ui.Event) { events = append(events, event) }); err != nil {
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
	cfg := config.Default()
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
	cfg := config.Default()
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
	if err := backend.UpsertProvider(context.Background(), ui.ProviderForm{Name: "new", BaseURL: "https://new.example/v1", Model: "new-model", Adapter: "openai_responses", APIKey: "provider-secret", ContextWindow: 64000}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "api_key = 'provider-secret'") {
		t.Fatalf("config=%s", data)
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

func TestTUIBackendPreparesGitAwareFileAndSkillReferences(t *testing.T) {
	workspace := t.TempDir()
	runAppGit(t, workspace, "init")
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
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
	home := t.TempDir()
	skillRoot := filepath.Join(home, ".eylu", "skills", "review")
	if err := os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := "---\nname: review\ndescription: Review repository changes\n---\nReview the referenced implementation carefully.\n"
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte(skillFile), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
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
	cfg := config.Default()
	runtime := &runtime{workspace: workspace}
	executor, err := runtime.toolExecutorWith(cfg, chatOptions{mode: "plan"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, definition := range executor.Definitions() {
		names[definition.Name] = true
	}
	if !names["read_file"] || !names["search_code"] || !names["list_directory"] || !names["bash"] {
		t.Fatalf("missing plan tools: %v", names)
	}
	if names["write_file"] || names["edit_file"] {
		t.Fatalf("write tools leaked into plan profile: %v", names)
	}
}

func runAppGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
