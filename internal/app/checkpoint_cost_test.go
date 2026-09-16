package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/tool"
)

// multiWriteDriver asks for one write_file call per path and then finishes.
type multiWriteDriver struct {
	paths    []string
	requests int
}

func (*multiWriteDriver) Name() string { return "stub" }
func (*multiWriteDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}

func (d *multiWriteDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests > 1 {
		done := protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}
		return protocol.ModelResponse{Turn: done, Stop: protocol.StopCompleted}, nil
	}
	parts := make([]protocol.Part, 0, len(d.paths))
	for index, path := range d.paths {
		call := protocol.ToolCall{
			ID:   fmt.Sprintf("write-%d", index),
			Name: "write_file",
			// The path has to be JSON-escaped rather than %q'd: on Windows a path
			// with backslashes is not valid JSON in a quoted Go literal.
			Arguments: json.RawMessage(fmt.Sprintf(`{"path":%s,"content":"c","reason":"test"}`, mustJSON(path))),
		}
		parts = append(parts, protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call})
	}
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: parts}, Stop: protocol.StopToolUse}, nil
}

func mustJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// runWriteBatch drives one request whose single batch writes every given path.
func runWriteBatch(t *testing.T, fixture *sessionSyncFixture, paths []string) error {
	t.Helper()
	write, err := tool.NewWriteFile(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Checkpoint: fixture.controller, Workspace: fixture.workspace, MaxParallelTools: 1,
	}
	conversation := conversationWithPrompts(t, fixture.sessionID)
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   &multiWriteDriver{paths: paths}, Workspace: fixture.workspace, PermissionMode: "full",
	}
	_, err = conversation.Run(context.Background(), "write the files", runtime, executor,
		agent.LoopOptions{MaxTurns: 3, MaxTotalTokens: 1_000_000}, false, nil)
	return err
}

// One batch of side-effecting calls costs one append for its intents instead of
// one per call, and the guarantee that no side effect starts before its intent is
// durable is untouched: the single write still completes before the first call
// starts. Completions stay immediate, one per call, which is what the count below
// shows.
func TestOneBatchCostsOneIntentAppendInsteadOfOnePerCall(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	appends := 0
	intentsAppended := 0
	completionsAppended := 0
	fixture.controller.appendEvents = func(id string, events []session.Event) ([]session.Event, error) {
		appends++
		for _, event := range events {
			switch event.Type {
			case session.EventToolExecutionIntent:
				intentsAppended++
			case session.EventToolCompleted:
				completionsAppended++
			}
		}
		return fixture.store.Append(id, events)
	}
	const calls = 3
	paths := []string{"one.txt", "two.txt", "three.txt"}
	if err := runWriteBatch(t, fixture, paths); err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(fixture.workspace, path)); err != nil {
			t.Fatalf("%s was not written: %v", path, err)
		}
	}
	if intentsAppended != calls {
		t.Fatalf("intent events = %d, want one per call", intentsAppended)
	}
	if completionsAppended != calls {
		t.Fatalf("completion events = %d, want one per call", completionsAppended)
	}
	// Before the batch write this was 2N = 6 appends: one intent and one completion
	// per call. The intents now share one append.
	if want := 1 + calls; appends != want {
		t.Fatalf("appends = %d, want %d (one for the batch intents plus one completion per call); it was %d before", appends, want, 2*calls)
	}
}

// A completion whose append fails is not a permanent unknown: the result is kept
// in memory, the next append that succeeds writes it, and a reload then reports
// the execution by its recorded outcome.
func TestAFailedCompletionIsRetriedInsteadOfBecomingAPermanentUnknown(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	failures := 1
	fixture.controller.appendEvents = func(id string, events []session.Event) ([]session.Event, error) {
		for _, event := range events {
			if event.Type == session.EventToolCompleted && failures > 0 {
				failures--
				return nil, errors.New("log unavailable")
			}
		}
		return fixture.store.Append(id, events)
	}
	if err := runWriteBatch(t, fixture, []string{"one.txt"}); err == nil {
		t.Fatal("the failed completion was not reported")
	}
	// The operation happened and its result is held, not lost.
	if _, err := os.Stat(filepath.Join(fixture.workspace, "one.txt")); err != nil {
		t.Fatalf("the write did not happen: %v", err)
	}
	if got := fixture.controller.Compensations(); got != 1 {
		t.Fatalf("held completions = %d, want the one whose append failed", got)
	}
	if got := countEvents(fixture.events(t), session.EventToolCompleted); got != 0 {
		t.Fatalf("a completion was written although its append failed: %d", got)
	}

	// The transient failure is over. The next successful sync writes the held
	// record, and the log then holds both sides of the execution once.
	conversation := conversationWithPrompts(t, fixture.sessionID)
	if err := fixture.controller.Sync(conversation, fixture.manager, chatOptions{}, nil); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := fixture.controller.Compensations(); got != 0 {
		t.Fatalf("held completions = %d after a successful sync", got)
	}
	events := fixture.events(t)
	if got := countEvents(events, session.EventToolExecutionIntent); got != 1 {
		t.Fatalf("intents = %d", got)
	}
	if got := countEvents(events, session.EventToolCompleted); got != 1 {
		t.Fatalf("completions = %d, want the compensated record exactly once", got)
	}
	// The whole point: a restart no longer calls this execution unknown.
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("the compensated execution is still reported as unknown: %#v", snapshot.PendingIntents)
	}
}

// The recovery conclusion is drawn from evidence and says which evidence it used.
func TestRecoveryConclusionStatesItsEvidence(t *testing.T) {
	workspace := t.TempDir()
	unchanged := filepath.Join(workspace, "unchanged.txt")
	if err := os.WriteFile(unchanged, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, ok := fileHashEvidence(unchanged)
	if !ok {
		t.Fatal("the fixture could not be hashed")
	}
	changed := filepath.Join(workspace, "changed.txt")
	if err := os.WriteFile(changed, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedBefore, _ := fileHashEvidence(changed)
	if err := os.WriteFile(changed, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(workspace, "missing.txt")

	tests := []struct {
		name    string
		intent  session.ToolIntent
		want    []string
		notWant []string
	}{
		{
			name:    "an unchanged target is judged not to have happened",
			intent:  session.ToolIntent{CallID: "c1", Tool: "write_file", TargetPath: unchanged, PreviousHash: before},
			want:    []string{"write_file", unchanged, "did not change its target", "judged not to have happened", before, "not replayed"},
			notWant: []string{"outcome unknown"},
		},
		{
			name:   "a changed target stays unknown and names both hashes",
			intent: session.ToolIntent{CallID: "c2", Tool: "write_file", TargetPath: changed, PreviousHash: changedBefore},
			want:   []string{"changed after the intent was recorded", changedBefore, "outcome is unknown", "not proven to be the cause", "not replayed"},
		},
		{
			name:   "an unreadable target is an absence of evidence",
			intent: session.ToolIntent{CallID: "c3", Tool: "write_file", TargetPath: missing, PreviousHash: "hash-before"},
			want:   []string{"outcome unknown", "cannot be read now", "hash-before", "not replayed"},
		},
		{
			name:   "no recorded hash is not evidence either",
			intent: session.ToolIntent{CallID: "c4", Tool: "bash", TargetPath: unchanged},
			want:   []string{"outcome unknown", "no hash was recorded before the call", "not replayed"},
		},
		{
			name:   "a call with no target says so",
			intent: session.ToolIntent{CallID: "c5", Tool: "bash"},
			want:   []string{"outcome unknown", "not replayed"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := describePendingIntent(test.intent)
			for _, want := range test.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message = %q, missing %q", message, want)
				}
			}
			for _, forbidden := range test.notWant {
				if strings.Contains(message, forbidden) {
					t.Fatalf("message = %q, should not contain %q", message, forbidden)
				}
			}
		})
	}
}
