package host

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestSchemaAndFixturesMatchDTOs(t *testing.T) {
	schema, err := ProtocolSchema()
	if err != nil {
		t.Fatal(err)
	}
	checked, err := os.ReadFile("../../docs/host/protocol.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(checked), bytes.TrimSpace(schema)) {
		t.Fatal("host schema drift: go run ./cmd/host-schema docs/host/protocol.schema.json docs/host/fixtures.jsonl")
	}
	fixtures, err := ProtocolFixtures()
	if err != nil {
		t.Fatal(err)
	}
	checked, err = os.ReadFile("../../docs/host/fixtures.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(checked, fixtures) {
		t.Fatal("fixture drift")
	}
	var document jsonschema.Schema
	if err = json.Unmarshal(schema, &document); err != nil {
		t.Fatal(err)
	}
	resolved, err := document.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(fixtures), []byte{'\n'}) {
		var value any
		_ = json.Unmarshal(line, &value)
		if err := resolved.Validate(value); err != nil {
			t.Fatalf("invalid fixture: %v\n%s", err, line)
		}
	}
}

func TestSchemaRejectsUnsupportedAndMissingShapes(t *testing.T) {
	schema, _ := ProtocolSchema()
	var root map[string]any
	_ = json.Unmarshal(schema, &root)
	delete(root, "oneOf")
	root["$ref"] = "#/$defs/State"
	raw, _ := json.Marshal(root)
	var doc jsonschema.Schema
	_ = json.Unmarshal(raw, &doc)
	compiled, err := doc.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Validate(map[string]any{"conversation_id": "missing-state"}) == nil {
		t.Fatal("state schema accepts missing fields")
	}
}

func TestSchemaAllowsPartialArgumentsOnlyOnStoppedModelOutput(t *testing.T) {
	schema, _ := ProtocolSchema()
	var root map[string]any
	_ = json.Unmarshal(schema, &root)
	delete(root, "oneOf")
	root["$ref"] = "#/$defs/ModelResult"
	raw, _ := json.Marshal(root)
	var doc jsonschema.Schema
	_ = json.Unmarshal(raw, &doc)
	compiled, err := doc.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	result := ModelResult{RequestID: "request", Message: Message{Role: "assistant", Parts: []Part{{Type: "text", Text: "usable"}, {Type: "tool_call", ToolCallID: "partial", Name: "tool", Arguments: json.RawMessage(`"{unfinished"`)}}}, FinishReason: "length", Usage: Usage{Source: "provider"}}
	for _, finish := range []string{"length", "tool_calls"} {
		result.FinishReason = finish
		raw, _ = json.Marshal(result)
		var value any
		_ = json.Unmarshal(raw, &value)
		err = compiled.Validate(value)
		if (finish == "length") != (err == nil) {
			t.Fatalf("schema mismatch for %s: %v", finish, err)
		}
	}
}
