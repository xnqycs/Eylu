package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

func TestAskDefinitionAndAnswer(t *testing.T) {
	var captured protocol.AskRequest
	item := NewAsk(func(_ context.Context, request protocol.AskRequest) (protocol.AskResponse, error) {
		captured = request
		return protocol.AskResponse{Answers: map[string][]string{
			"scope":  {"Focused"},
			"checks": {"Unit tests", "Integration tests", "Custom smoke"},
		}}, nil
	})
	definition := item.Definition()
	if definition.Name != "ask" || !json.Valid(definition.InputSchema) || item.Risk() != policy.RiskSession || item.UseExecutorTimeout() {
		t.Fatalf("definition=%#v risk=%q timeout=%t", definition, item.Risk(), item.UseExecutorTimeout())
	}
	result := item.Execute(context.Background(), json.RawMessage(`{"questions":[
		{"id":"scope","header":"Scope","question":"Which scope should be used?","options":[{"label":"Focused","description":"Touch the requested area."},{"label":"Broad","description":"Include adjacent cleanup."}]},
		{"id":"checks","header":"Checks","question":"Which checks should run?","multiple":true,"options":[{"label":"Unit tests","description":"Run unit tests."},{"label":"Integration tests","description":"Run integration tests."}]}
	]}`))
	if result.IsError || len(captured.Questions) != 2 || !captured.Questions[1].Multiple {
		t.Fatalf("captured=%#v result=%#v", captured, result)
	}
	if !strings.Contains(result.Content, `"Custom smoke"`) {
		t.Fatalf("content=%s", result.Content)
	}
}

func TestAskValidatesQuestionsAndAnswers(t *testing.T) {
	callback := func(_ context.Context, request protocol.AskRequest) (protocol.AskResponse, error) {
		return protocol.AskResponse{Answers: map[string][]string{request.Questions[0].ID: {}}}, nil
	}
	tests := []json.RawMessage{
		json.RawMessage(`{"questions":[]}`),
		json.RawMessage(`{"questions":[{"id":"bad id","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`),
		json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"}]}]}`),
		json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"A","description":"b"}]}]}`),
		json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}],"extra":true}`),
	}
	for _, input := range tests {
		if result := NewAsk(callback).Execute(context.Background(), input); !result.IsError {
			t.Fatalf("input=%s result=%#v", input, result)
		}
	}
	valid := json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`)
	if result := NewAsk(callback).Execute(context.Background(), valid); !result.IsError || !strings.Contains(result.Content, "requires exactly one answer") {
		t.Fatalf("invalid answer result=%#v", result)
	}
}

func TestAskDismissalInterruptsRequest(t *testing.T) {
	item := NewAsk(func(context.Context, protocol.AskRequest) (protocol.AskResponse, error) {
		return protocol.AskResponse{}, ErrAskDismissed
	})
	result := item.Execute(context.Background(), json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`))
	if !result.IsError || result.Metadata["interrupt_request"] != true || !strings.Contains(result.Content, "dismissed") {
		t.Fatalf("result=%#v", result)
	}
}

func TestAskSkipsExecutorTimeoutAndHonorsParentCancellation(t *testing.T) {
	input := json.RawMessage(`{"questions":[{"id":"q","header":"H","question":"Q?","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`)
	item := NewAsk(func(ctx context.Context, _ protocol.AskRequest) (protocol.AskResponse, error) {
		select {
		case <-time.After(25 * time.Millisecond):
			return protocol.AskResponse{Answers: map[string][]string{"q": {"A"}}}, nil
		case <-ctx.Done():
			return protocol.AskResponse{}, ctx.Err()
		}
	})
	executor := &Executor{Registry: NewRegistry(item), Policy: policy.AllowAllChecker{}, Timeout: time.Millisecond}
	result := executor.Execute(context.Background(), "request", protocol.ToolCall{ID: "ask", Name: "ask", Arguments: input})
	if result.IsError {
		t.Fatalf("timeout result=%#v", result)
	}

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	result = executor.Execute(parent, "request", protocol.ToolCall{ID: "cancel", Name: "ask", Arguments: input})
	if !result.IsError || !errors.Is(parent.Err(), context.Canceled) {
		t.Fatalf("cancel result=%#v", result)
	}
}
