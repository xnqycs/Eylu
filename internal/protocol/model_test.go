package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestProtocolV1Fixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "protocol_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var request ModelRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if request.ProtocolVersion != Version {
		t.Fatalf("version = %d, want %d", request.ProtocolVersion, Version)
	}
	if len(request.Turns) != 3 || request.Turns[1].Parts[0].ToolCall.ID != "call-1" {
		t.Fatalf("fixture was not decoded: %#v", request)
	}
	roundTrip, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ModelRequest
	if err := json.Unmarshal(roundTrip, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Turns[2].Parts[0].ToolResult.CallID != decoded.Turns[1].Parts[0].ToolCall.ID {
		t.Fatal("tool call/result relationship was not preserved")
	}
}

func TestErrorFormatting(t *testing.T) {
	err := &Error{Code: ErrConfig, Message: "missing provider"}
	if got := err.Error(); got != "config_error: missing provider" {
		t.Fatalf("Error() = %q", got)
	}
}
