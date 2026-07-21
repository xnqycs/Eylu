package protocol

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestMCPFieldsAreOptionalForProtocolV1JSON(t *testing.T) {
	result, err := json.Marshal(ToolResult{CallID: "call-1", Content: "package main"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte(`{"call_id":"call-1","content":"package main"}`); !bytes.Equal(result, want) {
		t.Fatalf("tool result JSON = %s, want %s", result, want)
	}

	definition, err := json.Marshal(ToolDefinition{
		Name:        "read_file",
		Description: "Read a file",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte(`{"name":"read_file","description":"Read a file","input_schema":{"type":"object"}}`); !bytes.Equal(definition, want) {
		t.Fatalf("tool definition JSON = %s, want %s", definition, want)
	}
}

func TestMCPProtocolFieldsRoundTripWithoutLosingContent(t *testing.T) {
	destructive, openWorld := false, true
	size := int64(42)
	want := ToolResult{
		CallID:  "call-rich",
		Content: "legacy rendering",
		ContentBlocks: []ContentBlock{
			{
				Type: ContentText,
				Text: "hello",
				Meta: map[string]any{"trace": "one"},
				Annotations: &ContentAnnotations{
					Audience:     []string{"user", "assistant"},
					Priority:     0.75,
					LastModified: "2026-07-21T10:00:00Z",
				},
			},
			{Type: ContentImage, MIMEType: "image/png", Data: []byte{0, 1, 2}},
			{Type: ContentAudio, MIMEType: "audio/wav", Data: []byte{3, 4, 5}},
			{
				Type: ContentEmbeddedResource,
				Resource: &ResourceContents{
					URI:      "file:///notes.txt",
					MIMEType: "text/plain",
					Text:     "notes",
					Meta:     map[string]any{"revision": "two"},
				},
			},
			{
				Type:        ContentResourceLink,
				URI:         "https://example.test/report",
				Name:        "report",
				Title:       "Report",
				Description: "Generated report",
				MIMEType:    "application/json",
				Size:        &size,
				Icons: []Icon{{
					Source:   "https://example.test/icon.png",
					MIMEType: "image/png",
					Sizes:    []string{"48x48"},
					Theme:    "dark",
				}},
			},
		},
		StructuredContent: json.RawMessage(`{"count":2,"ok":true}`),
	}
	definition := ToolDefinition{
		Name:         "remote_tool",
		Description:  "Remote tool",
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		OutputSchema: json.RawMessage(`{"type":"object","required":["count"]}`),
		Annotations: &ToolAnnotations{
			Title:           "Remote Tool",
			ReadOnlyHint:    true,
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &openWorld,
		},
	}

	encoded, err := json.Marshal(struct {
		Result     ToolResult     `json:"result"`
		Definition ToolDefinition `json:"definition"`
	}{Result: want, Definition: definition})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Result     ToolResult     `json:"result"`
		Definition ToolDefinition `json:"definition"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Result, want) {
		t.Fatalf("tool result changed across JSON round trip:\n got: %#v\nwant: %#v", decoded.Result, want)
	}
	if !reflect.DeepEqual(decoded.Definition, definition) {
		t.Fatalf("tool definition changed across JSON round trip:\n got: %#v\nwant: %#v", decoded.Definition, definition)
	}
}
