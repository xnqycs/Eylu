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
	"Eylu/internal/environment"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

type scriptedDriver struct {
	mu       sync.Mutex
	requests []driver.Request
	block    bool
}

func (d *scriptedDriver) Name() string { return "scripted" }
func (d *scriptedDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{TextStreaming: true}
}
func (d *scriptedDriver) Generate(ctx context.Context, request driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	d.mu.Lock()
	d.requests = append(d.requests, request)
	number := len(d.requests)
	d.mu.Unlock()
	if d.block {
		<-ctx.Done()
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrCancelled, Message: "cancelled", Cause: ctx.Err()}
	}
	text := "answer"
	if number == 2 {
		text = "remembered"
	}
	if emit != nil {
		_ = emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: text[:3]})
		_ = emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: text[3:]})
	}
	state, _ := json.Marshal(map[string]int{"request": number})
	return protocol.ModelResponse{
		Turn: protocol.Turn{ID: "agent-turn", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: text}}},
		Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: number * 10, OutputTokens: 2, Exact: true}, DriverState: state,
	}, nil
}

func TestConversationMultiTurnNewAndProviderGeneration(t *testing.T) {
	fake := &scriptedDriver{}
	runtime := testRuntime(fake, 1)
	conversation := NewConversation()
	firstID := conversation.SessionID()
	var streamed string
	for _, prompt := range []string{"remember blue", "what color"} {
		_, err := conversation.Send(context.Background(), prompt, runtime, true, func(event protocol.ModelEvent) error {
			if event.Kind == protocol.EventTextDelta {
				streamed += event.Delta
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if streamed != "answerremembered" {
		t.Fatalf("stream = %q", streamed)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d", len(fake.requests))
	}
	second := fake.requests[1].Model
	if len(second.Turns) != 4 || second.Turns[0].Role != protocol.RoleSystem || second.Turns[1].Role != protocol.RoleUser || second.Turns[2].Role != protocol.RoleAgent || second.Turns[3].Role != protocol.RoleUser {
		t.Fatalf("turn order = %#v", second.Turns)
	}
	if len(second.DriverState) == 0 {
		t.Fatal("driver state was not carried to the same provider generation")
	}
	report := conversation.ContextReport()
	if report.LastUsage.InputTokens != 20 || report.InputTokens == 0 || report.OutputReserve == 0 {
		t.Fatalf("context report = %#v", report)
	}

	runtime.Provider.Generation++
	if _, err := conversation.Send(context.Background(), "after update", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests[2].Model.DriverState) != 0 {
		t.Fatal("driver state survived a provider generation change")
	}
	closed := conversation.NewSession()
	if closed != firstID || conversation.SessionID() == firstID || len(conversation.Transcript()) != 0 {
		t.Fatal("new session boundary is incorrect")
	}
	closedTurns, ok := conversation.ClosedTranscript(firstID)
	if !ok || len(closedTurns) != 6 {
		t.Fatalf("closed transcript = %#v, %v", closedTurns, ok)
	}
	newReport := conversation.ContextReport()
	if newReport.LastUsage.InputTokens != 0 || newReport.InputTokens == 0 {
		t.Fatalf("new context baseline = %#v", newReport)
	}
}

func TestConversationCancellation(t *testing.T) {
	fake := &scriptedDriver{block: true}
	conversation := NewConversation()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := conversation.Send(ctx, "wait", testRuntime(fake, 1), true, nil)
	if typed, ok := err.(*protocol.Error); !ok || typed.Code != protocol.ErrCancelled {
		t.Fatalf("error = %#v", err)
	}
}

func TestConversationModeChangesPromptAndClearsDriverState(t *testing.T) {
	fake := &scriptedDriver{}
	conversation := NewConversation()
	runtime := testRuntime(fake, 1)
	runtime.PermissionMode = "plan"
	if _, err := conversation.Send(context.Background(), "plan it", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := fake.requests[0].Model.Turns[0].Parts[0].Text; !strings.Contains(got, "software architecture planner") || !strings.Contains(got, "decision-complete plan") {
		t.Fatalf("plan prompt = %q", got)
	}
	runtime.PermissionMode = "auto"
	if _, err := conversation.Send(context.Background(), "execute it", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests[1].Model.DriverState) != 0 {
		t.Fatal("driver state survived mode change")
	}
	if got := fake.requests[1].Model.Turns[0].Parts[0].Text; !strings.Contains(got, "Current permission mode: auto") {
		t.Fatalf("auto prompt = %q", got)
	}
	if got := fake.requests[1].Model.Turns[0].Parts[0].Text; !strings.Contains(got, "Act through tools early") || !strings.Contains(got, "Inspect only files relevant") {
		t.Fatalf("execution guidance missing from prompt = %q", got)
	}
}

func TestConversationEnvironmentSnapshotUsesCurrentRequestModel(t *testing.T) {
	fake := &scriptedDriver{}
	snapshot := environment.Context{
		WorkingDirectory: "C:/workspace", Platform: "windows", OSVersion: "Windows 11", Today: "2026-07-19",
		IsGitRepo: true, CurrentBranch: "feature", MainBranch: "main", Status: "(clean)", RecentCommits: "abc1234 change",
	}
	conversation := NewConversationWithEnvironment(snapshot)
	runtime := testRuntime(fake, 1)
	runtime.Provider.Config.Model = "model-one"
	if _, err := conversation.Send(context.Background(), "first", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	runtime.Provider.Generation++
	runtime.Provider.Config.Model = "model-two"
	if _, err := conversation.Send(context.Background(), "second", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	firstPrompt := fake.requests[0].Model.Turns[0].Parts[0].Text
	secondPrompt := fake.requests[1].Model.Turns[0].Parts[0].Text
	if !strings.Contains(firstPrompt, "Your model ID is model-one.") || strings.Contains(firstPrompt, "model-two") {
		t.Fatalf("first prompt = %q", firstPrompt)
	}
	if !strings.Contains(secondPrompt, "Your model ID is model-two.") || !strings.Contains(secondPrompt, "abc1234 change") {
		t.Fatalf("second prompt = %q", secondPrompt)
	}
	if state := conversation.ExportState(); state.Environment != snapshot {
		t.Fatalf("environment changed with model: %#v", state.Environment)
	}
}

func TestPlanProfileForksContextFiltersToolsAndAdoptsFinalResult(t *testing.T) {
	fake := &scriptedDriver{}
	environmentContext := environment.Context{WorkingDirectory: "C:/workspace", Platform: "windows", Today: "2026-07-19"}
	conversation := NewConversationWithEnvironment(environmentContext)
	runtime := testRuntime(fake, 1)
	if _, err := conversation.Send(context.Background(), "remember the parent context", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	parentState := conversation.ExportState()
	if len(parentState.DriverState) == 0 {
		t.Fatal("expected parent driver state")
	}

	profile := ProfileForMode("plan")
	if !profile.Isolated || profile.Model != ModelInherit || !profile.AllowsTool("read_file", policy.RiskRead) || !profile.AllowsTool("bash", policy.RiskExec) || !profile.AllowsTool("ask", policy.RiskSession) || profile.AllowsTool("todolist", policy.RiskSession) || profile.AllowsTool("write_file", policy.RiskWrite) {
		t.Fatalf("plan profile = %#v", profile)
	}
	fork, err := conversation.Fork(profile)
	if err != nil {
		t.Fatal(err)
	}
	forkState := fork.ExportState()
	if forkState.SessionID == parentState.SessionID || len(forkState.Turns) != len(parentState.Turns) || len(forkState.DriverState) != 0 || forkState.PermissionMode != "plan" || forkState.Environment != environmentContext {
		t.Fatalf("fork state = %#v parent = %#v", forkState, parentState)
	}
	if !strings.Contains(fork.systemPrompt, "software architecture planner") || !strings.Contains(fork.systemPrompt, "read-only") {
		t.Fatalf("fork prompt = %q", fork.systemPrompt)
	}

	planTurn := protocol.Turn{ID: "plan-final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "implementation plan"}}}
	planResponse := protocol.ModelResponse{Turn: planTurn, Stop: protocol.StopCompleted, Usage: protocol.Usage{InputTokens: 10, OutputTokens: 5, Exact: true}}
	runtime.PermissionMode = "plan"
	if err := conversation.Adopt("design the change", runtime, &planResponse); err != nil {
		t.Fatal(err)
	}
	adopted := conversation.ExportState()
	if len(adopted.Turns) != len(parentState.Turns)+2 || adopted.Turns[len(adopted.Turns)-1].ID != "plan-final" || len(adopted.DriverState) != 0 {
		t.Fatalf("adopted state = %#v", adopted)
	}
}

func testRuntime(modelDriver driver.ModelDriver, generation uint64) Runtime {
	return Runtime{
		Provider: provider.Snapshot{Name: "work", Generation: generation, Config: config.ProviderConfig{
			Adapter: modelDriver.Name(), BaseURL: "https://example.com/v1", Model: "test-model", ContextWindow: 32000,
		}},
		APIKey: "secret", Driver: modelDriver, Timeout: time.Second,
	}
}
