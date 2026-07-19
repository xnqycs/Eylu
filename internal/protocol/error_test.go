package protocol

import "testing"

func TestClassifyProviderHTTPErrorDetectsContextOverflow(t *testing.T) {
	err := ClassifyProviderHTTPError(400, "This model's maximum context length is 128,000 tokens; your messages used 140000")
	if err.Code != ErrContextWindow || err.ContextLimit != 128000 || err.Retryable {
		t.Fatalf("error = %#v", err)
	}
	err = ClassifyProviderHTTPError(413, "context_length_exceeded: input is too long")
	if err.Code != ErrContextWindow || err.ContextLimit != 0 {
		t.Fatalf("error = %#v", err)
	}
	ordinary := ClassifyProviderHTTPError(400, "invalid tool schema")
	if ordinary.Code != ErrProvider {
		t.Fatalf("ordinary error = %#v", ordinary)
	}
}
