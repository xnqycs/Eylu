package driver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/driver/anthropic_messages"
	"Eylu/internal/driver/mistral_conversations"
	"Eylu/internal/driver/openai_chat"
	"Eylu/internal/driver/openai_responses"
	"Eylu/internal/driver/perplexity_agent"
	"Eylu/internal/protocol"
)

// The interoperability policy is one table (driver.StopKindFor). Every driver
// has to reach the conclusion of that table for the provider behaviour it can
// express, and this file is the single case set they all pass.
//
// The rows are named in the canonical vocabulary; each dialect encodes them its
// own way and says which rows it cannot express at all, so a missing row is
// visible data rather than a silently skipped driver.
var stopContractRows = []struct {
	name     string
	reason   string
	hasCalls bool
}{
	{name: "tool use with calls", reason: "tool_use", hasCalls: true},
	{name: "tool use without calls", reason: "tool_use"},
	{name: "length", reason: "length"},
	{name: "length with a partial call", reason: "length", hasCalls: true},
	{name: "completed", reason: "completed"},
	{name: "completed with calls", reason: "completed", hasCalls: true},
	{name: "failed", reason: "failed"},
	{name: "cancelled", reason: "cancelled"},
	{name: "unrecognized value", reason: "queued"},
	{name: "unrecognized value with calls", reason: "queued", hasCalls: true},
}

// stopDialect is one driver under the shared contract.
type stopDialect struct {
	name string
	// new builds a driver against a test server.
	new func(*http.Client) driver.ModelDriver
	// streams lists the transport modes this dialect is exercised in.
	streams []bool
	// reason translates a canonical row into the stopping condition this dialect
	// derives from its own vocabulary. ok is false when the dialect has no way to
	// express the row.
	reason func(canonical string, hasCalls bool) (driver.StopReason, bool)
	// body renders the provider response.
	body func(canonical string, hasCalls, stream bool) (string, bool)
	// system reads back the system-level instruction a serialized request carries,
	// in the order the dialect sends it. The dialects place it in different
	// fields, and only the dialect under test knows which.
	system func(body map[string]any) []string
}

// systemMessages reads the segments one dialect sends as role "system" records of
// a message or input array.
func systemMessages(field string) func(map[string]any) []string {
	return func(body map[string]any) []string {
		items, _ := body[field].([]any)
		segments := make([]string, 0, len(items))
		for _, item := range items {
			message, ok := item.(map[string]any)
			if !ok || message["role"] != "system" {
				continue
			}
			if text, ok := message["content"].(string); ok {
				segments = append(segments, text)
			}
		}
		return segments
	}
}

// anthropicSystem reads the single system field of an Anthropic request, in both
// the string and the content-block form the API accepts.
func anthropicSystem(body map[string]any) []string {
	switch typed := body["system"].(type) {
	case string:
		return []string{typed}
	case []any:
		segments := make([]string, 0, len(typed))
		for _, item := range typed {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					segments = append(segments, text)
				}
			}
		}
		return segments
	default:
		return nil
	}
}

func stopContractDialects() []stopDialect {
	return []stopDialect{
		{
			name:    "openai_chat",
			new:     func(client *http.Client) driver.ModelDriver { return openai_chat.New(client) },
			streams: []bool{false, true},
			reason:  chatContractReason,
			body:    chatContractBody,
			system:  systemMessages("messages"),
		},
		{
			name:    "openai_responses",
			new:     func(client *http.Client) driver.ModelDriver { return openai_responses.New(client) },
			streams: []bool{false, true},
			reason:  responsesContractReason,
			body:    responsesContractBody,
			system:  systemMessages("input"),
		},
		{
			name:    "anthropic_messages",
			new:     func(client *http.Client) driver.ModelDriver { return anthropic_messages.New(client) },
			streams: []bool{false},
			reason:  anthropicContractReason,
			body:    anthropicContractBody,
			system:  anthropicSystem,
		},
		{
			// The remaining named adapters share this driver, so the dialect
			// without a stop vocabulary is covered once here.
			name:    "mistral_conversations",
			new:     func(client *http.Client) driver.ModelDriver { return mistral_conversations.New(client) },
			streams: []bool{false},
			reason:  contentContractReason,
			body:    contentContractBody,
			system:  systemMessages("inputs"),
		},
		{
			name:    "perplexity_agent",
			new:     func(client *http.Client) driver.ModelDriver { return perplexity_agent.New(client) },
			streams: []bool{false},
			reason:  contentContractReason,
			body:    contentContractBody,
			system:  systemMessages("messages"),
		},
	}
}

// The Chat Completions vocabulary, including the gateway quirk the relaxation
// exists for: "stop" returned beside tool calls.
func chatContractReason(canonical string, hasCalls bool) (driver.StopReason, bool) {
	switch canonical {
	case "tool_use":
		return driver.StopReasonToolUse, true
	case "length":
		return driver.StopReasonLength, true
	case "completed":
		return driver.StopReasonCompleted, true
	case "queued":
		return driver.StopReason("queued"), true
	default:
		// This dialect has no finish_reason for a transport failure or a
		// cancellation: those arrive as HTTP errors instead.
		return "", false
	}
}

func chatContractBody(canonical string, hasCalls, stream bool) (string, bool) {
	finish, ok := chatFinishReason(canonical)
	if !ok {
		return "", false
	}
	if stream {
		var builder strings.Builder
		builder.WriteString(sseEvent(`{"choices":[{"delta":{"content":"answer"},"finish_reason":""}]}`))
		if hasCalls {
			builder.WriteString(sseEvent(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"echo","arguments":"{}"}}]},"finish_reason":""}]}`))
		}
		builder.WriteString(sseEvent(`{"choices":[{"delta":{},"finish_reason":"` + finish + `"}]}`))
		builder.WriteString(sseEvent("[DONE]"))
		return builder.String(), true
	}
	message := `{"role":"assistant","content":"answer"`
	if hasCalls {
		message += `,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"echo","arguments":"{}"}}]`
	}
	message += "}"
	return `{"choices":[{"message":` + message + `,"finish_reason":"` + finish + `"}]}`, true
}

func chatFinishReason(canonical string) (string, bool) {
	switch canonical {
	case "tool_use":
		return "tool_calls", true
	case "length":
		return "length", true
	case "completed":
		return "stop", true
	default:
		return canonical, true
	}
}

// The Responses dialect has no tool-call status: it reports a completed
// envelope for a function call, so "tool use" and "completed with calls" are the
// same shape. An unrecognized status is read as "stopped early" rather than as a
// completion, which is why the unrecognized rows resolve to length here.
func responsesContractReason(canonical string, hasCalls bool) (driver.StopReason, bool) {
	switch canonical {
	case "tool_use":
		if !hasCalls {
			return "", false
		}
		return driver.StopReasonToolUse, true
	case "length":
		return driver.StopReasonLength, true
	case "completed":
		if hasCalls {
			return driver.StopReasonToolUse, true
		}
		return driver.StopReasonCompleted, true
	case "failed":
		return driver.StopReasonFailed, true
	case "cancelled":
		return driver.StopReasonCancelled, true
	case "queued":
		return driver.StopReasonLength, true
	default:
		return "", false
	}
}

func responsesContractBody(canonical string, hasCalls, stream bool) (string, bool) {
	if _, ok := responsesContractReason(canonical, hasCalls); !ok {
		return "", false
	}
	status := canonical
	if canonical == "queued" {
		status = "queued"
	} else if canonical == "tool_use" {
		status = "completed"
	}
	body := `{"id":"r1","status":"` + status + `","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}`
	if hasCalls {
		body += `,{"type":"function_call","call_id":"call-1","name":"echo","arguments":"{}"}`
	}
	body += `],"usage":{"input_tokens":1,"output_tokens":1}}`
	if !stream {
		return body, true
	}
	// A failed or cancelled envelope has its own event type that this client
	// reports as a transport failure rather than as a stop reason.
	if status == "failed" || status == "cancelled" {
		return "", false
	}
	eventType := "response.completed"
	if status == "incomplete" {
		eventType = "response.incomplete"
	}
	return sseEvent(`{"type":"` + eventType + `","response":` + body + `}`), true
}

// The Anthropic dialect is authoritative about its own stop reason.
func anthropicContractReason(canonical string, hasCalls bool) (driver.StopReason, bool) {
	switch canonical {
	case "tool_use":
		return driver.StopReasonToolUse, true
	case "length":
		return driver.StopReasonLength, true
	case "completed":
		return driver.StopReasonCompleted, true
	case "queued":
		return driver.StopReason("queued"), true
	default:
		return "", false
	}
}

func anthropicContractBody(canonical string, hasCalls, _ bool) (string, bool) {
	reason, ok := anthropicStopReason(canonical)
	if !ok {
		return "", false
	}
	content := `{"type":"text","text":"answer"}`
	if hasCalls {
		content += `,{"type":"tool_use","id":"call-1","name":"echo","input":{}}`
	}
	return `{"id":"msg_1","content":[` + content + `],"stop_reason":"` + reason + `","usage":{"input_tokens":1,"output_tokens":1}}`, true
}

func anthropicStopReason(canonical string) (string, bool) {
	switch canonical {
	case "tool_use":
		return "tool_use", true
	case "length":
		return "max_tokens", true
	case "completed":
		return "end_turn", true
	default:
		return canonical, true
	}
}

// The dialects without a stop vocabulary derive the condition from the content:
// a response that carries calls is asking for them, and everything else is a
// completion. Only those two rows are expressible.
func contentContractReason(canonical string, hasCalls bool) (driver.StopReason, bool) {
	switch {
	case canonical == "tool_use" && hasCalls, canonical == "completed" && hasCalls:
		return driver.StopReasonToolUse, true
	case canonical == "completed":
		return driver.StopReasonCompleted, true
	default:
		return "", false
	}
}

func contentContractBody(canonical string, hasCalls, _ bool) (string, bool) {
	if _, ok := contentContractReason(canonical, hasCalls); !ok {
		return "", false
	}
	output := `{"type":"message","content":[{"type":"output_text","text":"answer"}]}`
	if hasCalls {
		output += `,{"type":"function_call","id":"call-1","name":"echo","arguments":"{}"}`
	}
	return `{"id":"response_1","output":[` + output + `],"usage":{"input_tokens":1,"output_tokens":1}}`, true
}

func sseEvent(payload string) string {
	return "data: " + payload + "\n\n"
}

// Every driver reaches the policy's conclusion for every row its dialect can
// express, with and without the relaxation, on every transport it supports.
func TestDriversShareTheStopReasonContract(t *testing.T) {
	for _, dialect := range stopContractDialects() {
		for _, row := range stopContractRows {
			for _, stream := range dialect.streams {
				for _, accept := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/stream=%t/accept=%t", dialect.name, row.name, stream, accept)
					t.Run(name, func(t *testing.T) {
						canonical, ok := dialect.reason(row.reason, row.hasCalls)
						if !ok {
							t.Skip("the dialect cannot express this row")
						}
						body, ok := dialect.body(row.reason, row.hasCalls, stream)
						if !ok {
							t.Skip("the dialect cannot express this row on this transport")
						}
						wantKind, wantInterop, wantErr := driver.StopKindFor(canonical, row.hasCalls, accept)

						got, err := generateOnce(t, dialect, body, stream, accept)
						switch {
						case wantErr != nil:
							var protocolErr *protocol.Error
							if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.ErrProtocol {
								t.Fatalf("err = %v, want %v", err, wantErr)
							}
							return
						case err != nil:
							t.Fatalf("err = %v", err)
						}
						if got.Stop != wantKind {
							t.Fatalf("stop = %q, want %q", got.Stop, wantKind)
						}
						switch {
						case wantInterop == "" && len(got.Interop) != 0:
							t.Fatalf("interop = %v, want none", got.Interop)
						case wantInterop != "" && len(got.Interop) != 1:
							t.Fatalf("interop = %v, want exactly %q", got.Interop, wantInterop)
						case wantInterop != "" && got.Interop[0] != wantInterop:
							t.Fatalf("interop = %v, want %q", got.Interop, wantInterop)
						}
					})
				}
			}
		}
	}
}

// generateOnce drives one stubbed provider response through one driver.
func generateOnce(t *testing.T, dialect stopDialect, body string, stream, accept bool) (protocol.ModelResponse, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if stream {
			writer.Header().Set("Content-Type", "text/event-stream")
		} else {
			writer.Header().Set("Content-Type", "application/json")
		}
		_, _ = writer.Write([]byte(body))
	}))
	defer server.Close()

	request := driver.Request{
		BaseURL: server.URL, APIKey: "key", Stream: stream,
		AcceptToolCallsWithStop: accept,
		Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{{
			Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hello"}},
		}}},
	}
	return dialect.new(server.Client()).Generate(context.Background(), request, nil)
}

// Every system segment a session builds must reach the provider, in order, on
// every dialect.
//
// The session sends the base prompt, MCP instructions and resources, the skill
// catalog and bodies, the task list, the project map and the compaction summary as
// separate system turns. A dialect that can only express one instruction field has
// to merge them; keeping just one - the last - drops the instructions silently,
// and moving them into the conversation would change what the model was asked.
func TestDriversKeepEverySystemSegmentInOrder(t *testing.T) {
	segments := []string{"BASE_SYSTEM_PROMPT", "MCP_INSTRUCTIONS", "SKILL_CATALOG", "SKILL_BODY", "TASK_LIST", "PROJECT_MAP", "COMPACTION_SUMMARY"}
	for _, dialect := range stopContractDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			turns := make([]protocol.Turn, 0, len(segments)+2)
			for _, text := range segments {
				turns = append(turns, protocol.Turn{
					ID: text, Role: protocol.RoleSystem,
					Parts: []protocol.Part{{Kind: protocol.PartText, Text: "<" + text + ">"}},
				})
			}
			// An empty segment must not erase the ones around it.
			turns = append(turns, protocol.Turn{ID: "empty", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: ""}}})
			turns = append(turns, protocol.Turn{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "hello"}}})

			body := requestBodyOnce(t, dialect, turns)
			sent := strings.Join(dialect.system(body), "\n")
			position := 0
			for _, text := range segments {
				index := strings.Index(sent[position:], "<"+text+">")
				if index < 0 {
					t.Fatalf("segment %q is missing or reordered in %q", text, sent)
				}
				position += index + len(text) + 2
			}
			if strings.Count(sent, "<BASE_SYSTEM_PROMPT>") != 1 {
				t.Fatalf("the base prompt was duplicated or dropped: %q", sent)
			}
		})
	}
}

// requestBodyOnce runs one request against a stub provider and returns the JSON
// body the driver actually sent, so a contract can be asserted on the wire format
// rather than on the driver's internal state.
func requestBodyOnce(t *testing.T, dialect stopDialect, turns []protocol.Turn) map[string]any {
	t.Helper()
	responseBody, ok := dialect.body("completed", false, false)
	if !ok {
		t.Fatalf("%s cannot express a completion", dialect.name)
	}
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(responseBody))
	}))
	defer server.Close()

	request := driver.Request{
		BaseURL: server.URL, APIKey: "key",
		Model: protocol.ModelRequest{Model: "model", Turns: turns},
	}
	if _, err := dialect.new(server.Client()).Generate(context.Background(), request, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if captured == nil {
		t.Fatal("the driver sent no request body")
	}
	return captured
}
