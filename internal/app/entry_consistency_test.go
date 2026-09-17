package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"Eylu/internal/protocol"
	"Eylu/internal/session"
	"Eylu/internal/ui"
)

// entryScript is one scripted provider conversation, expressed once and replayed
// for whichever entry point is under test.
//
// The point of the type is that both entries see exactly the same model behaviour:
// the comparison below is about what the host does with it, not about what the
// model said.
type entryScript struct {
	// envelope returns the response body for the n-th model call (1-based).
	envelope func(call int) string
}

// entryOutcome is the comparable part of what one entry point did with a script.
//
// Text layout, timestamps and identifiers are deliberately absent: they are the
// parts the plan allows the two entries to differ in. What is compared is the
// reliability contract - which calls ran and how they ended, why the request
// stopped, what it cost, and which lifecycle records reached the log.
type entryOutcome struct {
	toolStates   map[string]string
	turns        []string
	counters     map[string]int
	stopReason   string
	modelCalls   int
	inputTokens  int
	outputTokens int
	lifecycle    []string
	sideEffect   bool
	failed       bool
}

func (o entryOutcome) String() string {
	ids := make([]string, 0, len(o.toolStates))
	for id := range o.toolStates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	states := make([]string, 0, len(ids))
	for _, id := range ids {
		states = append(states, id+"="+o.toolStates[id])
	}
	keys := make([]string, 0, len(o.counters))
	for key := range o.counters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	counts := make([]string, 0, len(keys))
	for _, key := range keys {
		counts = append(counts, fmt.Sprintf("%s=%d", key, o.counters[key]))
	}
	return fmt.Sprintf("tools=[%s] turns=%v counters=[%s] stop=%q calls=%d in=%d out=%d lifecycle=%v sideEffect=%t failed=%t",
		strings.Join(states, " "), o.turns, strings.Join(counts, " "), o.stopReason, o.modelCalls, o.inputTokens, o.outputTokens, o.lifecycle, o.sideEffect, o.failed)
}

// entryServer serves one script on the model endpoint and answers the provider's
// capability probes with an empty document.
func entryServer(t *testing.T, script entryScript) *httptest.Server {
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
		envelope := script.envelope(call)
		if wantsStream(request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"type\":\"response.completed\",\"response\":" + envelope + "}\n\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(envelope))
	}))
}

func textEnvelope(text string) string {
	return `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":` + string(mustEncode(text)) + `}]}],"usage":{"input_tokens":4,"output_tokens":2}}`
}

func writeEnvelope(call protocol.ToolCall) string {
	arguments, _ := json.Marshal(string(call.Arguments))
	return `{"id":"r1","status":"completed","output":[{"type":"function_call","call_id":` + string(mustEncode(call.ID)) + `,"name":` + string(mustEncode(call.Name)) + `,"arguments":` + string(arguments) + `}],"usage":{"input_tokens":5,"output_tokens":3}}`
}

func writeFileCall(id string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Name: "write_file", Arguments: json.RawMessage(`{"path":"entry-target.txt","content":"written","reason":"test"}`)}
}

func mustEncode(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// entryScenarios are the requests every entry point has to handle the same way.
//
// Each scenario carries the absolute contract as well as being compared between
// the entries: a comparison alone would be satisfied by two entry points that are
// wrong in the same way.
var entryScenarios = []struct {
	name             string
	mode             string
	script           entryScript
	cancelledBefore  bool
	allowsSideEffect bool
	// wantToolStates is the terminal state of every call that reached the
	// executor. A call that was refused before it could run has no lifecycle record
	// at all, so it appears in wantCounters instead.
	wantToolStates map[string]string
	// wantCounters is the part of the run report that describes the calls.
	wantCounters map[string]int
	// wantTurns is the role of every turn the session holds.
	wantTurns []string
	// wantStopReason is the reason the request must record for itself.
	wantStopReason string
	// wantLifecycle lists the lifecycle records the log must hold.
	wantLifecycle []session.EventType
	// forbidLifecycle lists records that must not appear: a call that never got an
	// intent must leave no trace of having been about to happen.
	forbidLifecycle []session.EventType
}{
	{
		name: "completed", mode: "full",
		script:         entryScript{envelope: func(int) string { return textEnvelope("an answer") }},
		wantToolStates: map[string]string{},
		wantCounters:   map[string]int{"tool_calls": 0, "succeeded": 0, "failed": 0, "iterations": 1},
		wantTurns:      []string{"user", "agent"},
		wantStopReason: "completed",
		wantLifecycle:  []session.EventType{session.EventRequestStarted, session.EventRunReported},
		forbidLifecycle: []session.EventType{
			session.EventToolPrepared, session.EventToolExecutionIntent, session.EventToolCompleted,
		},
	},
	{
		name: "a tool runs and the request completes", mode: "full", allowsSideEffect: true,
		script: entryScript{envelope: func(call int) string {
			if call == 1 {
				return writeEnvelope(writeFileCall("call-write"))
			}
			return textEnvelope("done")
		}},
		wantToolStates: map[string]string{"call-write": "succeeded"},
		wantCounters:   map[string]int{"tool_calls": 1, "succeeded": 1, "failed": 0, "iterations": 2},
		wantTurns:      []string{"user", "agent", "tool", "agent"},
		wantStopReason: "completed",
		wantLifecycle: []session.EventType{
			session.EventRequestStarted, session.EventToolPrepared, session.EventToolExecutionIntent,
			session.EventToolCompleted, session.EventRunReported,
		},
	},
	{
		// Plan mode does not offer write_file at all, so the call cannot run: it is
		// reported as a failed call and leaves no lifecycle record, because it never
		// reached the point where it could have had an effect.
		name: "the tool is not available", mode: "plan",
		script: entryScript{envelope: func(call int) string {
			if call == 1 {
				return writeEnvelope(writeFileCall("call-write"))
			}
			return textEnvelope("done")
		}},
		wantToolStates: map[string]string{},
		wantCounters:   map[string]int{"tool_calls": 1, "succeeded": 0, "failed": 1, "iterations": 2},
		wantTurns:      []string{"agent", "tool", "agent"},
		wantStopReason: "completed",
		wantLifecycle:  []session.EventType{session.EventRequestStarted, session.EventRunReported},
		forbidLifecycle: []session.EventType{
			session.EventToolExecutionIntent, session.EventToolCompleted,
		},
	},
	{
		name: "the model returns an unusable call", mode: "full",
		script: entryScript{envelope: func(int) string {
			return writeEnvelope(protocol.ToolCall{ID: "call-broken", Name: "write_file", Arguments: json.RawMessage(`{`)})
		}},
		wantToolStates: map[string]string{},
		wantCounters:   map[string]int{"tool_calls": 0, "succeeded": 0, "failed": 0, "iterations": 1},
		// The refused response never becomes a turn, so the session holds the user
		// turn alone, and the request still reports why it ended.
		wantTurns:      []string{"user"},
		wantStopReason: "aborted",
		wantLifecycle:  []session.EventType{session.EventRequestStarted, session.EventRunReported},
		forbidLifecycle: []session.EventType{
			session.EventToolPrepared, session.EventToolExecutionIntent, session.EventToolCompleted,
		},
	},
	{
		// The request context is already cancelled when the request starts. The
		// cancellation is in place before anything runs, so nothing about the outcome
		// depends on which side is faster - which a cancellation delivered from the
		// side cannot promise, because it races the response it means to interrupt.
		//
		// What this pins is the entry-level contract for a cancelled request: it must
		// start no model call and no tool, and it must not claim a run that never
		// happened. The cancellation semantics of a request that is already running -
		// which batch stops, what an in-flight call reports, which calls close as
		// not_executed - are pinned deterministically at the agent level instead, where
		// the model driver is scripted and the test places the cancellation between two
		// calls rather than racing a response (see internal/agent cancellation and
		// ownership tests).
		name: "the request context is already cancelled", mode: "full",
		cancelledBefore: true,
		script: entryScript{envelope: func(int) string {
			return writeEnvelope(writeFileCall("call-write"))
		}},
		wantToolStates: map[string]string{},
		wantCounters:   map[string]int{"tool_calls": 0, "succeeded": 0, "failed": 0},
		wantTurns:      []string{},
		wantStopReason: "",
		wantLifecycle:  []session.EventType{},
		forbidLifecycle: []session.EventType{
			session.EventRequestStarted, session.EventTurnAppended, session.EventRunReported,
			session.EventToolPrepared, session.EventToolExecutionIntent, session.EventToolCompleted,
		},
	},
}

// assertEntryContract checks the absolute part of one entry point's outcome.
func assertEntryContract(t *testing.T, entry string, outcome entryOutcome, scenario struct {
	name             string
	mode             string
	script           entryScript
	cancelledBefore  bool
	allowsSideEffect bool
	wantToolStates   map[string]string
	wantCounters     map[string]int
	wantTurns        []string
	wantStopReason   string
	wantLifecycle    []session.EventType
	forbidLifecycle  []session.EventType
}) {
	t.Helper()
	if outcome.stopReason != scenario.wantStopReason {
		t.Fatalf("%s stop reason = %q, want %q (%s)", entry, outcome.stopReason, scenario.wantStopReason, outcome)
	}
	if strings.Join(outcome.turns, ",") != strings.Join(scenario.wantTurns, ",") {
		t.Fatalf("%s turns = %v, want %v", entry, outcome.turns, scenario.wantTurns)
	}
	for name, want := range scenario.wantCounters {
		if outcome.counters[name] != want {
			t.Fatalf("%s report counter %s = %d, want %d (%s)", entry, name, outcome.counters[name], want, outcome)
		}
	}
	if len(outcome.toolStates) != len(scenario.wantToolStates) {
		t.Fatalf("%s tool states = %v, want %v", entry, outcome.toolStates, scenario.wantToolStates)
	}
	for id, want := range scenario.wantToolStates {
		if outcome.toolStates[id] != want {
			t.Fatalf("%s tool %s = %q, want %q", entry, id, outcome.toolStates[id], want)
		}
	}
	recorded := make(map[string]bool, len(outcome.lifecycle))
	for _, eventType := range outcome.lifecycle {
		recorded[eventType] = true
	}
	for _, want := range scenario.wantLifecycle {
		if !recorded[string(want)] {
			t.Fatalf("%s recorded no %s event (%s)", entry, want, outcome)
		}
	}
	for _, forbidden := range scenario.forbidLifecycle {
		if recorded[string(forbidden)] {
			t.Fatalf("%s recorded %s although that must not happen (%s)", entry, forbidden, outcome)
		}
	}
	if outcome.sideEffect != scenario.allowsSideEffect {
		t.Fatalf("%s side effect = %t, want %t (%s)", entry, outcome.sideEffect, scenario.allowsSideEffect, outcome)
	}
}

// Both entry points must reach the same conclusion on the same scripted model:
// which calls ran and how they ended, why the request stopped, what it cost and
// which lifecycle records reached the log.
//
// The two are allowed to differ in how they render an answer and in the
// identifiers and timestamps they assign. They are not allowed to differ in
// whether a side effect was checkpointed, whether a call that could not run left a
// trace of having run, or whether the reason a request stopped survived it.
func TestEntriesAgreeOnTheReliabilityContract(t *testing.T) {
	for _, scenario := range entryScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			cli := runCLIEntry(t, scenario.script, scenario.mode, scenario.cancelledBefore)
			tui := runTUIEntry(t, scenario.script, scenario.mode, scenario.cancelledBefore)
			assertEntryContract(t, "the text entry point", cli, scenario)
			assertEntryContract(t, "the interface", tui, scenario)
			if cli.String() != tui.String() {
				t.Fatalf("the entries disagree:\n cli: %s\n tui: %s", cli, tui)
			}
			if scenario.wantStopReason != "" && cli.stopReason == "" {
				t.Fatalf("the request recorded no stop reason: %s", cli)
			}
		})
	}
}

// cancelledContext returns a request context that is already cancelled when the
// request starts, so the scenario depends on no timing at all.
func cancelledContext(cancelled bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	if cancelled {
		cancel()
	}
	return ctx, cancel
}

// runCLIEntry drives one request through the text entry point.
func runCLIEntry(t *testing.T, script entryScript, mode string, cancelledBefore bool) entryOutcome {
	t.Helper()
	isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "entry-secret")
	ctx, cancel := cancelledContext(cancelledBefore)
	defer cancel()
	server := entryServer(t, script)
	defer server.Close()

	sessionID := "entry-cli-session"
	args := []string{
		"--config", filepath.Join(workspace, "config.toml"), "--workspace", workspace,
		"chat", "do it", "--base-url", server.URL, "--model", "test-model",
		"--session", sessionID, "--mode", mode,
	}
	var stdout, stderr bytes.Buffer
	failed := Execute(ctx, args, strings.NewReader(""), &stdout, &stderr) != 0
	return collectEntryOutcome(t, workspace, sessionID, failed)
}

// runTUIEntry drives one request through the interface entry point.
func runTUIEntry(t *testing.T, script entryScript, mode string, cancelledBefore bool) entryOutcome {
	t.Helper()
	isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "entry-secret")
	ctx, cancel := cancelledContext(cancelledBefore)
	defer cancel()
	server := entryServer(t, script)
	defer server.Close()

	sessionID := "entry-tui-session"
	backend, _ := tuiBackendWithSession(t, workspace, filepath.Join(workspace, "config.toml"), server.URL, sessionID)
	backend.mu.Lock()
	backend.opts.mode = mode
	backend.mu.Unlock()
	err := backend.Submit(ctx, "op-entry", ui.Submission{Text: "do it"}, func(ui.Event) {})
	return collectEntryOutcome(t, workspace, sessionID, err != nil)
}

// collectEntryOutcome reads one finished request out of the session log, which is
// the record both entries are required to leave.
func collectEntryOutcome(t *testing.T, workspace, sessionID string, failed bool) entryOutcome {
	t.Helper()
	outcome := entryOutcome{toolStates: map[string]string{}, counters: map[string]int{}, failed: failed}
	if _, err := os.Stat(filepath.Join(workspace, "entry-target.txt")); err == nil {
		outcome.sideEffect = true
	}
	store, err := session.Open("")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, loadErr := store.Load(sessionID)
	if loadErr != nil {
		// A request that never reached the log is still a comparable outcome.
		return outcome
	}
	if snapshot.LastRun != nil {
		run := snapshot.LastRun
		outcome.stopReason = run.StopReason
		outcome.modelCalls = run.ModelCalls
		outcome.inputTokens = run.InputTokens
		outcome.outputTokens = run.OutputTokens
		outcome.counters = map[string]int{
			"tool_calls": run.ToolCalls, "succeeded": run.Succeeded, "failed": run.Failed,
			"rejected": run.Rejected, "cancelled": run.Cancelled, "not_executed": run.NotExecuted,
			"outcome_unknown": run.OutcomeUnknown, "iterations": run.Iterations,
		}
	}
	for _, turn := range snapshot.Turns {
		outcome.turns = append(outcome.turns, string(turn.Role))
	}
	for _, event := range readSessionEvents(t, store, sessionID) {
		switch event.Type {
		case session.EventRequestStarted, session.EventToolPrepared, session.EventToolExecutionIntent,
			session.EventToolCompleted, session.EventRunReported, session.EventTurnAppended:
			outcome.lifecycle = append(outcome.lifecycle, string(event.Type))
		default:
			continue
		}
		switch {
		case event.Type == session.EventToolExecutionIntent && event.Intent != nil:
			outcome.toolStates[event.Intent.CallID] = "started"
		case event.Type == session.EventToolCompleted && event.Completion != nil:
			outcome.toolStates[event.Completion.CallID] = string(event.Completion.State)
		}
	}
	sort.Strings(outcome.lifecycle)
	return outcome
}
