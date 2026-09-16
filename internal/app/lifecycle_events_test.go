package app

import (
	"context"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/tool"
)

// The request lifecycle is readable from the log alone: a request start, the calls
// the executor accepted, the intents that precede their side effects, and the
// completions. The prepared record takes its identity from the conversation's
// pending set, which is the source the view itself is built from.
func TestRequestAndPreparedEventsReachTheLog(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	conversation := conversationWithPrompts(t, fixture.sessionID)
	const requestID = "request-7"
	if err := fixture.controller.RecordRequestStarted(requestID); err != nil {
		t.Fatal(err)
	}
	write, err := tool.NewWriteFile(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(write), Policy: policy.AllowAllChecker{},
		Checkpoint: fixture.controller, Workspace: fixture.workspace,
	}
	runtime := agent.Runtime{
		Provider: provider.Snapshot{Name: "stub", Config: config.ProviderConfig{Adapter: "stub", BaseURL: "https://example.test/v1", Model: "stub-model"}},
		Driver:   &writeDriver{path: "target.txt", content: "written"}, Workspace: fixture.workspace, PermissionMode: "full",
	}
	// This is the wiring the request path uses: the hook persists the prepared
	// record for the call the conversation just marked prepared.
	if _, err := conversation.Run(context.Background(), "write the file", runtime, executor, agent.LoopOptions{
		MaxTurns: 3, MaxTotalTokens: 1_000_000, RequestID: requestID,
		OnToolPrepared: func(call protocol.ToolCall) error {
			return fixture.controller.RecordToolPrepared(conversation, call)
		},
	}, false, nil); err != nil {
		t.Fatalf("err = %v", err)
	}

	events := fixture.events(t)
	var started, prepared *session.Event
	for index := range events {
		switch events[index].Type {
		case session.EventRequestStarted:
			started = &events[index]
		case session.EventToolPrepared:
			prepared = &events[index]
		}
	}
	if started == nil || started.RequestID != requestID {
		t.Fatalf("request_started = %#v", started)
	}
	if prepared == nil || prepared.Prepared == nil {
		t.Fatalf("tool_prepared = %#v", prepared)
	}
	// The identity comes from the pending set, so the record and the view agree
	// about which call was prepared and which round it belonged to.
	if prepared.Prepared.CallID != "write-1" || prepared.Prepared.Tool != "write_file" {
		t.Fatalf("prepared payload = %#v", prepared.Prepared)
	}
	if prepared.Prepared.Iteration != 1 || prepared.Prepared.RequestID != requestID {
		t.Fatalf("prepared payload = %#v", prepared.Prepared)
	}
	if prepared.Prepared.PreparedAt.IsZero() {
		t.Fatalf("prepared payload has no time: %#v", prepared.Prepared)
	}
	// The call reached both sides of its lifecycle, and the intent was closed.
	if countEvents(events, session.EventToolExecutionIntent) != 1 || countEvents(events, session.EventToolCompleted) != 1 {
		t.Fatalf("lifecycle events = %#v", events)
	}
	// Both new events are evidence, not state: rebuilding the session from the log
	// changes nothing about it, and the finished call is not pending.
	snapshot := reload(t, fixture)
	if len(snapshot.PendingIntents) != 0 {
		t.Fatalf("the prepared record made the call pending: %#v", snapshot.PendingIntents)
	}
	if _, err := agent.RestoreConversation(agentStateFromSnapshot(snapshot)); err != nil {
		t.Fatalf("restore = %v", err)
	}
}

// A request start with no request ID is not written, because a bracketing event
// that cannot name what it brackets answers nothing.
func TestRequestStartedNeedsARequestID(t *testing.T) {
	fixture := newSessionSyncFixture(t)
	if err := fixture.controller.RecordRequestStarted(""); err != nil {
		t.Fatal(err)
	}
	if got := countEvents(fixture.events(t), session.EventRequestStarted); got != 0 {
		t.Fatalf("request_started events = %d, want none", got)
	}
}
