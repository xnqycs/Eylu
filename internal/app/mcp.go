package app

//lint:file-ignore SA1019 MCP protocol 2025-11-25 compatibility.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	gostdruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/driver"
	"Eylu/internal/mcpclient"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

type mcpHostCallbacks struct {
	createMessage          func(context.Context, *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error)
	createMessageWithTools func(context.Context, *sdkmcp.CreateMessageWithToolsRequest) (*sdkmcp.CreateMessageWithToolsResult, error)
	elicitation            func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error)
	elicitationForm        bool
	elicitationURL         bool
}

var openMCPElicitationURL = openExternalURL

func (r *runtime) configureMCPRuntime(ctx context.Context, cfg config.Config, modelRuntime *agent.Runtime) error {
	r.mcpHostMu.RLock()
	host := r.mcpHost
	r.mcpHostMu.RUnlock()
	return r.configureMCPRuntimeWithHost(ctx, cfg, modelRuntime, host)
}

func (r *runtime) configureMCPRuntimeWithHost(ctx context.Context, cfg config.Config, modelRuntime *agent.Runtime, host mcpHostCallbacks) error {
	manager, err := r.loadMCPWithHost(ctx, cfg, host)
	if err != nil {
		return err
	}
	state := mcpStateFromManager(manager)
	r.storeMCPState(state)
	applyMCPState(modelRuntime, state)
	modelRuntime.MCPState = r.currentMCPState
	return nil
}

func (r *runtime) loadMCP(ctx context.Context, cfg config.Config) (*mcpclient.Manager, error) {
	return r.loadMCPWithHost(ctx, cfg, mcpHostCallbacks{})
}

func (r *runtime) loadMCPWithCurrentHost(ctx context.Context, cfg config.Config) (*mcpclient.Manager, error) {
	r.mcpHostMu.RLock()
	host := r.mcpHost
	r.mcpHostMu.RUnlock()
	return r.loadMCPWithHost(ctx, cfg, host)
}

func (r *runtime) loadMCPWithHost(ctx context.Context, cfg config.Config, host mcpHostCallbacks) (*mcpclient.Manager, error) {
	options, capabilityKey := r.setMCPHost(host)
	encoded, err := json.Marshal(struct {
		Workspace string                            `json:"workspace"`
		Servers   map[string]config.MCPServerConfig `json:"servers"`
		Host      string                            `json:"host"`
	}{Workspace: r.workspace, Servers: cfg.MCPServers, Host: capabilityKey})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	key := hex.EncodeToString(digest[:])
	r.mcpMu.Lock()
	defer r.mcpMu.Unlock()
	if r.mcp != nil && r.mcpKey == key {
		return r.mcp, nil
	}
	if err := r.closeMCPLocked(); err != nil {
		return nil, err
	}
	manager, diagnostics, err := mcpclient.OpenWithOptions(ctx, cfg.MCPServers, r.workspace, options)
	if err != nil {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "initialize MCP runtime: " + err.Error(), Cause: err}
	}
	r.mcp, r.mcpKey = manager, key
	r.startMCPWatcherLocked(manager)
	for _, diagnostic := range diagnostics {
		fmt.Fprintf(r.stderr, "[mcp] server=%s error=%s\n", r.redact(diagnostic.Server), r.redact(diagnostic.Message))
	}
	return manager, nil
}

func (r *runtime) closeMCP() error {
	r.mcpMu.Lock()
	defer r.mcpMu.Unlock()
	return r.closeMCPLocked()
}

func (r *runtime) closeMCPLocked() error {
	if r.mcp == nil {
		return nil
	}
	manager := r.mcp
	r.mcp, r.mcpKey = nil, ""
	stop, done := r.mcpWatchStop, r.mcpWatchDone
	r.mcpWatchStop, r.mcpWatchDone = nil, nil
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	r.storeMCPState(agent.MCPRuntimeState{})
	return manager.Close()
}

func (r *runtime) setMCPHost(host mcpHostCallbacks) (mcpclient.Options, string) {
	r.mcpHostMu.Lock()
	r.mcpHost = host
	r.mcpHostMu.Unlock()
	options := mcpclient.Options{ElicitationForm: host.elicitationForm, ElicitationURL: host.elicitationURL}
	capabilities := make([]string, 0, 3)
	if host.createMessageWithTools != nil {
		capabilities = append(capabilities, "sampling_tools")
		options.CreateMessageWithToolsHandler = func(ctx context.Context, request *sdkmcp.CreateMessageWithToolsRequest) (*sdkmcp.CreateMessageWithToolsResult, error) {
			r.mcpHostMu.RLock()
			handler := r.mcpHost.createMessageWithTools
			r.mcpHostMu.RUnlock()
			if handler == nil {
				return nil, errors.New("MCP sampling with tools is unavailable")
			}
			return handler(ctx, request)
		}
	} else if host.createMessage != nil {
		capabilities = append(capabilities, "sampling")
		options.CreateMessageHandler = func(ctx context.Context, request *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error) {
			r.mcpHostMu.RLock()
			handler := r.mcpHost.createMessage
			r.mcpHostMu.RUnlock()
			if handler == nil {
				return nil, errors.New("MCP sampling is unavailable")
			}
			return handler(ctx, request)
		}
	}
	if host.elicitation != nil {
		if host.elicitationForm {
			capabilities = append(capabilities, "elicitation_form")
		}
		if host.elicitationURL {
			capabilities = append(capabilities, "elicitation_url")
		}
		options.ElicitationHandler = func(ctx context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			r.mcpHostMu.RLock()
			handler := r.mcpHost.elicitation
			r.mcpHostMu.RUnlock()
			if handler == nil {
				return nil, errors.New("MCP elicitation is unavailable")
			}
			return handler(ctx, request)
		}
	}
	sort.Strings(capabilities)
	return options, strings.Join(capabilities, ",")
}

func (r *runtime) startMCPWatcherLocked(manager *mcpclient.Manager) {
	events, unsubscribe := manager.SubscribeEvents(64)
	var once sync.Once
	stop := func() { once.Do(unsubscribe) }
	done := make(chan struct{})
	r.mcpWatchStop, r.mcpWatchDone = stop, done
	go func() {
		defer close(done)
		for event := range events {
			r.storeMCPEvent(event)
			switch event.Kind {
			case mcpclient.EventCatalogChanged, mcpclient.EventResourceUpdate, mcpclient.EventStatus:
				r.storeMCPState(mcpStateFromManager(manager))
			case mcpclient.EventDiagnostic:
				fmt.Fprintf(r.stderr, "[mcp] server=%s diagnostic=%s\n", r.redact(event.Server), r.redact(event.Message))
			}
		}
	}()
}

func (r *runtime) storeMCPEvent(event mcpclient.Event) {
	r.mcpStateMu.Lock()
	r.mcpEvents = append(r.mcpEvents, event)
	if len(r.mcpEvents) > 128 {
		r.mcpEvents = append([]mcpclient.Event(nil), r.mcpEvents[len(r.mcpEvents)-128:]...)
	}
	r.mcpStateMu.Unlock()
}

func (r *runtime) mcpEventsForServer(server string) []mcpclient.Event {
	r.mcpStateMu.RLock()
	defer r.mcpStateMu.RUnlock()
	result := make([]mcpclient.Event, 0)
	for _, event := range r.mcpEvents {
		if event.Server == server {
			result = append(result, event)
		}
	}
	return result
}

func mcpStateFromManager(manager *mcpclient.Manager) agent.MCPRuntimeState {
	state := agent.MCPRuntimeState{Tools: manager.Tools(), ToolServers: make(map[string]string), Fingerprint: manager.Fingerprint()}
	for _, serverContext := range manager.Contexts() {
		state.Contexts = append(state.Contexts, agent.MCPContext{Server: serverContext.Server, Instructions: serverContext.Instructions, ResourceCatalog: serverContext.ResourceCatalog})
		for _, definition := range serverContext.ToolDefinitions {
			state.ToolServers[definition.Name] = serverContext.Server
		}
	}
	return state
}

func applyMCPState(modelRuntime *agent.Runtime, state agent.MCPRuntimeState) {
	modelRuntime.MCPContexts = append(modelRuntime.MCPContexts[:0], state.Contexts...)
	modelRuntime.MCPToolServers = make(map[string]string, len(state.ToolServers))
	for name, server := range state.ToolServers {
		modelRuntime.MCPToolServers[name] = server
	}
	modelRuntime.MCPFingerprint = state.Fingerprint
}

func (r *runtime) currentMCPState() agent.MCPRuntimeState {
	r.mcpStateMu.RLock()
	defer r.mcpStateMu.RUnlock()
	return cloneAppMCPState(r.mcpState)
}

func (r *runtime) storeMCPState(state agent.MCPRuntimeState) {
	state = cloneAppMCPState(state)
	r.mcpStateMu.Lock()
	r.mcpState = state
	conversations := make([]*agent.Conversation, 0, len(r.mcpConversations))
	for conversation := range r.mcpConversations {
		conversations = append(conversations, conversation)
	}
	r.mcpStateMu.Unlock()
	for _, conversation := range conversations {
		conversation.ApplyMCPRuntime(state)
	}
}

func cloneAppMCPState(state agent.MCPRuntimeState) agent.MCPRuntimeState {
	result := agent.MCPRuntimeState{Tools: append([]tool.Tool(nil), state.Tools...), Contexts: append([]agent.MCPContext(nil), state.Contexts...), ToolServers: make(map[string]string, len(state.ToolServers)), Fingerprint: state.Fingerprint}
	for name, server := range state.ToolServers {
		result.ToolServers[name] = server
	}
	return result
}

func (r *runtime) attachMCPConversation(conversation *agent.Conversation) func() {
	r.mcpStateMu.Lock()
	if r.mcpConversations == nil {
		r.mcpConversations = make(map[*agent.Conversation]int)
	}
	r.mcpConversations[conversation]++
	state := cloneAppMCPState(r.mcpState)
	r.mcpStateMu.Unlock()
	conversation.ApplyMCPRuntime(state)
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mcpStateMu.Lock()
			if r.mcpConversations[conversation] <= 1 {
				delete(r.mcpConversations, conversation)
			} else {
				r.mcpConversations[conversation]--
			}
			r.mcpStateMu.Unlock()
		})
	}
}

func buildMCPHostCallbacks(modelRuntime agent.Runtime, confirm tool.ConfirmFunc, ask tool.AskFunc, openURL func(string) error) mcpHostCallbacks {
	host := mcpHostCallbacks{}
	if modelRuntime.Driver != nil && confirm != nil {
		if modelRuntime.Driver.Capabilities().ToolCalling {
			host.createMessageWithTools = mcpCreateMessageWithToolsHandler(modelRuntime, confirm)
		} else {
			host.createMessage = mcpCreateMessageHandler(modelRuntime, confirm)
		}
	}
	if ask != nil || (confirm != nil && openURL != nil) {
		host.elicitationForm = ask != nil
		host.elicitationURL = confirm != nil && openURL != nil
		host.elicitation = mcpElicitationHandler(ask, confirm, openURL)
	}
	return host
}

func mcpCreateMessageHandler(modelRuntime agent.Runtime, confirm tool.ConfirmFunc) func(context.Context, *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error) {
	return func(ctx context.Context, request *sdkmcp.CreateMessageRequest) (*sdkmcp.CreateMessageResult, error) {
		turns, err := basicSamplingTurns(request.Params.SystemPrompt, request.Params.Messages)
		if err != nil {
			return nil, err
		}
		response, err := runMCPSampling(ctx, modelRuntime, confirm, turns, nil)
		if err != nil {
			return nil, err
		}
		content, err := basicSamplingContent(response)
		if err != nil {
			return nil, err
		}
		return &sdkmcp.CreateMessageResult{Role: sdkmcp.Role("assistant"), Model: modelRuntime.Provider.Config.Model, Content: content, StopReason: mcpStopReason(response.Stop)}, nil
	}
}

func mcpCreateMessageWithToolsHandler(modelRuntime agent.Runtime, confirm tool.ConfirmFunc) func(context.Context, *sdkmcp.CreateMessageWithToolsRequest) (*sdkmcp.CreateMessageWithToolsResult, error) {
	return func(ctx context.Context, request *sdkmcp.CreateMessageWithToolsRequest) (*sdkmcp.CreateMessageWithToolsResult, error) {
		turns, err := toolSamplingTurns(request.Params.SystemPrompt, request.Params.Messages)
		if err != nil {
			return nil, err
		}
		definitions := make([]protocol.ToolDefinition, 0, len(request.Params.Tools))
		for _, item := range request.Params.Tools {
			encoded, marshalErr := json.Marshal(item.InputSchema)
			if marshalErr != nil {
				return nil, marshalErr
			}
			definitions = append(definitions, protocol.ToolDefinition{Name: item.Name, Description: item.Description, InputSchema: encoded})
		}
		response, err := runMCPSampling(ctx, modelRuntime, confirm, turns, definitions)
		if err != nil {
			return nil, err
		}
		content, err := toolSamplingContent(response)
		if err != nil {
			return nil, err
		}
		return &sdkmcp.CreateMessageWithToolsResult{Role: sdkmcp.Role("assistant"), Model: modelRuntime.Provider.Config.Model, Content: content, StopReason: mcpStopReason(response.Stop)}, nil
	}
}

func runMCPSampling(ctx context.Context, modelRuntime agent.Runtime, confirm tool.ConfirmFunc, turns []protocol.Turn, definitions []protocol.ToolDefinition) (protocol.ModelResponse, error) {
	preview, _ := json.Marshal(map[string]any{"reason": "An MCP server requested model sampling.", "turns": turns, "tools": definitions})
	approved, err := confirmMCPHost(ctx, confirm, "mcp_sampling", preview)
	if err != nil || !approved {
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		return protocol.ModelResponse{}, errors.New("MCP sampling was declined")
	}
	response, err := modelRuntime.Driver.Generate(ctx, driverRequestForSampling(modelRuntime, turns, definitions), nil)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	responsePreview, _ := json.Marshal(map[string]any{"reason": "Share the sampled response with the MCP server.", "response": response.Turn})
	approved, err = confirmMCPHost(ctx, confirm, "mcp_sampling_response", responsePreview)
	if err != nil || !approved {
		if err != nil {
			return protocol.ModelResponse{}, err
		}
		return protocol.ModelResponse{}, errors.New("sharing the MCP sampling response was declined")
	}
	return response, nil
}

func driverRequestForSampling(modelRuntime agent.Runtime, turns []protocol.Turn, definitions []protocol.ToolDefinition) driver.Request {
	return driver.Request{
		BaseURL: modelRuntime.Provider.Config.BaseURL, APIKey: modelRuntime.APIKey, Headers: modelRuntime.Provider.Config.Headers,
		ReasoningEffort: modelRuntime.Provider.Config.ReasoningEffort,
		Model:           protocol.ModelRequest{ProtocolVersion: protocol.Version, Model: modelRuntime.Provider.Config.Model, Turns: turns, Tools: definitions},
	}
}

func basicSamplingTurns(systemPrompt string, messages []*sdkmcp.SamplingMessage) ([]protocol.Turn, error) {
	turns := samplingSystemTurn(systemPrompt)
	for index, message := range messages {
		part, err := samplingContentPart(message.Content)
		if err != nil {
			return nil, fmt.Errorf("sampling message %d: %w", index, err)
		}
		turns = append(turns, protocol.Turn{Role: samplingRole(message.Role), Parts: []protocol.Part{part}})
	}
	return turns, nil
}

func toolSamplingTurns(systemPrompt string, messages []*sdkmcp.SamplingMessageV2) ([]protocol.Turn, error) {
	turns := samplingSystemTurn(systemPrompt)
	for index, message := range messages {
		turn := protocol.Turn{Role: samplingRole(message.Role)}
		for blockIndex, content := range message.Content {
			part, err := samplingContentPart(content)
			if err != nil {
				return nil, fmt.Errorf("sampling message %d content %d: %w", index, blockIndex, err)
			}
			turn.Parts = append(turn.Parts, part)
		}
		turns = append(turns, turn)
	}
	return turns, nil
}

func samplingSystemTurn(systemPrompt string) []protocol.Turn {
	if strings.TrimSpace(systemPrompt) == "" {
		return nil
	}
	return []protocol.Turn{{Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: systemPrompt}}}}
}

func samplingRole(role sdkmcp.Role) protocol.Role {
	if strings.EqualFold(string(role), "assistant") {
		return protocol.RoleAgent
	}
	return protocol.RoleUser
}

func samplingContentPart(content sdkmcp.Content) (protocol.Part, error) {
	switch value := content.(type) {
	case *sdkmcp.TextContent:
		return protocol.Part{Kind: protocol.PartText, Text: value.Text}, nil
	case *sdkmcp.ToolUseContent:
		arguments, err := json.Marshal(value.Input)
		if err != nil {
			return protocol.Part{}, err
		}
		call := protocol.ToolCall{ID: value.ID, Name: value.Name, Arguments: arguments}
		return protocol.Part{Kind: protocol.PartToolCall, ToolCall: &call}, nil
	case *sdkmcp.ToolResultContent:
		texts := make([]string, 0, len(value.Content))
		blocks := make([]protocol.ContentBlock, 0, len(value.Content))
		for _, item := range value.Content {
			text, ok := item.(*sdkmcp.TextContent)
			if !ok {
				return protocol.Part{}, fmt.Errorf("unsupported MCP sampling tool result content %T", item)
			}
			texts = append(texts, text.Text)
			blocks = append(blocks, protocol.ContentBlock{Type: protocol.ContentText, Text: text.Text})
		}
		var structured json.RawMessage
		if value.StructuredContent != nil {
			encoded, err := json.Marshal(value.StructuredContent)
			if err != nil {
				return protocol.Part{}, err
			}
			structured = encoded
		}
		result := protocol.ToolResult{CallID: value.ToolUseID, Content: strings.Join(texts, "\n"), ContentBlocks: blocks, StructuredContent: structured, IsError: value.IsError}
		return protocol.Part{Kind: protocol.PartToolResult, ToolResult: &result}, nil
	default:
		return protocol.Part{}, fmt.Errorf("unsupported MCP sampling content %T", content)
	}
}

func basicSamplingContent(response protocol.ModelResponse) (sdkmcp.Content, error) {
	var text strings.Builder
	for _, part := range response.Turn.Parts {
		if part.Kind == protocol.PartText {
			text.WriteString(part.Text)
		}
	}
	if text.Len() == 0 {
		return nil, errors.New("sampled model response contained no text")
	}
	return &sdkmcp.TextContent{Text: text.String()}, nil
}

func toolSamplingContent(response protocol.ModelResponse) ([]sdkmcp.Content, error) {
	content := make([]sdkmcp.Content, 0, len(response.Turn.Parts))
	for _, part := range response.Turn.Parts {
		switch {
		case part.Kind == protocol.PartText:
			content = append(content, &sdkmcp.TextContent{Text: part.Text})
		case part.Kind == protocol.PartToolCall && part.ToolCall != nil:
			var input map[string]any
			if err := json.Unmarshal(part.ToolCall.Arguments, &input); err != nil {
				return nil, err
			}
			content = append(content, &sdkmcp.ToolUseContent{ID: part.ToolCall.ID, Name: part.ToolCall.Name, Input: input})
		}
	}
	if len(content) == 0 {
		return nil, errors.New("sampled model response contained no supported content")
	}
	return content, nil
}

func mcpStopReason(stop protocol.StopKind) string {
	switch stop {
	case protocol.StopToolUse:
		return "toolUse"
	case protocol.StopLength:
		return "maxTokens"
	default:
		return "endTurn"
	}
}

func confirmMCPHost(ctx context.Context, confirm tool.ConfirmFunc, name string, input json.RawMessage) (bool, error) {
	if confirm == nil {
		return false, nil
	}
	confirmation, err := confirm(ctx, policy.Request{Tool: name, Input: input, Risk: policy.RiskSession, ConfirmationStep: 1, ConfirmationTotal: 1}, policy.Outcome{
		Decision: policy.DecisionConfirm, Risk: policy.RiskSession, Reason: "MCP host interaction requires user consent", Confirmations: 1,
	})
	return confirmation.Approved, err
}

func mcpElicitationHandler(ask tool.AskFunc, confirm tool.ConfirmFunc, openURL func(string) error) func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
	return func(ctx context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		mode := strings.ToLower(strings.TrimSpace(request.Params.Mode))
		if mode == "" || mode == "form" {
			if ask == nil {
				return nil, errors.New("MCP form elicitation is unavailable")
			}
			questions, schemas, err := elicitationQuestions(request.Params.Message, request.Params.RequestedSchema)
			if err != nil {
				return nil, err
			}
			response, err := ask(ctx, protocol.AskRequest{Questions: questions})
			if errors.Is(err, tool.ErrAskDismissed) {
				return &sdkmcp.ElicitResult{Action: "cancel"}, nil
			}
			if err != nil {
				return nil, err
			}
			content, err := elicitationContent(response.Answers, schemas)
			if err != nil {
				return nil, err
			}
			return &sdkmcp.ElicitResult{Action: "accept", Content: content}, nil
		}
		if mode != "url" {
			return nil, fmt.Errorf("unsupported MCP elicitation mode %q", request.Params.Mode)
		}
		if confirm == nil || openURL == nil {
			return nil, errors.New("MCP URL elicitation is unavailable")
		}
		parsed, err := url.Parse(request.Params.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, errors.New("MCP elicitation URL must be an absolute HTTP(S) URL")
		}
		preview, _ := json.Marshal(map[string]any{"reason": request.Params.Message, "url": request.Params.URL, "host": parsed.Host})
		approved, err := confirmMCPHost(ctx, confirm, "mcp_url_elicitation", preview)
		if err != nil {
			return nil, err
		}
		if !approved {
			return &sdkmcp.ElicitResult{Action: "decline"}, nil
		}
		if err := openURL(request.Params.URL); err != nil {
			return nil, fmt.Errorf("open MCP elicitation URL: %w", err)
		}
		return &sdkmcp.ElicitResult{Action: "accept"}, nil
	}
}

type elicitationProperty struct {
	Type     string
	Required bool
	Enum     []any
	Title    string
	Default  any
}

func elicitationQuestions(message string, requestedSchema any) ([]protocol.AskQuestion, map[string]elicitationProperty, error) {
	encoded, err := json.Marshal(requestedSchema)
	if err != nil {
		return nil, nil, err
	}
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return nil, nil, fmt.Errorf("decode MCP elicitation schema: %w", err)
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	questions := make([]protocol.AskQuestion, 0, len(names))
	properties := make(map[string]elicitationProperty, len(names))
	for _, name := range names {
		value := schema.Properties[name]
		property := elicitationProperty{Type: stringValue(value["type"]), Required: required[name], Title: stringValue(value["title"]), Default: value["default"]}
		if enum, ok := value["enum"].([]any); ok {
			property.Enum = enum
		}
		if property.Title == "" {
			property.Title = name
		}
		questionText := stringValue(value["description"])
		if questionText == "" {
			questionText = message
		}
		question := protocol.AskQuestion{ID: name, Header: property.Title, Question: questionText}
		for _, item := range property.Enum {
			question.Options = append(question.Options, protocol.AskOption{Label: fmt.Sprint(item), Description: "MCP option"})
		}
		if property.Type == "boolean" && len(question.Options) == 0 {
			question.Options = []protocol.AskOption{{Label: "true", Description: "Yes"}, {Label: "false", Description: "No"}}
		}
		if !property.Required {
			question.Options = append(question.Options, protocol.AskOption{Label: "Skip", Description: "Leave this field unset"})
		}
		questions = append(questions, question)
		properties[name] = property
	}
	return questions, properties, nil
}

func elicitationContent(answers map[string][]string, properties map[string]elicitationProperty) (map[string]any, error) {
	content := make(map[string]any)
	for name, property := range properties {
		values := answers[name]
		if len(values) == 0 || (!property.Required && strings.EqualFold(values[0], "skip")) {
			if property.Default != nil {
				content[name] = property.Default
			}
			continue
		}
		value := values[0]
		switch property.Type {
		case "boolean":
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("MCP elicitation field %s: %w", name, err)
			}
			content[name] = parsed
		case "integer":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("MCP elicitation field %s: %w", name, err)
			}
			content[name] = parsed
		case "number":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, fmt.Errorf("MCP elicitation field %s: %w", name, err)
			}
			content[name] = parsed
		default:
			content[name] = value
		}
	}
	return content, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func openExternalURL(target string) error {
	var command *exec.Cmd
	switch gostdruntime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		command = exec.Command("open", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}

type mcpCommandBackend interface {
	Servers() ([]mcpclient.ServerInfo, string)
	Inspect(string) (mcpclient.ServerDetail, error)
	Reconnect(context.Context, string) error
	Tools(string) ([]mcpclient.ToolInfo, error)
	Tool(string, string) (mcpclient.ToolInfo, error)
	Login(context.Context, string) error
	Logout(context.Context, string) error
	Resources(string) ([]mcpclient.ResourceInfo, error)
	Resource(context.Context, string, string) (any, error)
	Prompts(string) ([]mcpclient.PromptInfo, error)
	Prompt(context.Context, string, string, map[string]string) (any, error)
	Complete(context.Context, string, *sdkmcp.CompleteParams) (any, error)
	SubscribeResource(context.Context, string, string) error
	UnsubscribeResource(context.Context, string, string) error
	SubscribeEvents(int) (<-chan mcpclient.Event, func())
}

type mcpBackendLoader func(context.Context) (mcpCommandBackend, error)
type mcpToggleServerFunc func(context.Context, string, bool) error

type managerMCPCommandBackend struct {
	manager *mcpclient.Manager
}

func (b *managerMCPCommandBackend) Servers() ([]mcpclient.ServerInfo, string) {
	return b.manager.List(), b.manager.Fingerprint()
}

func (b *managerMCPCommandBackend) Inspect(name string) (mcpclient.ServerDetail, error) {
	return b.manager.Inspect(name)
}

func (b *managerMCPCommandBackend) Reconnect(ctx context.Context, name string) error {
	return b.manager.Reconnect(ctx, name)
}

func (b *managerMCPCommandBackend) Tools(name string) ([]mcpclient.ToolInfo, error) {
	return b.manager.ServerTools(name)
}

func (b *managerMCPCommandBackend) Tool(server, name string) (mcpclient.ToolInfo, error) {
	return b.manager.Tool(server, name)
}

func (b *managerMCPCommandBackend) Login(ctx context.Context, name string) error {
	return b.manager.Login(ctx, name)
}

func (b *managerMCPCommandBackend) Logout(ctx context.Context, name string) error {
	return b.manager.Logout(ctx, name)
}

func (b *managerMCPCommandBackend) Resources(name string) ([]mcpclient.ResourceInfo, error) {
	return b.manager.Resources(name)
}

func (b *managerMCPCommandBackend) Resource(ctx context.Context, server, uri string) (any, error) {
	return b.manager.ReadResource(ctx, server, uri)
}

func (b *managerMCPCommandBackend) Prompts(name string) ([]mcpclient.PromptInfo, error) {
	return b.manager.Prompts(name)
}

func (b *managerMCPCommandBackend) Prompt(ctx context.Context, server, name string, arguments map[string]string) (any, error) {
	return b.manager.GetPrompt(ctx, server, name, arguments)
}

func (b *managerMCPCommandBackend) Complete(ctx context.Context, server string, params *sdkmcp.CompleteParams) (any, error) {
	return b.manager.Complete(ctx, server, params)
}

func (b *managerMCPCommandBackend) SubscribeResource(ctx context.Context, server, uri string) error {
	return b.manager.SubscribeResource(ctx, server, uri)
}

func (b *managerMCPCommandBackend) UnsubscribeResource(ctx context.Context, server, uri string) error {
	return b.manager.UnsubscribeResource(ctx, server, uri)
}

func (b *managerMCPCommandBackend) SubscribeEvents(buffer int) (<-chan mcpclient.Event, func()) {
	return b.manager.SubscribeEvents(buffer)
}

func mcpServerNotFound(name string) error {
	return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("configured MCP server %q was not found", name)}
}

func mcpManagerCapabilityUnavailable(capability string) error {
	return &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("the MCP runtime does not expose %s yet", capability)}
}

func (r *runtime) mcpCommand(ctx context.Context) *cobra.Command {
	loader := func(loadContext context.Context) (mcpCommandBackend, error) {
		loaded, _, err := r.loadManager()
		if err != nil {
			return nil, err
		}
		manager, err := r.loadMCP(loadContext, loaded.Config)
		if err != nil {
			return nil, err
		}
		return &managerMCPCommandBackend{manager: manager}, nil
	}
	toggle := func(toggleContext context.Context, name string, enabled bool) error {
		loaded, _, err := r.loadManager()
		if err != nil {
			return err
		}
		if _, ok := loaded.Config.MCPServers[name]; !ok {
			return mcpServerNotFound(name)
		}
		updated, err := loaded.Store.SetMCPServerEnabled(name, enabled)
		if err != nil {
			return err
		}
		if err := r.closeMCP(); err != nil {
			return err
		}
		if enabled {
			_, err = r.loadMCP(toggleContext, updated)
		}
		return err
	}
	return r.mcpCommandWithBackend(ctx, loader, toggle)
}

func (r *runtime) mcpCommandWithBackend(ctx context.Context, load mcpBackendLoader, toggle mcpToggleServerFunc) *cobra.Command {
	command := &cobra.Command{Use: "mcp", Short: "inspect and manage MCP servers"}
	backend := func() (mcpCommandBackend, error) {
		if load == nil {
			return nil, mcpManagerCapabilityUnavailable("command backend")
		}
		return load(ctx)
	}
	command.AddCommand(&cobra.Command{Use: "list", Short: "list configured MCP servers", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		servers, fingerprint := service.Servers()
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"servers": servers, "fingerprint": fingerprint})
		}
		if len(servers) == 0 {
			fmt.Fprintln(r.stdout, "No configured MCP servers.")
			return nil
		}
		for _, server := range servers {
			fmt.Fprintf(r.stdout, "%s\tstatus=%s\ttransport=%s\tprotocol=%s\ttools=%d\tresources=%d\tprompts=%d\n", r.redact(server.Name), server.Status, server.Transport, server.ProtocolVersion, server.Tools, server.Resources, server.Prompts)
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "inspect <name>", Short: "show MCP server details", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		detail, err := service.Inspect(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(detail)
		}
		return r.writeMCPTextValue(detail)
	}})
	command.AddCommand(&cobra.Command{Use: "diagnostics <name>", Short: "show MCP server diagnostics", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		detail, err := service.Inspect(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "diagnostics": detail.Diagnostics})
		}
		return r.writeMCPTextValue(detail.Diagnostics)
	}})
	command.AddCommand(mcpActionCommand("reconnect", "reconnect an MCP server", backend, func(service mcpCommandBackend, name string) error { return service.Reconnect(ctx, name) }, r))
	command.AddCommand(r.mcpToggleCommand(ctx, "enable", true, toggle), r.mcpToggleCommand(ctx, "disable", false, toggle))
	command.AddCommand(&cobra.Command{Use: "tools <server>", Short: "list MCP server tools", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Tools(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "tools": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\n", r.redact(item.Name), r.redact(item.LocalName), r.redact(item.Description))
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "tool <server> <name>", Short: "show an MCP tool", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		item, err := service.Tool(args[0], args[1])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "tool": item})
		}
		return r.writeMCPTextValue(item)
	}})
	command.AddCommand(mcpActionCommand("login", "authenticate an MCP server", backend, func(service mcpCommandBackend, name string) error { return service.Login(ctx, name) }, r))
	command.AddCommand(mcpActionCommand("logout", "clear MCP server authentication", backend, func(service mcpCommandBackend, name string) error { return service.Logout(ctx, name) }, r))
	command.AddCommand(&cobra.Command{Use: "resources <server>", Short: "list MCP server resources", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Resources(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "resources": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\t%s\n", r.redact(item.URI), r.redact(item.Name), r.redact(item.MIMEType))
		}
		return nil
	}})
	command.AddCommand(&cobra.Command{Use: "resource <server> <uri>", Short: "read an MCP resource", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		item, err := service.Resource(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "resource": item})
		}
		return r.writeMCPTextValue(item)
	}})
	command.AddCommand(&cobra.Command{Use: "subscribe <server> <uri>", Short: "subscribe to MCP resource updates", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		if err := service.SubscribeResource(ctx, args[0], args[1]); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], "subscribe "+args[1])
	}})
	command.AddCommand(&cobra.Command{Use: "unsubscribe <server> <uri>", Short: "unsubscribe from MCP resource updates", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		if err := service.UnsubscribeResource(ctx, args[0], args[1]); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], "unsubscribe "+args[1])
	}})
	command.AddCommand(&cobra.Command{Use: "prompts <server>", Short: "list MCP server prompts", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		items, err := service.Prompts(args[0])
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "prompts": items})
		}
		for _, item := range items {
			fmt.Fprintf(r.stdout, "%s\t%s\n", r.redact(item.Name), r.redact(item.Description))
		}
		return nil
	}})
	var promptArguments string
	prompt := &cobra.Command{Use: "prompt <server> <name> [name=value...]", Short: "get an MCP prompt", Args: cobra.MinimumNArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		arguments := make(map[string]string)
		if err := json.Unmarshal([]byte(promptArguments), &arguments); err != nil {
			return &protocol.Error{Code: protocol.ErrConfig, Message: "arguments must be a JSON object with string values", Cause: err}
		}
		for _, raw := range args[2:] {
			name, value, ok := strings.Cut(raw, "=")
			if !ok || strings.TrimSpace(name) == "" {
				return &protocol.Error{Code: protocol.ErrConfig, Message: "prompt arguments must use name=value"}
			}
			arguments[name] = value
		}
		service, err := backend()
		if err != nil {
			return err
		}
		result, err := service.Prompt(ctx, args[0], args[1], arguments)
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "prompt": result})
		}
		return r.writeMCPTextValue(result)
	}}
	prompt.ValidArgsFunction = func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		service, err := backend()
		if err != nil {
			return nil, cobra.ShellCompDirectiveError
		}
		if len(args) == 0 {
			servers, _ := service.Servers()
			values := make([]string, 0, len(servers))
			for _, server := range servers {
				if strings.HasPrefix(server.Name, toComplete) {
					values = append(values, server.Name)
				}
			}
			return values, cobra.ShellCompDirectiveNoFileComp
		}
		if len(args) == 1 {
			prompts, listErr := service.Prompts(args[0])
			if listErr != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			values := make([]string, 0, len(prompts))
			for _, item := range prompts {
				if strings.HasPrefix(item.Name, toComplete) {
					values = append(values, item.Name)
				}
			}
			return values, cobra.ShellCompDirectiveNoFileComp
		}
		if len(args) == 2 {
			prompts, listErr := service.Prompts(args[0])
			if listErr != nil {
				return nil, cobra.ShellCompDirectiveError
			}
			for _, item := range prompts {
				if item.Name != args[1] {
					continue
				}
				values := make([]string, 0, len(item.Arguments))
				for _, argument := range item.Arguments {
					candidate := argument.Name + "="
					if strings.HasPrefix(candidate, toComplete) {
						values = append(values, candidate)
					}
				}
				return values, cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveNoFileComp
			}
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	prompt.Flags().StringVar(&promptArguments, "arguments", "{}", "prompt arguments as a JSON object with string values")
	command.AddCommand(prompt)
	var completionContext string
	complete := &cobra.Command{Use: "complete <server> <prompt|resource> <name-or-uri> <argument> <value>", Short: "complete an MCP prompt argument or resource URI", Args: cobra.ExactArgs(5), RunE: func(_ *cobra.Command, args []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		ref := &sdkmcp.CompleteReference{}
		switch args[1] {
		case "prompt":
			ref.Type, ref.Name = "ref/prompt", args[2]
		case "resource":
			ref.Type, ref.URI = "ref/resource", args[2]
		default:
			return &protocol.Error{Code: protocol.ErrConfig, Message: "completion reference must be prompt or resource"}
		}
		params := &sdkmcp.CompleteParams{Ref: ref, Argument: sdkmcp.CompleteParamsArgument{Name: args[3], Value: args[4]}}
		if completionContext != "" {
			var arguments map[string]string
			if err := json.Unmarshal([]byte(completionContext), &arguments); err != nil {
				return &protocol.Error{Code: protocol.ErrConfig, Message: "completion context must be a JSON object with string values", Cause: err}
			}
			params.Context = &sdkmcp.CompleteContext{Arguments: arguments}
		}
		result, err := service.Complete(ctx, args[0], params)
		if err != nil {
			return err
		}
		if r.output != "text" {
			return r.writeMCPJSON(map[string]any{"server": args[0], "completion": result})
		}
		return r.writeMCPTextValue(result)
	}}
	complete.Flags().StringVar(&completionContext, "context", "", "previously resolved arguments as JSON")
	command.AddCommand(complete)
	var eventWait time.Duration
	eventsCommand := &cobra.Command{Use: "events", Short: "observe live MCP events", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		service, err := backend()
		if err != nil {
			return err
		}
		events, unsubscribe := service.SubscribeEvents(64)
		defer unsubscribe()
		timer := time.NewTimer(eventWait)
		defer timer.Stop()
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return nil
				}
				if r.output != "text" {
					if err := r.writeMCPJSON(event); err != nil {
						return err
					}
				} else if err := r.writeMCPTextValue(event); err != nil {
					return err
				}
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}}
	eventsCommand.Flags().DurationVar(&eventWait, "wait", time.Second, "event observation duration")
	command.AddCommand(eventsCommand)
	return command
}

func mcpActionCommand(verb, short string, load func() (mcpCommandBackend, error), action func(mcpCommandBackend, string) error, r *runtime) *cobra.Command {
	return &cobra.Command{Use: verb + " <server>", Short: short, Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		service, err := load()
		if err != nil {
			return err
		}
		if err := action(service, args[0]); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], verb)
	}}
}

func (r *runtime) mcpToggleCommand(ctx context.Context, verb string, enabled bool, toggle mcpToggleServerFunc) *cobra.Command {
	return &cobra.Command{Use: verb + " <server>", Short: verb + " an MCP server", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if toggle == nil {
			return mcpManagerCapabilityUnavailable("server enable/disable persistence")
		}
		if err := toggle(ctx, args[0], enabled); err != nil {
			return err
		}
		return r.writeMCPAction(args[0], verb)
	}}
}

func (r *runtime) writeMCPAction(server, action string) error {
	if r.output != "text" {
		return r.writeMCPJSON(map[string]any{"server": server, "action": action})
	}
	_, err := fmt.Fprintf(r.stdout, "%s\t%s\n", r.redact(server), action)
	return err
}

func (r *runtime) writeMCPJSON(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	return json.NewEncoder(r.stdout).Encode(r.redactMCPJSONValue(document))
}

func (r *runtime) redactMCPJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return r.redact(typed)
	case []any:
		for index := range typed {
			typed[index] = r.redactMCPJSONValue(typed[index])
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[r.redact(key)] = r.redactMCPJSONValue(item)
		}
		return redacted
	default:
		return value
	}
}

func (r *runtime) writeMCPTextValue(value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(r.stdout, r.redact(string(encoded)))
	return err
}
