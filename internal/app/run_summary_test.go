package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/metrics"
	"Eylu/internal/protocol"
	"Eylu/internal/ui"
)

// A request that did not finish must not be timed with a word that claims it did:
// "Completed in 1ms" above a truncated answer is the same defect as printing the
// answer with no note at all.
func TestAStoppedRequestDoesNotClaimCompletion(t *testing.T) {
	metric := metrics.RequestMetric{DurationMS: 1000}
	if got := formatRequestCompletion(metric, false, false); !strings.HasPrefix(got, "Completed in") {
		t.Fatalf("a finished request = %q", got)
	}
	if got := formatRequestCompletion(metric, false, true); !strings.HasPrefix(got, "Stopped after") {
		t.Fatalf("a stopped request = %q", got)
	}
	// An interruption is the more specific statement and wins.
	if got := formatRequestCompletion(metric, true, true); !strings.HasPrefix(got, "Interrupted after") {
		t.Fatalf("an interrupted request = %q", got)
	}
}

// /run answers from the run report rather than from the transcript, which is what
// makes the interface and the log agree about the same request.
func TestRunCommandShowsTheLastRunSummary(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "run-secret")
	const envelope = `"id":"response-cut","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"half an answer"}]}],"usage":{"input_tokens":5,"output_tokens":3}`
	server := stopServer(t, envelope)
	configPath := filepath.Join(workspace, "config.toml")
	backend, events := tuiBackendForServer(t, workspace, configPath, server.URL)

	before, err := backend.Command(context.Background(), "/run")
	if err != nil {
		t.Fatal(err)
	}
	if before != "No request has finished yet." {
		t.Fatalf("before any request: %q", before)
	}

	if err := backend.Submit(context.Background(), "op-run", ui.Submission{Text: "explain"}, func(event ui.Event) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The history states the stop, and the timing line does not claim completion.
	notes := make([]string, 0, 4)
	for _, event := range events {
		if event.Kind == ui.EventNotice {
			notes = append(notes, event.Notice)
		}
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "stopped the response before it finished") {
		t.Fatalf("the history did not state the stop: %#v", notes)
	}
	if !strings.Contains(joined, "Stopped after") {
		t.Fatalf("the timing line still claims completion: %#v", notes)
	}

	text, err := backend.Command(context.Background(), "/run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stop_reason=" + string(protocol.StopLength), "iterations=", "model_calls=", "tool_calls=", "calls:", "tokens:", "input=5", "output=3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("/run = %q, missing %q", text, want)
		}
	}
	// The summary is the report the session recorded, so the JSON a caller reads
	// and the text the interface shows cannot drift apart.
	encoded, err := json.Marshal(backend.lastReport())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["stop_reason"] != string(protocol.StopLength) {
		t.Fatalf("the recorded report = %s", encoded)
	}
}

// lastReport reads the backend's stored report for a test.
func (b *tuiBackend) lastReport() agent.RunReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastRun
}
