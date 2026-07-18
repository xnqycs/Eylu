package agent

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"Eylu/internal/config"
	"Eylu/internal/driver"
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

func testRuntime(modelDriver driver.ModelDriver, generation uint64) Runtime {
	return Runtime{
		Provider: provider.Snapshot{Name: "work", Generation: generation, Config: config.ProviderConfig{
			Adapter: modelDriver.Name(), BaseURL: "https://example.com/v1", Model: "test-model", ContextWindow: 32000,
		}},
		APIKey: "secret", Driver: modelDriver, Timeout: time.Second,
	}
}
