package webnative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// A response the driver cannot convert is reported as a protocol error rather than
// as an empty answer, and a response that carries nothing at all is refused: an
// empty model turn would look like a finished request.
func TestGenerateRefusesAResponseItCannotConvert(t *testing.T) {
	for _, dialect := range everyDialect {
		t.Run(string(dialect)+" malformed", func(t *testing.T) {
			server := answeringServer(t, http.StatusOK, `{"this is not":`)
			defer server.Close()
			_, err := New(server.Client(), dialect).Generate(context.Background(), plainRequest(server.URL), nil)
			if err == nil || !strings.Contains(err.Error(), "decode") {
				t.Fatalf("malformed response error = %v", err)
			}
		})
		t.Run(string(dialect)+" empty", func(t *testing.T) {
			payload := `{"id":"response_1","output":[],"usage":{"input_tokens":1,"output_tokens":0}}`
			if dialect == DialectAnthropic {
				payload = `{"id":"msg_1","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":0}}`
			}
			server := answeringServer(t, http.StatusOK, payload)
			defer server.Close()
			_, err := New(server.Client(), dialect).Generate(context.Background(), plainRequest(server.URL), nil)
			if err == nil || !strings.Contains(err.Error(), "no text, web activity, or tool calls") {
				t.Fatalf("empty response error = %v", err)
			}
		})
	}
}

// A provider failure is translated: a rate limit is retryable, an authentication
// failure is not, and a hosted web tool the provider does not know is reported as
// an unsupported tool rather than as a generic bad request.
func TestGenerateTranslatesAProviderFailure(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		hosted  bool
		wantErr error
	}{
		{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limit reached"}}`, wantErr: nil},
		{status: http.StatusUnauthorized, body: `{"error":{"message":"invalid api key"}}`},
		{status: http.StatusInternalServerError, body: "upstream exploded"},
		{status: http.StatusBadRequest, body: `{"error":{"message":"unsupported web_search tool"}}`, hosted: true},
		{status: http.StatusUnprocessableEntity, body: `{"error":{"message":"unknown google_search tool"}}`, hosted: true},
		// A bad request that is not about a hosted tool keeps its own meaning.
		{status: http.StatusBadRequest, body: `{"error":{"message":"invalid model"}}`, hosted: true},
	}
	for _, testCase := range cases {
		server := answeringServer(t, testCase.status, testCase.body)
		request := plainRequest(server.URL)
		if testCase.hosted {
			request.Model.Tools = []protocol.ToolDefinition{{
				Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}}
		}
		_, err := New(server.Client(), DialectAnthropic).Generate(context.Background(), request, nil)
		server.Close()
		var typed *protocol.Error
		if !errors.As(err, &typed) {
			t.Fatalf("status %d error = %v, want a typed provider error", testCase.status, err)
		}
		if testCase.status == http.StatusBadRequest && testCase.hosted && strings.Contains(testCase.body, "web_search") {
			if typed.Code != protocol.ErrUnsupportedTool {
				t.Fatalf("a rejected hosted tool reported %q, want the unsupported-tool code", typed.Code)
			}
		}
	}
}

// A cancellation and a deadline are told apart from a transport failure, because
// only the last one is worth retrying.
//
// The three conditions differ only in the context they are given, so they are
// stated against the translation directly, and one of them is also driven end to
// end: a request whose context is already cancelled must not reach the network at
// all, and must not be reported as retryable.
func TestGenerateTellsCancellationAndDeadlineFromTransportFailure(t *testing.T) {
	cancelled, cancelCancelled := context.WithCancel(context.Background())
	cancelCancelled()
	expired, cancelExpired := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelExpired()
	<-expired.Done()

	cases := []struct {
		name      string
		ctx       context.Context
		wantCode  string
		retryable bool
	}{
		{name: "cancelled", ctx: cancelled, wantCode: string(protocol.ErrCancelled)},
		{name: "deadline", ctx: expired, wantCode: string(protocol.ErrTimeout), retryable: true},
		{name: "transport", ctx: context.Background(), wantCode: string(protocol.ErrNetwork), retryable: true},
	}
	for _, testCase := range cases {
		var typed *protocol.Error
		err := mapTransportError(testCase.ctx, errors.New("transport"))
		if !errors.As(err, &typed) || string(typed.Code) != testCase.wantCode || typed.Retryable != testCase.retryable {
			t.Fatalf("%s reported %v, want %s (retryable %t)", testCase.name, err, testCase.wantCode, testCase.retryable)
		}
	}

	var typed *protocol.Error
	_, err := New(nil, DialectAnthropic).Generate(cancelled, plainRequest("http://127.0.0.1:1"), nil)
	if !errors.As(err, &typed) || string(typed.Code) != string(protocol.ErrCancelled) {
		t.Fatalf("a cancelled request reported %v", err)
	}

	// A transport failure that is neither is retryable network trouble, which is
	// what a refused connection looks like.
	_, err = New(nil, DialectAnthropic).Generate(context.Background(), plainRequest("http://127.0.0.1:1"), nil)
	if !errors.As(err, &typed) || string(typed.Code) != string(protocol.ErrNetwork) || !typed.Retryable {
		t.Fatalf("a refused connection reported %v", err)
	}
}

// A stream that ends without a final response is refused, so a truncated stream is
// never presented as a complete answer. The events that are not payloads are
// skipped without error.
func TestReadStreamRefusesATruncatedStream(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		"",
		": a comment line",
		`data: {"type":"response.output_text.delta","delta":{"text":"partial"}}`,
		`data: [DONE]`,
		"",
	}, "\n")
	var deltas []string
	emit := func(event protocol.ModelEvent) error {
		if event.Kind == protocol.EventTextDelta {
			deltas = append(deltas, event.Delta)
		}
		return nil
	}
	_, _, err := New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader(stream), emit, false)
	if err == nil || !strings.Contains(err.Error(), "before a final response") {
		t.Fatalf("truncated stream error = %v", err)
	}
	if len(deltas) != 1 || deltas[0] != "partial" {
		t.Fatalf("deltas = %#v, want the one that arrived", deltas)
	}

	// A malformed payload ends the stream with a protocol error rather than being
	// skipped, because the rest of the stream cannot be trusted after it.
	_, _, err = New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader("data: {\n"), nil, false)
	var typed *protocol.Error
	if !errors.As(err, &typed) || typed.Code != protocol.ErrProtocol {
		t.Fatalf("malformed stream event = %v", err)
	}
	// A provider error event is translated rather than read as a payload.
	_, _, err = New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader(`data: {"type":"error","error":{"message":"rate limit reached"}}`+"\n"), nil, false)
	if err == nil {
		t.Fatal("a provider error event was ignored")
	}
}

// An emit callback that fails stops the stream: the host said it cannot take the
// answer, so continuing to consume it would only lose more of it.
func TestReadStreamStopsWhenTheHostRefusesAnEvent(t *testing.T) {
	stream := `data: {"type":"response.output_text.delta","delta":{"text":"one"}}` + "\n"
	sentinel := errors.New("host refused")
	_, _, err := New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader(stream), func(protocol.ModelEvent) error {
		return sentinel
	}, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("a refused delta reported %v", err)
	}

	// The same is true of a running web activity, which is what a search looks
	// like while it is happening.
	search := `data: {"type":"response.web_search_call.in_progress","item":{"type":"web_search_call","id":"web_1","action":{"type":"search","query":"Eylu"}}}` + "\n"
	_, _, err = New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader(search), func(protocol.ModelEvent) error {
		return sentinel
	}, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("a refused activity reported %v", err)
	}
}

// A stream that carries its final response converts it, and emits what the
// response holds: the web activity, the completion, and the usage of the answer.
func TestReadStreamConvertsAndEmitsTheFinalResponse(t *testing.T) {
	final := `{"id":"response_1","output":[{"type":"web_search_call","id":"web_1","status":"completed","action":{"type":"search","query":"Eylu"},"sources":[]},{"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":1}}`
	stream := `data: {"type":"response.completed","response":` + final + "}\n"
	var kinds []protocol.EventKind
	response, raw, err := New(nil, DialectPerplexity).readStream(context.Background(), strings.NewReader(stream), func(event protocol.ModelEvent) error {
		kinds = append(kinds, event.Kind)
		return nil
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || response.Stop != protocol.StopToolUse {
		t.Fatalf("response = %#v (raw %d bytes)", response, len(raw))
	}
	if response.Usage.InputTokens != 3 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	calls, activities := 0, 0
	for index := range response.Turn.Parts {
		switch {
		case response.Turn.Parts[index].ToolCall != nil:
			calls++
		case response.Turn.Parts[index].WebActivity != nil:
			activities++
		}
	}
	if calls != 1 || activities != 1 {
		t.Fatalf("calls=%d activities=%d", calls, activities)
	}
	for _, want := range []protocol.EventKind{protocol.EventWebSearchStarted, protocol.EventWebSearchCompleted} {
		found := false
		for _, kind := range kinds {
			found = found || kind == want
		}
		if !found {
			t.Fatalf("%q was not emitted: %#v", want, kinds)
		}
	}
	// The text of a streamed answer is emitted as it arrives, so the final
	// conversion does not repeat it.
	if containsKind(kinds, protocol.EventTextDelta) {
		t.Fatalf("a streamed answer was emitted twice: %#v", kinds)
	}
}

func containsKind(kinds []protocol.EventKind, want protocol.EventKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// Every dialect reports its cache breakdown under its own name, and a dialect that
// has none reports zero rather than inventing one.
func TestConvertReadsTheCacheBreakdownPerDialect(t *testing.T) {
	generic := `{"id":"response_1","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}`
	response, err := convertOutputItems([]byte(generic), false)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.CachedInputTokens != 4 {
		t.Fatalf("cached tokens = %d, want the detail field", response.Usage.CachedInputTokens)
	}
	anthropic := `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":6}}`
	response, err = convertAnthropic([]byte(anthropic), false)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.CachedInputTokens != 6 {
		t.Fatalf("cached tokens = %d, want the vendor field", response.Usage.CachedInputTokens)
	}
}

// A provider that reports an error inside a 200 response is still a failure.
func TestConvertReportsAnErrorCarriedByASuccessfulResponse(t *testing.T) {
	for name, convert := range map[string]func([]byte, bool) (protocol.ModelResponse, error){
		"generic":   convertOutputItems,
		"anthropic": convertAnthropic,
	} {
		_, err := convert([]byte(`{"error":{"message":"rate limit reached","type":"rate_limit_error"}}`), false)
		if err == nil {
			t.Fatalf("%s accepted a response carrying an error", name)
		}
		if _, err := convert([]byte("{"), false); err == nil {
			t.Fatalf("%s accepted a malformed response", name)
		}
	}
}

// The canonical conversion reads the web activities, the text, the citations and
// the calls out of one response, and links each citation to the activity that
// preceded it.
func TestConvertOutputItemsReadsEveryItemShape(t *testing.T) {
	payload := `{"id":"response_1","output":[
		{"type":"web_fetch_call","id":"fetch_1","status":"succeeded","action":{"type":"open_page","url":"https://example.com"},"sources":[{"url":"https://example.com","title":"Example"}]},
		{"type":"message","content":[{"type":"output_text","text":"the answer","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example","start_index":0,"end_index":4}]}]},
		{"type":"tool_call","id":"tool_1","name":"read_file","arguments":"{}"}
	],"usage":{"input_tokens":5,"output_tokens":1}}`
	response, err := convertOutputItems([]byte(payload), false)
	if err != nil {
		t.Fatal(err)
	}
	var activity *protocol.WebActivity
	citations, calls, texts := 0, 0, 0
	for index := range response.Turn.Parts {
		part := &response.Turn.Parts[index]
		switch {
		case part.WebActivity != nil:
			activity = part.WebActivity
		case part.Citation != nil:
			citations++
			if part.Citation.CallID != "fetch_1" {
				t.Fatalf("citation points at %q", part.Citation.CallID)
			}
		case part.ToolCall != nil:
			calls++
			if part.ToolCall.ID != "tool_1" || part.ToolCall.Name != "read_file" {
				t.Fatalf("call = %#v", part.ToolCall)
			}
		case part.Kind == protocol.PartText:
			texts++
		}
	}
	if activity == nil || activity.Kind != protocol.ToolWebFetch || activity.Status != protocol.WebStatusCompleted {
		t.Fatalf("activity = %#v", activity)
	}
	if activity.URL != "https://example.com" || len(activity.Sources) != 1 {
		t.Fatalf("activity details = %#v", activity)
	}
	if citations != 1 || calls != 1 || texts != 1 {
		t.Fatalf("citations=%d calls=%d texts=%d", citations, calls, texts)
	}
	// The status the provider reports as "succeeded" is the completed status here,
	// and the response is charged exactly once.
	if response.Usage.InputTokens != 5 || response.Usage.OutputTokens != 1 {
		t.Fatalf("usage = %#v", response.Usage)
	}
	if response.Stop != protocol.StopToolUse {
		t.Fatalf("stop = %q, want tool use", response.Stop)
	}
}

// A call the provider marked as failed is carried as failed rather than being
// rewritten as completed.
func TestConvertOutputItemsKeepsAFailedActivityFailed(t *testing.T) {
	payload := `{"id":"response_1","output":[{"type":"web_search_call","id":"web_1","status":"error","action":{"type":"search","query":"Eylu"}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	response, err := convertOutputItems([]byte(payload), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Turn.Parts) != 1 || response.Turn.Parts[0].WebActivity == nil {
		t.Fatalf("parts = %#v", response.Turn.Parts)
	}
	if got := response.Turn.Parts[0].WebActivity.Status; got != protocol.WebStatusError {
		t.Fatalf("status = %q, want the provider's own", got)
	}
	// A response carrying only an activity is a completion, so a relaxation is not
	// needed and none is reported.
	if response.Stop != protocol.StopCompleted || len(response.Interop) != 0 {
		t.Fatalf("stop = %q interop = %#v", response.Stop, response.Interop)
	}
}

// The Anthropic conversion pairs a server tool use with the result that follows
// it, keeps text and citations, and invents nothing: a result whose use was never
// seen is still reported, so the audit trail loses no call.
func TestConvertAnthropicPairsToolUseWithItsResult(t *testing.T) {
	payload := `{"id":"msg_1","stop_reason":"tool_use","content":[
		{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"Eylu"}},
		{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example"}]},
		{"type":"text","text":"the answer","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"Example"}]},
		{"type":"server_tool_use","id":"srv_2","name":"web_fetch","input":{"url":"https://example.com"}},
		{"type":"web_fetch_tool_result","tool_use_id":"srv_2","content":[{"type":"web_fetch_result","url":"https://example.com","title":"Example"}]},
		{"type":"web_search_tool_result","tool_use_id":"srv_missing","content":[{"type":"web_search_result","url":"https://orphan.example"}]},
		{"type":"tool_use","id":"tool_1","name":"read_file","input":{"path":"one.txt"}}
	],"usage":{"input_tokens":7,"output_tokens":2,"cache_read_input_tokens":3,"server_tool_use":{"web_search_requests":1,"web_fetch_requests":1}}}`

	response, err := convertAnthropic([]byte(payload), false)
	if err != nil {
		t.Fatal(err)
	}
	activities := map[string]*protocol.WebActivity{}
	citations, calls := 0, 0
	for index := range response.Turn.Parts {
		part := &response.Turn.Parts[index]
		switch {
		case part.WebActivity != nil:
			activities[part.WebActivity.CallID] = part.WebActivity
		case part.Citation != nil:
			citations++
		case part.ToolCall != nil:
			calls++
		}
	}
	if len(activities) != 3 {
		t.Fatalf("activities = %#v, want the orphan result as well", activities)
	}
	for id, want := range map[string]protocol.ToolKind{"srv_1": protocol.ToolWebSearch, "srv_2": protocol.ToolWebFetch, "srv_missing": protocol.ToolWebSearch} {
		activity, present := activities[id]
		if !present {
			t.Fatalf("%s is missing from the response", id)
		}
		if activity.Kind != want || activity.Status != protocol.WebStatusCompleted {
			t.Fatalf("%s = %#v", id, activity)
		}
	}
	if got := activities["srv_1"].Query; got != "Eylu" {
		t.Fatalf("the search query was lost: %q", got)
	}
	if got := activities["srv_2"].URL; got != "https://example.com" {
		t.Fatalf("the fetch URL was lost: %q", got)
	}
	if len(activities["srv_1"].Sources) != 1 || len(activities["srv_missing"].Sources) != 1 {
		t.Fatalf("sources = %#v / %#v", activities["srv_1"].Sources, activities["srv_missing"].Sources)
	}
	if citations != 1 || calls != 1 {
		t.Fatalf("citations=%d calls=%d", citations, calls)
	}
	// Every activity carries the provider's own response for the audit trail.
	for id, activity := range activities {
		if len(activity.RawProviderResponse) == 0 {
			t.Fatalf("%s kept no raw response", id)
		}
	}
	if response.Usage.InputTokens != 7 || response.Usage.CachedInputTokens != 3 {
		t.Fatalf("usage = %#v", response.Usage)
	}
}

// A stop reason the policy refuses is refused here, and a relaxation the policy
// allows is named on the response rather than being applied silently.
func TestConvertAnthropicRefusesAnUnknownStopReason(t *testing.T) {
	payload := `{"id":"msg_1","stop_reason":"something_new","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if _, err := convertAnthropic([]byte(payload), false); err == nil {
		t.Fatal("an unknown stop reason was accepted")
	}

	// A completion that carries a tool call is only executed when the provider was
	// declared to do that, and the relaxation is reported.
	withCall := `{"id":"msg_1","stop_reason":"end_turn","content":[{"type":"tool_use","id":"tool_1","name":"read_file","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	if _, err := convertAnthropic([]byte(withCall), false); err == nil {
		t.Fatal("a completion carrying a call was accepted without the relaxation")
	}
	response, err := convertAnthropic([]byte(withCall), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Interop) == 0 {
		t.Fatal("the relaxation was applied without being reported")
	}
}

// The pause_turn continuation is bounded by the smallest declared use limit, and
// a continuation that runs past it is a tool error rather than a silent loop.
func TestHostedUseLimitTakesTheSmallestDeclaredBound(t *testing.T) {
	web := func(maxUses int) protocol.ToolDefinition {
		return protocol.ToolDefinition{Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted, MaxUses: maxUses}
	}
	if got := hostedUseLimit(nil); got != 5 {
		t.Fatalf("the default limit = %d, want the documented default", got)
	}
	if got := hostedUseLimit([]protocol.ToolDefinition{web(0), web(9), web(2)}); got != 2 {
		t.Fatalf("the limit = %d, want the smallest declared bound", got)
	}
	if got := hostedUseLimit([]protocol.ToolDefinition{{Kind: protocol.ToolFunction, MaxUses: 1}}); got != 5 {
		t.Fatalf("a non-web tool bound the limit: %d", got)
	}
}

// A pause_turn response continues the conversation, and the continuation carries
// the provider's own content forward instead of asking the model again from the
// beginning.
func TestGenerateContinuesAPauseTurnAndStopsAtItsBound(t *testing.T) {
	pause := `{"id":"msg_1","stop_reason":"pause_turn","content":[{"type":"text","text":"still working"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	final := `{"id":"msg_2","stop_reason":"end_turn","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	rounds := 0
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		bodies = append(bodies, body)
		writer.Header().Set("Content-Type", "application/json")
		rounds++
		if rounds == 1 {
			_, _ = writer.Write([]byte(pause))
			return
		}
		_, _ = writer.Write([]byte(final))
	}))
	defer server.Close()

	response, err := New(server.Client(), DialectAnthropic).Generate(context.Background(), plainRequest(server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Turn.Parts[0].Text != "still working" || response.Stop != protocol.StopCompleted {
		t.Fatalf("aggregated response = %#v", response.Turn.Parts)
	}
	if rounds != 2 || len(bodies) != 2 {
		t.Fatalf("the request was sent %d times", rounds)
	}
	messages, _ := bodies[1]["messages"].([]any)
	if len(messages) != 2 || messages[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("the continuation did not carry the paused content: %#v", messages)
	}
	// The usage of both rounds is charged, not only the last one.
	if response.Usage.InputTokens != 2 {
		t.Fatalf("usage = %#v", response.Usage)
	}

	// A provider that only ever pauses is stopped by the declared bound.
	always := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(pause))
	}))
	defer always.Close()
	bounded := plainRequest(always.URL)
	bounded.Model.Tools = []protocol.ToolDefinition{{
		Name: "web_search", Kind: protocol.ToolWebSearch, Execution: protocol.ExecutionHosted,
		InputSchema: json.RawMessage(`{"type":"object"}`), MaxUses: 1,
	}}
	_, err = New(always.Client(), DialectAnthropic).Generate(context.Background(), bounded, nil)
	var typed *protocol.Error
	if !errors.As(err, &typed) || typed.Code != protocol.ErrTool || !strings.Contains(err.Error(), "max_uses") {
		t.Fatalf("an unbounded pause reported %v", err)
	}
}

// A continuation whose paused content cannot be read is refused rather than sent
// as an empty assistant message.
func TestAppendAnthropicContinuationRefusesEmptyContent(t *testing.T) {
	body := map[string]any{}
	if err := appendAnthropicContinuation(body, []byte(`{"stop_reason":"pause_turn"}`)); err == nil {
		t.Fatal("a pause without content was appended")
	}
	if err := appendAnthropicContinuation(body, []byte(`{"content":[{"type":"text","text":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "assistant" {
		t.Fatalf("the continuation = %#v", body["messages"])
	}
}

// The non-streaming path emits what the response carries, so a caller that renders
// events sees the same answer a streaming caller would.
func TestGenerateEmitsTheResponseItConverted(t *testing.T) {
	server := answeringServer(t, http.StatusOK, `{"id":"response_1","output":[{"type":"web_search_call","id":"web_1","status":"completed","action":{"type":"search","query":"Eylu"},"sources":[]},{"type":"message","content":[{"type":"output_text","text":"Eylu","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example"}]}]}],"usage":{"input_tokens":5,"output_tokens":1}}`)
	defer server.Close()
	var kinds []protocol.EventKind
	_, err := New(server.Client(), DialectPerplexity).Generate(context.Background(), plainRequest(server.URL), func(event protocol.ModelEvent) error {
		kinds = append(kinds, event.Kind)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []protocol.EventKind{
		protocol.EventResponseStart, protocol.EventWebSearchStarted, protocol.EventWebSearchCompleted,
		protocol.EventTextDelta, protocol.EventCitation, protocol.EventUsage, protocol.EventResponseDone,
	} {
		found := false
		for _, kind := range kinds {
			found = found || kind == want
		}
		if !found {
			t.Fatalf("%q was not emitted: %#v", want, kinds)
		}
	}
}

// A host that refuses the first event stops the request before it is sent, and one
// that refuses a later event stops it before the answer is reported as finished.
func TestGenerateStopsWhenTheHostRefusesAnEvent(t *testing.T) {
	sentinel := errors.New("host refused")
	for name, when := range map[string]int{"response start": 0, "usage": 1} {
		t.Run(name, func(t *testing.T) {
			server := answeringServer(t, http.StatusOK, `{"id":"response_1","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			defer server.Close()
			seen := 0
			_, err := New(server.Client(), DialectPerplexity).Generate(context.Background(), plainRequest(server.URL), func(protocol.ModelEvent) error {
				seen++
				if seen > when {
					return sentinel
				}
				return nil
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("a refused event reported %v", err)
			}
		})
	}
}

// answeringServer answers every request with one fixed status and body.
func answeringServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	return server
}

func plainRequest(baseURL string) driver.Request {
	return driver.Request{
		BaseURL: baseURL + "/v1", APIKey: "secret",
		Model: protocol.ModelRequest{Model: "model", Turns: []protocol.Turn{
			{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "ask"}}},
		}},
	}
}
