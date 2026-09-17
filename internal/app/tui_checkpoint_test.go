package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/environment"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
	"Eylu/internal/ui"
)

// scriptedWriteServer answers the first model call with one write_file call and
// every later one with a final answer, so a test can drive a real side effect
// through a real entry point. Both the streaming and the whole-response transport
// are served, because the interface streams.
//
// Only the model endpoint is scripted: the app also probes the provider for its
// capabilities, and those probe responses are not part of the script.
func scriptedWriteServer(t *testing.T, path, content string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		envelope := `{"id":"r2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":2}}`
		if call == 1 {
			arguments, err := json.Marshal(map[string]string{"path": path, "content": content, "reason": "test"})
			if err != nil {
				t.Errorf("encode arguments: %v", err)
				return
			}
			encodedArguments, err := json.Marshal(string(arguments))
			if err != nil {
				t.Errorf("encode arguments string: %v", err)
				return
			}
			envelope = `{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":"call-write","name":"write_file","arguments":` + string(encodedArguments) + `}],"usage":{"input_tokens":5,"output_tokens":3}}`
		}
		if wantsStream(request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + envelope + "}\n\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(envelope))
	}))
}

// wantsStream reports whether the request asked for a streamed response.
func wantsStream(request *http.Request) bool {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return false
	}
	return bytes.Contains(body, []byte(`"stream":true`))
}

// tuiBackendWithSession wires the real TUI entry point against a scripted
// provider and a real session store, which is the wiring the interface uses.
func tuiBackendWithSession(t *testing.T, workspace, configPath, baseURL, sessionID string) (*tuiBackend, *session.Store) {
	t.Helper()
	isolateUserState(t)
	cfg := config.Default()
	cfg.PermissionMode = "full"
	cfg.ActiveProvider = "work"
	cfg.Providers["work"] = config.ProviderConfig{Adapter: "openai_responses", BaseURL: baseURL, Model: "test-model"}
	manager, err := provider.NewManager(configPath, cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Active()
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.Open("")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(session.Snapshot{
		SessionID: sessionID, Workspace: workspace, Environment: environment.Context{WorkingDirectory: workspace},
		PermissionMode: "full",
		Provider: session.ProviderState{
			Name: snapshot.Name, Generation: snapshot.Generation,
			Adapter: "openai_responses", BaseURL: baseURL, Model: "test-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	appRuntime := &runtime{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		workspace: workspace, trustPrompted: make(map[string]bool),
	}
	appRuntime.session = newSessionRuntime(store, created, workspace, nil)
	backend := &tuiBackend{
		runtime: appRuntime, conversation: conversationWithPrompts(t, created.SessionID), manager: manager,
		skills: registry, skillSession: skill.NewSession(registry, nil),
	}
	return backend, store
}

// readSessionEvents reads the log of one session as events.
func readSessionEvents(t *testing.T, store *session.Store, sessionID string) []session.Event {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(store.Root(), sessionID, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	events := make([]session.Event, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event session.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// A side effect performed through the TUI entry point must be protected by the
// same checkpoint the text entry point uses, and the request's run summary must
// survive a restart.
//
// The two entry points share the reliability guarantees; only what they display
// may differ. Recording the intent and the completion only for the CLI leaves the
// interface able to change a file with nothing in the log that says the operation
// was about to happen.
func TestTUIEntryPointCheckpointsSideEffectsAndPersistsTheRunReport(t *testing.T) {
	workspace := t.TempDir()
	server := scriptedWriteServer(t, "tui-target.txt", "written by the interface")
	defer server.Close()
	backend, store := tuiBackendWithSession(t, workspace, filepath.Join(workspace, "config.toml"), server.URL, "tui-checkpoint")

	events := make([]ui.Event, 0, 8)
	if err := backend.Submit(context.Background(), "op-tui", ui.Submission{Text: "write the file"}, func(event ui.Event) {
		events = append(events, event)
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "tui-target.txt"))
	if err != nil || string(data) != "written by the interface" {
		t.Fatalf("the side effect did not happen through the interface: %q err=%v", data, err)
	}

	recorded := readSessionEvents(t, store, "tui-checkpoint")
	counts := map[session.EventType]int{}
	for _, event := range recorded {
		counts[event.Type]++
	}
	for _, want := range []session.EventType{
		session.EventRequestStarted, session.EventToolPrepared, session.EventToolExecutionIntent,
		session.EventToolCompleted, session.EventTurnAppended, session.EventRunReported,
	} {
		if counts[want] == 0 {
			t.Fatalf("the interface recorded no %s event: %#v", want, counts)
		}
	}

	// A restart reads the reason the request stopped from the log alone.
	reopened, err := session.Open(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, diagnostics, err := reopened.LoadRecovering("tui-checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if snapshot.LastRun == nil {
		t.Fatal("the interface recorded no run summary")
	}
	if snapshot.LastRun.RequestID == "" || snapshot.LastRun.ModelCalls == 0 {
		t.Fatalf("the stored summary is incomplete: %#v", snapshot.LastRun)
	}
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("a completed call stayed pending: %#v", snapshot.PendingIntents)
	}
}

// A general subagent runs its own model conversation inside the parent session, so
// its provider-assigned call IDs live in the same space as the parent's. A parent
// call and a subagent call that happen to share a call ID are two different calls:
// recording the second must not be refused as a rewrite of the first, and its
// completion must not close the parent's pending record.
func TestToolLifecycleRecordsAreScopedToTheirRequest(t *testing.T) {
	paths := map[string]string{"parent": "parent.txt", "subagent-task": "subagent.txt"}
	records := []struct {
		name    string
		record  func(*sessionRuntime, tool.Intent) error
		wantIDs int
	}{
		{
			name: "batched write",
			record: func(runtime *sessionRuntime, intent tool.Intent) error {
				return runtime.RecordIntents([]tool.Intent{intent})
			},
			wantIDs: 1,
		},
		{
			name:    "per-call write",
			record:  func(runtime *sessionRuntime, intent tool.Intent) error { return runtime.RecordIntent(intent) },
			wantIDs: 1,
		},
	}
	for _, test := range records {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSessionSyncFixture(t)
			for requestID, path := range paths {
				intent := tool.Intent{RequestID: requestID, CallID: "call-1", Tool: "write_file", Risk: "write", TargetPath: path}
				if err := test.record(fixture.controller, intent); err != nil {
					t.Fatalf("recording %s call-1 was refused: %v", requestID, err)
				}
			}
			events := fixture.events(t)
			if got := countEvents(events, session.EventToolExecutionIntent); got != 2 {
				t.Fatalf("intents recorded = %d, want one per request", got)
			}
			if pending := fixture.controller.PendingIntents(); len(pending) != 2 {
				t.Fatalf("pending intents = %#v, want one per request", pending)
			}
			// Completing the subagent's call leaves the parent's pending.
			if err := fixture.controller.RecordCompletion(tool.Completion{
				RequestID: "subagent-task", CallID: "call-1", Tool: "write_file", State: "succeeded",
			}); err != nil {
				t.Fatal(err)
			}
			pending := fixture.controller.PendingIntents()
			if len(pending) != 1 || pending[0].RequestID != "parent" || pending[0].TargetPath != paths["parent"] {
				t.Fatalf("pending intents = %#v, want only the parent's call", pending)
			}
			// The same holds after a restart, which rebuilds the set from the log.
			snapshot := reload(t, fixture)
			if len(snapshot.PendingIntents) != 1 || snapshot.PendingIntents[0].RequestID != "parent" {
				t.Fatalf("reloaded pending intents = %#v", snapshot.PendingIntents)
			}
		})
	}
}

// A completion written before the request was part of the record still closes its
// intent: the older log keeps the behaviour it had.
func TestAnOlderCompletionWithoutARequestStillClosesItsIntent(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	if err := fixture.controller.RecordIntent(tool.Intent{RequestID: "request-1", CallID: "exec-1", Tool: "write_file", Risk: "write"}); err != nil {
		t.Fatal(err)
	}
	completion := session.ToolCompletion{CallID: "exec-1", Tool: "write_file", State: "succeeded"}
	if _, err := fixture.store.Append(fixture.sessionID, []session.Event{{Type: session.EventToolCompleted, Completion: &completion}}); err != nil {
		t.Fatal(err)
	}
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("an older completion no longer closes its intent: %#v", snapshot.PendingIntents)
	}
}

// An intent that cannot be recorded must stop the side effect on the interface
// entry point exactly as it does on the text one.
func TestTUIEntryPointStopsWhenTheIntentCannotBeRecorded(t *testing.T) {
	workspace := t.TempDir()
	server := scriptedWriteServer(t, "blocked-target.txt", "must not appear")
	defer server.Close()
	backend, _ := tuiBackendWithSession(t, workspace, filepath.Join(workspace, "config.toml"), server.URL, "tui-blocked")
	// The lifecycle record is the only thing that fails; every other append works.
	store := backend.runtime.session.store
	backend.runtime.session.appendEvents = func(id string, events []session.Event) ([]session.Event, error) {
		for _, event := range events {
			if event.Type == session.EventToolExecutionIntent {
				return nil, fmt.Errorf("log unavailable")
			}
		}
		return store.Append(id, events)
	}

	err := backend.Submit(context.Background(), "op-blocked", ui.Submission{Text: "write the file"}, func(ui.Event) {})
	if err == nil {
		t.Fatal("the blocked lifecycle record was not reported")
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "blocked-target.txt")); !os.IsNotExist(statErr) {
		t.Fatal("the file was written although its intent could not be recorded")
	}
}
