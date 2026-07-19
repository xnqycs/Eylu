package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
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
	Provider              provider.Snapshot
	APIKey                string
	Driver                driver.ModelDriver
	Timeout               time.Duration
	PermissionMode        string
	SkillCatalog          string
	Workspace             string
	TokenEstimator        contextledger.TokenEstimator
	OutputReserveTokens   int
	ContextRecentRounds   int
	MaxProjectMapBytes    int
	MaxToolContextBytes   int
	SkillCatalogPageBytes int
	MaxSummaryBytes       int
	ContextEvent          func(contextledger.Event)
	MCPContexts           []MCPContext
	MCPToolServers        map[string]string
	MCPFingerprint        string
}

type MCPContext struct {
	Server          string
	Instructions    string
	ResourceCatalog string
}

type ProtectedSkill struct {
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Entry        string    `json:"entry"`
	Root         string    `json:"root"`
	Digest       string    `json:"digest"`
	Content      string    `json:"content,omitempty"`
	Trigger      string    `json:"trigger,omitempty"`
	ActivatedAt  time.Time `json:"activated_at,omitempty"`
	AllowedTools string    `json:"allowed_tools,omitempty"`
}

type Conversation struct {
	mu                  sync.Mutex
	sessionID           string
	turns               []protocol.Turn
	closed              map[string][]protocol.Turn
	driverState         json.RawMessage
	providerName        string
	providerGeneration  uint64
	providerAdapter     string
	providerBaseURL     string
	providerModel       string
	permissionMode      string
	systemPrompt        string
	skillCatalog        string
	protectedSkills     map[string]ProtectedSkill
	toolDefinitions     []protocol.ToolDefinition
	ledger              *contextledger.Ledger
	lastRuntime         Runtime
	summary             string
	omittedTurnIDs      map[string]struct{}
	projectMap          string
	projectMapWorkspace string
	projectMapMaxBytes  int
	projectMapDirty     bool
	mcpFingerprint      string
}

func NewConversation() *Conversation {
	conversation := &Conversation{sessionID: uuid.NewString(), closed: make(map[string][]protocol.Turn), ledger: contextledger.New(nil), permissionMode: "manual", protectedSkills: make(map[string]ProtectedSkill), omittedTurnIDs: make(map[string]struct{}), projectMapDirty: true}
	conversation.systemPrompt = promptForRuntime("manual")
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
	c.mcpFingerprint = ""
	c.permissionMode = c.lastRuntime.PermissionMode
	if c.permissionMode == "" {
		c.permissionMode = "manual"
	}
	c.skillCatalog = c.lastRuntime.SkillCatalog
	c.protectedSkills = make(map[string]ProtectedSkill)
	c.summary = ""
	c.omittedTurnIDs = make(map[string]struct{})
	c.projectMap = ""
	c.projectMapWorkspace = ""
	c.projectMapMaxBytes = 0
	c.projectMapDirty = true
	c.systemPrompt = promptForRuntime(c.permissionMode)
	c.ledger.Reset()
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
	if c.providerName != runtime.Provider.Name || c.providerGeneration != runtime.Provider.Generation || c.providerAdapter != runtime.Provider.Config.Adapter || c.providerBaseURL != runtime.Provider.Config.BaseURL || c.providerModel != runtime.Provider.Config.Model || c.permissionMode != mode || c.skillCatalog != runtime.SkillCatalog || c.mcpFingerprint != runtime.MCPFingerprint {
		c.driverState = nil
		c.providerName = runtime.Provider.Name
		c.providerGeneration = runtime.Provider.Generation
		c.providerAdapter = runtime.Provider.Config.Adapter
		c.providerBaseURL = runtime.Provider.Config.BaseURL
		c.providerModel = runtime.Provider.Config.Model
		c.permissionMode = mode
		c.skillCatalog = runtime.SkillCatalog
		c.mcpFingerprint = runtime.MCPFingerprint
		c.systemPrompt = promptForRuntime(mode)
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
	prepared, err := c.prepareRequestContext(runtime, definitions)
	if err != nil {
		return protocol.ModelResponse{}, err
	}
	request := driver.Request{
		BaseURL: runtime.Provider.Config.BaseURL,
		APIKey:  runtime.APIKey,
		Headers: runtime.Provider.Config.Headers,
		Stream:  stream,
		Model: protocol.ModelRequest{
			ProtocolVersion: protocol.Version,
			Model:           runtime.Provider.Config.Model,
			Turns:           prepared.Turns,
			Tools:           prepared.Tools,
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
	c.refreshProjectMap(runtime)
	prepared := c.buildPromptContext(runtime, c.toolDefinitions)
	c.ledger.ReplaceBlocks(prepared.Blocks)
}

func promptForRuntime(mode string) string {
	base := SystemPrompt + "\nCurrent permission mode: " + mode + ". Local policy decisions are final."
	modePrompt := ""
	switch mode {
	case "plan":
		modePrompt = " You may read, search, list files, and run commands classified as read-only. Finish with a concrete modification plan, file list, risks, and validation commands."
	case "auto":
		modePrompt = " Workspace edits run automatically. Allowlisted commands run automatically; other commands request confirmation."
	case "full":
		modePrompt = " Ordinary workspace tools and commands run automatically. Dangerous operations always request a prominent confirmation."
	default:
		modePrompt = " Reads run automatically. Writes and commands request confirmation; dangerous operations require two confirmations."
	}
	return base + modePrompt
}

func (c *Conversation) ActivatedSkillDigests() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]string, len(c.protectedSkills))
	for name, item := range c.protectedSkills {
		result[name] = item.Digest
	}
	return result
}

func (c *Conversation) RegisterSkillResult(result protocol.ToolResult) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.captureSkillResult(result)
	if changed {
		c.rebuildLedger(c.lastRuntime)
	}
	return changed
}

func (c *Conversation) captureSkillResult(result protocol.ToolResult) bool {
	if result.Metadata == nil || result.Metadata["skill_activation"] != true {
		return false
	}
	content, _ := result.Metadata["protected_content"].(string)
	name, _ := result.Metadata["skill_name"].(string)
	digest, _ := result.Metadata["skill_digest"].(string)
	if content == "" || name == "" || digest == "" {
		return false
	}
	current, exists := c.protectedSkills[name]
	if exists && current.Digest == digest {
		return false
	}
	c.protectedSkills[name] = ProtectedSkill{
		Name: name, Source: stringMetadata(result.Metadata, "skill_source"), Entry: stringMetadata(result.Metadata, "skill_entry"),
		Root: stringMetadata(result.Metadata, "skill_root"), Digest: digest, Content: content,
		Trigger: stringMetadata(result.Metadata, "trigger"), AllowedTools: stringMetadata(result.Metadata, "allowed_tools"),
	}
	if activated := stringMetadata(result.Metadata, "activated_at"); activated != "" {
		c.protectedSkills[name] = withActivatedAt(c.protectedSkills[name], activated)
	}
	return true
}

func withActivatedAt(item ProtectedSkill, value string) ProtectedSkill {
	item.ActivatedAt, _ = time.Parse(time.RFC3339Nano, value)
	return item
}

func stringMetadata(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return value
}

func protectedNamesFromMap(items map[string]ProtectedSkill) []string {
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func cloneTurns(turns []protocol.Turn) []protocol.Turn {
	result := make([]protocol.Turn, len(turns))
	for index, turn := range turns {
		result[index] = turn
		result[index].Parts = make([]protocol.Part, len(turn.Parts))
		for partIndex, part := range turn.Parts {
			result[index].Parts[partIndex] = part
			if part.ToolCall != nil {
				call := *part.ToolCall
				call.Arguments = append(json.RawMessage(nil), part.ToolCall.Arguments...)
				result[index].Parts[partIndex].ToolCall = &call
			}
			if part.ToolResult != nil {
				toolResult := *part.ToolResult
				toolResult.Metadata = make(map[string]any, len(part.ToolResult.Metadata))
				for key, value := range part.ToolResult.Metadata {
					toolResult.Metadata[key] = value
				}
				result[index].Parts[partIndex].ToolResult = &toolResult
			}
		}
	}
	return result
}
