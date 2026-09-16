package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// appendProgress records what the event log has durably accepted.
//
// It is deliberately independent of the snapshot checkpoint: once Append
// confirms, that progress advances and a later snapshot failure never rolls it
// back. Otherwise the next Sync would append the same turn, prompt, skill or
// state event a second time and break replay.
type appendProgress struct {
	turns    int
	prompts  int
	skills   map[string]string
	sequence uint64
	// driverState is the raw value of the last appended driver-state event. An
	// empty state is a real value (the remote state was cleared), so it is
	// compared rather than treated as absent.
	driverState json.RawMessage
	// fingerprints identify the last appended payload of each state event type.
	fingerprints map[session.EventType]string
}

type sessionRuntime struct {
	mu sync.Mutex
	// store persists the event log and the snapshot.
	store *session.Store
	// snapshot is the last snapshot known to be on disk. It may lag behind the
	// log while a save is pending.
	snapshot session.Snapshot
	// log is the durable append progress, which is never rolled back by a
	// failed snapshot save.
	log appendProgress
	// snapshotPending reports that the log is ahead of the snapshot on disk.
	snapshotPending bool
	// appendEvents and saveSnapshot are fault-injection seams for tests. They
	// default to the store methods.
	appendEvents func(string, []session.Event) ([]session.Event, error)
	saveSnapshot func(session.Snapshot) error
	revalidated  bool
	agentTasks   func(string) []tool.AgentTask
	workspace    string
	redact       func(string) string
}

// eventFingerprint returns a stable digest of one event payload. The volatile
// fields a store assigns (version, sequence, session ID, timestamp) are excluded
// so the same logical state always yields the same fingerprint and a retry after
// a failed snapshot save is recognized instead of being appended again.
func eventFingerprint(event session.Event) string {
	event.Version, event.Sequence, event.SessionID, event.At = 0, 0, "", time.Time{}
	encoded, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
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
	return &sessionRuntime{
		store: store, snapshot: snapshot, workspace: workspace, redact: redact,
		// A restored snapshot describes what the log already contains, so both
		// progress trackers start from it.
		log: appendProgress{
			turns: len(snapshot.Turns), prompts: len(snapshot.PromptHistory), skills: digests,
			sequence: snapshot.Sequence, driverState: append(json.RawMessage(nil), snapshot.DriverState...),
			fingerprints: make(map[session.EventType]string),
		},
	}
}

func (s *sessionRuntime) append(id string, events []session.Event) ([]session.Event, error) {
	if s.appendEvents != nil {
		return s.appendEvents(id, events)
	}
	return s.store.Append(id, events)
}

func (s *sessionRuntime) save(snapshot session.Snapshot) error {
	if s.saveSnapshot != nil {
		return s.saveSnapshot(snapshot)
	}
	return s.store.Save(snapshot)
}

// acceptAppended advances the durable append progress from the events the store
// confirmed. It is only called after a successful append, so the progress always
// describes what the log actually holds.
func (s *sessionRuntime) acceptAppended(prepared []session.Event) {
	for _, event := range prepared {
		switch event.Type {
		case session.EventTurnAppended:
			s.log.turns++
		case session.EventPromptRecorded:
			s.log.prompts++
		case session.EventSkillActivated:
			if event.Skill != nil {
				if s.log.skills == nil {
					s.log.skills = make(map[string]string)
				}
				s.log.skills[event.Skill.Name] = event.Skill.Digest
			}
		case session.EventDriverState:
			s.log.driverState = append(json.RawMessage(nil), event.DriverState...)
		case session.EventRuntimeUpdated, session.EventContextUpdated, session.EventAgentTasksUpdated, session.EventErrorRecorded:
			if s.log.fingerprints == nil {
				s.log.fingerprints = make(map[session.EventType]string)
			}
			s.log.fingerprints[event.Type] = eventFingerprint(event)
		}
		if event.Sequence > s.log.sequence {
			s.log.sequence = event.Sequence
		}
	}
}

// stateChanged reports whether a state event has to be appended, comparing it
// with the payload the log already accepted.
func (s *sessionRuntime) stateChanged(event session.Event) bool {
	fingerprint := eventFingerprint(event)
	if fingerprint == "" {
		return true
	}
	return s.log.fingerprints[event.Type] != fingerprint
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
	if len(state.Turns) < s.log.turns {
		return fmt.Errorf("session transcript shrank from %d to %d turns", s.log.turns, len(state.Turns))
	}
	if len(state.PromptHistory) < s.log.prompts {
		return fmt.Errorf("session prompt history shrank from %d to %d entries", s.log.prompts, len(state.PromptHistory))
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

	// Only events the log does not already hold are appended. The progress
	// counters describe confirmed appends, not saved snapshots, so a retry after
	// a failed save does not duplicate an earlier append.
	events := make([]session.Event, 0, len(state.Turns)-s.log.turns+4)
	for index := s.log.turns; index < len(state.Turns); index++ {
		turn := state.Turns[index]
		events = append(events, session.Event{Type: session.EventTurnAppended, Turn: &turn})
	}
	for index := s.log.prompts; index < len(state.PromptHistory); index++ {
		events = append(events, session.Event{Type: session.EventPromptRecorded, Prompt: state.PromptHistory[index]})
	}
	agentTasks := append([]tool.AgentTask(nil), s.snapshot.AgentTasks...)
	if s.agentTasks != nil {
		agentTasks = s.agentTasks(state.SessionID)
	}
	stateEvents := []session.Event{
		{Type: session.EventRuntimeUpdated, Workspace: state.Workspace, PermissionMode: state.PermissionMode, Provider: &providerState},
		{Type: session.EventAgentTasksUpdated, AgentTasks: agentTasks},
	}
	if !bytes.Equal(state.DriverState, s.log.driverState) {
		stateEvents = append(stateEvents, session.Event{Type: session.EventDriverState, DriverState: append(json.RawMessage(nil), state.DriverState...)})
	}
	ledger := state.Ledger
	todoList := state.TodoList
	stateEvents = append(stateEvents, session.Event{
		Type: session.EventContextUpdated, SkillCatalog: state.SkillCatalog, Summary: state.Summary,
		TodoList: &todoList, OmittedTurnIDs: append([]string(nil), state.OmittedTurnIDs...), Ledger: &ledger,
	})
	lastError := ""
	if runErr != nil {
		lastError = runErr.Error()
		if s.redact != nil {
			lastError = s.redact(lastError)
		}
		stateEvents = append(stateEvents, session.Event{Type: session.EventErrorRecorded, Error: lastError})
	}
	for _, event := range stateEvents {
		if s.stateChanged(event) {
			events = append(events, event)
		}
	}
	for _, item := range state.ProtectedSkills {
		if s.log.skills[item.Name] == item.Digest {
			continue
		}
		skillState := skillStateFromProtected(item)
		events = append(events, session.Event{Type: session.EventSkillActivated, Skill: &skillState})
	}
	prepared, err := s.append(state.SessionID, events)
	if err != nil {
		return sessionProtocolError("append session events", err)
	}
	// The append is durable now, so the log progress advances here. A later
	// snapshot failure marks the checkpoint as behind instead of undoing it.
	s.acceptAppended(prepared)
	next := snapshotFromAgentState(state, s.snapshot)
	next.AgentTasks = agentTasks
	next.Sequence = s.log.sequence
	next.LastError = lastError
	if err := s.save(next); err != nil {
		s.snapshotPending = true
		return sessionProtocolError("save session snapshot", err)
	}
	s.snapshot = next
	s.snapshotPending = false
	cfg := manager.Config()
	if _, err := s.store.Cleanup(cfg.MaxSessions, cfg.MaxSessionBytes, state.SessionID); err != nil {
		return sessionProtocolError("clean session store", err)
	}
	return nil
}

func (s *sessionRuntime) SetAgentTaskSource(source func(string) []tool.AgentTask) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.agentTasks = source
	s.mu.Unlock()
}

func (s *sessionRuntime) AgentTasks() []tool.AgentTask {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tool.AgentTask(nil), s.snapshot.AgentTasks...)
}

func (s *sessionRuntime) Close(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions) error {
	if err := s.Sync(conversation, manager, opts, nil); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.append(s.snapshot.SessionID, []session.Event{{Type: session.EventSessionClosed}})
	if err != nil {
		return sessionProtocolError("close session", err)
	}
	s.acceptAppended(events)
	closedAt := events[len(events)-1].At
	s.snapshot.Sequence = s.log.sequence
	s.snapshot.ClosedAt = &closedAt
	if err := s.save(s.snapshot); err != nil {
		s.snapshotPending = true
		return sessionProtocolError("save closed session", err)
	}
	s.snapshotPending = false
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
		AgentTasks: append([]tool.AgentTask(nil), previous.AgentTasks...),
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
