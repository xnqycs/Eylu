package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"

	"Eylu/internal/session"
)

// The session log explains why a request stopped and what it did, so the answer
// does not depend on a transient UI event.
func TestSessionLogRecordsWhyARequestStopped(t *testing.T) {
	tests := []struct {
		name       string
		envelope   string
		wantReason string
	}{
		{
			name:       "completed",
			envelope:   `"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":5,"output_tokens":3}`,
			wantReason: "completed",
		},
		{
			name:       "truncated",
			envelope:   `"id":"r2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"half"}]}],"usage":{"input_tokens":5,"output_tokens":3}`,
			wantReason: "length",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateUserState(t)
			workspace := t.TempDir()
			t.Setenv("EYLU_API_KEY", "report-secret")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if bytes.Contains(body, []byte(`"stream":true`)) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{" + test.envelope + "}}\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("{" + test.envelope + "}"))
			}))
			defer server.Close()
			sessionID := []string{"reported-completed", "reported-truncated"}[index]
			args := []string{
				"--config", filepath.Join(workspace, "config.toml"), "--workspace", workspace, "chat", "explain",
				"--base-url", server.URL, "--model", "test-model", "--session", sessionID,
			}
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr.String())
			}
			store, err := session.Open("")
			if err != nil {
				t.Fatal(err)
			}
			snapshot, diagnostics, err := store.Load(sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if len(diagnostics) != 0 {
				t.Fatalf("diagnostics = %#v", diagnostics)
			}
			summary := snapshot.LastRun
			if summary == nil {
				t.Fatal("the log records no run summary")
			}
			if summary.StopReason != test.wantReason {
				t.Fatalf("stop reason = %q, want %q", summary.StopReason, test.wantReason)
			}
			if summary.RequestID == "" || summary.ModelCalls != 1 || summary.InputTokens != 5 || summary.OutputTokens != 3 {
				t.Fatalf("summary = %#v", summary)
			}
		})
	}
}

// A provider error stored in the run report is redacted like the session's own
// last error, so a credential echoed by a provider never reaches the log.
func TestRunReportRedactsTheStoredError(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	redacted := false
	fixture.controller.redact = func(value string) string {
		redacted = true
		return strings.ReplaceAll(value, "sk-secret", "[REDACTED]")
	}
	if err := fixture.controller.RecordRunReport(agent.RunReport{
		RequestID: "request-1", StopReason: "aborted", Error: "provider rejected sk-secret",
	}); err != nil {
		t.Fatal(err)
	}
	if !redacted {
		t.Fatal("the run report error was stored without redaction")
	}
	store, err := session.Open(fixture.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := store.Load(fixture.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastRun == nil || strings.Contains(snapshot.LastRun.Error, "sk-secret") {
		t.Fatalf("stored summary = %#v", snapshot.LastRun)
	}
	if !strings.Contains(snapshot.LastRun.Error, "[REDACTED]") {
		t.Fatalf("stored summary = %#v", snapshot.LastRun)
	}
}
