package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
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
	t.Setenv("EYLU_API_KEY", "e2e-secret")
	temp := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", filepath.Join(temp, "config.toml"),
		"--workspace", temp,
		"chat", "hello", "--base-url", server.URL + "/v1", "--model", "test-model",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if stdout.String() != "phase zero works\n" {
		t.Fatalf("stdout = %q", stdout.String())
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
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, credentials: provider.NewCredentialStore(), trustPrompted: make(map[string]bool)}
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
			if len(body["tools"].([]any)) != 6 {
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
	return home
}

func TestModeSlashCommand(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.Default(workspace)
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, credentials: provider.NewCredentialStore()}
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

func TestContextSlashRendersAllCategories(t *testing.T) {
	var output bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &output, stderr: &bytes.Buffer{}, credentials: provider.NewCredentialStore(), trustPrompted: make(map[string]bool)}
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
