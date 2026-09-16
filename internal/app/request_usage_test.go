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

// The two rounds of one request deliberately report different usage, so the last
// call and the whole request cannot be confused for each other.
const (
	requestUsageFirstRound  = `"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"write_file","arguments":"{\"path\":\"out.txt\",\"content\":\"x\",\"reason\":\"test\"}"}],"usage":{"input_tokens":100,"output_tokens":10,"input_tokens_details":{"cached_tokens":60}}`
	requestUsageSecondRound = `"id":"r2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":40,"output_tokens":5}`
)

// requestUsageServer answers two rounds, in whichever transport the caller uses.
func requestUsageServer(t *testing.T) *httptest.Server {
	t.Helper()
	round := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		round++
		envelope := requestUsageFirstRound
		if round > 1 {
			envelope = requestUsageSecondRound
		}
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{" + envelope + "}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{" + envelope + "}"))
	}))
	t.Cleanup(server.Close)
	return server
}

func requestUsageArgs(workspace, configPath, baseURL string, extra ...string) []string {
	args := []string{"--config", configPath, "--workspace", workspace, "chat", "write it", "--mode", "full", "--base-url", baseURL, "--model", "test-model"}
	return append(args, extra...)
}

// A multi-round request reports the whole request beside the last call, and the two
// are never the same number in this fixture.
func TestRequestUsageIsReportedBesideTheLastCall(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "usage-secret")
	configPath := filepath.Join(workspace, "config.toml")

	t.Run("json adds the request totals without changing the response fields", func(t *testing.T) {
		// Each sub-test owns its server: the fixture counts rounds, so sharing one
		// would serve the second case a later round than it expects.
		server := requestUsageServer(t)
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), requestUsageArgs(workspace, configPath, server.URL, "--output", "json"), strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		var decoded struct {
			protocol.ModelResponse
			RequestUsage      protocol.Usage `json:"request_usage"`
			RequestModelCalls int            `json:"request_model_calls"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %q: %v", stdout.String(), err)
		}
		// The last call is unchanged and still named "usage".
		if decoded.Usage.InputTokens != 40 || decoded.Usage.OutputTokens != 5 {
			t.Fatalf("last-call usage = %#v", decoded.Usage)
		}
		// The request totals are the sum of both rounds, and they are different.
		if decoded.RequestUsage.InputTokens != 140 || decoded.RequestUsage.OutputTokens != 15 {
			t.Fatalf("request usage = %#v", decoded.RequestUsage)
		}
		if decoded.RequestUsage.CachedInputTokens != 60 {
			t.Fatalf("request cache hits = %#v", decoded.RequestUsage)
		}
		if decoded.RequestModelCalls != 2 {
			t.Fatalf("request model calls = %d", decoded.RequestModelCalls)
		}
		if decoded.RequestUsage.InputTokens == decoded.Usage.InputTokens {
			t.Fatal("the request total and the last call were the same number, so this fixture proves nothing")
		}
		// Every field the response had is still there under its own name.
		raw := map[string]any{}
		if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"turn", "stop", "usage", "request_usage", "request_model_calls"} {
			if _, ok := raw[field]; !ok {
				t.Fatalf("the json output lost %q: %s", field, stdout.String())
			}
		}
		if raw["stop"] != string(protocol.StopCompleted) {
			t.Fatalf("stop = %v", raw["stop"])
		}
	})

	t.Run("jsonl carries the request totals as their own line", func(t *testing.T) {
		server := requestUsageServer(t)
		var stdout, stderr bytes.Buffer
		if code := Execute(context.Background(), requestUsageArgs(workspace, configPath, server.URL, "--output", "jsonl"), strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		found := false
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			if line == "" {
				continue
			}
			var decoded struct {
				Type              string         `json:"type"`
				RequestUsage      protocol.Usage `json:"request_usage"`
				RequestModelCalls int            `json:"request_model_calls"`
			}
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			if decoded.Type != "request_usage" {
				continue
			}
			found = true
			if decoded.RequestUsage.InputTokens != 140 || decoded.RequestModelCalls != 2 {
				t.Fatalf("request usage line = %s", line)
			}
		}
		if !found {
			t.Fatalf("no request_usage line in %q", stdout.String())
		}
		// The response line still carries the last call under "usage".
		if !strings.Contains(stdout.String(), `"type":"response"`) {
			t.Fatalf("the response line is missing: %q", stdout.String())
		}
	})
}
