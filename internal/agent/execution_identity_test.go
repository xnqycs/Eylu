package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

// A host execution identity is never taken from a model call ID, and an activity
// identity is derived only from a host identity.
func TestExecutionAllocatorAvoidsReservedModelCallIDs(t *testing.T) {
	allocator := newExecutionAllocator([]string{"exec-1", "exec-2", ""})
	first, second, third := allocator.allocate(), allocator.allocate(), allocator.allocate()
	for _, id := range []string{first, second, third} {
		if id != "exec-3" && id != "exec-4" && id != "exec-5" {
			t.Fatalf("allocated %q, which is not the next free identity", id)
		}
	}
	if first == second || second == third || first == third {
		t.Fatalf("identities collided: %q %q %q", first, second, third)
	}
	if activityCallID("exec-9", 0) != "exec-9" || activityCallID("exec-9", 1) != "exec-9#2" || activityCallID("exec-9", 2) != "exec-9#3" {
		t.Fatalf("activity identities = %q %q", activityCallID("exec-9", 1), activityCallID("exec-9", 2))
	}
}

// A model that returns "a" and "a:1" must not make the fan-out of "a" collide
// with the model's own "a:1" call.
func TestExpandToolCallsSeparatesModelAndExecutionIdentity(t *testing.T) {
	plan := webtool.ResolvedWebToolPlan{Local: []webtool.ResolvedTool{
		{Definition: protocol.ToolDefinition{Name: "web_search", Kind: protocol.ToolWebSearch}, Execution: protocol.ExecutionDelegated, Target: "default"},
		{Definition: protocol.ToolDefinition{Name: "web_fetch", Kind: protocol.ToolWebFetch}, Execution: protocol.ExecutionDelegated, Target: "default"},
	}}
	calls := []protocol.ToolCall{
		{ID: "a", Name: "web_search", Arguments: json.RawMessage(`{"queries":["one","two"]}`)},
		{ID: "a:1", Name: "echo", Arguments: json.RawMessage(`{}`)},
		{ID: "b", Name: "web_fetch", Arguments: json.RawMessage(`{"url":"https://example.com"}`)},
	}
	expansion := expandToolCalls(calls, plan, newExecutionAllocator(callIDs(calls)))

	modelIDs := map[string]bool{"a": true, "a:1": true, "b": true}
	seen := make(map[string]int)
	for _, call := range expansion.calls {
		seen[call.ID]++
		if seen[call.ID] > 1 {
			t.Fatalf("duplicate execution identity %q", call.ID)
		}
	}
	if len(expansion.calls) != 4 {
		t.Fatalf("executions = %#v", expansion.calls)
	}
	// The two search queries and the single fetch are host executions; the plain
	// call is already a provider-level call and keeps its model ID.
	for _, call := range expansion.calls {
		switch {
		case strings.HasPrefix(call.ID, "exec-"):
			if modelIDs[call.ID] {
				t.Fatalf("execution identity %q reused a model call ID", call.ID)
			}
			if call.ParentCallID != "a" && call.ParentCallID != "b" {
				t.Fatalf("execution %q has parent %q", call.ID, call.ParentCallID)
			}
		case call.ID == "a:1":
			if call.Name != "echo" || call.ParentCallID != "" {
				t.Fatalf("plain call = %#v", call)
			}
		default:
			t.Fatalf("unexpected execution %#v", call)
		}
	}
	// The fan-out children are attributed to their own parents.
	for id, info := range expansion.web {
		if info.executionID != id {
			t.Fatalf("web identity mismatch: %q vs %q", id, info.executionID)
		}
		if info.parentCallID != "a" && info.parentCallID != "b" {
			t.Fatalf("web call %q has parent %q", id, info.parentCallID)
		}
	}
}

type recordingAudit struct {
	mu      sync.Mutex
	records []tool.AuditRecord
}

func (a *recordingAudit) Record(record tool.AuditRecord) {
	a.mu.Lock()
	a.records = append(a.records, record)
	a.mu.Unlock()
}

// idCollisionDriver issues one fan-out search call "a" and one plain call named
// "a:1" in the same response.
type idCollisionDriver struct {
	requests int
}

func (*idCollisionDriver) Name() string { return "openai_responses" }
func (*idCollisionDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true, HostedWebSearch: true, HostedAndFunctionTools: true}
}
func (d *idCollisionDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests == 1 {
		search := protocol.ToolCall{ID: "a", Name: "web_search", Arguments: json.RawMessage(`{"queries":["one","two"]}`)}
		plain := protocol.ToolCall{ID: "a:1", Name: "echo", Arguments: json.RawMessage(`{}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{
			{Kind: protocol.PartToolCall, ToolCall: &search},
			{Kind: protocol.PartToolCall, ToolCall: &plain},
		}}, Stop: protocol.StopToolUse}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

// The regression from the plan's matrix: a model call "a" and a model call "a:1"
// plus Web fan-out must stay separate executions with correct attribution.
func TestAgentLoopFanOutDoesNotCollideWithModelCallIDs(t *testing.T) {
	model := &idCollisionDriver{}
	item := &countingEchoTool{}
	audit := &recordingAudit{}
	executor := &tool.Executor{
		Registry: tool.NewRegistry(item), Policy: policy.AllowAllChecker{}, Audit: audit,
		MaxParallelTools: 2, Timeout: 2 * time.Second, SessionID: "session",
	}
	runtime := webFanOutRuntime(model, config.WebPermissionAllow)
	events := make([]protocol.ModelEvent, 0, 16)
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "collide", runtime, executor, LoopOptions{MaxTurns: 3}, false, func(event protocol.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || response.Turn.Parts[0].Text != "done" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	if item.calls.Load() != 1 {
		t.Fatalf("plain tool ran %d times", item.calls.Load())
	}
	transcript := conversation.Transcript()
	if len(transcript) != 4 || transcript[2].Role != protocol.RoleTool {
		t.Fatalf("transcript = %#v", transcript)
	}
	toolTurn := transcript[2]
	results := make([]*protocol.ToolResult, 0, 2)
	activities := make([]*protocol.WebActivity, 0, 2)
	for index := range toolTurn.Parts {
		part := &toolTurn.Parts[index]
		if part.ToolResult != nil {
			results = append(results, part.ToolResult)
		}
		if part.WebActivity != nil {
			activities = append(activities, part.WebActivity)
		}
	}
	// Results keep the model order and the model call IDs.
	if len(results) != 2 || results[0].CallID != "a" || results[1].CallID != "a:1" {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results[0].Content, "one") || results[1].Content != "echoed" {
		t.Fatalf("result content = %#v", results)
	}
	if len(activities) != 2 {
		t.Fatalf("activities = %#v", activities)
	}
	seen := map[string]bool{}
	for index, activity := range activities {
		if !strings.HasPrefix(activity.CallID, "exec-") || seen[activity.CallID] {
			t.Fatalf("activity[%d] identity = %q", index, activity.CallID)
		}
		seen[activity.CallID] = true
	}
	// The audit attributes each execution to its own parent call.
	audit.mu.Lock()
	defer audit.mu.Unlock()
	if len(audit.records) != 3 {
		t.Fatalf("audit records = %#v", audit.records)
	}
	parents := map[string]string{}
	for _, record := range audit.records {
		if prior, duplicate := parents[record.CallID]; duplicate {
			t.Fatalf("audit identity %q appeared twice (was %q)", record.CallID, prior)
		}
		parents[record.CallID] = record.ParentCallID
	}
	for id, parent := range parents {
		if id == "a:1" {
			if parent != "" {
				t.Fatalf("the plain call has parent %q", parent)
			}
			continue
		}
		if !strings.HasPrefix(id, "exec-") || parent != "a" {
			t.Fatalf("execution %q has parent %q", id, parent)
		}
	}
	// Every execution produced exactly one start and one terminal event: three
	// executions, of which the two fan-out queries report as web activities and
	// the plain call reports as a tool call.
	starts, terminals, webStarted, webCompleted := 0, 0, 0, 0
	for _, event := range events {
		switch event.Kind {
		case protocol.EventToolStart:
			starts++
		case protocol.EventToolResult:
			terminals++
		case protocol.EventWebSearchStarted:
			webStarted++
		case protocol.EventWebSearchCompleted:
			webCompleted++
		}
	}
	if starts+webStarted != 3 || terminals+webCompleted != 3 {
		t.Fatalf("events: starts=%d terminals=%d webStarted=%d webCompleted=%d", starts, terminals, webStarted, webCompleted)
	}
	if starts != 1 || terminals != 1 || webStarted != 2 || webCompleted != 2 {
		t.Fatalf("unexpected event split: starts=%d terminals=%d webStarted=%d webCompleted=%d", starts, terminals, webStarted, webCompleted)
	}
}

// Two fan-out parents in one response keep their own children, their own
// activities and their own results.
func TestAgentLoopTwoFanOutParentsStaySeparate(t *testing.T) {
	model := &twoSearchDriver{}
	executor := &tool.Executor{Registry: tool.NewRegistry(), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: 2 * time.Second}
	runtime := webFanOutRuntime(model, config.WebPermissionAllow)
	runtime.WebDelegate = func(_ context.Context, _ webtool.ResolvedTool, input json.RawMessage) protocol.ToolResult {
		var values struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(input, &values)
		return protocol.ToolResult{Content: "content " + values.Query}
	}
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "two", runtime, executor, LoopOptions{MaxTurns: 3}, false, nil)
	if err != nil || response.Turn.Parts[0].Text != "done" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	toolTurn := conversation.Transcript()[2]
	results := map[string]*protocol.ToolResult{}
	activities := make([]*protocol.WebActivity, 0, 4)
	for index := range toolTurn.Parts {
		part := &toolTurn.Parts[index]
		if part.ToolResult != nil {
			results[part.ToolResult.CallID] = part.ToolResult
		}
		if part.WebActivity != nil {
			activities = append(activities, part.WebActivity)
		}
	}
	if len(results) != 2 || results["first"] == nil || results["second"] == nil {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results["first"].Content, "alpha-one") || strings.Contains(results["first"].Content, "beta-one") {
		t.Fatalf("first result = %q", results["first"].Content)
	}
	if !strings.Contains(results["second"].Content, "beta-one") || strings.Contains(results["second"].Content, "alpha-one") {
		t.Fatalf("second result = %q", results["second"].Content)
	}
	if len(activities) != 4 {
		t.Fatalf("activities = %#v", activities)
	}
	seen := map[string]bool{}
	for index, activity := range activities {
		if seen[activity.CallID] {
			t.Fatalf("activity[%d] identity %q was reused", index, activity.CallID)
		}
		seen[activity.CallID] = true
	}
}

type twoSearchDriver struct{ requests int }

func (*twoSearchDriver) Name() string { return "openai_responses" }
func (*twoSearchDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true, ParallelTools: true, HostedWebSearch: true, HostedAndFunctionTools: true}
}
func (d *twoSearchDriver) Generate(_ context.Context, _ driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.requests++
	if d.requests == 1 {
		first := protocol.ToolCall{ID: "first", Name: "web_search", Arguments: json.RawMessage(`{"queries":["alpha-one","alpha-two"]}`)}
		second := protocol.ToolCall{ID: "second", Name: "web_search", Arguments: json.RawMessage(`{"queries":["beta-one","beta-two"]}`)}
		return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{
			{Kind: protocol.PartToolCall, ToolCall: &first},
			{Kind: protocol.PartToolCall, ToolCall: &second},
		}}, Stop: protocol.StopToolUse}, nil
	}
	return protocol.ModelResponse{Turn: protocol.Turn{Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}
