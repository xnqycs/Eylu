package webnative

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// captureDialectBody runs one request against a stub server that answers in the
// dialect's own shape, and returns the JSON body the driver actually sent.
//
// The older helper answers with an Anthropic envelope, which only Anthropic can
// convert, so a case that covers all four dialects needs a response each of them
// understands.
func captureDialectBody(t *testing.T, dialect Dialect, turns []protocol.Turn) map[string]any {
	t.Helper()
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&captured); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		if dialect == DialectAnthropic {
			_, _ = writer.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
			return
		}
		_, _ = writer.Write([]byte(genericNativeResponse("web_search_call")))
	}))
	defer server.Close()
	model := New(server.Client(), dialect)
	if _, err := model.Generate(context.Background(), driver.Request{
		BaseURL: server.URL + "/v1", APIKey: "secret", Stream: false,
		Model: protocol.ModelRequest{Model: "model", Turns: turns},
	}, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if captured == nil {
		t.Fatal("the driver sent no request body")
	}
	return captured
}

// everyDialect is the whole dialect space, so a case that only makes sense for one
// of them has to say what the other three do instead of leaving them untested.
var everyDialect = []Dialect{DialectAnthropic, DialectGemini, DialectMistral, DialectPerplexity}

// messageField names the field each dialect carries its conversation in.
var messageField = map[Dialect]string{
	DialectAnthropic:  "messages",
	DialectGemini:     "input",
	DialectMistral:    "inputs",
	DialectPerplexity: "messages",
}

// Only Anthropic has a single system field, so only Anthropic may move a system
// turn out of the conversation. The other three carry it as an ordinary message,
// and a special case that leaked into them would silently drop the instructions a
// session builds.
func TestOnlyAnthropicMovesSystemTurnsIntoTheSystemField(t *testing.T) {
	turns := []protocol.Turn{
		{ID: "system", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "BASE_SYSTEM_PROMPT"}}},
		{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "do the thing"}}},
	}
	for _, dialect := range everyDialect {
		t.Run(string(dialect), func(t *testing.T) {
			body := captureDialectBody(t, dialect, turns)
			field := messageField[dialect]
			messages, ok := body[field].([]any)
			if !ok || len(messages) == 0 {
				t.Fatalf("the %s request carries no %s: %#v", dialect, field, body)
			}
			carriesSystem := false
			for _, message := range messages {
				entry, ok := message.(map[string]any)
				if !ok {
					t.Fatalf("unexpected message %#v", message)
				}
				if entry["role"] == string(protocol.RoleSystem) {
					carriesSystem = true
				}
			}

			if dialect != DialectAnthropic {
				// The other three must keep it in the conversation, under their own
				// field, and must not gain Anthropic's fields.
				if !carriesSystem {
					t.Fatalf("%s lost the system turn: %#v", dialect, messages)
				}
				if _, present := body["system"]; present {
					t.Fatalf("%s gained an Anthropic system field: %#v", dialect, body)
				}
				if _, present := body["max_tokens"]; present {
					t.Fatalf("%s gained Anthropic's max_tokens field: %#v", dialect, body)
				}
				return
			}
			// Anthropic takes it out of the conversation and puts it in one field.
			if carriesSystem {
				t.Fatalf("Anthropic left a system message in the conversation: %#v", messages)
			}
			if system := systemText(t, body); !strings.Contains(system, "BASE_SYSTEM_PROMPT") {
				t.Fatalf("Anthropic lost the system text: %q", system)
			}
			if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
				t.Fatalf("the conversation is not exactly the user turn: %#v", messages)
			}
			if _, present := body["max_tokens"]; !present {
				t.Fatalf("Anthropic sent no output bound: %#v", body)
			}
		})
	}
}

// A turn that carries no content is dropped, because an empty message is invalid
// in every one of these protocols.
func TestATurnWithoutContentIsNotSerialized(t *testing.T) {
	turns := []protocol.Turn{
		{ID: "empty-system", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: ""}}},
		{ID: "empty-agent", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: ""}}},
		{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "only this one"}}},
	}
	for _, dialect := range everyDialect {
		t.Run(string(dialect), func(t *testing.T) {
			body := captureDialectBody(t, dialect, turns)
			messages, _ := body[messageField[dialect]].([]any)
			if len(messages) != 1 {
				t.Fatalf("%s serialized %d messages, want only the non-empty one: %#v", dialect, len(messages), messages)
			}
		})
	}
}

// A function tool is a hosted-provider shape that differs per dialect: Anthropic
// names its schema field, the OpenAI-shaped ones wrap the definition in a type.
func TestFunctionToolsAreShapedPerDialect(t *testing.T) {
	definition := protocol.ToolDefinition{
		Name: "read_file", Description: "read a file", Kind: protocol.ToolFunction,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
	for _, dialect := range everyDialect {
		t.Run(string(dialect), func(t *testing.T) {
			mapped, err := New(nil, dialect).mapTool(definition)
			if err != nil {
				t.Fatal(err)
			}
			if mapped["name"] != "read_file" || mapped["description"] != "read a file" {
				t.Fatalf("the tool lost its identity: %#v", mapped)
			}
			if dialect == DialectAnthropic {
				if _, present := mapped["input_schema"]; !present {
					t.Fatalf("Anthropic did not carry the schema: %#v", mapped)
				}
				if _, present := mapped["type"]; present {
					t.Fatalf("Anthropic gained a wrapper type: %#v", mapped)
				}
				return
			}
			if mapped["type"] != "function" {
				t.Fatalf("%s tool type = %#v", dialect, mapped["type"])
			}
			if _, present := mapped["parameters"]; !present {
				t.Fatalf("%s did not carry the schema: %#v", dialect, mapped)
			}
		})
	}
}

// A tool the model may not choose is not sent at all, and a kind the client does
// not know is refused by name rather than dropped silently.
func TestMapToolDropsARefusedChoiceAndRefusesAnUnknownKind(t *testing.T) {
	refused := protocol.ToolDefinition{
		Name: "read_file", Kind: protocol.ToolFunction, InputSchema: json.RawMessage(`{"type":"object"}`),
		ToolChoice: protocol.ToolChoiceNone,
	}
	for _, dialect := range everyDialect {
		mapped, err := New(nil, dialect).mapTool(refused)
		if err != nil {
			t.Fatalf("%s refused a tool choice it should simply omit: %v", dialect, err)
		}
		if mapped != nil {
			t.Fatalf("%s sent a tool the model may not choose: %#v", dialect, mapped)
		}
		_, err = New(nil, dialect).mapTool(protocol.ToolDefinition{
			Name: "mystery", Kind: protocol.ToolKind("mystery"), Execution: protocol.ExecutionHosted,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		})
		if err == nil || !strings.Contains(err.Error(), "mystery") {
			t.Fatalf("%s accepted an unknown tool kind: %v", dialect, err)
		}
	}
}

// A hosted web tool is named per dialect, and a provider option is only accepted
// by the dialect it belongs to: forwarding one to the wrong provider is a request
// that provider rejects, so it is refused here with the option's name.
func TestHostedWebToolsAndTheirProviderOptionsArePerDialect(t *testing.T) {
	types := map[Dialect][2]string{
		DialectAnthropic:  {"web_search_20260318", "web_fetch_20260318"},
		DialectGemini:     {"google_search", "url_context"},
		DialectPerplexity: {"web_search", "fetch_url"},
		DialectMistral:    {"web_search", "web_fetch"},
	}
	for _, dialect := range everyDialect {
		t.Run(string(dialect), func(t *testing.T) {
			for index, kind := range []protocol.ToolKind{protocol.ToolWebSearch, protocol.ToolWebFetch} {
				mapped, err := New(nil, dialect).mapTool(protocol.ToolDefinition{
					Name: string(kind), Kind: kind, Execution: protocol.ExecutionHosted,
					InputSchema: json.RawMessage(`{"type":"object"}`),
				})
				if err != nil {
					t.Fatalf("%s refused %s: %v", dialect, kind, err)
				}
				if mapped["type"] != types[dialect][index] {
					t.Fatalf("%s %s type = %#v, want %q", dialect, kind, mapped["type"], types[dialect][index])
				}
				if dialect == DialectAnthropic && mapped["name"] != string(kind) {
					t.Fatalf("Anthropic did not name the tool it is typing: %#v", mapped)
				}
			}

			// Each dialect accepts exactly its own option and refuses the others'.
			own := map[Dialect]string{
				DialectAnthropic: "version", DialectMistral: "premium",
				DialectGemini: "dynamic_retrieval_config", DialectPerplexity: "search_mode",
			}
			for _, other := range everyDialect {
				option := own[other]
				value := json.RawMessage(`"20260101"`)
				if option == "premium" {
					value = json.RawMessage(`true`)
				}
				if option == "dynamic_retrieval_config" || option == "search_mode" {
					value = json.RawMessage(`{"mode":"auto"}`)
				}
				mapped, err := New(nil, dialect).mapTool(protocol.ToolDefinition{
					Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
					InputSchema:     json.RawMessage(`{"type":"object"}`),
					ProviderOptions: map[string]json.RawMessage{option: value},
				})
				if dialect == other {
					if err != nil {
						t.Fatalf("%s refused its own option %q: %v", dialect, option, err)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), option) {
					t.Fatalf("%s accepted %s's option %q: %#v, %v", dialect, other, option, mapped, err)
				}
			}

			// Anthropic's version option also rewrites the tool type, and only a
			// well-formed date does so.
			mapped, err := New(nil, DialectAnthropic).mapTool(protocol.ToolDefinition{
				Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
				InputSchema: json.RawMessage(`{"type":"object"}`),
				ProviderOptions: map[string]json.RawMessage{
					"version": json.RawMessage(`"20260101"`),
				},
			})
			if err != nil || mapped["type"] != "web_search_20260101" {
				t.Fatalf("Anthropic versioned type = %#v, %v", mapped["type"], err)
			}
			if _, err := New(nil, DialectAnthropic).mapTool(protocol.ToolDefinition{
				Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
				InputSchema: json.RawMessage(`{"type":"object"}`),
				ProviderOptions: map[string]json.RawMessage{
					"version": json.RawMessage(`"v1"`),
				},
			}); err == nil {
				t.Fatal("Anthropic accepted a version that is not a date")
			}
		})
	}
}

// A hosted tool's own limits travel with it, in every dialect.
func TestHostedWebToolLimitsTravelWithTheTool(t *testing.T) {
	location := &protocol.UserLocation{Country: "NL", City: "Amsterdam"}
	mapped, err := New(nil, DialectAnthropic).mapTool(protocol.ToolDefinition{
		Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		MaxUses:     3, AllowedDomains: []string{"example.com"}, BlockedDomains: []string{"spam.example"},
		UserLocation: location,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapped["max_uses"] != 3 {
		t.Fatalf("max_uses = %#v", mapped["max_uses"])
	}
	if domains, ok := mapped["allowed_domains"].([]string); !ok || len(domains) != 1 {
		t.Fatalf("allowed_domains = %#v", mapped["allowed_domains"])
	}
	if domains, ok := mapped["blocked_domains"].([]string); !ok || len(domains) != 1 {
		t.Fatalf("blocked_domains = %#v", mapped["blocked_domains"])
	}
	if mapped["user_location"] != location {
		t.Fatalf("user_location = %#v", mapped["user_location"])
	}
	// A tool with none of them carries none of them.
	plain, err := New(nil, DialectAnthropic).mapTool(protocol.ToolDefinition{
		Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_uses", "allowed_domains", "blocked_domains", "user_location"} {
		if _, present := plain[field]; present {
			t.Fatalf("an unlimited tool carried %q: %#v", field, plain)
		}
	}
}

// Each dialect has its own endpoint, and the base URL is not rewritten for the
// ones that do not need it.
func TestEachDialectAddressesItsOwnEndpoint(t *testing.T) {
	endpoints := map[Dialect]string{
		DialectAnthropic:  "https://example.test/v1/messages",
		DialectGemini:     "https://example.test/v1beta/interactions",
		DialectMistral:    "https://example.test/v1/conversations",
		DialectPerplexity: "https://example.test/v1/agent",
	}
	for _, dialect := range everyDialect {
		if got := New(nil, dialect).endpoint("https://example.test/v1"); got != endpoints[dialect] {
			t.Fatalf("%s endpoint = %q, want %q", dialect, got, endpoints[dialect])
		}
	}
	// A trailing separator on the base URL is not doubled.
	if got := New(nil, DialectAnthropic).endpoint("https://example.test/v1/"); got != endpoints[DialectAnthropic] {
		t.Fatalf("a trailing separator was doubled: %q", got)
	}
}

// The stop vocabulary is the boundary between what the provider said and what the
// policy is willing to believe. A reason this client cannot explain is carried
// through so the policy refuses it by name, never guessed into a completion.
func TestStopReasonVocabularyIsTranslatedAndNeverGuessed(t *testing.T) {
	cases := map[string]driver.StopReason{
		"":                              driver.StopReasonCompleted,
		"end_turn":                      driver.StopReasonCompleted,
		"STOP_SEQUENCE":                 driver.StopReasonCompleted,
		"pause_turn":                    driver.StopReasonCompleted,
		"tool_use":                      driver.StopReasonToolUse,
		"max_tokens":                    driver.StopReasonLength,
		"model_context_window_exceeded": driver.StopReasonLength,
		"refusal":                       driver.StopReasonLength,
		" something_new ":               driver.StopReason("something_new"),
	}
	for reason, want := range cases {
		if got := stopReasonFromAnthropic(reason); got != want {
			t.Fatalf("stopReasonFromAnthropic(%q) = %q, want %q", reason, got, want)
		}
	}
	// The dialects that only distinguish "asked for tools" from "finished" reach
	// the policy through the same translation.
	call := protocol.Part{Kind: protocol.PartToolCall, ToolCall: &protocol.ToolCall{ID: "call"}}
	if got := stopReasonForCalls([]protocol.Part{call}); got != driver.StopReasonToolUse {
		t.Fatalf("a response carrying a call stopped with %q", got)
	}
	if got := stopReasonForCalls([]protocol.Part{{Kind: protocol.PartText, Text: "done"}}); got != driver.StopReasonCompleted {
		t.Fatalf("a response without a call stopped with %q", got)
	}
	// A part that claims to be a call without carrying one is not a call.
	if hasToolCalls([]protocol.Part{call, {Kind: protocol.PartToolCall}}) != true {
		t.Fatal("a real call was not seen")
	}
	if hasToolCalls([]protocol.Part{{Kind: protocol.PartToolCall}}) {
		t.Fatal("an empty call part was read as a call")
	}
	if hasToolCalls(nil) {
		t.Fatal("no parts were read as a call")
	}
}

// The cache breakdown is reported under two names across the dialects, and the
// vendor-specific one wins when both are present.
func TestCachedInputTokensReadsEitherName(t *testing.T) {
	var usage nativeUsage
	if got := usage.cachedInputTokens(); got != 0 {
		t.Fatalf("an empty usage reported %d cached tokens", got)
	}
	usage.InputDetail.CachedTokens = 12
	if got := usage.cachedInputTokens(); got != 12 {
		t.Fatalf("the detail field was ignored: %d", got)
	}
	usage.CacheReadInputTokens = 34
	if got := usage.cachedInputTokens(); got != 34 {
		t.Fatalf("both fields present reported %d, want the vendor-specific one", got)
	}
}

// The model-facing names of a turn's role.
func TestAgentTurnsAreNamedAssistant(t *testing.T) {
	if got := roleName(protocol.RoleAgent); got != "assistant" {
		t.Fatalf("agent role = %q", got)
	}
	for _, role := range []protocol.Role{protocol.RoleUser, protocol.RoleSystem, protocol.RoleTool} {
		if got := roleName(role); got != string(role) {
			t.Fatalf("role %q became %q", role, got)
		}
	}
}

// A hosted activity's action and its counted usage follow the tool it belongs to.
func TestActivityActionAndUsageFollowTheTool(t *testing.T) {
	if got := actionFor(protocol.ToolWebFetch); got != "open_page" {
		t.Fatalf("fetch action = %q", got)
	}
	if got := actionFor(protocol.ToolWebSearch); got != "search" {
		t.Fatalf("search action = %q", got)
	}
	if got := webUsage(protocol.ToolWebFetch); got.Fetches != 1 || got.Searches != 0 {
		t.Fatalf("fetch usage = %#v", got)
	}
	if got := webUsage(protocol.ToolWebSearch); got.Searches != 1 || got.Fetches != 0 {
		t.Fatalf("search usage = %#v", got)
	}
}

// The provider's raw response is kept for the audit trail but bounded, because it
// is charged to whatever holds it.
func TestTheRawProviderResponseIsBounded(t *testing.T) {
	small := []byte(`{"ok":true}`)
	if got := string(boundedRaw(small)); got != string(small) {
		t.Fatalf("a small response was changed: %q", got)
	}
	large := []byte(strings.Repeat("x", (256<<10)+1024))
	if got := len(boundedRaw(large)); got != 256<<10 {
		t.Fatalf("a large response was kept at %d bytes", got)
	}
	// The copy is independent of the caller's buffer, which the stream reuses.
	mutable := []byte(`{"ok":true}`)
	copied := boundedRaw(mutable)
	mutable[0] = '['
	if string(copied) != `{"ok":true}` {
		t.Fatalf("the copy followed its source: %q", copied)
	}
}

// A response item is decoded with its raw form bounded as well, so a huge item
// cannot reach the audit trail in full.
func TestResponseItemsCarryTheirBoundedRawForm(t *testing.T) {
	var item nativeItem
	if err := json.Unmarshal([]byte(`{"type":"message","id":"msg_1"}`), &item); err != nil {
		t.Fatal(err)
	}
	if item.RawTruncated || len(item.Raw) == 0 {
		t.Fatalf("a small item = %#v", item)
	}
	var huge nativeItem
	payload := `{"type":"message","id":"msg_2","status":"` + strings.Repeat("s", (256<<10)+1) + `"}`
	if err := json.Unmarshal([]byte(payload), &huge); err != nil {
		t.Fatal(err)
	}
	if !huge.RawTruncated || len(huge.Raw) != 256<<10 {
		t.Fatalf("a huge item kept %d bytes (truncated %t)", len(huge.Raw), huge.RawTruncated)
	}
	// A malformed item is an error rather than a silently empty value.
	if err := json.Unmarshal([]byte(`{"type":`), &nativeItem{}); err == nil {
		t.Fatal("a malformed item was accepted")
	}
}
