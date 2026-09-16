package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
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
	// pending maps the identity of one logical event to the event ID given to it.
	// An ID stays here until the log confirms it, so a retry after an uncertain
	// append reuses it and the log recognizes the duplicate instead of writing the
	// same logical change twice.
	pending     map[string]string
	eventPrefix string
	nextEventID uint64
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
	// compensations holds completions whose own append failed. The operation
	// already happened and the in-memory result is kept, so the record is retried
	// instead of being reported as an unknown outcome forever.
	compensations []session.ToolCompletion
}

// eventFingerprint returns a stable digest of one event payload. The volatile
// fields a store assigns (version, sequence, session ID, timestamp) and the event
// ID are excluded, so the same logical state always yields the same fingerprint
// and a retry after a failed snapshot save is recognized instead of being
// appended again.
func eventFingerprint(event session.Event) string {
	event.Version, event.Sequence, event.SessionID, event.At, event.ID = 0, 0, "", time.Time{}, ""
	encoded, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// newEventPrefix makes event IDs unique across processes. An ID only has to be
// stable across the retries of one runtime, so a per-runtime prefix is enough and
// it can never collide with an ID an earlier process wrote.
func newEventPrefix() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(raw[:])
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
			// A diagnostic the loader resolved without losing information - a
			// repeated event or turn identical to the copy already applied - does
			// not stop a resume. Anything else needs a human before the session is
			// used, so the resume refuses it and says what it found instead of
			// guessing.
			if resuming {
				for _, diagnostic := range diagnostics {
					if diagnostic.Benign {
						continue
					}
					return nil, &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("load session %s: %s: %s", id, diagnostic.Path, diagnostic.Message)}
				}
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
			// An execution that started without a recorded outcome is reported
			// here: its effect may have happened, so it is never replayed and the
			// recorded hints are offered for verification.
			for _, intent := range controller.PendingIntents() {
				fmt.Fprintf(r.stderr, "[session] %s\n", describePendingIntent(intent))
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
			fingerprints: make(map[session.EventType]string), eventPrefix: newEventPrefix(),
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
	confirmed := make(map[string]struct{}, len(prepared))
	for _, event := range prepared {
		if event.ID != "" {
			confirmed[event.ID] = struct{}{}
		}
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
	// A confirmed event is no longer pending, so a later state that happens to be
	// identical gets a fresh ID rather than being mistaken for the same event.
	for identity, eventID := range s.log.pending {
		if _, ok := confirmed[eventID]; ok {
			delete(s.log.pending, identity)
		}
	}
}

// eventIDFor returns the stable ID of one logical event. The first attempt
// assigns it; a retry of an event the log has not confirmed keeps the same ID, so
// the log can recognize it as a duplicate instead of writing it again.
func (s *sessionRuntime) eventIDFor(identity string) string {
	if identity == "" {
		return ""
	}
	// A turn identity is the turn's own host-owned UUID, so it is already unique
	// across runs and processes. Using it as the event ID makes the incremental
	// write of a turn and a later replay of the same turn literally the same
	// event, which is what lets the log recognize the second one as a retry
	// instead of writing the turn twice. The same holds for an intent or a
	// completion named after its call: replaying one is the same event.
	if strings.HasPrefix(identity, "turn:") || strings.HasPrefix(identity, "intent:") || strings.HasPrefix(identity, "completion:") {
		return identity
	}
	if eventID, ok := s.log.pending[identity]; ok {
		return eventID
	}
	if s.log.pending == nil {
		s.log.pending = make(map[string]string)
	}
	s.log.nextEventID++
	eventID := fmt.Sprintf("%s-%d", s.log.eventPrefix, s.log.nextEventID)
	s.log.pending[identity] = eventID
	return eventID
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
	//
	// Each logical event also carries an identity, which gives it a stable event
	// ID across retries.
	type pendingEvent struct {
		identity string
		event    session.Event
	}
	pending := make([]pendingEvent, 0, len(state.Turns)-s.log.turns+4)
	for index := s.log.turns; index < len(state.Turns); index++ {
		turn := state.Turns[index]
		pending = append(pending, pendingEvent{identity: "turn:" + turn.ID, event: session.Event{Type: session.EventTurnAppended, Turn: &turn}})
	}
	for index := s.log.prompts; index < len(state.PromptHistory); index++ {
		// The position is part of the identity, because the same prompt text
		// submitted twice is a legitimate repeat rather than a duplicate.
		pending = append(pending, pendingEvent{
			identity: fmt.Sprintf("prompt:%d", index),
			event:    session.Event{Type: session.EventPromptRecorded, Prompt: state.PromptHistory[index]},
		})
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
			pending = append(pending, pendingEvent{identity: "state:" + string(event.Type) + ":" + eventFingerprint(event), event: event})
		}
	}
	for _, item := range state.ProtectedSkills {
		if s.log.skills[item.Name] == item.Digest {
			continue
		}
		skillState := skillStateFromProtected(item)
		pending = append(pending, pendingEvent{
			identity: "skill:" + item.Name + ":" + item.Digest,
			event:    session.Event{Type: session.EventSkillActivated, Skill: &skillState},
		})
	}
	events := make([]session.Event, 0, len(pending))
	for _, item := range pending {
		event := item.event
		event.ID = s.eventIDFor(item.identity)
		events = append(events, event)
	}
	prepared, err := s.append(state.SessionID, events)
	if err != nil {
		return sessionProtocolError("append session events", err)
	}
	// The append is durable now, so the log progress advances here. A later
	// snapshot failure marks the checkpoint as behind instead of undoing it.
	s.acceptAppended(prepared)
	// A completion whose own append failed is retried here, on the first append
	// that succeeds, so a transient write failure does not leave the intent in the
	// log as a permanent unknown outcome.
	if err := s.flushCompensations(); err != nil {
		return err
	}
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

// RecordTurn persists one committed transcript turn as soon as the request
// commits it.
//
// Sync writes the whole conversation at the end of a request, so a crash in the
// middle used to leave the log holding only the lifecycle evidence of a side
// effect while the turn that produced it was never written: the file existed and
// the conversation did not mention it. This writes the turn while the request is
// still running.
//
// It is synchronous on purpose, and it is not a queue: the append completes, in
// transcript order, before the loop continues. The identity and therefore the
// event ID are derived from the turn ID, which is the same identity Sync uses, so
// an incremental write and a later replay of the same turn are the same event and
// the log keeps exactly one copy.
func (s *sessionRuntime) RecordTurn(turn protocol.Turn) error {
	if s == nil || turn.ID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	event := session.Event{Type: session.EventTurnAppended, ID: s.eventIDFor("turn:" + turn.ID), Turn: &turn}
	prepared, err := s.append(s.snapshot.SessionID, []session.Event{event})
	if err != nil {
		return sessionProtocolError("append committed turn", err)
	}
	// The append is durable, so the log progress advances here: the later Sync
	// must not write this turn a second time.
	s.acceptAppended(prepared)
	// The snapshot is behind the log until the next Sync; that is the same state
	// a failed snapshot save already leaves, and recovery replays the log.
	s.snapshotPending = true
	return nil
}

// RecordRequestStarted brackets a request in the log, before its first model call.
//
// It exists so a log entry that is followed by no report can be read as "the
// request was interrupted" rather than as "no request happened", which is the one
// question the completion-side records cannot answer on their own.
func (s *sessionRuntime) RecordRequestStarted(requestID string) error {
	if s == nil || requestID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	events := []session.Event{{Type: session.EventRequestStarted, RequestID: requestID, ID: s.eventIDFor("request:" + requestID)}}
	appended, err := s.append(s.snapshot.SessionID, events)
	if err != nil {
		return sessionProtocolError("record request start", err)
	}
	s.acceptAppended(appended)
	return nil
}

// RecordToolPrepared records that the executor accepted one call for execution.
//
// The call's identity comes from the conversation's pending set, which is the same
// source the set itself is built from, so the event and the view cannot disagree
// about which call was prepared or which round it belonged to.
func (s *sessionRuntime) RecordToolPrepared(conversation *agent.Conversation, call protocol.ToolCall) error {
	if s == nil || call.ID == "" {
		return nil
	}
	entry, ok := pendingEntryFor(conversation, call.ID)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared := session.ToolPrepared{
		RequestID: entry.RequestID, CallID: call.ID, Tool: call.Name,
		Iteration: entry.Iteration, PreparedAt: time.Now().UTC(),
	}
	events := []session.Event{{Type: session.EventToolPrepared, Prepared: &prepared, ID: s.eventIDFor("prepared:" + call.ID)}}
	appended, err := s.append(s.snapshot.SessionID, events)
	if err != nil {
		return sessionProtocolError("record prepared tool call", err)
	}
	s.acceptAppended(appended)
	return nil
}

// pendingEntryFor looks one call up in the conversation's pending view.
func pendingEntryFor(conversation *agent.Conversation, callID string) (agent.PendingCall, bool) {
	if conversation == nil {
		return agent.PendingCall{}, false
	}
	for _, entry := range conversation.PendingCalls() {
		if entry.CallID == callID {
			return entry, true
		}
	}
	return agent.PendingCall{}, false
}

// RecordIntents persists the intents of one batch in a single append.
//
// One write for the whole batch is what turns 2N appends for N side-effecting
// calls into one, without weakening the guarantee: the append still completes
// before the first call of the batch starts, so no side effect can begin before
// its intent is durable. A batch that cannot be written prevents every call in it
// from running, which is the same fail-closed rule as the per-call path.
func (s *sessionRuntime) RecordIntents(intents []tool.Intent) error {
	if s == nil || len(intents) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at := time.Now().UTC()
	events := make([]session.Event, 0, len(intents))
	records := make([]session.ToolIntent, 0, len(intents))
	for _, intent := range intents {
		record := session.ToolIntent{
			RequestID: intent.RequestID, CallID: intent.CallID, ParentCallID: intent.ParentCallID,
			Tool: intent.Tool, Risk: string(intent.Risk), TargetPath: intent.TargetPath,
			PreviousHash: intent.PreviousHash, StartedAt: at,
		}
		// The identity is the call, so a retry after an uncertain append is the
		// same event rather than a second intent.
		events = append(events, session.Event{Type: session.EventToolExecutionIntent, ID: s.eventIDFor("intent:" + intent.CallID), Intent: &record})
		records = append(records, record)
	}
	appended, err := s.append(s.snapshot.SessionID, events)
	if err != nil {
		return sessionProtocolError("record tool intents", err)
	}
	s.acceptAppended(appended)
	for _, record := range records {
		s.rememberPendingIntent(record)
	}
	return nil
}

// rememberPendingIntent mirrors one confirmed intent into the in-memory snapshot,
// so the next save carries it.
func (s *sessionRuntime) rememberPendingIntent(record session.ToolIntent) {
	for index, existing := range s.snapshot.PendingIntents {
		if existing.CallID == record.CallID {
			s.snapshot.PendingIntents[index] = record
			return
		}
	}
	s.snapshot.PendingIntents = append(s.snapshot.PendingIntents, record)
}

// RecordIntent persists the intent to run one side-effecting call before it
// starts.
//
// The write is synchronous on purpose: the guarantee is that a tool never runs
// before its intent is durable. An error is returned to the executor, which then
// refuses to start the call.
func (s *sessionRuntime) RecordIntent(intent tool.Intent) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := session.ToolIntent{
		RequestID: intent.RequestID, CallID: intent.CallID, ParentCallID: intent.ParentCallID,
		Tool: intent.Tool, Risk: string(intent.Risk), TargetPath: intent.TargetPath,
		PreviousHash: intent.PreviousHash, StartedAt: time.Now().UTC(),
	}
	events, err := s.append(s.snapshot.SessionID, []session.Event{{Type: session.EventToolExecutionIntent, Intent: &record}})
	if err != nil {
		return sessionProtocolError("record tool intent", err)
	}
	s.acceptAppended(events)
	// The in-memory snapshot mirrors the log, so the next save carries the intent.
	s.rememberPendingIntent(record)
	return nil
}

// RecordCompletion persists the terminal outcome of one call.
//
// The operation itself already happened, so an error here is reported as "the
// result may exist but its record does not": the in-memory result is kept and the
// batch stops starting new side effects.
func (s *sessionRuntime) RecordCompletion(completion tool.Completion) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := session.ToolCompletion{
		CallID: completion.CallID, Tool: completion.Tool, State: string(completion.State),
		IsError: completion.IsError, TargetPath: completion.TargetPath, ResultHash: completion.ResultHash,
		CompletedAt: time.Now().UTC(),
	}
	events, err := s.append(s.snapshot.SessionID, []session.Event{{Type: session.EventToolCompleted, ID: s.eventIDFor("completion:" + record.CallID), Completion: &record}})
	if err != nil {
		// The operation already happened, so the result is not thrown away: it is
		// held here and written by the next successful append. Without this, one
		// transient write failure left the intent in the log forever and the next
		// load reported a call whose outcome we knew as "unknown".
		s.compensations = append(s.compensations, record)
		return sessionProtocolError("record tool completion", err)
	}
	s.acceptAppended(events)
	s.forgetPendingIntent(record.CallID)
	return nil
}

// forgetPendingIntent drops the intent a confirmed completion closed.
func (s *sessionRuntime) forgetPendingIntent(callID string) {
	kept := s.snapshot.PendingIntents[:0]
	for _, intent := range s.snapshot.PendingIntents {
		if intent.CallID == callID {
			continue
		}
		kept = append(kept, intent)
	}
	s.snapshot.PendingIntents = kept
}

// flushCompensations writes the completions whose own append failed.
//
// It is called on every append path, so the record of an operation that really
// happened is retried as soon as the log accepts writes again. The identity of the
// event is the call, so replaying it is the same event rather than a second
// completion, and a caller that never manages another append loses nothing that
// the in-memory result did not already hold.
func (s *sessionRuntime) flushCompensations() error {
	if len(s.compensations) == 0 {
		return nil
	}
	events := make([]session.Event, 0, len(s.compensations))
	for _, record := range s.compensations {
		compensation := record
		events = append(events, session.Event{Type: session.EventToolCompleted, ID: s.eventIDFor("completion:" + record.CallID), Completion: &compensation})
	}
	appended, err := s.append(s.snapshot.SessionID, events)
	if err != nil {
		return sessionProtocolError("record compensating tool completions", err)
	}
	s.acceptAppended(appended)
	for _, record := range s.compensations {
		s.forgetPendingIntent(record.CallID)
	}
	s.compensations = nil
	return nil
}

// Compensations reports how many completions are waiting for the log to accept
// writes. It is a diagnostic: a nonzero value means the in-memory result is ahead
// of the record.
func (s *sessionRuntime) Compensations() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.compensations)
}

// RecordRunReport persists the summary of one finished request, so why it stopped
// and what it did can be explained from the log instead of only from a transient
// UI event.
func (s *sessionRuntime) RecordRunReport(report agent.RunReport) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A provider error can echo a credential, so it goes through the same
	// redaction as the session's own last error before it is written to the log.
	message := report.Error
	if message != "" && s.redact != nil {
		message = s.redact(message)
	}
	summary := session.RunSummary{
		RequestID: report.RequestID, Iterations: report.Iterations, StopReason: report.StopReason,
		Error: message, ModelCalls: report.ModelCalls, ToolCalls: report.ToolCalls,
		Succeeded: report.Succeeded, Failed: report.Failed, Rejected: report.Rejected,
		Cancelled: report.Cancelled, NotExecuted: report.NotExecuted, OutcomeUnknown: report.OutcomeUnknown,
		InputTokens: report.InputTokens, OutputTokens: report.OutputTokens, ExactUsage: report.ExactUsage,
		ReportedAt: time.Now().UTC(),
	}
	events, err := s.append(s.snapshot.SessionID, []session.Event{{Type: session.EventRunReported, Run: &summary}})
	if err != nil {
		return sessionProtocolError("record run report", err)
	}
	s.acceptAppended(events)
	s.snapshot.LastRun = &summary
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

// PendingIntents reports executions that started without a recorded outcome.
// They are never replayed; the caller reports them so a human can verify the
// effect with the recorded hints.
func (s *sessionRuntime) PendingIntents() []session.ToolIntent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]session.ToolIntent(nil), s.snapshot.PendingIntents...)
}

// describePendingIntent renders one started-without-outcome execution as the host
// reports it.
//
// The conclusion is drawn from evidence, not from a guess, and every message says
// which evidence it rests on:
//
//   - the recorded content hash matches what is on disk now, so the operation
//     cannot have changed the target: it did not happen, with proof.
//   - the content differs, so something changed the target after the intent was
//     written. That is not proof that this call did it, so the outcome stays
//     unknown and a human is asked to confirm.
//   - there is no hash evidence at all (the target did not exist, or the call
//     never described one), so the outcome stays unknown and the message says why
//     rather than implying the file was inspected.
//
// It is never replayed in any case.
func describePendingIntent(intent session.ToolIntent) string {
	target := strings.TrimSpace(intent.TargetPath)
	if target == "" {
		message := fmt.Sprintf("tool %s started but its outcome was not recorded (outcome unknown; it was not replayed)", intent.Tool)
		if intent.PreviousHash != "" {
			message += fmt.Sprintf("; its content before the call had hash %s", intent.PreviousHash)
		}
		return message
	}
	current, readable := fileHashEvidence(target)
	// Evidence that is a size and a timestamp rather than a content hash cannot prove
	// anything: a rewrite that preserves both leaves the same value. It is reported
	// as a hint and never as a conclusion.
	weak := strings.HasPrefix(intent.PreviousHash, "weak:")
	switch {
	case weak:
		state := "still matches"
		if !readable || current != intent.PreviousHash {
			state = "no longer matches"
		}
		return fmt.Sprintf("tool %s on %s started but its outcome was not recorded (outcome unknown: the evidence recorded for the target is weak (%s) - size and modification time, not a content hash - and it %s now, which is a hint and not proof; it was not replayed)", intent.Tool, target, intent.PreviousHash, state)
	case intent.PreviousHash != "" && readable && current == intent.PreviousHash:
		return fmt.Sprintf("tool %s on %s did not change its target: the content still matches the hash recorded before the call (%s), so the operation is judged not to have happened (it was not replayed)", intent.Tool, target, intent.PreviousHash)
	case intent.PreviousHash != "" && readable:
		return fmt.Sprintf("tool %s on %s started and its target changed after the intent was recorded; the content before the call had hash %s and it now has %s, so the outcome is unknown: this call is not proven to be the cause (it was not replayed)", intent.Tool, target, intent.PreviousHash, current)
	case intent.PreviousHash != "":
		return fmt.Sprintf("tool %s on %s started but its outcome was not recorded (outcome unknown: the target cannot be read now, so the hash recorded before the call (%s) cannot be compared; it was not replayed)", intent.Tool, target, intent.PreviousHash)
	default:
		return fmt.Sprintf("tool %s on %s started but its outcome was not recorded (outcome unknown: no hash was recorded before the call, so the current content proves nothing; it was not replayed)", intent.Tool, target)
	}
}

// fileHashEvidence returns the hash of a file's current content, and whether it
// could be read at all. A directory or a missing file yields no hash, which is an
// absence of evidence rather than evidence of absence.
func fileHashEvidence(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
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
		// The lifecycle records and the last run summary are owned by their events,
		// so a rebuild carries them forward instead of dropping them.
		PendingIntents: append([]session.ToolIntent(nil), previous.PendingIntents...),
		LastRun:        previous.LastRun,
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
