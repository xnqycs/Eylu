package app

import (
	"bytes"
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
	"Eylu/internal/logging"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

type sessionRuntime struct {
	mu              sync.Mutex
	store           *session.Store
	snapshot        session.Snapshot
	persistedTurns  int
	persistedSkills map[string]string
	revalidated     bool
}

func (r *runtime) openConversation(manager *provider.Manager, opts *chatOptions) (*agent.Conversation, error) {
	store, err := session.Open("")
	if err != nil {
		return nil, sessionProtocolError("open session store", err)
	}
	cfg := manager.Config()
	id := opts.sessionID
	if opts.resume {
		latest, found, latestErr := store.Latest(cfg.Workspace)
		if latestErr != nil {
			return nil, sessionProtocolError("find recent session", latestErr)
		}
		if !found {
			return nil, &protocol.Error{Code: protocol.ErrConfig, Message: "no resumable session exists for this workspace"}
		}
		id = latest.SessionID
	}
	if id != "" && !session.ValidID(id) {
		return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("invalid session ID %q", id)}
	}

	if id != "" {
		stored, diagnostics, loadErr := store.Load(id)
		if loadErr == nil {
			if stored.Workspace != "" && !sameWorkspace(stored.Workspace, cfg.Workspace) {
				return nil, &protocol.Error{Code: protocol.ErrConfig, Message: fmt.Sprintf("session %s belongs to workspace %s; select it with --workspace", id, stored.Workspace)}
			}
			applyStoredRuntimeOptions(manager, stored, opts, r.stderr)
			conversation, restoreErr := agent.RestoreConversation(agentStateFromSnapshot(stored))
			if restoreErr != nil {
				return nil, sessionProtocolError("restore session", restoreErr)
			}
			controller := newSessionRuntime(store, stored)
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
		if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, sessionProtocolError("load session", loadErr)
		}
	}

	conversation := agent.NewConversation()
	if id != "" {
		providerState, providerErr := selectedSessionProvider(manager, *opts)
		if providerErr != nil {
			return nil, providerErr
		}
		mode := selectedMode(manager, *opts)
		conversation, err = agent.RestoreConversation(agent.ConversationState{
			SessionID: id, Provider: agentProviderState(providerState), Workspace: cfg.Workspace, PermissionMode: mode,
		})
		if err != nil {
			return nil, sessionProtocolError("initialize named session", err)
		}
	}
	stored, err := store.Create(snapshotFromConversation(conversation, manager, *opts, session.Snapshot{}))
	if err != nil {
		return nil, sessionProtocolError("create session", err)
	}
	r.session = newSessionRuntime(store, stored)
	return conversation, nil
}

func newSessionRuntime(store *session.Store, snapshot session.Snapshot) *sessionRuntime {
	digests := make(map[string]string, len(snapshot.Skills))
	for _, item := range snapshot.Skills {
		digests[item.Name] = item.Digest
	}
	return &sessionRuntime{store: store, snapshot: snapshot, persistedTurns: len(snapshot.Turns), persistedSkills: digests}
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
	providerState, err := selectedSessionProvider(manager, opts)
	if err != nil {
		return err
	}
	state.Provider = agentProviderState(providerState)
	state.Workspace = manager.Config().Workspace
	state.PermissionMode = selectedMode(manager, opts)

	events := make([]session.Event, 0, len(state.Turns)-s.persistedTurns+4)
	for index := s.persistedTurns; index < len(state.Turns); index++ {
		turn := state.Turns[index]
		events = append(events, session.Event{Type: session.EventTurnAppended, Turn: &turn})
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
	events = append(events, session.Event{
		Type: session.EventContextUpdated, SkillCatalog: state.SkillCatalog, Summary: state.Summary,
		OmittedTurnIDs: append([]string(nil), state.OmittedTurnIDs...), Ledger: &ledger,
	})
	lastError := ""
	if runErr != nil {
		lastError = logging.Redact(runErr.Error(), os.Getenv("EYLU_API_KEY"))
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

func (r *runtime) rotateSession(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions) (string, string, error) {
	oldID := conversation.SessionID()
	if r.session == nil {
		conversation.NewSession()
		return oldID, conversation.SessionID(), nil
	}
	if err := r.session.Close(conversation, manager, opts); err != nil {
		return oldID, "", err
	}
	conversation.NewSession()
	stored, err := r.session.store.Create(snapshotFromConversation(conversation, manager, opts, session.Snapshot{}))
	if err != nil {
		return oldID, "", sessionProtocolError("create replacement session", err)
	}
	r.session = newSessionRuntime(r.session.store, stored)
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
		result := activationTool.Execute(nil, input)
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

func snapshotFromConversation(conversation *agent.Conversation, manager *provider.Manager, opts chatOptions, previous session.Snapshot) session.Snapshot {
	state := conversation.ExportState()
	providerState, _ := selectedSessionProvider(manager, opts)
	state.Provider = agentProviderState(providerState)
	state.Workspace = manager.Config().Workspace
	state.PermissionMode = selectedMode(manager, opts)
	return snapshotFromAgentState(state, previous)
}

func snapshotFromAgentState(state agent.ConversationState, previous session.Snapshot) session.Snapshot {
	snapshot := session.Snapshot{
		Version: session.SchemaVersion, Sequence: previous.Sequence, SessionID: state.SessionID,
		CreatedAt: previous.CreatedAt, UpdatedAt: time.Now().UTC(), ClosedAt: previous.ClosedAt,
		Workspace: state.Workspace, PermissionMode: state.PermissionMode,
		Provider: session.ProviderState{
			Name: state.Provider.Name, Generation: state.Provider.Generation, Adapter: state.Provider.Adapter,
			BaseURL: state.Provider.BaseURL, Model: state.Provider.Model, ContextWindow: state.Provider.ContextWindow,
		},
		Turns: state.Turns, DriverState: append(json.RawMessage(nil), state.DriverState...), SkillCatalog: state.SkillCatalog,
		Summary: state.Summary, OmittedTurnIDs: append([]string(nil), state.OmittedTurnIDs...), Ledger: state.Ledger,
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
	return agent.ConversationState{
		SessionID: snapshot.SessionID, Turns: snapshot.Turns, DriverState: append(json.RawMessage(nil), snapshot.DriverState...),
		Provider: agent.ProviderState{
			Name: snapshot.Provider.Name, Generation: snapshot.Provider.Generation, Adapter: snapshot.Provider.Adapter,
			BaseURL: snapshot.Provider.BaseURL, Model: snapshot.Provider.Model, ContextWindow: snapshot.Provider.ContextWindow,
		},
		Workspace: snapshot.Workspace, PermissionMode: snapshot.PermissionMode, SkillCatalog: snapshot.SkillCatalog,
		Summary: snapshot.Summary, OmittedTurnIDs: snapshot.OmittedTurnIDs, Ledger: snapshot.Ledger,
	}
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
		Model: providerConfig.Model, ContextWindow: providerConfig.ContextWindow,
	}, nil
}

func agentProviderState(state session.ProviderState) agent.ProviderState {
	return agent.ProviderState{
		Name: state.Name, Generation: state.Generation, Adapter: state.Adapter, BaseURL: state.BaseURL,
		Model: state.Model, ContextWindow: state.ContextWindow,
	}
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
