package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

func TestChatEndToEnd(t *testing.T) {
	isolateUserState(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer e2e-secret" {
			t.Fatal("missing API key")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(body)
		if !bytes.Contains(encoded, []byte("Here is useful information about the environment")) || !bytes.Contains(encoded, []byte("Your model ID is test-model.")) {
			t.Fatalf("environment prompt missing from request: %s", encoded)
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"phase zero \"}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"works\"}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"phase zero works\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":3}}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"phase zero works"}]}],"usage":{"input_tokens":3,"output_tokens":3}}`))
	}))
	defer server.Close()
	t.Setenv("EYLU_API_KEY", "")
	temp := t.TempDir()
	configPath := filepath.Join(temp, "config.toml")
	cfg := config.Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", APIKey: "e2e-secret", Model: "test-model"}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", configPath,
		"--workspace", temp,
		"chat", "hello",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "phase zero works\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestResolveWorkspacePrecedence(t *testing.T) {
	environmentWorkspace := t.TempDir()
	t.Setenv("EYLU_WORKSPACE", environmentWorkspace)
	explicitWorkspace := t.TempDir()

	appRuntime := &runtime{workspace: explicitWorkspace}
	resolved, err := appRuntime.resolveWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	explicitAbsolute, _ := filepath.Abs(explicitWorkspace)
	if resolved != explicitAbsolute {
		t.Fatalf("explicit workspace = %s, want %s", resolved, explicitAbsolute)
	}

	appRuntime = &runtime{}
	resolved, err = appRuntime.resolveWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	environmentAbsolute, _ := filepath.Abs(environmentWorkspace)
	if resolved != environmentAbsolute {
		t.Fatalf("environment workspace = %s, want %s", resolved, environmentAbsolute)
	}

	t.Setenv("EYLU_WORKSPACE", "")
	appRuntime = &runtime{}
	resolved, err = appRuntime.resolveWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	current, _ := os.Getwd()
	current, _ = filepath.Abs(current)
	if resolved != filepath.Clean(current) {
		t.Fatalf("current workspace = %s, want %s", resolved, current)
	}
}

func TestProvidersAddPersistsAPIKeyInConfig(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "config.toml")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", configPath,
		"--workspace", workspace,
		"providers", "add", "work",
		"--base-url", "https://example.com/v1",
		"--model", "test-model",
		"--api-key", "stored-secret",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err := config.Load(config.LoadOptions{ExplicitPath: configPath, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Providers["work"].APIKey != "stored-secret" {
		t.Fatalf("provider=%#v", loaded.Config.Providers["work"])
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("credential")) {
		t.Fatalf("legacy credential configuration remains: %s", data)
	}
	for _, unexpected := range [][]byte{[]byte("timeout_seconds"), []byte("context_window"), []byte("model_metadata"), []byte("max_turns")} {
		if bytes.Contains(data, unexpected) {
			t.Fatalf("provider command expanded default %q: %s", unexpected, data)
		}
	}
}

func TestRuntimeRedactsConfiguredAPIKeys(t *testing.T) {
	runtime := &runtime{}
	cfg := config.Default()
	cfg.Providers["work"] = config.ProviderConfig{APIKey: "stored-secret"}
	runtime.rememberProviderAPIKeys(cfg)
	redacted := runtime.redact("request failed with stored-secret")
	if strings.Contains(redacted, "stored-secret") || !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("redacted=%q", redacted)
	}
}

func TestInteractiveStartupProbesActiveModel(t *testing.T) {
	isolateUserState(t)
	t.Setenv("EYLU_MODEL_METADATA_ENABLED", "true")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"startup-model","context_length":131072,"max_completion_tokens":16384}]}`)
	}))
	defer server.Close()

	workspace := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := config.Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "startup-model"}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, output: "text",
		workspace: workspace, configPath: configPath, metadataCachePath: filepath.Join(t.TempDir(), "metadata.json"), trustPrompted: make(map[string]bool),
	}
	if err := runtime.runInteractive(context.Background(), chatOptions{noTUI: true}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("metadata requests = %d, want 1", requests.Load())
	}
}

func TestStartupProbeResolvesAutomaticRoutingCandidates(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"model-a","context_length":64000},{"id":"model-b","context_length":128000}]}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.RoutingMode = "auto"
	cfg.ActiveProvider = "a"
	cfg.Providers["a"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "model-a"}
	cfg.Providers["b"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: server.URL + "/v1", Model: "model-b"}
	manager, err := provider.NewManager(filepath.Join(t.TempDir(), "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runtime{metadataCachePath: filepath.Join(t.TempDir(), "metadata.json")}
	active, err := runtime.probeStartupModelLimits(context.Background(), manager, chatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || active.Name != "a" || active.Limits.ContextWindow != 64000 {
		t.Fatalf("requests=%d active=%#v", requests.Load(), active)
	}
}

func TestRootChatEntryPoints(t *testing.T) {
	testCases := []struct {
		name           string
		expectedPrompt string
		stdin          string
		arguments      func([]string) []string
	}{
		{
			name:           "positional prompt",
			expectedPrompt: "hello from root",
			arguments: func(base []string) []string {
				return append(base, "hello from root")
			},
		},
		{
			name:           "piped prompt",
			expectedPrompt: "hello from pipe",
			stdin:          "hello from pipe\n",
			arguments: func(base []string) []string {
				return base
			},
		},
		{
			name:           "reserved prompt after double dash",
			expectedPrompt: "sessions",
			arguments: func(base []string) []string {
				return append(base, "--", "sessions")
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			isolateUserState(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				input, ok := body["input"].([]any)
				if !ok || len(input) == 0 {
					t.Fatalf("input = %#v", body["input"])
				}
				var prompt any
				for _, raw := range input {
					item, itemOK := raw.(map[string]any)
					if itemOK && item["role"] == "user" {
						prompt = item["content"]
						break
					}
				}
				if prompt != testCase.expectedPrompt || body["model"] != "root-model" {
					t.Fatalf("request body = %#v", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"root chat works\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_root\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"root chat works\"}]}]}}\n\n"))
			}))
			defer server.Close()
			t.Setenv("EYLU_API_KEY", "root-secret")
			workspace := t.TempDir()
			base := []string{
				"--config", filepath.Join(workspace, "config.toml"),
				"--workspace", workspace,
				"--base-url", server.URL + "/v1",
				"--model", "root-model",
			}
			var stdout, stderr bytes.Buffer
			code := Execute(context.Background(), testCase.arguments(base), strings.NewReader(testCase.stdin), &stdout, &stderr)
			if code != 0 || stdout.String() != "root chat works\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRootAndChatExposeSameChatFlags(t *testing.T) {
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, trustPrompted: make(map[string]bool)}
	root := runtime.rootCommand(context.Background())
	var chatCommandFound bool
	for _, command := range root.Commands() {
		if command.Name() != "chat" {
			continue
		}
		chatCommandFound = true
		for _, name := range []string{"provider", "model", "base-url", "adapter", "timeout", "yes", "mode", "trust-workspace-skills", "no-animation", "no-tui", "session", "resume", "route", "task", "require-reasoning"} {
			if root.Flags().Lookup(name) == nil || command.Flags().Lookup(name) == nil {
				t.Errorf("chat flag %q is not registered on both entry points", name)
			}
		}
	}
	if !chatCommandFound {
		t.Fatal("chat compatibility command is missing")
	}
}

func TestRootChatValidationAndSubcommandDispatch(t *testing.T) {
	t.Run("empty pipe points to direct syntax", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), nil, strings.NewReader(""), &stdout, &stderr)
		if code != exitConfig || !strings.Contains(stderr.String(), `use eylu "your request"`) {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("maximum one prompt", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"first", "second"}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "accepts at most 1 arg") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("session and resume are mutually exclusive", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"--session", "test", "--resume"}, strings.NewReader(""), &stdout, &stderr)
		if code == 0 || !strings.Contains(stderr.String(), "none of the others can be") {
			t.Fatalf("exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("help remains help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"--help"}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "eylu [prompt] [flags]") || !strings.Contains(stdout.String(), "--model") {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})

	t.Run("version remains a subcommand", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := Execute(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr)
		if code != 0 || strings.TrimSpace(stdout.String()) == "" || stderr.Len() != 0 {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	})
}

func TestChatToolLoopReadsAndBuilds(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module fixture\n\ngo 1.24.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		number := requests.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		input := body["input"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		switch number {
		case 1:
			tools := body["tools"].([]any)
			if len(tools) != 7 || !containsFunctionTool(tools, "todolist") {
				t.Fatalf("tools = %#v", body["tools"])
			}
			writeResponsesCompleted(w, `{"id":"resp_1","output":[{"type":"function_call","id":"fc_1","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}]}`)
		case 2:
			if !containsFunctionOutput(input, "call-read", "package main") {
				t.Fatalf("read result missing: %#v", input)
			}
			writeResponsesCompleted(w, `{"id":"resp_2","output":[{"type":"function_call","id":"fc_2","call_id":"call-build","name":"bash","arguments":"{\"command\":\"go build ./...\"}"}]}`)
		case 3:
			if !containsFunctionOutput(input, "call-build", "exit_code: 0") {
				t.Fatalf("build result missing: %#v", input)
			}
			writeResponsesCompleted(w, `{"id":"resp_3","output":[{"type":"message","content":[{"type":"output_text","text":"tool loop complete"}]}]}`)
		default:
			t.Fatalf("unexpected request %d", number)
		}
	}))
	defer server.Close()
	t.Setenv("EYLU_API_KEY", "tool-secret")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", filepath.Join(workspace, "config.toml"), "--workspace", workspace,
		"chat", "read and build", "--base-url", server.URL, "--model", "test-model", "--yes",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stdout.String() != "tool loop complete\n" || requests.Load() != 3 {
		t.Fatalf("exit=%d requests=%d stdout=%q stderr=%q", code, requests.Load(), stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "call_id=call-read") || !strings.Contains(stderr.String(), "decision=confirm") || !strings.Contains(stderr.String(), "call_id=call-build") {
		t.Fatalf("tool diagnostics missing: %s", stderr.String())
	}
}

func writeResponsesCompleted(w http.ResponseWriter, response string) {
	_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"))
}

func containsFunctionTool(tools []any, name string) bool {
	for _, raw := range tools {
		definition, ok := raw.(map[string]any)
		if ok && definition["name"] == name {
			return true
		}
	}
	return false
}

func containsFunctionOutput(input []any, callID, content string) bool {
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if ok && item["type"] == "function_call_output" && item["call_id"] == callID && strings.Contains(item["output"].(string), content) {
			return true
		}
	}
	return false
}

func TestChatMissingProviderIsStructured(t *testing.T) {
	isolateUserState(t)
	temp := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", filepath.Join(temp, "config.toml"), "--workspace", temp, "--output", "json", "chat", "hello",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), `"code":"config_error"`) {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}

func isolateUserState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("EYLU_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("EYLU_MODEL_METADATA_ENABLED", "false")
	return home
}

func TestModeSlashCommand(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr}
	conversation := agent.NewConversation()
	opts := chatOptions{}
	if err := runtime.handleSlashCommand(context.Background(), bufio.NewReader(strings.NewReader("")), "/mode plan", conversation, manager, &opts); err != nil {
		t.Fatal(err)
	}
	if opts.mode != "plan" || !strings.Contains(stdout.String(), "Permission mode: plan") {
		t.Fatalf("opts = %#v, stdout = %q", opts, stdout.String())
	}
	if err := runtime.handleSlashCommand(context.Background(), bufio.NewReader(strings.NewReader("")), "/mode unsafe", conversation, manager, &opts); err == nil {
		t.Fatal("expected invalid mode error")
	}
}

func TestEffortSlashCommandReportsUpdatesAndValidates(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "gpt-5.6-sol", ReasoningEffort: "medium"}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runtime := &runtime{stdout: &output, stderr: &bytes.Buffer{}}
	conversation := agent.NewConversation()
	opts := chatOptions{}
	reader := bufio.NewReader(strings.NewReader(""))
	if err := runtime.handleSlashCommand(context.Background(), reader, "/effort", conversation, manager, &opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Reasoning effort: medium") || !strings.Contains(output.String(), "auto, low, medium, high, xhigh, max, ultra") {
		t.Fatalf("output=%q", output.String())
	}
	if err := runtime.handleSlashCommand(context.Background(), reader, "/effort ultra", conversation, manager, &opts); err != nil {
		t.Fatal(err)
	}
	updated, _ := manager.Get("work")
	if updated.ReasoningEffort != "ultra" {
		t.Fatalf("provider=%#v", updated)
	}
	if err := runtime.handleSlashCommand(context.Background(), reader, "/effort impossible", conversation, manager, &opts); err == nil || !strings.Contains(err.Error(), "available:") {
		t.Fatalf("invalid effort error=%v", err)
	}
}

func TestContextSlashRendersAllCategories(t *testing.T) {
	var output bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &output, stderr: &bytes.Buffer{}, trustPrompted: make(map[string]bool)}
	conversation := agent.NewConversation()
	if err := runtime.handleSlashCommand(context.Background(), bufio.NewReader(strings.NewReader("")), "/context", conversation, nil, &chatOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"System prompt", "Skill catalog", "Skill resources", "MCP instructions", "Tool schemas", "Project context", "Driver state", "Output reserve"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("context output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestModelSlashAcceptsManualContextWindow(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default()
	cfg.ModelMetadata.Enabled = false
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: "https://example.com/v1", Model: "old-model"}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	runtime := &runtime{stdout: &output, stderr: &bytes.Buffer{}, metadataCachePath: filepath.Join(t.TempDir(), "metadata.json")}
	conversation := agent.NewConversation()
	reader := bufio.NewReader(strings.NewReader("n\n200000\n"))
	if err := runtime.handleSlashCommand(context.Background(), reader, "/model next-model", conversation, manager, &chatOptions{}); err != nil {
		t.Fatal(err)
	}
	configured, _ := manager.Get("work")
	report := conversation.ContextReport()
	if configured.ContextWindow != 200000 || report.ContextWindow != 200000 || report.DetectedContextWindow != 256000 || report.LimitSource != string(provider.LimitSourceUserCap) {
		t.Fatalf("configured=%#v report=%#v output=%q", configured, report, output.String())
	}
}

func TestTasksSlashRendersCurrentTodoList(t *testing.T) {
	var output bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &output, stderr: &bytes.Buffer{}, trustPrompted: make(map[string]bool)}
	state := agent.NewConversation().ExportState()
	state.TodoList = protocol.TodoList{Items: []protocol.TodoItem{
		{ID: "inspect", Content: "Inspect current files", Status: protocol.TodoCompleted},
		{ID: "implement", Content: "Implement terminal flow", Status: protocol.TodoInProgress},
		{ID: "later", Content: "Run smoke test", Status: protocol.TodoPending},
		{ID: "dropped", Content: "Discard obsolete path", Status: protocol.TodoCancelled},
	}}
	conversation, err := agent.RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleSlashCommand(context.Background(), bufio.NewReader(strings.NewReader("")), "/tasks", conversation, nil, &chatOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"[x] Inspect current files", "[>] Implement terminal flow", "[ ] Run smoke test", "[-] Discard obsolete path"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("tasks output missing %q:\n%s", expected, output.String())
		}
	}
}

func TestTextAskSupportsSingleMultipleAndCustomAnswers(t *testing.T) {
	input := strings.NewReader("1abc\n2\n1,3,o\ncustom detail\n")
	var output bytes.Buffer
	runtime := &runtime{stdin: input, stdout: &bytes.Buffer{}, stderr: &output, inputReader: bufio.NewReader(input)}
	response, err := runtime.askUser(context.Background(), protocol.AskRequest{Questions: []protocol.AskQuestion{
		{ID: "scope", Header: "Scope", Question: "Choose scope", Options: []protocol.AskOption{{Label: "Small", Description: "Small change"}, {Label: "Full", Description: "Full change"}}},
		{ID: "checks", Header: "Checks", Question: "Choose checks", Multiple: true, Options: []protocol.AskOption{{Label: "Unit", Description: "Unit tests"}, {Label: "Vet", Description: "Static checks"}, {Label: "Smoke", Description: "Smoke test"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Answers["scope"]; len(got) != 1 || got[0] != "Full" {
		t.Fatalf("scope answers = %#v", got)
	}
	if got := response.Answers["checks"]; len(got) != 3 || got[0] != "Unit" || got[1] != "Smoke" || got[2] != "custom detail" {
		t.Fatalf("check answers = %#v", got)
	}
	if !strings.Contains(output.String(), "Other") || !strings.Contains(output.String(), "Invalid selection") {
		t.Fatalf("text ask output = %q", output.String())
	}
}

func TestTextAskCancellationReleasesAndPreservesPendingInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	buffered := bufio.NewReader(reader)
	runtime := &runtime{stdin: reader, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, inputReader: buffered}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runtime.askUser(ctx, protocol.AskRequest{Questions: []protocol.AskQuestion{{
			ID: "scope", Header: "Scope", Question: "Choose scope", Options: []protocol.AskOption{{Label: "Small", Description: "Focused"}, {Label: "Full", Description: "Complete"}},
		}}})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		runtime.inputMu.Lock()
		started := runtime.inputRead != nil
		runtime.inputMu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ask did not start terminal read")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("text ask did not release after cancellation")
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "next prompt\n")
		writeDone <- err
	}()
	line, err := runtime.readInteractiveLine(context.Background(), buffered)
	if err != nil || strings.TrimSpace(line) != "next prompt" {
		t.Fatalf("preserved line=%q err=%v", line, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestJSONLStreamingOutputIsLineDelimitedAndStructured(t *testing.T) {
	isolateUserState(t)
	t.Setenv("EYLU_API_KEY", "jsonl-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"jsonl\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_jsonl\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"jsonl\"}]}]}}\n\n"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"--config", filepath.Join(t.TempDir(), "config.toml"), "--workspace", workspace, "--output", "jsonl", "chat", "hello", "--base-url", server.URL + "/v1", "--model", "test"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	types := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		typeName, _ := envelope["type"].(string)
		types[typeName] = true
	}
	if !types["context"] || !types["model_event"] || !types["response"] || strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("types=%#v output=%s", types, stdout.String())
	}
}

func TestInvalidOutputFormatIsConfigurationError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"--output", "yaml", "skills", "list"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), "output must be text, json, or jsonl") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
}
