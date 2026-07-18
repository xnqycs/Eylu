package app

import (
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
)

func TestChatEndToEnd(t *testing.T) {
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

func TestChatToolLoopReadsAndBuilds(t *testing.T) {
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
	temp := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{
		"--config", filepath.Join(temp, "config.toml"), "--workspace", temp, "--output", "json", "chat", "hello",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), `"code":"config_error"`) {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
}
