package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	contextledger "Eylu/internal/context"
	"Eylu/internal/driver"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

const SystemPrompt = `You are Eylu, a terminal programming agent working in a local repository. Follow the user's request, preserve unrelated files, report failures accurately, and keep responses concise. Tool availability and local permission policy are authoritative.`

type Runtime struct {
	Provider       provider.Snapshot
	APIKey         string
	Driver         driver.ModelDriver
	Timeout        time.Duration
	PermissionMode string
}

type Conversation struct {
	mu                 sync.Mutex
	sessionID          string
	turns              []protocol.Turn
	closed             map[string][]protocol.Turn
	driverState        json.RawMessage
	providerName       string
	providerGeneration uint64
	providerAdapter    string
	providerBaseURL    string
	providerModel      string
	permissionMode     string
	systemPrompt       string
	toolDefinitions    []protocol.ToolDefinition
	ledger             *contextledger.Ledger
	lastRuntime        Runtime
}

func NewConversation() *Conversation {
	conversation := &Conversation{sessionID: uuid.NewString(), closed: make(map[string][]protocol.Turn), ledger: contextledger.New(nil), permissionMode: "manual"}
	conversation.systemPrompt = promptForMode("manual")
	conversation.rebuildLedger(Runtime{})
	return conversation
}

func (c *Conversation) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Conversation) Transcript() []protocol.Turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneTurns(c.turns)
}

func (c *Conversation) ClosedTranscript(sessionID string) ([]protocol.Turn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turns, ok := c.closed[sessionID]
	return cloneTurns(turns), ok
}

func (c *Conversation) NewSession() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.sessionID
	c.closed[old] = cloneTurns(c.turns)
	c.sessionID = uuid.NewString()
	c.turns = nil
	c.driverState = nil
	c.providerName = ""
	c.providerGeneration = 0
	c.providerAdapter = ""
	c.providerBaseURL = ""
	c.providerModel = ""
	c.permissionMode = c.lastRuntime.PermissionMode
	if c.permissionMode == "" {
		c.permissionMode = "manual"
	}
	c.systemPrompt = promptForMode(c.permissionMode)
	c.rebuildLedger(c.lastRuntime)
	return old
}

func (c *Conversation) Send(ctx context.Context, prompt string, runtime Runtime, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareRuntime(prompt, runtime); err != nil {
		return protocol.ModelResponse{}, err
	}
	c.appendUser(prompt)
	c.toolDefinitions = nil
	return c.generate(ctx, runtime, nil, stream, emit)
}

func (c *Conversation) prepareRuntime(prompt string, runtime Runtime) error {
	if prompt == "" {
		return errors.New("prompt is empty")
	}
	if runtime.Driver == nil {
		return errors.New("model driver is nil")
	}
	mode := runtime.PermissionMode
	if mode == "" {
		mode = "manual"
	}
	if c.providerName != runtime.Provider.Name || c.providerGeneration != runtime.Provider.Generation || c.providerAdapter != runtime.Provider.Config.Adapter || c.providerBaseURL != runtime.Provider.Config.BaseURL || c.providerModel != runtime.Provider.Config.Model || c.permissionMode != mode {
		c.driverState = nil
		c.providerName = runtime.Provider.Name
		c.providerGeneration = runtime.Provider.Generation
		c.providerAdapter = runtime.Provider.Config.Adapter
		c.providerBaseURL = runtime.Provider.Config.BaseURL
		c.providerModel = runtime.Provider.Config.Model
		c.permissionMode = mode
		c.systemPrompt = promptForMode(mode)
	}
	return nil
}

func (c *Conversation) appendUser(prompt string) {
	userTurn := protocol.Turn{
		ID: uuid.NewString(), Role: protocol.RoleUser, CreatedAt: time.Now().UTC(),
		Parts: []protocol.Part{{Kind: protocol.PartText, Text: prompt}},
	}
	c.turns = append(c.turns, userTurn)
}

func (c *Conversation) generate(ctx context.Context, runtime Runtime, definitions []protocol.ToolDefinition, stream bool, emit driver.EmitFunc) (protocol.ModelResponse, error) {
	requestTurns := make([]protocol.Turn, 0, len(c.turns)+1)
	requestTurns = append(requestTurns, protocol.Turn{ID: "system", Role: protocol.RoleSystem, Parts: []protocol.Part{{Kind: protocol.PartText, Text: c.systemPrompt}}})
	requestTurns = append(requestTurns, cloneTurns(c.turns)...)
	request := driver.Request{
		BaseURL: runtime.Provider.Config.BaseURL,
		APIKey:  runtime.APIKey,
		Headers: runtime.Provider.Config.Headers,
		Stream:  stream,
		Model: protocol.ModelRequest{
			ProtocolVersion: protocol.Version,
			Model:           runtime.Provider.Config.Model,
			Turns:           requestTurns,
			Tools:           definitions,
			DriverState:     append(json.RawMessage(nil), c.driverState...),
		},
	}
	response, err := runtime.Driver.Generate(ctx, request, emit)
	if err != nil {
		c.lastRuntime = runtime
		c.rebuildLedger(runtime)
		return protocol.ModelResponse{}, err
	}
	if len(response.Turn.Parts) == 0 {
		return protocol.ModelResponse{}, &protocol.Error{Code: protocol.ErrProtocol, Message: "model returned an empty turn"}
	}
	c.turns = append(c.turns, response.Turn)
	c.driverState = append(c.driverState[:0], response.DriverState...)
	c.lastRuntime = runtime
	c.rebuildLedger(runtime)
	c.ledger.SetLastUsage(response.Usage)
	return response, nil
}

func (c *Conversation) ContextReport() contextledger.Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ledger.Report(c.lastRuntime.Provider.Name, c.lastRuntime.Provider.Config.Model, c.lastRuntime.Provider.Config.ContextWindow)
}

func (c *Conversation) rebuildLedger(runtime Runtime) {
	c.ledger.Reset()
	c.ledger.AddText("system", contextledger.CategorySystemPrompt, "eylu", c.systemPrompt, true)
	for _, turn := range c.turns {
		for index, part := range turn.Parts {
			source := turn.ID
			id := turn.ID + ":" + strconv.Itoa(index)
			switch {
			case part.Kind == protocol.PartText && turn.Role == protocol.RoleUser:
				c.ledger.AddText(id, contextledger.CategoryUserMessage, source, part.Text, false)
			case part.Kind == protocol.PartText && turn.Role == protocol.RoleAgent:
				c.ledger.AddText(id, contextledger.CategoryAgentMessage, source, part.Text, false)
			case part.Kind == protocol.PartToolResult && part.ToolResult != nil:
				c.ledger.AddText(id, contextledger.CategoryBuiltinToolResult, source, part.ToolResult.Content, false)
			}
		}
	}
	for _, definition := range c.toolDefinitions {
		content := definition.Description + "\n" + string(definition.InputSchema)
		c.ledger.AddText("tool-schema:"+definition.Name, contextledger.CategoryBuiltinToolSchema, definition.Name, content, true)
	}
	if len(c.driverState) > 0 {
		c.ledger.AddText("driver-state", contextledger.CategoryDriverState, runtime.Provider.Name, string(c.driverState), false)
	}
	reserve := 8192
	if runtime.Provider.Config.ContextWindow > 0 && runtime.Provider.Config.ContextWindow < reserve*2 {
		reserve = runtime.Provider.Config.ContextWindow / 4
	}
	c.ledger.Add(contextledger.Block{ID: "output-reserve", Category: contextledger.CategoryOutputReserve, Source: "runtime", Tokens: reserve, Exact: false})
}

func promptForMode(mode string) string {
	base := SystemPrompt + "\nCurrent permission mode: " + mode + ". Local policy decisions are final."
	switch mode {
	case "plan":
		return base + " You may read, search, list files, and run commands classified as read-only. Finish with a concrete modification plan, file list, risks, and validation commands."
	case "auto":
		return base + " Workspace edits run automatically. Allowlisted commands run automatically; other commands request confirmation."
	case "full":
		return base + " Ordinary workspace tools and commands run automatically. Dangerous operations always request a prominent confirmation."
	default:
		return base + " Reads run automatically. Writes and commands request confirmation; dangerous operations require two confirmations."
	}
}

func cloneTurns(turns []protocol.Turn) []protocol.Turn {
	result := make([]protocol.Turn, len(turns))
	for index, turn := range turns {
		result[index] = turn
		result[index].Parts = append([]protocol.Part(nil), turn.Parts...)
	}
	return result
}
