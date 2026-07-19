package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

type overflowDriver struct {
	calls       int
	always      bool
	emitVisible bool
	requests    []driver.Request
}

func (d *overflowDriver) Name() string { return "overflow" }
func (d *overflowDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{TextStreaming: true}
}
func (d *overflowDriver) Generate(_ context.Context, request driver.Request, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	d.calls++
	d.requests = append(d.requests, request)
	if d.always || d.calls == 1 {
		if d.emitVisible && emit != nil {
			_ = emit(protocol.ModelEvent{Kind: protocol.EventTextDelta, Delta: "partial"})
		}
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrContextWindow, Message: "too long", ContextLimit: 32000}
	}
	return protocol.ModelResponse{Turn: protocol.Turn{ID: "done", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "done"}}}, Stop: protocol.StopCompleted}, nil
}

func recoveryRuntime(t *testing.T, model driver.ModelDriver) Runtime {
	t.Helper()
	metadata := config.Default().ModelMetadata
	metadata.Enabled = false
	resolver := provider.NewLimitResolver(metadata, filepath.Join(t.TempDir(), "metadata.json"), nil)
	snapshot, err := resolver.Resolve(context.Background(), provider.Snapshot{Name: "work", Config: config.ProviderConfig{Adapter: model.Name(), BaseURL: "https://example.com/v1", Model: "model"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return Runtime{Provider: snapshot, Driver: model, LimitResolver: resolver, OutputReserveTokens: 1024}
}

func TestContextOverflowRetriesAndPreservesProtectedTaskState(t *testing.T) {
	model := &overflowDriver{}
	runtime := recoveryRuntime(t, model)
	state := NewConversation().ExportState()
	state.DriverState = json.RawMessage(`{"response_id":"old"}`)
	state.Provider = ProviderState{Name: runtime.Provider.Name, Generation: runtime.Provider.Generation, Adapter: runtime.Provider.Config.Adapter, BaseURL: runtime.Provider.Config.BaseURL, Model: runtime.Provider.Config.Model, ContextWindow: runtime.Provider.Config.ContextWindow}
	state.TodoList = protocol.TodoList{Items: []protocol.TodoItem{{ID: "implement", Content: "Implement recovery", Status: protocol.TodoInProgress}}}
	conversation, err := RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	response, err := conversation.Send(context.Background(), "continue", runtime, true, func(protocol.ModelEvent) error { return nil })
	if err != nil || response.Turn.ID != "done" || model.calls != 2 {
		t.Fatalf("response=%#v calls=%d err=%v", response, model.calls, err)
	}
	if len(model.requests[0].Model.DriverState) == 0 || len(model.requests[1].Model.DriverState) != 0 {
		t.Fatalf("driver states = %s / %s", model.requests[0].Model.DriverState, model.requests[1].Model.DriverState)
	}
	if !strings.Contains(requestText(model.requests[1].Model.Turns), "Implement recovery") {
		t.Fatal("task state was lost during context retry")
	}
	if report := conversation.ContextReport(); report.ContextWindow != 32000 {
		t.Fatalf("context report = %#v", report)
	}
}

func TestContextOverflowStopsAfterThreeRetriesOrVisibleOutput(t *testing.T) {
	always := &overflowDriver{always: true}
	_, err := NewConversation().Send(context.Background(), "continue", recoveryRuntime(t, always), true, func(protocol.ModelEvent) error { return nil })
	if err == nil || always.calls != 4 {
		t.Fatalf("calls=%d err=%v", always.calls, err)
	}
	visible := &overflowDriver{always: true, emitVisible: true}
	_, err = NewConversation().Send(context.Background(), "continue", recoveryRuntime(t, visible), true, func(protocol.ModelEvent) error { return nil })
	if err == nil || visible.calls != 1 {
		t.Fatalf("visible calls=%d err=%v", visible.calls, err)
	}
}
