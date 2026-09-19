package driver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/protocol"
)

// outputBound describes how one dialect expresses the output bound a request
// declares.
type outputBound struct {
	// field is the JSON field the dialect writes it in, or empty when the dialect
	// has no such field at all.
	field string
	// fixed is the bound the dialect sends when the request declares none. It is
	// zero for a dialect that sends nothing.
	fixed int
}

// outputBounds is the whole answer, per dialect, including the dialects that
// cannot express one: a request that declares a bound is never mapped onto a field
// a dialect does not have, and the four wrappers around one implementation are
// listed separately because they are separate wire formats.
var outputBounds = map[string]outputBound{
	"openai_chat":           {field: "max_completion_tokens"},
	"openai_responses":      {field: "max_output_tokens"},
	"anthropic_messages":    {field: "max_tokens", fixed: 4096},
	"gemini_interactions":   {},
	"mistral_conversations": {},
	"perplexity_agent":      {},
}

// A request that declares an output budget asks the provider for at most that much,
// and a request that declares none is byte-for-byte the request it was before the
// field existed.
//
// This is the contract half of the budget hardening: the wallet that decides
// whether a call may start is the admission check, and this is what stops an
// admitted call from overshooting by asking for an unbounded answer.
func TestDriversForwardTheOutputBoundTheyWereGiven(t *testing.T) {
	turns := []protocol.Turn{
		{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "ask"}}},
	}
	for _, dialect := range stopContractDialects() {
		t.Run(dialect.name, func(t *testing.T) {
			want, known := outputBounds[dialect.name]
			if !known {
				t.Fatalf("%s has no entry in outputBounds, so this contract is not checking it", dialect.name)
			}

			// The bound the caller declared is forwarded, and never widened.
			declared := 777
			bounded := generateWithBound(t, dialect, turns, declared)
			unbounded := generateWithBound(t, dialect, turns, 0)
			if want.field == "" {
				for _, body := range []map[string]any{bounded, unbounded} {
					for _, field := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
						if _, present := body[field]; present {
							t.Fatalf("%s has no output bound in its format but sent %q: %#v", dialect.name, field, body)
						}
					}
				}
				return
			}

			if got := bounded[want.field]; got != float64(declared) {
				t.Fatalf("%s sent %s=%#v for a declared bound of %d", dialect.name, want.field, got, declared)
			}
			// A dialect with a fixed bound takes the more conservative of the two:
			// declaring a budget can only narrow it, never widen it.
			if want.fixed > 0 {
				if got := bounded[want.field]; got != float64(min(declared, want.fixed)) {
					t.Fatalf("%s sent %s=%#v, want the smaller of %d and %d", dialect.name, want.field, got, declared, want.fixed)
				}
				wider := generateWithBound(t, dialect, turns, want.fixed*4)
				if got := wider[want.field]; got != float64(want.fixed) {
					t.Fatalf("%s widened its fixed bound to %#v", dialect.name, got)
				}
			}
			// Without a declared bound the request is what it always was.
			if want.fixed == 0 {
				if _, present := unbounded[want.field]; present {
					t.Fatalf("%s sent %s without being given a bound: %#v", dialect.name, want.field, unbounded)
				}
			} else if got := unbounded[want.field]; got != float64(want.fixed) {
				t.Fatalf("%s sent %s=%#v without a declared bound, want its own %d", dialect.name, want.field, got, want.fixed)
			}
		})
	}
}

// The bound is a bound and not a reservation: a driver that cannot honour it still
// produces a response, and the request is refused by the admission check rather
// than by the provider.
func TestAnOutputBoundDoesNotChangeWhatTheResponseMeans(t *testing.T) {
	turns := []protocol.Turn{
		{ID: "user", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "ask"}}},
	}
	for _, dialect := range stopContractDialects() {
		withBound := generateWithBound(t, dialect, turns, 1)
		without := generateWithBound(t, dialect, turns, 0)
		// Whatever the bound, the request still carries the conversation and the
		// model: a bound that changed those would be a different request, not a
		// bounded one.
		for _, field := range []string{"model", "messages", "input", "inputs"} {
			before, hadBefore := without[field]
			after, hasAfter := withBound[field]
			if hadBefore != hasAfter {
				t.Fatalf("%s dropped %q when a bound was declared", dialect.name, field)
			}
			if hadBefore && !jsonEqual(before, after) {
				t.Fatalf("%s changed %q when a bound was declared", dialect.name, field)
			}
		}
	}
}

func jsonEqual(left, right any) bool {
	encodedLeft, errLeft := json.Marshal(left)
	encodedRight, errRight := json.Marshal(right)
	if errLeft != nil || errRight != nil {
		return false
	}
	return string(encodedLeft) == string(encodedRight)
}

// generateOnce runs one request with the given output bound against a stub
// provider and returns the JSON body the driver sent.
func generateWithBound(t *testing.T, dialect stopDialect, turns []protocol.Turn, maxOutputTokens int) map[string]any {
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
		BaseURL: server.URL, APIKey: "key", MaxOutputTokens: maxOutputTokens,
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
