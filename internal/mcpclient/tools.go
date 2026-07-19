package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type remoteToolAdapter struct {
	server     *serverRuntime
	remote     *sdkmcp.Tool
	definition protocol.ToolDefinition
	readOnly   bool
}

func (t *remoteToolAdapter) Definition() protocol.ToolDefinition { return t.definition }
func (t *remoteToolAdapter) ParallelSafe() bool                  { return t.readOnly }
func (t *remoteToolAdapter) Risk() policy.Risk {
	if t.readOnly {
		return policy.RiskRead
	}
	return policy.RiskWrite
}
func (t *remoteToolAdapter) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	arguments := make(map[string]any)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return protocol.ToolResult{Content: "invalid MCP tool arguments: " + err.Error(), IsError: true}
		}
	}
	result, err := t.server.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: t.remote.Name, Arguments: arguments})
	if err != nil {
		return protocol.ToolResult{Content: "MCP tool call failed: " + err.Error(), IsError: true, Metadata: mcpMetadata(t.server.name, t.remote.Name, "tool")}
	}
	return protocol.ToolResult{Content: renderToolResult(result), IsError: result.IsError, Metadata: mcpMetadata(t.server.name, t.remote.Name, "tool")}
}

type resourceTool struct {
	server     *serverRuntime
	definition protocol.ToolDefinition
}

func newResourceTool(server *serverRuntime) (*resourceTool, protocol.ToolDefinition, error) {
	localName := localToolName(server.name, "read_resource")
	values := make([]string, 0, len(server.resources))
	for _, resource := range server.resources {
		values = append(values, resource.URI)
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{"uri": map[string]any{"type": "string", "enum": values}},
		"required": []string{"uri"}, "additionalProperties": false,
	})
	if err != nil || len(schema) > maxSchemaBytes {
		return nil, protocol.ToolDefinition{}, errors.New("MCP resource schema is oversized")
	}
	definition := protocol.ToolDefinition{Name: localName, Description: "Read one advertised resource from MCP server " + server.name + ".", InputSchema: schema}
	return &resourceTool{server: server, definition: definition}, definition, nil
}

func (t *resourceTool) Definition() protocol.ToolDefinition { return t.definition }
func (t *resourceTool) Risk() policy.Risk                   { return policy.RiskRead }
func (t *resourceTool) ParallelSafe() bool                  { return true }
func (t *resourceTool) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || input.URI == "" {
		return protocol.ToolResult{Content: "invalid MCP resource input", IsError: true}
	}
	allowed := false
	for _, resource := range t.server.resources {
		if resource.URI == input.URI {
			allowed = true
			break
		}
	}
	if !allowed {
		return protocol.ToolResult{Content: "MCP resource URI was not advertised by the server", IsError: true}
	}
	result, err := t.server.session.ReadResource(ctx, &sdkmcp.ReadResourceParams{URI: input.URI})
	if err != nil {
		return protocol.ToolResult{Content: "MCP resource read failed: " + err.Error(), IsError: true, Metadata: mcpMetadata(t.server.name, input.URI, "resource")}
	}
	parts := make([]string, 0, len(result.Contents))
	for _, content := range result.Contents {
		if content.Text != "" {
			parts = append(parts, content.Text)
		} else {
			parts = append(parts, fmt.Sprintf("[binary resource uri=%s mime=%s bytes=%d]", content.URI, content.MIMEType, len(content.Blob)))
		}
	}
	return protocol.ToolResult{Content: strings.Join(parts, "\n"), Metadata: mcpMetadata(t.server.name, input.URI, "resource")}
}

func renderToolResult(result *sdkmcp.CallToolResult) string {
	if result == nil {
		return ""
	}
	parts := make([]string, 0, len(result.Content)+1)
	for _, content := range result.Content {
		switch typed := content.(type) {
		case *sdkmcp.TextContent:
			parts = append(parts, typed.Text)
		case *sdkmcp.EmbeddedResource:
			if typed.Resource != nil && typed.Resource.Text != "" {
				parts = append(parts, typed.Resource.Text)
			} else if typed.Resource != nil {
				parts = append(parts, fmt.Sprintf("[embedded resource uri=%s mime=%s bytes=%d]", typed.Resource.URI, typed.Resource.MIMEType, len(typed.Resource.Blob)))
			}
		case *sdkmcp.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s %s]", typed.Name, typed.URI))
		case *sdkmcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image mime=%s encoded_bytes=%d]", typed.MIMEType, len(typed.Data)))
		case *sdkmcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio mime=%s encoded_bytes=%d]", typed.MIMEType, len(typed.Data)))
		default:
			encoded, err := content.MarshalJSON()
			if err == nil {
				parts = append(parts, string(encoded))
			}
		}
	}
	if result.StructuredContent != nil {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil {
			parts = append(parts, string(encoded))
		}
	}
	return strings.Join(parts, "\n")
}

func mcpMetadata(server, name, kind string) map[string]any {
	return map[string]any{"mcp_server": server, "mcp_name": name, "mcp_kind": kind}
}
