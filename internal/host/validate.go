package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

func digest(v any) string {
	b, _ := json.Marshal(v)
	var normalized any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&normalized)
	b, _ = json.Marshal(normalized)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func identifier(id string) bool   { return strings.TrimSpace(id) != "" && len(id) <= 256 }
func validBinding(b Binding) bool { return identifier(b.BindingID) && b.BindingRevision > 0 }
func validCreate(v Create) bool {
	if !identifier(v.ConversationID) || strings.TrimSpace(v.Instructions) == "" || v.Context == nil {
		return false
	}
	for _, c := range v.Context {
		if !identifier(c.Source) {
			return false
		}
	}
	return true
}
func validateStart(v Start) *RPCError {
	if !identifier(v.ConversationID) || !identifier(v.RunID) || v.ExpectedRevision < 1 || strings.TrimSpace(v.Input.Text) == "" || !validBinding(v.ModelBinding.Binding) || !validBinding(v.ToolCatalog.Binding) || !validBinding(v.TargetBinding) {
		return fault("INVALID_PARAMS")
	}
	l := v.Limits
	const maxDuration = int64(1<<63-1) / 1000000
	if l.MaxModelCalls <= 0 || l.MaxOutputTokensTotal <= 0 || l.MaxElapsedMS <= 0 || l.CallbackTimeoutMS <= 0 || l.StopGraceMS <= 0 || l.MaxElapsedMS > maxDuration || l.CallbackTimeoutMS > maxDuration || l.StopGraceMS > maxDuration {
		return fault("INVALID_PARAMS")
	}
	if v.ModelBinding.ContextWindowTokens <= 0 || v.ModelBinding.MaxOutputTokens <= 0 || v.ModelBinding.MaxOutputTokens >= v.ModelBinding.ContextWindowTokens {
		return fault("INVALID_PARAMS")
	}
	seen := map[string]bool{}
	for _, cap := range v.ModelBinding.Capabilities {
		if !slices.Contains([]string{"text", "tool_calls", "text_deltas"}, cap) || seen[cap] {
			return fault("UNSUPPORTED_CAPABILITY")
		}
		seen[cap] = true
	}
	if !seen["text"] || len(v.ToolCatalog.Tools) > 0 && !seen["tool_calls"] {
		return fault("UNSUPPORTED_CAPABILITY")
	}
	if v.ToolCatalog.Tools == nil {
		return fault("INVALID_PARAMS")
	}
	seen = map[string]bool{}
	for _, t := range v.ToolCatalog.Tools {
		if !identifier(t.Name) || seen[t.Name] || strings.TrimSpace(t.Description) == "" {
			return fault("INVALID_PARAMS")
		}
		seen[t.Name] = true
		if _, err := toolSchema(t.InputSchema); err != nil {
			return fault("UNSUPPORTED_CAPABILITY")
		}
	}
	return nil
}

// The negotiated v1 subset has no references or remote resolution. Constraints
// outside this list fail rather than disappearing during provider adaptation.
func toolSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := uniqueJSON(d, 0); err != nil {
		return nil, err
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil, fmt.Errorf("schema JSON")
	}
	object, ok := value.(map[string]any)
	if !ok || object["type"] != "object" {
		return nil, fmt.Errorf("root schema must be object")
	}
	if err := schemaSubset(value); err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return schema.Resolve(nil)
}
func schemaSubset(v any) error {
	if _, ok := v.(bool); ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("schema object")
	}
	for k, v := range m {
		switch k {
		case "$schema":
			if v != "https://json-schema.org/draft/2020-12/schema" {
				return fmt.Errorf("schema version")
			}
		case "type", "description", "title", "required", "enum", "const", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern", "minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties":
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("schema properties")
			}
			for _, p := range props {
				if err := schemaSubset(p); err != nil {
					return err
				}
			}
		case "items", "additionalProperties":
			if err := schemaSubset(v); err != nil {
				return err
			}
		case "allOf", "anyOf", "oneOf":
			a, ok := v.([]any)
			if !ok || len(a) == 0 {
				return fmt.Errorf("schema alternatives")
			}
			for _, s := range a {
				if err := schemaSubset(s); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unsupported schema keyword")
		}
	}
	return nil
}
func validUsage(u Usage) bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 && slices.Contains([]string{"provider", "estimated", "unknown"}, u.Source)
}
func validateMessage(m Message) error {
	if !slices.Contains([]string{"system", "user", "assistant", "tool"}, m.Role) || len(m.Parts) == 0 {
		return fault("INVALID_PARAMS")
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if p.ToolCallID != "" || p.Name != "" || p.Arguments != nil || p.Result != nil {
				return fault("INVALID_PARAMS")
			}
		case "tool_call":
			if m.Role != "assistant" || !identifier(p.ToolCallID) || !identifier(p.Name) || p.Text != "" || p.Result != nil {
				return fault("INVALID_PARAMS")
			}
			var a map[string]any
			if json.Unmarshal(p.Arguments, &a) != nil || a == nil {
				return fault("INVALID_PARAMS")
			}
		case "tool_result":
			if m.Role != "tool" || !identifier(p.ToolCallID) || p.Text != "" || p.Name != "" || p.Arguments != nil || p.Result == nil || validateToolResult(*p.Result, p.Result.RequestID) != nil {
				return fault("INVALID_PARAMS")
			}
		default:
			return fault("UNSUPPORTED_CAPABILITY")
		}
	}
	return nil
}
func validateModelResult(v ModelResult, id string) error {
	if v.RequestID != id || v.Message.Role != "assistant" || !validUsage(v.Usage) || !slices.Contains([]string{"completed", "tool_calls", "length", "blocked"}, v.FinishReason) {
		return fault("INVALID_PARAMS")
	}
	if err := validateMessage(v.Message); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range v.Message.Parts {
		if p.Type == "tool_call" {
			if seen[p.ToolCallID] {
				return fault("ID_CONFLICT")
			}
			seen[p.ToolCallID] = true
		}
	}
	if v.FinishReason == "tool_calls" && len(seen) == 0 || v.FinishReason == "completed" && len(seen) > 0 {
		return fault("INVALID_PARAMS")
	}
	return nil
}

// Truncated/blocked output can contain an unfinished tool argument value. Keep
// its usable text but never turn that fragment into a durable executable call.
func normalizeStoppedModel(v ModelResult) ModelResult {
	if v.FinishReason != "length" && v.FinishReason != "blocked" {
		return v
	}
	parts := []Part{}
	for _, p := range v.Message.Parts {
		if p.Type != "tool_call" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, Part{Type: "text", Text: ""})
	}
	v.Message.Parts = parts
	return v
}
func validateToolResult(v ToolResult, id string) error {
	if v.RequestID != id || !identifier(id) || !slices.Contains([]string{"succeeded", "failed", "denied", "cancelled", "unknown"}, v.Status) || !slices.Contains([]string{"none", "applied", "unknown"}, v.Effect) || v.Content == nil {
		return fault("INVALID_PARAMS")
	}
	if v.OperationID != nil && !identifier(*v.OperationID) {
		return fault("INVALID_PARAMS")
	}
	if v.Status == "succeeded" && v.Error != nil || v.Status != "succeeded" && (v.Error == nil || !identifier(v.Error.Code)) {
		return fault("INVALID_PARAMS")
	}
	if v.Status == "denied" && v.Effect != "none" {
		return fault("INVALID_PARAMS")
	}
	if v.Truncated && (v.ResourceRef == "" || v.TruncationReason == "") {
		return fault("INVALID_PARAMS")
	}
	for _, p := range v.Content {
		if p.Type != "text" || p.ToolCallID != "" || p.Name != "" || p.Arguments != nil || p.Result != nil {
			return fault("UNSUPPORTED_CAPABILITY")
		}
	}
	return nil
}
