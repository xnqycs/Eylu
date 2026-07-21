package mcpclient

//lint:file-ignore SA1019 MCP protocol 2025-11-25 requires roots, sampling, and logging coverage.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"Eylu/internal/config"
	"Eylu/internal/tool"
)

func TestManagerHostCallbacksUtilitiesAndCompletion(t *testing.T) {
	server := newHostProtocolServer()
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{JSONResponse: true})
	capture := &protocolCapture{handler: handler}
	httpServer := httptest.NewServer(capture)
	defer httpServer.Close()
	workspace := t.TempDir()
	manager, diagnostics, err := OpenWithOptions(context.Background(), map[string]config.MCPServerConfig{
		"host": {Transport: config.MCPTransportStreamableHTTP, URL: httpServer.URL, StartupTimeoutSeconds: 5},
	}, workspace, Options{
		CreateMessageHandler: func(context.Context, *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error) {
			return &sdkmcp.CreateMessageResult{Role: sdkmcp.Role("assistant"), Model: "fixture-model", Content: &sdkmcp.TextContent{Text: "sampled"}}, nil
		},
		ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "approved"}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics=%#v", diagnostics)
	}
	events, unsubscribe := manager.SubscribeEvents(16)
	defer unsubscribe()
	loggingDeadline := time.Now().Add(2 * time.Second)
	for !containsPrefix(capture.snapshot().methods, "logging/setlevel") {
		if time.Now().After(loggingDeadline) {
			t.Fatalf("logging level was not configured in the background; methods=%#v", capture.snapshot().methods)
		}
		time.Sleep(10 * time.Millisecond)
	}
	registry := tool.NewRegistry(manager.Tools()...)
	for name, contains := range map[string]string{
		"mcp__host__roots":   "file://",
		"mcp__host__sample":  "sampled",
		"mcp__host__elicit":  "approved",
		"mcp__host__utility": "sent",
	} {
		remote, ok := registry.Get(name)
		if !ok {
			t.Fatalf("tool %s missing", name)
		}
		result := remote.Execute(context.Background(), json.RawMessage(`{}`))
		if result.IsError || !strings.Contains(result.Content, contains) {
			t.Fatalf("tool %s result=%#v", name, result)
		}
	}
	completion, err := manager.Complete(context.Background(), "host", &sdkmcp.CompleteParams{
		Ref: &sdkmcp.CompleteReference{Type: "ref/prompt", Name: "fixture"}, Argument: sdkmcp.CompleteParamsArgument{Name: "name", Value: "E"},
	})
	if err != nil || len(completion.Completion.Values) != 1 || completion.Completion.Values[0] != "Eylu" {
		t.Fatalf("completion=%#v error=%v", completion, err)
	}
	if err := manager.Ping(context.Background(), "host"); err != nil {
		t.Fatal(err)
	}
	if err := manager.SubscribeResource(context.Background(), "host", "fixture://host"); err != nil {
		t.Fatal(err)
	}
	if err := manager.UnsubscribeResource(context.Background(), "host", "fixture://host"); err != nil {
		t.Fatal(err)
	}
	want := map[EventKind]bool{EventProgress: false, EventLogging: false}
	deadline := time.After(2 * time.Second)
	for !want[EventProgress] || !want[EventLogging] {
		select {
		case event := <-events:
			if _, ok := want[event.Kind]; ok {
				want[event.Kind] = true
			}
		case <-deadline:
			t.Fatalf("utility events=%#v", want)
		}
	}
}

func newHostProtocolServer() *sdkmcp.Server {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "host-fixture", Version: "1.0.0"}, &sdkmcp.ServerOptions{
		Capabilities: &sdkmcp.ServerCapabilities{Logging: &sdkmcp.LoggingCapabilities{}},
		CompletionHandler: func(_ context.Context, request *sdkmcp.CompleteRequest) (*sdkmcp.CompleteResult, error) {
			return &sdkmcp.CompleteResult{Completion: sdkmcp.CompletionResultDetails{Values: []string{request.Params.Argument.Value + "ylu"}, Total: 1}}, nil
		},
		SubscribeHandler:   func(context.Context, *sdkmcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *sdkmcp.UnsubscribeRequest) error { return nil },
	})
	type output struct {
		Value string `json:"value"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "roots"}, func(ctx context.Context, request *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, output, error) {
		result, err := request.Session.ListRoots(ctx, nil)
		if err != nil {
			return nil, output{}, err
		}
		return nil, output{Value: result.Roots[0].URI}, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "sample"}, func(ctx context.Context, request *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, output, error) {
		result, err := request.Session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
			MaxTokens: 16, Messages: []*sdkmcp.SamplingMessage{{Role: sdkmcp.Role("user"), Content: &sdkmcp.TextContent{Text: "sample"}}},
		})
		if err != nil {
			return nil, output{}, err
		}
		return nil, output{Value: result.Content.(*sdkmcp.TextContent).Text}, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "elicit"}, func(ctx context.Context, request *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, output, error) {
		result, err := request.Session.Elicit(ctx, &sdkmcp.ElicitParams{
			Mode: "form", Message: "Approve", RequestedSchema: map[string]any{
				"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"},
			},
		})
		if err != nil {
			return nil, output{}, err
		}
		return nil, output{Value: result.Content["answer"].(string)}, nil
	})
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "utility"}, func(ctx context.Context, request *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, output, error) {
		if err := request.Session.NotifyProgress(ctx, &sdkmcp.ProgressNotificationParams{ProgressToken: "fixture", Progress: 1, Total: 1, Message: "done"}); err != nil {
			return nil, output{}, err
		}
		if err := request.Session.Log(ctx, &sdkmcp.LoggingMessageParams{Level: sdkmcp.LoggingLevel("info"), Logger: "fixture", Data: "logged"}); err != nil {
			return nil, output{}, err
		}
		return nil, output{Value: "sent"}, nil
	})
	server.AddResource(&sdkmcp.Resource{URI: "fixture://host", Name: "host"}, func(_ context.Context, request *sdkmcp.ReadResourceRequest) (*sdkmcp.ReadResourceResult, error) {
		return &sdkmcp.ReadResourceResult{Contents: []*sdkmcp.ResourceContents{{URI: request.Params.URI, Text: "host"}}}, nil
	})
	return server
}
