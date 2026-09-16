package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// A truncated answer must not be presented as a finished one: the text output
// states the stop reason and the structured output carries it.
func TestTruncatedAnswerIsReportedAsTruncated(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "truncation-secret")
	// The handler answers the streaming and the non-streaming shape, because the
	// structured output mode does not stream.
	const envelope = `"id":"response-cut","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"half an answer"}]}],"usage":{"input_tokens":5,"output_tokens":3}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{" + envelope + "}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{" + envelope + "}"))
	}))
	defer server.Close()
	configPath := filepath.Join(workspace, "config.toml")
	baseArgs := []string{"--config", configPath, "--workspace", workspace, "chat", "explain", "--base-url", server.URL, "--model", "test-model"}

	t.Run("text output states the stop reason", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), baseArgs, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "half an answer") {
			t.Fatalf("stdout = %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "stopped the response before it finished") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	})

	t.Run("json output carries the stop reason", func(t *testing.T) {
		args := append(append([]string(nil), baseArgs...), "--output", "json")
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		var response protocol.ModelResponse
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		if response.Stop != protocol.StopLength {
			t.Fatalf("stop = %q", response.Stop)
		}
		if len(response.Turn.Parts) == 0 || response.Turn.Parts[0].Text != "half an answer" {
			t.Fatalf("response = %#v", response)
		}
	})
}
