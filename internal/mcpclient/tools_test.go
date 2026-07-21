package mcpclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/protocol"
)

func TestApplyRemoteToolDetailsPreservesAnnotationsAndOutputSchema(t *testing.T) {
	destructive, openWorld := false, true
	remote := &sdkmcp.Tool{
		OutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"count": map[string]any{"type": "integer"}},
		},
		Annotations: &sdkmcp.ToolAnnotations{
			Title:           "Count records",
			ReadOnlyHint:    true,
			DestructiveHint: &destructive,
			IdempotentHint:  true,
			OpenWorldHint:   &openWorld,
		},
	}
	definition, err := applyRemoteToolDetails(protocol.ToolDefinition{Name: "count"}, remote)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Annotations == nil || definition.Annotations.Title != "Count records" || !definition.Annotations.ReadOnlyHint {
		t.Fatalf("annotations = %#v", definition.Annotations)
	}
	if definition.Annotations.DestructiveHint == nil || *definition.Annotations.DestructiveHint {
		t.Fatalf("destructive hint = %#v", definition.Annotations.DestructiveHint)
	}
	if definition.Annotations.OpenWorldHint == nil || !*definition.Annotations.OpenWorldHint {
		t.Fatalf("open world hint = %#v", definition.Annotations.OpenWorldHint)
	}
	if !json.Valid(definition.OutputSchema) || !strings.Contains(string(definition.OutputSchema), `"count"`) {
		t.Fatalf("output schema = %s", definition.OutputSchema)
	}
}

func TestMapToolResultPreservesAllMCPContentKinds(t *testing.T) {
	size := int64(123)
	result := mapToolResult(&sdkmcp.CallToolResult{
		Content: []sdkmcp.Content{
			&sdkmcp.TextContent{
				Text: "hello",
				Meta: sdkmcp.Meta{"trace": "text"},
				Annotations: &sdkmcp.Annotations{
					Audience:     []sdkmcp.Role{"user", "assistant"},
					Priority:     0.8,
					LastModified: "2026-07-21T10:00:00Z",
				},
			},
			&sdkmcp.ImageContent{MIMEType: "image/png", Data: []byte{0, 1, 2}, Meta: sdkmcp.Meta{"trace": "image"}},
			&sdkmcp.AudioContent{MIMEType: "audio/wav", Data: []byte{3, 4, 5}},
			&sdkmcp.EmbeddedResource{
				Meta: sdkmcp.Meta{"trace": "embedded"},
				Resource: &sdkmcp.ResourceContents{
					URI:      "file:///notes.txt",
					MIMEType: "text/plain",
					Text:     "notes",
					Meta:     sdkmcp.Meta{"revision": "two"},
				},
			},
			&sdkmcp.ResourceLink{
				URI:         "https://example.test/report",
				Name:        "report",
				Title:       "Report",
				Description: "Generated report",
				MIMEType:    "application/json",
				Size:        &size,
				Icons: []sdkmcp.Icon{{
					Source:   "https://example.test/icon.png",
					MIMEType: "image/png",
					Sizes:    []string{"48x48"},
					Theme:    sdkmcp.IconThemeDark,
				}},
			},
		},
		StructuredContent: map[string]any{"count": 2, "ok": true},
		IsError:           true,
	})

	if !result.IsError || len(result.ContentBlocks) != 5 {
		t.Fatalf("result = %#v", result)
	}
	if result.ContentBlocks[0].Type != protocol.ContentText || result.ContentBlocks[0].Text != "hello" {
		t.Fatalf("text content = %#v", result.ContentBlocks[0])
	}
	if got := result.ContentBlocks[0].Annotations; got == nil || !reflect.DeepEqual(got.Audience, []string{"user", "assistant"}) || got.Priority != 0.8 {
		t.Fatalf("text annotations = %#v", got)
	}
	if got := result.ContentBlocks[1]; got.Type != protocol.ContentImage || got.MIMEType != "image/png" || !reflect.DeepEqual(got.Data, []byte{0, 1, 2}) {
		t.Fatalf("image content = %#v", got)
	}
	if got := result.ContentBlocks[2]; got.Type != protocol.ContentAudio || got.MIMEType != "audio/wav" || !reflect.DeepEqual(got.Data, []byte{3, 4, 5}) {
		t.Fatalf("audio content = %#v", got)
	}
	if got := result.ContentBlocks[3]; got.Type != protocol.ContentEmbeddedResource || got.Resource == nil || got.Resource.Text != "notes" || got.Resource.Meta["revision"] != "two" {
		t.Fatalf("embedded resource = %#v", got)
	}
	if got := result.ContentBlocks[4]; got.Type != protocol.ContentResourceLink || got.Size == nil || *got.Size != size || len(got.Icons) != 1 || got.Icons[0].Theme != "dark" {
		t.Fatalf("resource link = %#v", got)
	}
	var structured map[string]any
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil || structured["count"] != float64(2) {
		t.Fatalf("structured content = %s, err = %v", result.StructuredContent, err)
	}
	if !strings.Contains(result.Content, "hello") || !strings.Contains(result.Content, "notes") || !strings.Contains(result.Content, `"count":2`) {
		t.Fatalf("legacy content = %q", result.Content)
	}
}

func TestMapToolResultHandlesNil(t *testing.T) {
	result := mapToolResult(nil)
	if result.Content != "" || len(result.ContentBlocks) != 0 || len(result.StructuredContent) != 0 {
		t.Fatalf("result = %#v", result)
	}
}
