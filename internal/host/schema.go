package host

import (
	"encoding/json"
	"reflect"
	"strings"
)

// ProtocolSchema is generated from the independent wire/storage DTOs. The
// checked-in schema is compared byte-for-byte in tests so a DTO edit cannot
// silently change the durable protocol.
func ProtocolSchema() ([]byte, error) {
	defs := map[string]any{}
	var schemaFor func(reflect.Type) map[string]any
	schemaFor = func(t reflect.Type) map[string]any {
		if t == rawType {
			return map[string]any{}
		}
		if t.Kind() == reflect.Pointer {
			return map[string]any{"anyOf": []any{schemaFor(t.Elem()), map[string]any{"type": "null"}}}
		}
		switch t.Kind() {
		case reflect.String:
			return map[string]any{"type": "string"}
		case reflect.Bool:
			return map[string]any{"type": "boolean"}
		case reflect.Int, reflect.Int64:
			return map[string]any{"type": "integer"}
		case reflect.Slice:
			return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
		case reflect.Map:
			return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
		case reflect.Interface:
			return map[string]any{}
		case reflect.Struct:
			name := t.Name()
			ref := map[string]any{"$ref": "#/$defs/" + name}
			if _, exists := defs[name]; exists {
				return ref
			}
			defs[name] = map[string]any{}
			props := map[string]any{}
			required := []string{}
			var add func(reflect.Type)
			add = func(t reflect.Type) {
				for i := 0; i < t.NumField(); i++ {
					f := t.Field(i)
					if f.Anonymous {
						add(f.Type)
						continue
					}
					tag := strings.Split(f.Tag.Get("json"), ",")
					props[tag[0]] = schemaFor(f.Type)
					if len(tag) == 1 {
						required = append(required, tag[0])
					}
				}
			}
			add(t)
			defs[name] = map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": required}
			return ref
		}
		return map[string]any{}
	}
	types := []any{Initialize{}, Initialized{}, Create{}, Start{}, Get{}, Interrupt{}, Shutdown{}, Accepted{}, ModelRequest{}, ModelResult{}, ToolRequest{}, ToolResult{}, CancelCalls{}, Delta{}, CallProgress{}, Commit{}, Committed{}, CommittedNotification{}, View{}, Restore{}, RPCError{}}
	for _, value := range types {
		schemaFor(reflect.TypeOf(value))
	}
	property := func(name, field string, value any) {
		defs[name].(map[string]any)["properties"].(map[string]any)[field] = value
	}
	enum := func(values ...string) any { return map[string]any{"type": "string", "enum": values} }
	for _, name := range []string{"Initialize", "Initialized"} {
		property(name, "protocol_version", map[string]any{"const": ProtocolVersion})
		property(name, "max_message_bytes", map[string]any{"type": "integer", "minimum": 1024, "maximum": MaxMessageBytes})
	}
	for _, name := range []string{"Initialized", "Commit", "Restore"} {
		property(name, "state_schema", map[string]any{"const": StateSchema})
	}
	property("Message", "role", enum("system", "user", "assistant", "tool"))
	property("ModelRequest", "purpose", enum("conversation", "compaction"))
	property("ModelResult", "finish_reason", enum("completed", "tool_calls", "length", "blocked"))
	property("Usage", "source", enum("provider", "estimated", "unknown"))
	property("ToolResult", "status", enum("succeeded", "failed", "denied", "cancelled", "unknown"))
	property("ToolResult", "effect", enum("none", "applied", "unknown"))
	property("Run", "status", enum("running", "stopping", "completed", "interrupted", "failed"))
	property("Run", "phase", enum("preparing", "waiting_model", "waiting_host", "committing"))
	property("Reconciliation", "status", enum("not_dispatched", "settled", "unknown"))
	property("PendingCall", "kind", enum("model", "tool"))
	property("CallProgress", "phase", enum("awaiting_approval", "awaiting_input", "executing"))
	property("Delta", "delta_seq", map[string]any{"type": "integer", "minimum": 1})
	for _, name := range []string{"ModelBinding", "ToolCatalog", "Binding"} {
		property(name, "binding_revision", map[string]any{"type": "integer", "minimum": 1})
	}
	for _, field := range []string{"max_model_calls", "max_output_tokens_total", "max_elapsed_ms", "callback_timeout_ms", "stop_grace_ms"} {
		property("Limits", field, map[string]any{"type": "integer", "minimum": 1})
	}
	obj := func(props map[string]any, required ...string) any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	ref := func(name string) any { return map[string]any{"$ref": "#/$defs/" + name} }
	str := map[string]any{"type": "string"}
	defs["TextPart"] = obj(map[string]any{"type": map[string]any{"const": "text"}, "text": str}, "type", "text")
	defs["Part"] = map[string]any{"oneOf": []any{
		ref("TextPart"),
		obj(map[string]any{"type": map[string]any{"const": "tool_call"}, "tool_call_id": str, "name": str, "arguments": map[string]any{"type": "object"}}, "type", "tool_call_id", "name", "arguments"),
		obj(map[string]any{"type": map[string]any{"const": "tool_result"}, "tool_call_id": str, "result": ref("ToolResult")}, "type", "tool_call_id", "result"),
	}}
	// A length/blocked response can carry a partial argument value. It remains
	// data: the engine keeps text and discards these calls before committing.
	stoppedCall := obj(map[string]any{"type": map[string]any{"const": "tool_call"}, "tool_call_id": str, "name": str, "arguments": map[string]any{}}, "type", "tool_call_id", "name", "arguments")
	defs["StoppedModelMessage"] = obj(map[string]any{"role": map[string]any{"const": "assistant"}, "parts": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"anyOf": []any{ref("TextPart"), stoppedCall}}}}, "role", "parts")
	property("ModelResult", "message", map[string]any{})
	defs["ModelResult"].(map[string]any)["allOf"] = []any{map[string]any{
		"if":   map[string]any{"properties": map[string]any{"finish_reason": enum("length", "blocked")}},
		"then": map[string]any{"properties": map[string]any{"message": ref("StoppedModelMessage")}},
		"else": map[string]any{"properties": map[string]any{"message": ref("Message")}},
	}}
	property("ToolResult", "content", map[string]any{"type": "array", "items": ref("TextPart")})
	property("PendingCall", "request", map[string]any{"oneOf": []any{ref("ModelRequest"), ref("ToolRequest")}})
	property("ToolDefinition", "input_schema", map[string]any{"type": "object"})
	variants := []any{}
	for _, e := range []struct {
		name string
		data any
	}{{"conversation.created", ref("Create")}, {"message.committed", ref("StoredMessage")}, {"run.started", ref("Start")}, {"call.prepared", map[string]any{"oneOf": []any{ref("ModelRequest"), ref("ToolRequest")}}}, {"call.settled", map[string]any{"oneOf": []any{ref("ModelResult"), ref("ToolResult"), ref("Reconciliation")}}}, {"run.finished", ref("Run")}, {"context.compacted", obj(map[string]any{"summary": str}, "summary")}} {
		variants = append(variants, obj(map[string]any{"event_id": str, "type": map[string]any{"const": e.name}, "run_id": str, "data": e.data}, "event_id", "type", "data"))
	}
	defs["Event"] = map[string]any{"oneOf": variants}
	methodVariants := []any{}
	for _, m := range []struct {
		name, params, prefix string
		notify               bool
	}{
		{"initialize", "Initialize", "h:", false}, {"conversation.create", "Create", "h:", false}, {"conversation.restore", "Restore", "h:", false}, {"conversation.get", "Get", "h:", false}, {"run.start", "Start", "h:", false}, {"run.interrupt", "Interrupt", "h:", false}, {"shutdown", "Shutdown", "h:", false},
		{"host.model.generate", "ModelRequest", "e:", false}, {"host.tool.execute", "ToolRequest", "e:", false}, {"host.checkpoint.commit", "Commit", "e:", false}, {"host.calls.cancel", "CancelCalls", "e:", false},
		{"host.model.delta", "Delta", "", true}, {"host.call.progress", "CallProgress", "", true}, {"engine.committed", "CommittedNotification", "", true},
	} {
		props := map[string]any{"jsonrpc": map[string]any{"const": "2.0"}, "method": map[string]any{"const": m.name}, "params": ref(m.params)}
		required := []string{"jsonrpc", "method", "params"}
		if !m.notify {
			props["id"] = map[string]any{"type": "string", "pattern": "^" + m.prefix}
			required = append(required, "id")
		}
		methodVariants = append(methodVariants, obj(props, required...))
	}
	methodVariants = append(methodVariants, obj(map[string]any{"jsonrpc": map[string]any{"const": "2.0"}, "method": map[string]any{"const": "engine.progress"}, "params": map[string]any{"oneOf": []any{ref("Delta"), ref("CallProgress")}}}, "jsonrpc", "method", "params"))
	methodVariants = append(methodVariants, obj(map[string]any{"jsonrpc": map[string]any{"const": "2.0"}, "id": map[string]any{"type": "string", "pattern": "^[he]:"}, "result": map[string]any{}}, "jsonrpc", "id", "result"))
	methodVariants = append(methodVariants, obj(map[string]any{"jsonrpc": map[string]any{"const": "2.0"}, "id": map[string]any{"type": []string{"string", "null"}}, "error": ref("RPCError")}, "jsonrpc", "id", "error"))
	return json.MarshalIndent(map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "$id": "urn:eylu:bastion-host:1.0", "title": "Bastion host protocol 1.0 (DTO and checkpoint schema 1)", "$defs": defs, "oneOf": methodVariants}, "", "  ")
}
