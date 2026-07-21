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
	"Eylu/internal/tool"
)

type remoteToolAdapter struct {
	server     *serverRuntime
	remote     *sdkmcp.Tool
	definition protocol.ToolDefinition
	readOnly   bool
}

func (t *remoteToolAdapter) Definition() protocol.ToolDefinition { return t.definition }
func (t *remoteToolAdapter) ParallelSafe() bool                  { return t.readOnly }
func (t *remoteToolAdapter) ClassifyConcurrency(json.RawMessage, policy.Outcome) tool.ConcurrencySpec {
	if t.readOnly {
		return tool.ConcurrencySpec{Mode: tool.ConcurrencyShared}
	}
	return tool.ConcurrencySpec{Mode: tool.ConcurrencyExclusive}
}
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
	value, err := t.server.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return session.CallTool(callCtx, &sdkmcp.CallToolParams{Name: t.remote.Name, Arguments: arguments})
	})
	if err != nil {
		return protocol.ToolResult{Content: "MCP tool call failed: " + err.Error(), IsError: true, Metadata: mcpMetadata(t.server.name, t.remote.Name, "tool")}
	}
	result := value.(*sdkmcp.CallToolResult)
	mapped := mapToolResult(result)
	mapped.Metadata = mcpMetadata(t.server.name, t.remote.Name, "tool")
	if result != nil && len(result.Meta) > 0 {
		mapped.Metadata["mcp_result_meta"] = cloneMap(result.Meta)
	}
	return mapped
}

type resourceTool struct {
	server     *serverRuntime
	definition protocol.ToolDefinition
}

func newResourceToolForResources(server *serverRuntime, resources []*sdkmcp.Resource) (*resourceTool, protocol.ToolDefinition, error) {
	localName := localToolName(server.name, "read_resource")
	values := make([]string, 0, len(resources))
	for _, resource := range resources {
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
func (t *resourceTool) ClassifyConcurrency(json.RawMessage, policy.Outcome) tool.ConcurrencySpec {
	return tool.ConcurrencySpec{Mode: tool.ConcurrencyShared}
}
func (t *resourceTool) Execute(ctx context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &input); err != nil || input.URI == "" {
		return protocol.ToolResult{Content: "invalid MCP resource input", IsError: true}
	}
	allowed := false
	for _, resource := range t.server.resourcesSnapshot() {
		if resource.URI == input.URI {
			allowed = true
			break
		}
	}
	if !allowed {
		return protocol.ToolResult{Content: "MCP resource URI was not advertised by the server", IsError: true}
	}
	value, err := t.server.withSessionCall(ctx, func(callCtx context.Context, session *sdkmcp.ClientSession) (any, error) {
		return session.ReadResource(callCtx, &sdkmcp.ReadResourceParams{URI: input.URI})
	})
	if err != nil {
		return protocol.ToolResult{Content: "MCP resource read failed: " + err.Error(), IsError: true, Metadata: mcpMetadata(t.server.name, input.URI, "resource")}
	}
	result := value.(*sdkmcp.ReadResourceResult)
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

func applyRemoteToolDetails(definition protocol.ToolDefinition, remote *sdkmcp.Tool) (protocol.ToolDefinition, error) {
	if remote == nil {
		return definition, nil
	}
	if remote.OutputSchema != nil {
		encoded, err := json.Marshal(remote.OutputSchema)
		if err != nil || !json.Valid(encoded) || len(encoded) > maxSchemaBytes || string(encoded) == "null" {
			return protocol.ToolDefinition{}, fmt.Errorf("MCP tool %s has an invalid or oversized output schema", remote.Name)
		}
		definition.OutputSchema = encoded
	}
	definition.Annotations = mapToolAnnotations(remote.Annotations)
	return definition, nil
}

func mapToolAnnotations(source *sdkmcp.ToolAnnotations) *protocol.ToolAnnotations {
	if source == nil {
		return nil
	}
	return &protocol.ToolAnnotations{
		Title:           source.Title,
		ReadOnlyHint:    source.ReadOnlyHint,
		DestructiveHint: cloneBool(source.DestructiveHint),
		IdempotentHint:  source.IdempotentHint,
		OpenWorldHint:   cloneBool(source.OpenWorldHint),
	}
}

func mapToolResult(result *sdkmcp.CallToolResult) protocol.ToolResult {
	if result == nil {
		return protocol.ToolResult{}
	}
	mapped := protocol.ToolResult{
		Content: renderToolResult(result),
		IsError: result.IsError,
	}
	if len(result.Content) > 0 {
		mapped.ContentBlocks = make([]protocol.ContentBlock, 0, len(result.Content))
		for _, content := range result.Content {
			if block, ok := mapContentBlock(content); ok {
				mapped.ContentBlocks = append(mapped.ContentBlocks, block)
			}
		}
	}
	if result.StructuredContent != nil {
		if encoded, err := json.Marshal(result.StructuredContent); err == nil {
			mapped.StructuredContent = encoded
		}
	}
	return mapped
}

func mapContentBlock(content sdkmcp.Content) (protocol.ContentBlock, bool) {
	switch typed := content.(type) {
	case *sdkmcp.TextContent:
		if typed == nil {
			return protocol.ContentBlock{}, false
		}
		return protocol.ContentBlock{
			Type:        protocol.ContentText,
			Text:        typed.Text,
			Meta:        cloneMap(typed.Meta),
			Annotations: mapContentAnnotations(typed.Annotations),
		}, true
	case *sdkmcp.ImageContent:
		if typed == nil {
			return protocol.ContentBlock{}, false
		}
		return protocol.ContentBlock{
			Type:        protocol.ContentImage,
			Data:        cloneBytes(typed.Data),
			MIMEType:    typed.MIMEType,
			Meta:        cloneMap(typed.Meta),
			Annotations: mapContentAnnotations(typed.Annotations),
		}, true
	case *sdkmcp.AudioContent:
		if typed == nil {
			return protocol.ContentBlock{}, false
		}
		return protocol.ContentBlock{
			Type:        protocol.ContentAudio,
			Data:        cloneBytes(typed.Data),
			MIMEType:    typed.MIMEType,
			Meta:        cloneMap(typed.Meta),
			Annotations: mapContentAnnotations(typed.Annotations),
		}, true
	case *sdkmcp.EmbeddedResource:
		if typed == nil {
			return protocol.ContentBlock{}, false
		}
		return protocol.ContentBlock{
			Type:        protocol.ContentEmbeddedResource,
			Resource:    mapResourceContents(typed.Resource),
			Meta:        cloneMap(typed.Meta),
			Annotations: mapContentAnnotations(typed.Annotations),
		}, true
	case *sdkmcp.ResourceLink:
		if typed == nil {
			return protocol.ContentBlock{}, false
		}
		return protocol.ContentBlock{
			Type:        protocol.ContentResourceLink,
			URI:         typed.URI,
			Name:        typed.Name,
			Title:       typed.Title,
			Description: typed.Description,
			MIMEType:    typed.MIMEType,
			Size:        cloneInt64(typed.Size),
			Icons:       mapIcons(typed.Icons),
			Meta:        cloneMap(typed.Meta),
			Annotations: mapContentAnnotations(typed.Annotations),
		}, true
	default:
		return protocol.ContentBlock{}, false
	}
}

func mapContentAnnotations(source *sdkmcp.Annotations) *protocol.ContentAnnotations {
	if source == nil {
		return nil
	}
	audience := make([]string, len(source.Audience))
	for index, role := range source.Audience {
		audience[index] = string(role)
	}
	return &protocol.ContentAnnotations{
		Audience:     audience,
		Priority:     source.Priority,
		LastModified: source.LastModified,
	}
}

func mapResourceContents(source *sdkmcp.ResourceContents) *protocol.ResourceContents {
	if source == nil {
		return nil
	}
	return &protocol.ResourceContents{
		URI:      source.URI,
		MIMEType: source.MIMEType,
		Text:     source.Text,
		Blob:     cloneBytes(source.Blob),
		Meta:     cloneMap(source.Meta),
	}
}

func mapIcons(source []sdkmcp.Icon) []protocol.Icon {
	if len(source) == 0 {
		return nil
	}
	result := make([]protocol.Icon, len(source))
	for index, icon := range source {
		result[index] = protocol.Icon{
			Source:   icon.Source,
			MIMEType: icon.MIMEType,
			Sizes:    append([]string(nil), icon.Sizes...),
			Theme:    string(icon.Theme),
		}
	}
	return result
}

func cloneMap(source map[string]any) map[string]any {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneBytes(source []byte) []byte {
	if source == nil {
		return nil
	}
	return append([]byte(nil), source...)
}

func cloneBool(source *bool) *bool {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

func cloneInt64(source *int64) *int64 {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

func mcpMetadata(server, name, kind string) map[string]any {
	return map[string]any{"mcp_server": server, "mcp_name": name, "mcp_kind": kind}
}
