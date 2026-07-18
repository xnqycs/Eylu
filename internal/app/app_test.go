package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
