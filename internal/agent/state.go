package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"unicode/utf8"

	"Eylu/internal/config"
	contextledger "Eylu/internal/context"
	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

type ProviderState struct {
	Name          string `json:"name"`
	Generation    uint64 `json:"generation"`
	Adapter       string `json:"adapter"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	ContextWindow int    `json:"context_window,omitempty"`
}

type ConversationState struct {
	SessionID       string                    `json:"session_id"`
	Turns           []protocol.Turn           `json:"turns"`
	DriverState     json.RawMessage           `json:"driver_state,omitempty"`
	Provider        ProviderState             `json:"provider"`
	Workspace       string                    `json:"workspace"`
	Environment     environment.Context       `json:"environment,omitzero"`
	PermissionMode  string                    `json:"permission_mode"`
	SkillCatalog    string                    `json:"skill_catalog,omitempty"`
	ProtectedSkills []ProtectedSkill          `json:"protected_skills,omitempty"`
	Summary         string                    `json:"summary,omitempty"`
	TodoList        protocol.TodoList         `json:"todo_list,omitzero"`
	OmittedTurnIDs  []string                  `json:"omitted_turn_ids,omitempty"`
	Ledger          contextledger.LedgerState `json:"ledger"`
}

func (c *Conversation) ExportState() ConversationState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := ConversationState{
		SessionID: c.sessionID, Turns: cloneTurns(c.turns), DriverState: append(json.RawMessage(nil), c.driverState...),
		Provider:  ProviderState{Name: c.providerName, Generation: c.providerGeneration, Adapter: c.providerAdapter, BaseURL: c.providerBaseURL, Model: c.providerModel, ContextWindow: c.lastRuntime.Provider.Config.ContextWindow},
		Workspace: c.lastRuntime.Workspace, Environment: c.environment, PermissionMode: c.permissionMode, SkillCatalog: c.skillCatalog,
		Summary: c.summary, TodoList: cloneTodoList(c.todoList), Ledger: c.ledger.State(),
	}
	for _, name := range protectedNamesFromMap(c.protectedSkills) {
		state.ProtectedSkills = append(state.ProtectedSkills, c.protectedSkills[name])
	}
	for id := range c.omittedTurnIDs {
		state.OmittedTurnIDs = append(state.OmittedTurnIDs, id)
	}
	sort.Strings(state.OmittedTurnIDs)
	return state
}

func RestoreConversation(state ConversationState) (*Conversation, error) {
	if state.SessionID == "" {
		return nil, fmt.Errorf("session ID is required")
	}
	if err := validateTurns(state.Turns); err != nil {
		return nil, err
	}
	if err := validateTodoListState(state.TodoList); err != nil {
		return nil, err
	}
	conversation := NewConversation()
	conversation.mu.Lock()
	defer conversation.mu.Unlock()
	conversation.sessionID = state.SessionID
	conversation.turns = cloneTurns(state.Turns)
	conversation.driverState = append(json.RawMessage(nil), state.DriverState...)
	conversation.providerName = state.Provider.Name
	conversation.providerGeneration = state.Provider.Generation
	conversation.providerAdapter = state.Provider.Adapter
	conversation.providerBaseURL = state.Provider.BaseURL
	conversation.providerModel = state.Provider.Model
	conversation.permissionMode = state.PermissionMode
	conversation.environment = state.Environment
	if conversation.permissionMode == "" {
		conversation.permissionMode = "manual"
	}
	conversation.skillCatalog = state.SkillCatalog
	conversation.protectedSkills = make(map[string]ProtectedSkill)
	for _, item := range state.ProtectedSkills {
		if item.Name != "" && item.Digest != "" && item.Content != "" {
			conversation.protectedSkills[item.Name] = item
		}
	}
	conversation.summary = state.Summary
	conversation.todoList = cloneTodoList(state.TodoList)
	conversation.omittedTurnIDs = make(map[string]struct{}, len(state.OmittedTurnIDs))
	for _, id := range state.OmittedTurnIDs {
		conversation.omittedTurnIDs[id] = struct{}{}
	}
	conversation.systemPrompt = promptForRuntime(conversation.permissionMode)
	conversation.lastRuntime = Runtime{
		Provider:  provider.Snapshot{Name: state.Provider.Name, Generation: state.Provider.Generation, Config: configForState(state.Provider)},
		Workspace: state.Workspace, PermissionMode: conversation.permissionMode, SkillCatalog: state.SkillCatalog,
	}
	conversation.projectMapDirty = true
	if len(state.Ledger.Blocks) > 0 {
		conversation.ledger.Restore(state.Ledger)
	} else {
		conversation.rebuildLedger(conversation.lastRuntime)
	}
	return conversation, nil
}

func cloneTodoList(list protocol.TodoList) protocol.TodoList {
	return protocol.TodoList{Explanation: list.Explanation, Items: append([]protocol.TodoItem(nil), list.Items...)}
}

func validateTodoListState(list protocol.TodoList) error {
	if len(list.Items) > 20 {
		return fmt.Errorf("session todo list exceeds 20 items")
	}
	seen := make(map[string]struct{}, len(list.Items))
	inProgress := 0
	idPattern := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	for _, item := range list.Items {
		if !idPattern.MatchString(item.ID) || item.Content == "" || utf8.RuneCountInString(item.Content) > 200 {
			return fmt.Errorf("session todo list contains an incomplete item")
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return fmt.Errorf("session todo list contains duplicate item %q", item.ID)
		}
		seen[item.ID] = struct{}{}
		switch item.Status {
		case protocol.TodoPending, protocol.TodoCompleted, protocol.TodoCancelled:
		case protocol.TodoInProgress:
			inProgress++
		default:
			return fmt.Errorf("session todo item %q has invalid status %q", item.ID, item.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("session todo list contains multiple in_progress items")
	}
	return nil
}

func configForState(state ProviderState) config.ProviderConfig {
	return config.ProviderConfig{Adapter: state.Adapter, BaseURL: state.BaseURL, Model: state.Model, ContextWindow: state.ContextWindow}
}

func validateTurns(turns []protocol.Turn) error {
	seen := make(map[string]struct{}, len(turns))
	for _, turn := range turns {
		if turn.ID == "" {
			return fmt.Errorf("session contains a turn without an ID")
		}
		if _, duplicate := seen[turn.ID]; duplicate {
			return fmt.Errorf("session contains duplicate turn ID %q", turn.ID)
		}
		seen[turn.ID] = struct{}{}
		switch turn.Role {
		case protocol.RoleSystem, protocol.RoleUser, protocol.RoleAgent, protocol.RoleTool:
		default:
			return fmt.Errorf("session turn %q has invalid role %q", turn.ID, turn.Role)
		}
		for _, part := range turn.Parts {
			if part.Kind == protocol.PartToolCall && part.ToolCall != nil && !json.Valid(part.ToolCall.Arguments) {
				return fmt.Errorf("session turn %q has invalid tool arguments", turn.ID)
			}
		}
	}
	return nil
}
