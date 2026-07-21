package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

type sessionRuntime struct {
	mu               sync.Mutex
	store            *session.Store
	snapshot         session.Snapshot
	persistedTurns   int
	persistedPrompts int
	persistedSkills  map[string]string
	revalidated      bool
	workspace        string
	redact           func(string) string
}

func (r *runtime) openConversation(ctx context.Context, manager *provider.Manager, opts *chatOptions) (*agent.Conversation, error) {
	store, err := session.Open("")
	if err != nil {
		return nil, sessionProtocolError("open session store", err)
	}
	workspace := r.workspace
	id := opts.sessionID
	resuming := opts.resumeSet || opts.resumeID != ""
	if resuming {
		id = opts.resumeID
	}
	if id != "" && !session.ValidID(id) {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid session ID %q", id)}
	}

	if id != "" {
		load := store.LoadRecovering
		if resuming {
			load = store.Load
		}
		stored, diagnostics, loadErr := load(id)
		if loadErr == nil {
			if resuming && len(diagnostics) > 0 {
				diagnostic := diagnostics[0]
				return nil, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("load session %s: %s: %s", id, diagnostic.Path, diagnostic.Message)}
			}
			if stored.Workspace != "" && !sameWorkspace(stored.Workspace, workspace) {
				return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("session %s belongs to workspace %s; select it with --workspace", id, stored.Workspace)}
			}
			if stored.Environment.Empty() {
				captured := r.captureEnvironment(ctx, workspace)
				stored.Environment = captured
				events, captureErr := store.Append(id, []session.Event{
					{Type: session.EventRuntimeUpdated, Environment: &captured},
					{Type: session.EventDriverState},
				})
				if captureErr != nil {
					return nil, sessionProtocolError("persist restored session environment", captureErr)
				}
				stored.Sequence = events[len(events)-1].Sequence
				stored.DriverState = nil
				if saveErr := store.Save(stored); saveErr != nil {
					return nil, sessionProtocolError("save restored session environment", saveErr)
				}
			}
			applyStoredRuntimeOptions(manager, stored, opts, r.stderr)
			conversation, restoreErr := agent.RestoreConversation(agentStateFromSnapshot(stored))
			if restoreErr != nil {
				return nil, sessionProtocolError("restore session", restoreErr)
			}
			controller := newSessionRuntime(store, stored, workspace, r.redact)
			if stored.ClosedAt != nil {
				events, reopenErr := store.Append(id, []session.Event{{Type: session.EventSessionReopened}})
				if reopenErr != nil {
					return nil, sessionProtocolError("reopen session", reopenErr)
				}
				stored.Sequence = events[len(events)-1].Sequence
				stored.ClosedAt = nil
				if saveErr := store.Save(stored); saveErr != nil {
					return nil, sessionProtocolError("save reopened session", saveErr)
				}
				controller.snapshot = stored
			}
			r.session = controller
			for _, diagnostic := range diagnostics {
				fmt.Fprintf(r.stderr, "[session] %s: %s\n", diagnostic.Path, diagnostic.Message)
			}
			return conversation, nil
		}
		if resuming && errors.Is(loadErr, os.ErrNotExist) {
			return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("session %q does not exist", id), Cause: loadErr}
		}
		if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, sessionProtocolError("load session", loadErr)
		}
	}

	environmentContext := r.captureEnvironment(ctx, workspace)
	conversation := agent.NewConversationWithEnvironment(environmentContext)
	if id != "" {
		providerState, providerErr := selectedSessionProvider(manager, *opts)
		if providerErr != nil {
			return nil, providerErr
		}
		mode := selectedMode(manager, *opts)
		conversation, err = agent.RestoreConversation(agent.ConversationState{
			SessionID: id, Provider: agentProviderState(providerState), Workspace: workspace, Environment: environmentContext, PermissionMode: mode,
		})
		if err != nil {
			return nil, sessionProtocolError("initialize named session", err)
		}
	}
	stored, err := store.Create(snapshotFromConversation(conversation, manager, *opts, session.Snapshot{}, workspace))
	if err != nil {
		return nil, sessionProtocolError("create session", err)
	}
	r.session = newSessionRuntime(store, stored, workspace, r.redact)
	return conversation, nil
}

func newSessionRuntime(store *session.Store, snapshot session.Snapshot, workspace string, redact func(string) string) *sessionRuntime {
	digests := make(map[string]string, len(snapshot.Skills))
	for _, item := range snapshot.Skills {
		digests[item.Name] = item.Digest
	}
	return &sessionRuntime{store: store, snapshot: snapshot, persistedTurns: len(snapshot.Turns), persistedPrompts: len(snapshot.PromptHistory), persistedSkills: digests, workspace: workspace, redact: redact}
}

func (s *sessionRuntime) Sync(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions, runErr error) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := conversation.ExportState()
	if state.SessionID != s.snapshot.SessionID {
		return fmt.Errorf("conversation session %q does not match persistence session %q", state.SessionID, s.snapshot.SessionID)
	}
	if len(state.Turns) < s.persistedTurns {
		return fmt.Errorf("session transcript shrank from %d to %d turns", s.persistedTurns, len(state.Turns))
	}
	if len(state.PromptHistory) < s.persistedPrompts {
		return fmt.Errorf("session prompt history shrank from %d to %d entries", s.persistedPrompts, len(state.PromptHistory))
	}
	providerState := sessionProviderState(state.Provider)
	selectedProvider, selectedErr := selectedSessionProvider(manager, opts)
	if selectedErr != nil {
		return selectedErr
	}
	if providerState.Name == "" || !sameSessionProvider(providerState, selectedProvider) {
		providerState = selectedProvider
	}
	state.Provider = agentProviderState(providerState)
	state.Workspace = s.workspace
	state.PermissionMode = selectedMode(manager, opts)

	events := make([]session.Event, 0, len(state.Turns)-s.persistedTurns+4)
	for index := s.persistedTurns; index < len(state.Turns); index++ {
		turn := state.Turns[index]
		events = append(events, session.Event{Type: session.EventTurnAppended, Turn: &turn})
	}
	for index := s.persistedPrompts; index < len(state.PromptHistory); index++ {
		events = append(events, session.Event{Type: session.EventPromptRecorded, Prompt: state.PromptHistory[index]})
	}
	events = append(events, session.Event{
		Type: session.EventRuntimeUpdated, Workspace: state.Workspace, PermissionMode: state.PermissionMode, Provider: &providerState,
	})
	if !bytes.Equal(state.DriverState, s.snapshot.DriverState) {
		events = append(events, session.Event{Type: session.EventDriverState, DriverState: append(json.RawMessage(nil), state.DriverState...)})
	}
	for _, item := range state.ProtectedSkills {
		if s.persistedSkills[item.Name] == item.Digest {
			continue
		}
		skillState := skillStateFromProtected(item)
		events = append(events, session.Event{Type: session.EventSkillActivated, Skill: &skillState})
	}
	ledger := state.Ledger
	todoList := state.TodoList
	events = append(events, session.Event{
		Type: session.EventContextUpdated, SkillCatalog: state.SkillCatalog, Summary: state.Summary,
		TodoList: &todoList, OmittedTurnIDs: append([]string(nil), state.OmittedTurnIDs...), Ledger: &ledger,
	})
	lastError := ""
	if runErr != nil {
		lastError = runErr.Error()
		if s.redact != nil {
			lastError = s.redact(lastError)
		}
		events = append(events, session.Event{Type: session.EventErrorRecorded, Error: lastError})
	}
	prepared, err := s.store.Append(state.SessionID, events)
	if err != nil {
		return sessionProtocolError("append session events", err)
	}
	if len(prepared) > 0 {
		s.snapshot.Sequence = prepared[len(prepared)-1].Sequence
	}
	next := snapshotFromAgentState(state, s.snapshot)
	next.Sequence = s.snapshot.Sequence
	next.LastError = lastError
	if err := s.store.Save(next); err != nil {
		return sessionProtocolError("save session snapshot", err)
	}
	s.snapshot = next
	s.persistedTurns = len(state.Turns)
	s.persistedPrompts = len(state.PromptHistory)
	s.persistedSkills = make(map[string]string, len(state.ProtectedSkills))
	for _, item := range state.ProtectedSkills {
		s.persistedSkills[item.Name] = item.Digest
	}
	cfg := manager.Config()
	if _, err := s.store.Cleanup(cfg.MaxSessions, cfg.MaxSessionBytes, state.SessionID); err != nil {
		return sessionProtocolError("clean session store", err)
	}
	return nil
}

func (s *sessionRuntime) Close(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions) error {
	if err := s.Sync(conversation, manager, opts, nil); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.store.Append(s.snapshot.SessionID, []session.Event{{Type: session.EventSessionClosed}})
	if err != nil {
		return sessionProtocolError("close session", err)
	}
	closedAt := events[len(events)-1].At
	s.snapshot.Sequence = events[len(events)-1].Sequence
	s.snapshot.ClosedAt = &closedAt
	if err := s.store.Save(s.snapshot); err != nil {
		return sessionProtocolError("save closed session", err)
	}
	return nil
}

func (r *runtime) rotateSession(ctx context.Context, conversation *agent.Conversation, manager *provider.Manager, opts chatOptions) (string, string, error) {
	oldID := conversation.SessionID()
	environmentContext := r.captureEnvironment(ctx, r.workspace)
	if r.session == nil {
		conversation.NewSessionWithEnvironment(environmentContext)
		return oldID, conversation.SessionID(), nil
	}
	if err := r.session.Close(conversation, manager, opts); err != nil {
		return oldID, "", err
	}
	if err := r.closeMCP(); err != nil {
		return oldID, "", sessionProtocolError("close MCP sessions", err)
	}
	conversation.NewSessionWithEnvironment(environmentContext)
	stored, err := r.session.store.Create(snapshotFromConversation(conversation, manager, opts, session.Snapshot{}, r.workspace))
	if err != nil {
		return oldID, "", sessionProtocolError("create replacement session", err)
	}
	r.session = newSessionRuntime(r.session.store, stored, r.workspace, r.redact)
	return oldID, conversation.SessionID(), nil
}

func (s *sessionRuntime) RevalidateSkills(registry *skill.Registry, skillSession *skill.Session, conversation *agent.Conversation) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revalidated {
		return nil
	}
	s.revalidated = true
	diagnostics := make([]string, 0)
	activationTool := tool.NewActivateSkill(registry, skillSession)
	for _, persisted := range s.snapshot.Skills {
		current, ok := registry.Get(persisted.Name)
		if !ok {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s is unavailable during resume", persisted.Name))
			continue
		}
		if current.Digest != persisted.Digest {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s changed: saved digest %s, current digest %s", persisted.Name, persisted.Digest, current.Digest))
			continue
		}
		if current.Source.String() != persisted.Source {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s source changed from %s to %s", persisted.Name, persisted.Source, current.Source.String()))
			continue
		}
		input, _ := json.Marshal(map[string]string{"name": persisted.Name})
		result := activationTool.Execute(context.Background(), input)
		if result.IsError {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s could not be revalidated: %s", persisted.Name, result.Content))
			continue
		}
		reloadedDigest, _ := result.Metadata["skill_digest"].(string)
		if reloadedDigest != persisted.Digest {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s changed while revalidating: saved digest %s, reloaded digest %s", persisted.Name, persisted.Digest, reloadedDigest))
			continue
		}
		result.Metadata["trigger"] = persisted.Trigger
		result.Metadata["activated_at"] = persisted.ActivatedAt.Format(time.RFC3339Nano)
		if !conversation.RegisterSkillResult(result) {
			diagnostics = append(diagnostics, fmt.Sprintf("skill %s produced no protected context during resume", persisted.Name))
		}
	}
	return diagnostics
}

func snapshotFromConversation(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions, previous session.Snapshot, workspace string) session.Snapshot {
	state := conversation.ExportState()
	providerState, _ := selectedSessionProvider(manager, opts)
	current := sessionProviderState(state.Provider)
	if state.Provider.Name == "" || !sameSessionProvider(current, providerState) {
		state.Provider = agentProviderState(providerState)
	}
	state.Workspace = workspace
	state.PermissionMode = selectedMode(manager, opts)
	return snapshotFromAgentState(state, previous)
}

func snapshotFromAgentState(state agent.ConversationState, previous session.Snapshot) session.Snapshot {
	snapshot := session.Snapshot{
		Version: session.SchemaVersion, Sequence: previous.Sequence, SessionID: state.SessionID,
		CreatedAt: previous.CreatedAt, UpdatedAt: time.Now().UTC(), ClosedAt: previous.ClosedAt,
		Workspace: state.Workspace, Environment: state.Environment, PermissionMode: state.PermissionMode,
		Provider: sessionProviderState(state.Provider),
		Turns:    state.Turns, PromptHistory: append([]string{}, state.PromptHistory...), DriverState: append(json.RawMessage(nil), state.DriverState...), SkillCatalog: state.SkillCatalog,
		Summary: state.Summary, TodoList: cloneProtocolTodoList(state.TodoList), OmittedTurnIDs: append([]string(nil), state.OmittedTurnIDs...), Ledger: state.Ledger,
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	for _, item := range state.ProtectedSkills {
		snapshot.Skills = append(snapshot.Skills, skillStateFromProtected(item))
	}
	return snapshot
}

func agentStateFromSnapshot(snapshot session.Snapshot) agent.ConversationState {
	promptHistory := append([]string(nil), snapshot.PromptHistory...)
	if snapshot.PromptHistory == nil {
		promptHistory = legacyPromptHistory(snapshot.Turns)
	}
	return agent.ConversationState{
		SessionID: snapshot.SessionID, Turns: snapshot.Turns, PromptHistory: promptHistory, DriverState: append(json.RawMessage(nil), snapshot.DriverState...),
		Provider: agent.ProviderState{
			Name: snapshot.Provider.Name, Generation: snapshot.Provider.Generation, Adapter: snapshot.Provider.Adapter,
			BaseURL: snapshot.Provider.BaseURL, Model: snapshot.Provider.Model, ReasoningEffort: snapshot.Provider.ReasoningEffort,
			CatalogProvider: snapshot.Provider.CatalogProvider, ContextWindow: snapshot.Provider.ContextWindow,
			DetectedContextWindow: snapshot.Provider.DetectedContextWindow, EffectiveContextWindow: snapshot.Provider.EffectiveContextWindow,
			LimitSource: snapshot.Provider.LimitSource, LimitObservedAt: snapshot.Provider.LimitObservedAt,
			LimitCached: snapshot.Provider.LimitCached, LimitAssumed: snapshot.Provider.LimitAssumed, LimitDegradations: snapshot.Provider.LimitDegradations,
		},
		Workspace: snapshot.Workspace, Environment: snapshot.Environment, PermissionMode: snapshot.PermissionMode, SkillCatalog: snapshot.SkillCatalog,
		Summary: snapshot.Summary, TodoList: cloneProtocolTodoList(snapshot.TodoList), OmittedTurnIDs: snapshot.OmittedTurnIDs, Ledger: snapshot.Ledger,
	}
}

const (
	legacyPlanImplementationPrompt = "Implement the approved plan now. Follow the plan already present in this conversation, inspect current files before editing, and run the relevant verification."
	legacyPlanFeedbackPrefix       = "Revise the current implementation plan using this user feedback:\n\n"
	legacyUserRequestStart         = "</referenced_files>\n\n<user_request>\n"
	legacyUserRequestEnd           = "\n</user_request>"
)

func legacyPromptHistory(turns []protocol.Turn) []string {
	history := make([]string, 0)
	for _, turn := range turns {
		if turn.Role != protocol.RoleUser {
			continue
		}
		var text strings.Builder
		for _, part := range turn.Parts {
			if part.Kind == protocol.PartText {
				text.WriteString(part.Text)
			}
		}
		prompt := text.String()
		switch {
		case prompt == "", prompt == legacyPlanImplementationPrompt:
			continue
		case strings.HasPrefix(prompt, legacyPlanFeedbackPrefix):
			prompt = strings.TrimPrefix(prompt, legacyPlanFeedbackPrefix)
		case strings.Contains(prompt, legacyUserRequestStart) && strings.HasSuffix(prompt, legacyUserRequestEnd):
			start := strings.Index(prompt, legacyUserRequestStart) + len(legacyUserRequestStart)
			prompt = prompt[start : len(prompt)-len(legacyUserRequestEnd)]
		}
		if prompt = strings.TrimSpace(prompt); prompt != "" {
			history = append(history, prompt)
		}
	}
	return history
}

func cloneProtocolTodoList(list protocol.TodoList) protocol.TodoList {
	return protocol.TodoList{Explanation: list.Explanation, Items: append([]protocol.TodoItem(nil), list.Items...)}
}

func (r *runtime) captureEnvironment(ctx context.Context, workspace string) environment.Context {
	if r.environmentCapture != nil {
		return r.environmentCapture(ctx, workspace)
	}
	return environment.Capture(ctx, workspace)
}

func skillStateFromProtected(item agent.ProtectedSkill) session.SkillState {
	return session.SkillState{
		Name: item.Name, Source: item.Source, Entry: item.Entry, Root: item.Root, Digest: item.Digest,
		Trigger: item.Trigger, ActivatedAt: item.ActivatedAt, AllowedTools: item.AllowedTools,
	}
}

func selectedSessionProvider(manager *provider.Manager, opts chatOptions) (session.ProviderState, error) {
	var selected provider.Snapshot
	var err error
	if opts.provider != "" {
		var ok bool
		selected, ok = manager.Snapshot(opts.provider)
		if !ok {
			return session.ProviderState{}, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("provider %q does not exist", opts.provider)}
		}
	} else {
		selected, err = manager.Active()
		if err != nil {
			return session.ProviderState{}, &protocol.Error{Code: protocol.ErrConfig, Message: err.Error()}
		}
	}
	providerConfig := selected.Config
	if opts.model != "" {
		providerConfig.Model = opts.model
	}
	if opts.baseURL != "" {
		providerConfig.BaseURL = opts.baseURL
	}
	if opts.adapter != "" && opts.adapter != "openai_responses" {
		providerConfig.Adapter = opts.adapter
	}
	return session.ProviderState{
		Name: selected.Name, Generation: selected.Generation, Adapter: providerConfig.Adapter, BaseURL: providerConfig.BaseURL,
		Model: providerConfig.Model, ReasoningEffort: providerConfig.ReasoningEffort,
		CatalogProvider: providerConfig.CatalogProvider, ContextWindow: providerConfig.ContextWindow,
	}, nil
}

func agentProviderState(state session.ProviderState) agent.ProviderState {
	return agent.ProviderState{
		Name: state.Name, Generation: state.Generation, Adapter: state.Adapter, BaseURL: state.BaseURL,
		Model: state.Model, ReasoningEffort: state.ReasoningEffort, CatalogProvider: state.CatalogProvider, ContextWindow: state.ContextWindow,
		DetectedContextWindow: state.DetectedContextWindow, EffectiveContextWindow: state.EffectiveContextWindow,
		LimitSource: state.LimitSource, LimitObservedAt: state.LimitObservedAt,
		LimitCached: state.LimitCached, LimitAssumed: state.LimitAssumed, LimitDegradations: state.LimitDegradations,
	}
}

func sessionProviderState(state agent.ProviderState) session.ProviderState {
	return session.ProviderState{
		Name: state.Name, Generation: state.Generation, Adapter: state.Adapter, BaseURL: state.BaseURL,
		Model: state.Model, ReasoningEffort: state.ReasoningEffort, CatalogProvider: state.CatalogProvider, ContextWindow: state.ContextWindow,
		DetectedContextWindow: state.DetectedContextWindow, EffectiveContextWindow: state.EffectiveContextWindow,
		LimitSource: state.LimitSource, LimitObservedAt: state.LimitObservedAt,
		LimitCached: state.LimitCached, LimitAssumed: state.LimitAssumed, LimitDegradations: state.LimitDegradations,
	}
}

func sameSessionProvider(left, right session.ProviderState) bool {
	return left.Name == right.Name && left.Generation == right.Generation && left.Adapter == right.Adapter && left.BaseURL == right.BaseURL && left.Model == right.Model && left.ReasoningEffort == right.ReasoningEffort && left.CatalogProvider == right.CatalogProvider && left.ContextWindow == right.ContextWindow
}

func selectedMode(manager *provider.Manager, opts chatOptions) string {
	if opts.mode != "" {
		return opts.mode
	}
	return manager.Config().PermissionMode
}

func applyStoredRuntimeOptions(manager *provider.Manager, snapshot session.Snapshot, opts *chatOptions, diagnosticsWriter interface{ Write([]byte) (int, error) }) {
	if opts.mode == "" && snapshot.PermissionMode != "" {
		opts.mode = snapshot.PermissionMode
	}
	if opts.provider != "" || snapshot.Provider.Name == "" {
		return
	}
	current, ok := manager.Snapshot(snapshot.Provider.Name)
	if !ok {
		fmt.Fprintf(diagnosticsWriter, "[session] saved provider %s is unavailable; using the active provider\n", snapshot.Provider.Name)
		return
	}
	opts.provider = snapshot.Provider.Name
	if current.Generation == snapshot.Provider.Generation {
		opts.model = snapshot.Provider.Model
		return
	}
	fmt.Fprintf(diagnosticsWriter, "[session] provider %s generation changed from %d to %d; using current provider configuration\n", snapshot.Provider.Name, snapshot.Provider.Generation, current.Generation)
}

func sameWorkspace(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftPath = filepath.Clean(leftPath)
	rightPath = filepath.Clean(rightPath)
	if goruntime.GOOS == "windows" {
		return strings.EqualFold(leftPath, rightPath)
	}
	return leftPath == rightPath
}

func sessionProtocolError(operation string, err error) error {
	var typed *protocol.Error
	if errors.As(err, &typed) {
		return typed
	}
	return &protocol.Error{Code: protocol.ErrProtocol, Message: operation + ": " + err.Error(), Cause: err}
}
