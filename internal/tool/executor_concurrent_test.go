package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type cancellingParallelTool struct {
	active  atomic.Int32
	started atomic.Int32
}

func (t *cancellingParallelTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "wait", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *cancellingParallelTool) Risk() policy.Risk  { return policy.RiskRead }
func (t *cancellingParallelTool) ParallelSafe() bool { return true }
func (t *cancellingParallelTool) Execute(ctx context.Context, _ json.RawMessage) protocol.ToolResult {
	t.started.Add(1)
	t.active.Add(1)
	defer t.active.Add(-1)
	<-ctx.Done()
	return protocol.ToolResult{Content: ctx.Err().Error(), IsError: true}
}

func TestExecuteConcurrentCancellationConverges(t *testing.T) {
	parallelTool := &cancellingParallelTool{}
	executor := &Executor{Registry: NewRegistry(parallelTool), Policy: policy.AllowAllChecker{}, MaxParallelTools: 2, Timeout: time.Second}
	calls := make([]protocol.ToolCall, 5)
	for index := range calls {
		calls[index] = protocol.ToolCall{ID: string(rune('a' + index)), Name: "wait", Arguments: json.RawMessage(`{}`)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []protocol.ToolResult, 1)
	go func() { done <- executor.ExecuteConcurrent(ctx, "request", calls) }()
	deadline := time.After(time.Second)
	for parallelTool.active.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("parallel tools did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case results := <-done:
		if len(results) != len(calls) || parallelTool.active.Load() != 0 || parallelTool.started.Load() != 2 {
			t.Fatalf("results=%d active=%d started=%d", len(results), parallelTool.active.Load(), parallelTool.started.Load())
		}
		for index, result := range results {
			if result.CallID != calls[index].ID || !result.IsError || !strings.Contains(result.Content, "cancel") {
				t.Fatalf("result[%d] = %#v", index, result)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("parallel cancellation did not converge")
	}
}

type panickingParallelTool struct{}

func (panickingParallelTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "panic", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (panickingParallelTool) Risk() policy.Risk  { return policy.RiskRead }
func (panickingParallelTool) ParallelSafe() bool { return true }
func (panickingParallelTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	panic("fixture panic")
}

func TestExecuteRecoversToolPanic(t *testing.T) {
	executor := &Executor{Registry: NewRegistry(panickingParallelTool{}), Policy: policy.AllowAllChecker{}}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "panic-call", Name: "panic", Arguments: json.RawMessage(`{}`)})
	if !result.IsError || result.CallID != "panic-call" || !strings.Contains(result.Content, "fixture panic") {
		t.Fatalf("result = %#v", result)
	}
}

type scheduledBatchTool struct {
	started chan string
	release map[string]chan struct{}
	calls   atomic.Int32
}

func (t *scheduledBatchTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "scheduled", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *scheduledBatchTool) Risk() policy.Risk { return policy.RiskRead }
func (t *scheduledBatchTool) ClassifyConcurrency(input json.RawMessage, _ policy.Outcome) ConcurrencySpec {
	var value struct {
		ID       string `json:"id"`
		Mode     string `json:"mode"`
		Path     string `json:"path"`
		Access   string `json:"access"`
		Resource string `json:"resource"`
	}
	_ = json.Unmarshal(input, &value)
	switch value.Mode {
	case "exclusive":
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	case "shared":
		return ConcurrencySpec{Mode: ConcurrencyShared}
	default:
		access := ResourceRead
		if value.Access == "write" {
			access = ResourceWrite
		}
		kind := ResourceFile
		if value.Resource == "tree" {
			kind = ResourceTree
		}
		return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: kind, Path: value.Path, Access: access}}}
	}
}
func (t *scheduledBatchTool) Execute(ctx context.Context, input json.RawMessage) protocol.ToolResult {
	var value struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(input, &value)
	t.calls.Add(1)
	select {
	case t.started <- value.ID:
	case <-ctx.Done():
		return toolError(ctx.Err().Error())
	}
	select {
	case <-t.release[value.ID]:
		return protocol.ToolResult{Content: value.ID}
	case <-ctx.Done():
		return toolError(ctx.Err().Error())
	}
}

func TestExecuteBatchSchedulesResourceConflictsAndCompletionEvents(t *testing.T) {
	item := &scheduledBatchTool{started: make(chan string, 4), release: map[string]chan struct{}{
		"first": make(chan struct{}), "blocked": make(chan struct{}), "independent": make(chan struct{}),
	}}
	audit := &memoryAudit{}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: time.Second, Audit: audit}
	calls := []protocol.ToolCall{
		{ID: "call-first", Name: "scheduled", Arguments: json.RawMessage(`{"id":"first","path":"same.go","access":"write"}`)},
		{ID: "call-blocked", Name: "scheduled", Arguments: json.RawMessage(`{"id":"blocked","path":"same.go","access":"write"}`)},
		{ID: "call-independent", Name: "scheduled", Arguments: json.RawMessage(`{"id":"independent","path":"other.go","access":"write"}`)},
	}
	var mu sync.Mutex
	starts, completions := make([]string, 0, 3), make([]string, 0, 3)
	done := make(chan []protocol.ToolResult, 1)
	go func() {
		results, err := executor.ExecuteBatch(context.Background(), "request", calls, BatchHooks{
			OnStart: func(call protocol.ToolCall) error {
				mu.Lock()
				starts = append(starts, call.ID)
				mu.Unlock()
				return nil
			},
			OnResult: func(result protocol.ToolResult) error {
				mu.Lock()
				completions = append(completions, result.CallID)
				mu.Unlock()
				return nil
			},
		})
		if err != nil {
			t.Errorf("ExecuteBatch() error = %v", err)
		}
		done <- results
	}()

	started := map[string]bool{}
	for len(started) < 2 {
		select {
		case id := <-item.started:
			started[id] = true
		case <-time.After(time.Second):
			t.Fatal("independent calls did not start")
		}
	}
	if !started["first"] || !started["independent"] || started["blocked"] {
		t.Fatalf("initial starts = %#v", started)
	}
	close(item.release["independent"])
	waitForBatchEvent(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(completions) == 1 && completions[0] == "call-independent"
	})
	close(item.release["first"])
	select {
	case id := <-item.started:
		if id != "blocked" {
			t.Fatalf("next start = %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("conflicting call did not start after predecessor")
	}
	close(item.release["blocked"])
	results := <-done
	for index, expected := range []string{"call-first", "call-blocked", "call-independent"} {
		if results[index].CallID != expected {
			t.Fatalf("result[%d] = %#v", index, results[index])
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(starts, ",") != "call-first,call-independent,call-blocked" {
		t.Fatalf("starts = %#v", starts)
	}
	if strings.Join(completions, ",") != "call-independent,call-first,call-blocked" {
		t.Fatalf("completions = %#v", completions)
	}
	if len(audit.records) != 3 {
		t.Fatalf("audit records = %#v", audit.records)
	}
	batchID := audit.records[0].BatchID
	indices := make(map[string]int, 3)
	for _, record := range audit.records {
		if batchID == "" || record.BatchID != batchID || record.ConcurrencyMode != string(ConcurrencyClaimed) || len(record.ResourceClaims) != 1 {
			t.Fatalf("audit record = %#v", record)
		}
		indices[record.CallID] = record.BatchIndex
	}
	if indices["call-first"] != 0 || indices["call-blocked"] != 1 || indices["call-independent"] != 2 {
		t.Fatalf("audit indices = %#v", indices)
	}
}

func TestExecuteBatchExclusiveCallIsBarrier(t *testing.T) {
	item := &scheduledBatchTool{started: make(chan string, 3), release: map[string]chan struct{}{
		"before": make(chan struct{}), "exclusive": make(chan struct{}), "after": make(chan struct{}),
	}}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, MaxParallelTools: 3, Timeout: time.Second}
	calls := []protocol.ToolCall{
		{ID: "before", Name: "scheduled", Arguments: json.RawMessage(`{"id":"before","mode":"shared"}`)},
		{ID: "exclusive", Name: "scheduled", Arguments: json.RawMessage(`{"id":"exclusive","mode":"exclusive"}`)},
		{ID: "after", Name: "scheduled", Arguments: json.RawMessage(`{"id":"after","mode":"shared"}`)},
	}
	done := make(chan struct{})
	go func() {
		_, _ = executor.ExecuteBatch(context.Background(), "request", calls, BatchHooks{})
		close(done)
	}()
	expectBatchStart(t, item.started, "before")
	assertNoBatchStart(t, item.started)
	close(item.release["before"])
	expectBatchStart(t, item.started, "exclusive")
	assertNoBatchStart(t, item.started)
	close(item.release["exclusive"])
	expectBatchStart(t, item.started, "after")
	close(item.release["after"])
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("exclusive batch did not finish")
	}
}

type confirmedBatchTool struct{ calls atomic.Int32 }

func (t *confirmedBatchTool) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{Name: "confirmed", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (t *confirmedBatchTool) Risk() policy.Risk { return policy.RiskWrite }
func (t *confirmedBatchTool) Execute(context.Context, json.RawMessage) protocol.ToolResult {
	t.calls.Add(1)
	return protocol.ToolResult{Content: "executed"}
}

func TestExecuteBatchPreflightsBeforeExecutionAndInterruptsOnRejection(t *testing.T) {
	item := &confirmedBatchTool{}
	confirmations := 0
	executor := &Executor{
		Registry: NewRegistry(item), Policy: policy.NewChecker(policy.DefaultConfig(policy.ModeManual)), MaxParallelTools: 3,
		Confirm: func(context.Context, policy.Request, policy.Outcome) (Confirmation, error) {
			confirmations++
			if item.calls.Load() != 0 {
				t.Fatal("tool executed before batch preflight completed")
			}
			if confirmations == 2 {
				return Confirmation{}, nil
			}
			return Confirmation{Approved: true}, nil
		},
	}
	calls := []protocol.ToolCall{
		{ID: "first", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
		{ID: "rejected", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
		{ID: "remaining", Name: "confirmed", Arguments: json.RawMessage(`{}`)},
	}
	results, err := executor.ExecuteBatch(context.Background(), "request", calls, BatchHooks{})
	if err != nil || confirmations != 2 || item.calls.Load() != 0 || len(results) != 3 {
		t.Fatalf("results=%#v confirmations=%d calls=%d err=%v", results, confirmations, item.calls.Load(), err)
	}
	if results[1].Metadata["interrupt_request"] != true || results[1].Metadata["approval_rejected"] != true {
		t.Fatalf("rejected result = %#v", results[1])
	}
	for _, index := range []int{0, 2} {
		if !results[index].IsError || !strings.Contains(results[index].Content, "cancelled") {
			t.Fatalf("cancelled result[%d] = %#v", index, results[index])
		}
	}
}

func TestBuiltinConcurrencyClassifiers(t *testing.T) {
	workspace := t.TempDir()
	read, err := NewReadFile(workspace, 0)
	if err != nil {
		t.Fatal(err)
	}
	write, err := NewWriteFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	index, err := NewRepositoryIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	list := NewListDirectory(index, 0)
	readSpec := read.ClassifyConcurrency(json.RawMessage(`{"path":"same.go"}`), policy.Outcome{})
	writeSpec := write.ClassifyConcurrency(json.RawMessage(`{"path":"same.go"}`), policy.Outcome{})
	otherWrite := write.ClassifyConcurrency(json.RawMessage(`{"path":"other.go"}`), policy.Outcome{})
	treeRead := list.ClassifyConcurrency(json.RawMessage(`{"path":"."}`), policy.Outcome{})
	if !concurrencyConflicts(readSpec, writeSpec) || concurrencyConflicts(writeSpec, otherWrite) || !concurrencyConflicts(treeRead, writeSpec) {
		t.Fatalf("read=%#v write=%#v other=%#v tree=%#v", readSpec, writeSpec, otherWrite, treeRead)
	}
	bash, err := NewBash(workspace, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if spec := bash.ClassifyConcurrency(nil, policy.Outcome{Classification: policy.CommandReadOnly}); spec.Mode != ConcurrencyShared {
		t.Fatalf("read-only bash = %#v", spec)
	}
	if spec := bash.ClassifyConcurrency(nil, policy.Outcome{Classification: policy.CommandAutoAllowed}); spec.Mode != ConcurrencyExclusive {
		t.Fatalf("mutating bash = %#v", spec)
	}
	for _, sessionTool := range []Tool{NewTodoList(), NewAsk(nil)} {
		if spec := concurrencySpec(sessionTool, json.RawMessage(`{}`), policy.Outcome{}); spec.Mode != ConcurrencyExclusive {
			t.Fatalf("session tool %s = %#v", sessionTool.Definition().Name, spec)
		}
	}
}

func TestExecuteBatchDoesNotRepeatStartHookAfterHookFailure(t *testing.T) {
	item := &scheduledBatchTool{started: make(chan string, 1), release: map[string]chan struct{}{"one": make(chan struct{})}}
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}}
	starts := 0
	results, err := executor.ExecuteBatch(context.Background(), "request", []protocol.ToolCall{{
		ID: "one", Name: "scheduled", Arguments: json.RawMessage(`{"id":"one","mode":"shared"}`),
	}}, BatchHooks{OnStart: func(protocol.ToolCall) error {
		starts++
		return context.Canceled
	}})
	if !errors.Is(err, context.Canceled) || starts != 1 || item.calls.Load() != 0 || len(results) != 1 || !results[0].IsError {
		t.Fatalf("err=%v starts=%d calls=%d results=%#v", err, starts, item.calls.Load(), results)
	}
}

func TestRootResourceClaimRemainsUsable(t *testing.T) {
	root := normalizeConcurrencySpec(ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceTree, Path: "/", Access: ResourceRead}}})
	file := ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: []ResourceClaim{{Kind: ResourceFile, Path: "/tmp/file.go", Access: ResourceWrite}}}
	if root.Mode != ConcurrencyClaimed || !concurrencyConflicts(root, file) {
		t.Fatalf("root=%#v file=%#v", root, file)
	}
}

func waitForBatchEvent(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for batch event")
		}
		time.Sleep(time.Millisecond)
	}
}

func expectBatchStart(t *testing.T, started <-chan string, expected string) {
	t.Helper()
	select {
	case actual := <-started:
		if actual != expected {
			t.Fatalf("start = %q, want %q", actual, expected)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s did not start", expected)
	}
}

func assertNoBatchStart(t *testing.T, started <-chan string) {
	t.Helper()
	select {
	case actual := <-started:
		t.Fatalf("unexpected start %q", actual)
	case <-time.After(25 * time.Millisecond):
	}
}
