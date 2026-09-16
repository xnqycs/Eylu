package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

const (
	AttachmentThreshold = 16 << 10
	MaxAttachmentBytes  = 16 << 20
	maxEventLineBytes   = 8 << 20
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type Store struct {
	mu        sync.Mutex
	root      string
	sequences map[string]uint64
	// eventIDs indexes every event ID the log already holds, per session, with a
	// digest of its content. It lets a repeated append be recognized as a retry of
	// one logical event instead of being written a second time, and it detects the
	// same ID carrying different content.
	eventIDs map[string]map[string]storedEvent
}

// storedEvent is one entry of the per-session event index.
type storedEvent struct {
	fingerprint string
	sequence    uint64
}

// eventContentFingerprint returns a stable digest of an event's logical content.
// The fields the store assigns (version, sequence, session ID, timestamp) and the
// event ID itself are excluded, so a retried event compares equal to the copy the
// log already holds.
func eventContentFingerprint(event Event) string {
	event.Version, event.Sequence, event.SessionID, event.At, event.ID = 0, 0, "", time.Time{}, ""
	encoded, err := json.Marshal(event)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// turnContentFingerprint returns a stable digest of what a turn says.
//
// The turn ID is the identity being compared, and the timestamp is what the
// writer happened to record, so neither is part of the content: two copies of the
// same turn written by different processes differ only in those.
func turnContentFingerprint(turn protocol.Turn) string {
	turn.CreatedAt = time.Time{}
	encoded, err := json.Marshal(turn)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// derivedEventID names an event that was written before stable IDs existed. The
// derivation matches what the writer would have produced, so old and new logs
// share one identity space.
func derivedEventID(sessionID string, sequence uint64) string {
	return fmt.Sprintf("%s:%d", sessionID, sequence)
}

func Open(root string) (*Store, error) {
	if root == "" {
		root = DefaultRoot()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create session root: %w", err)
	}
	return &Store{root: absolute, sequences: make(map[string]uint64), eventIDs: make(map[string]map[string]storedEvent)}, nil
}

func DefaultRoot() string {
	if state := os.Getenv("EYLU_STATE_DIR"); state != "" {
		return filepath.Join(state, "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".eylu", "state", "sessions")
	}
	return filepath.Join(home, ".eylu", "state", "sessions")
}

func ValidID(id string) bool { return sessionIDPattern.MatchString(id) }

func (s *Store) Root() string { return s.root }

func (s *Store) Create(snapshot Snapshot) (Snapshot, error) {
	if !ValidID(snapshot.SessionID) {
		return Snapshot{}, fmt.Errorf("invalid session ID %q", snapshot.SessionID)
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = time.Now().UTC()
	}
	snapshot.UpdatedAt = snapshot.CreatedAt
	snapshot.Version = SchemaVersion
	provider := snapshot.Provider
	var environmentContext *environment.Context
	if !snapshot.Environment.Empty() {
		captured := snapshot.Environment
		environmentContext = &captured
	}
	events, err := s.Append(snapshot.SessionID, []Event{{
		Type: EventSessionCreated, Workspace: snapshot.Workspace, Environment: environmentContext, PermissionMode: snapshot.PermissionMode, Provider: &provider,
	}})
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Sequence = events[len(events)-1].Sequence
	if err := s.Save(snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// Append writes events to the log and returns the events the log now holds.
//
// An event that carries an ID the log already contains is either a retry of one
// logical event (the same content) or a conflict (different content). A retry is
// reported as accepted without writing a second copy, so re-submitting a batch
// never duplicates a turn, a prompt or a state change. A conflict is refused
// instead of silently overwriting the stored event.
//
// When the write result is uncertain the cached tail and event index are dropped,
// so the next attempt re-reads and re-confirms the log before it continues the
// sequence.
func (s *Store) Append(id string, events []Event) ([]Event, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	if len(events) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.sessionDirectory(id, true)
	if err != nil {
		return nil, err
	}
	index, err := s.eventIndexLocked(id, directory)
	if err != nil {
		return nil, err
	}
	sequence, ok := s.sequences[id]
	if !ok {
		sequence, err = lastEventSequence(filepath.Join(directory, "events.jsonl"))
		if err != nil {
			return nil, err
		}
	}
	var encoded bytes.Buffer
	prepared := make([]Event, len(events))
	// The index is only merged into the store once the write succeeded, so a
	// failed attempt never claims an ID the log does not contain.
	batch := make(map[string]storedEvent, len(events))
	written := 0
	for position, event := range events {
		event.Version = SchemaVersion
		event.SessionID = id
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		if event.Turn != nil {
			turn, prepareErr := s.externalizeTurn(directory, *event.Turn)
			if prepareErr != nil {
				return nil, prepareErr
			}
			event.Turn = &turn
		}
		// The digest describes exactly what would be written, so a retry of the
		// same logical event compares equal to the copy already in the log.
		fingerprint := eventContentFingerprint(event)
		if event.ID != "" {
			existing, duplicate := index[event.ID]
			if !duplicate {
				existing, duplicate = batch[event.ID]
			}
			if duplicate {
				if existing.fingerprint != fingerprint {
					return nil, fmt.Errorf("session event ID %q already exists with different content", event.ID)
				}
				retried := event
				retried.Sequence = existing.sequence
				prepared[position] = retried
				continue
			}
		}
		sequence++
		event.Sequence = sequence
		if event.ID == "" {
			event.ID = derivedEventID(id, sequence)
		}
		line, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return nil, marshalErr
		}
		encoded.Write(line)
		encoded.WriteByte('\n')
		batch[event.ID] = storedEvent{fingerprint: fingerprint, sequence: sequence}
		prepared[position] = event
		written++
	}
	if written == 0 {
		return prepared, nil
	}
	file, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		s.forgetSessionLocked(id)
		return nil, err
	}
	if _, err = file.Write(encoded.Bytes()); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		// Bytes may already have reached the log, so the cached tail and index are
		// no longer trustworthy: the next attempt re-reads the log first.
		s.forgetSessionLocked(id)
		if err != nil {
			return nil, err
		}
		return nil, closeErr
	}
	s.sequences[id] = sequence
	for eventID, entry := range batch {
		index[eventID] = entry
	}
	return prepared, nil
}

// forgetSessionLocked invalidates the cached tail and event index of one session.
func (s *Store) forgetSessionLocked(id string) {
	delete(s.sequences, id)
	delete(s.eventIDs, id)
}

// eventIndexLocked returns the per-session event index, reading the log once.
func (s *Store) eventIndexLocked(id, directory string) (map[string]storedEvent, error) {
	if index, ok := s.eventIDs[id]; ok {
		return index, nil
	}
	events, _, _, err := readEventsDetailed(filepath.Join(directory, "events.jsonl"), id)
	if err != nil {
		return nil, err
	}
	index := make(map[string]storedEvent, len(events))
	for _, event := range events {
		eventID := event.ID
		if eventID == "" {
			eventID = derivedEventID(id, event.Sequence)
		}
		index[eventID] = storedEvent{fingerprint: eventContentFingerprint(event), sequence: event.Sequence}
	}
	s.eventIDs[id] = index
	return index, nil
}

func (s *Store) Save(snapshot Snapshot) error {
	if !ValidID(snapshot.SessionID) {
		return fmt.Errorf("invalid session ID %q", snapshot.SessionID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.sessionDirectory(snapshot.SessionID, true)
	if err != nil {
		return err
	}
	snapshot.Version = SchemaVersion
	snapshot.UpdatedAt = time.Now().UTC()
	snapshot.Turns = append([]protocol.Turn(nil), snapshot.Turns...)
	for index := range snapshot.Turns {
		snapshot.Turns[index], err = s.externalizeTurn(directory, snapshot.Turns[index])
		if err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(directory, "snapshot.json"), data, 0o600); err != nil {
		return err
	}
	if snapshot.Sequence > s.sequences[snapshot.SessionID] {
		s.sequences[snapshot.SessionID] = snapshot.Sequence
	}
	return nil
}

func (s *Store) Load(id string) (Snapshot, []Diagnostic, error) {
	return s.load(id, false)
}

func (s *Store) LoadRecovering(id string) (Snapshot, []Diagnostic, error) {
	return s.load(id, true)
}

func (s *Store) load(id string, repairEventTail bool) (Snapshot, []Diagnostic, error) {
	if !ValidID(id) {
		return Snapshot{}, nil, fmt.Errorf("invalid session ID %q", id)
	}
	directory, err := s.sessionDirectory(id, false)
	if err != nil {
		return Snapshot{}, nil, err
	}
	var snapshot Snapshot
	diagnostics := make([]Diagnostic, 0)
	snapshotPath := filepath.Join(directory, "snapshot.json")
	data, readErr := os.ReadFile(snapshotPath)
	snapshotMissing := errors.Is(readErr, os.ErrNotExist)
	if readErr == nil {
		var header struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &header) == nil && !readableSchemaVersion(header.Version) {
			return Snapshot{}, diagnostics, schemaError(header.Version, id)
		}
		if err := json.Unmarshal(data, &snapshot); err != nil {
			diagnostics = append(diagnostics, Diagnostic{Path: snapshotPath, Message: "snapshot ignored: " + err.Error()})
			snapshot = Snapshot{Version: SchemaVersion, SessionID: id}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Snapshot{}, diagnostics, readErr
	} else {
		snapshot = Snapshot{Version: SchemaVersion, SessionID: id}
	}
	if snapshot.SessionID != "" && snapshot.SessionID != id {
		return Snapshot{}, diagnostics, fmt.Errorf("snapshot session ID %q does not match %q", snapshot.SessionID, id)
	}
	snapshot.SessionID = id
	eventsPath := filepath.Join(directory, "events.jsonl")
	_, eventsStatErr := os.Stat(eventsPath)
	eventsMissing := errors.Is(eventsStatErr, os.ErrNotExist)
	if eventsStatErr != nil && !eventsMissing {
		return Snapshot{}, diagnostics, eventsStatErr
	}
	if !repairEventTail && snapshotMissing && eventsMissing {
		return Snapshot{}, diagnostics, fmt.Errorf("session %q has no snapshot or event log", id)
	}
	events, eventDiagnostics, validEventBytes, err := readEventsDetailed(eventsPath, id)
	diagnostics = append(diagnostics, eventDiagnostics...)
	if err != nil {
		return Snapshot{}, diagnostics, err
	}
	if repairEventTail && len(eventDiagnostics) > 0 {
		if err := truncateEventTail(eventsPath, validEventBytes); err != nil {
			return Snapshot{}, diagnostics, fmt.Errorf("repair session event tail: %w", err)
		}
	}
	// A duplicate event ID is a conservative diagnostic: an identical copy is the
	// same logical event replayed, so it is not applied twice, and a conflicting
	// copy is reported instead of being guessed at. The raw log keeps both.
	seenEventIDs := make(map[string]string, len(events))
	// A log written before event IDs existed can hold the same turn twice under
	// two different (or absent) event IDs. The turn ID is what the transcript is
	// built from, so a repeated one is diagnosed the same conservative way: an
	// identical copy is merged, a conflicting one is reported and the raw log is
	// left exactly as it is. Neither case rewrites the file, because the log is
	// evidence and a guess about which copy is right would destroy it.
	seenTurnIDs := make(map[string]string, len(events))
	skip := make(map[int]struct{})
	for position, event := range events {
		if event.Type == EventTurnAppended && event.Turn != nil && event.Turn.ID != "" {
			fingerprint := turnContentFingerprint(*event.Turn)
			previous, duplicate := seenTurnIDs[event.Turn.ID]
			if !duplicate {
				seenTurnIDs[event.Turn.ID] = fingerprint
			} else {
				message := fmt.Sprintf("merged turn %q repeated at sequence %d", event.Turn.ID, event.Sequence)
				benign := true
				if previous != fingerprint {
					message = fmt.Sprintf("turn %q at sequence %d repeats an earlier turn ID with different content: the transcript keeps the first copy and the session needs manual confirmation", event.Turn.ID, event.Sequence)
					benign = false
				}
				diagnostics = append(diagnostics, Diagnostic{Path: eventsPath, Message: message, Benign: benign})
				skip[position] = struct{}{}
				continue
			}
		}
		if event.ID == "" {
			continue
		}
		fingerprint := eventContentFingerprint(event)
		previous, duplicate := seenEventIDs[event.ID]
		if !duplicate {
			seenEventIDs[event.ID] = fingerprint
			continue
		}
		message := fmt.Sprintf("ignored repeated event ID %q at sequence %d", event.ID, event.Sequence)
		benign := true
		if previous != fingerprint {
			message = fmt.Sprintf("ignored conflicting content for event ID %q at sequence %d", event.ID, event.Sequence)
			benign = false
		}
		diagnostics = append(diagnostics, Diagnostic{Path: eventsPath, Message: message, Benign: benign})
		skip[position] = struct{}{}
	}
	for position, event := range events {
		if _, ignored := skip[position]; ignored {
			continue
		}
		if event.Sequence <= snapshot.Sequence {
			continue
		}
		if snapshot.Sequence > 0 && event.Sequence != snapshot.Sequence+1 {
			return Snapshot{}, diagnostics, fmt.Errorf("session event sequence gap: have %d, next %d", snapshot.Sequence, event.Sequence)
		}
		applyEvent(&snapshot, event)
	}
	if snapshot.CreatedAt.IsZero() && len(events) > 0 {
		snapshot.CreatedAt = events[0].At
	}
	for turnIndex := range snapshot.Turns {
		diagnostics = append(diagnostics, s.hydrateTurn(directory, &snapshot.Turns[turnIndex])...)
	}
	s.mu.Lock()
	s.sequences[id] = snapshot.Sequence
	s.mu.Unlock()
	return snapshot, diagnostics, nil
}

func (s *Store) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	result := make([]SessionInfo, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !ValidID(entry.Name()) {
			continue
		}
		snapshot, diagnostics, loadErr := s.LoadRecovering(entry.Name())
		info := SessionInfo{SessionID: entry.Name()}
		if loadErr != nil {
			info.Diagnostic = loadErr.Error()
		} else {
			info.Loadable = true
			info.Workspace = snapshot.Workspace
			info.Mode = snapshot.PermissionMode
			info.Provider = snapshot.Provider.Name
			info.Model = snapshot.Provider.Model
			info.Turns = len(snapshot.Turns)
			info.UpdatedAt = snapshot.UpdatedAt
			info.ClosedAt = snapshot.ClosedAt
			if len(diagnostics) > 0 {
				info.Diagnostic = diagnostics[0].Message
			}
		}
		info.Bytes, _ = directorySize(filepath.Join(s.root, entry.Name()))
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].SessionID < result[j].SessionID
		}
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *Store) Latest(workspace string) (SessionInfo, bool, error) {
	items, err := s.List()
	if err != nil {
		return SessionInfo{}, false, err
	}
	for _, item := range items {
		if item.Loadable && (workspace == "" || samePath(item.Workspace, workspace)) {
			return item, true, nil
		}
	}
	return SessionInfo{}, false, nil
}

func (s *Store) Delete(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid session ID %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.sessionDirectory(id, false)
	if err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("session target is not a regular directory")
	}
	relative, err := filepath.Rel(s.root, directory)
	if err != nil || relative != id || filepath.IsAbs(relative) {
		return fmt.Errorf("session target escapes the store root")
	}
	if err := os.RemoveAll(directory); err != nil {
		return err
	}
	delete(s.sequences, id)
	return nil
}

func (s *Store) Cleanup(maxSessions int, maxBytes int64, protectedID string) ([]string, error) {
	items, err := s.List()
	if err != nil {
		return nil, err
	}
	if maxSessions <= 0 && maxBytes <= 0 {
		return nil, nil
	}
	sort.SliceStable(items, func(i, j int) bool {
		leftClosed, rightClosed := items[i].ClosedAt != nil, items[j].ClosedAt != nil
		if leftClosed != rightClosed {
			return leftClosed
		}
		return items[i].UpdatedAt.Before(items[j].UpdatedAt)
	})
	totalBytes := int64(0)
	for _, item := range items {
		totalBytes += item.Bytes
	}
	remaining := len(items)
	deleted := make([]string, 0)
	for _, item := range items {
		overCount := maxSessions > 0 && remaining > maxSessions
		overBytes := maxBytes > 0 && totalBytes > maxBytes
		if (!overCount && !overBytes) || item.SessionID == protectedID {
			continue
		}
		if err := s.Delete(item.SessionID); err != nil {
			return deleted, err
		}
		deleted = append(deleted, item.SessionID)
		remaining--
		totalBytes -= item.Bytes
	}
	return deleted, nil
}

func (s *Store) Migrate(id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid session ID %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := s.sessionDirectory(id, false)
	if err != nil {
		return err
	}
	snapshotPath := filepath.Join(directory, "snapshot.json")
	if data, readErr := os.ReadFile(snapshotPath); readErr == nil {
		migrated, previous, changed, migrateErr := migrateJSONDocument(data)
		if migrateErr != nil {
			return migrateErr
		}
		if changed {
			// The backup keeps the only copy of the older document, so a failed
			// migration can never destroy the data it was migrating.
			if err := writeAtomic(fmt.Sprintf("%s.v%d.bak", snapshotPath, previous), data, 0o600); err != nil {
				return err
			}
			if err := writeAtomic(snapshotPath, migrated, 0o600); err != nil {
				return err
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	eventsPath := filepath.Join(directory, "events.jsonl")
	if data, readErr := os.ReadFile(eventsPath); readErr == nil {
		lines := bytes.Split(data, []byte{'\n'})
		var output bytes.Buffer
		changed := false
		previousVersion := SchemaVersion
		for _, line := range lines {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			migrated, previous, lineChanged, migrateErr := migrateJSONDocument(line)
			if migrateErr != nil {
				return migrateErr
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, migrated); err != nil {
				return err
			}
			changed = changed || lineChanged
			if lineChanged {
				previousVersion = previous
			}
			output.Write(compact.Bytes())
			output.WriteByte('\n')
		}
		if changed {
			if err := writeAtomic(fmt.Sprintf("%s.v%d.bak", eventsPath, previousVersion), data, 0o600); err != nil {
				return err
			}
			if err := writeAtomic(eventsPath, output.Bytes(), 0o600); err != nil {
				return err
			}
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	delete(s.sequences, id)
	delete(s.eventIDs, id)
	return nil
}

func migrateJSONDocument(data []byte) ([]byte, int, bool, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, 0, false, err
	}
	version := 0
	if raw, ok := document["version"].(float64); ok {
		version = int(raw)
	}
	if !migratableSchemaVersion(version) {
		return nil, version, false, fmt.Errorf("unsupported session schema version %d", version)
	}
	if version == SchemaVersion {
		return data, version, false, nil
	}
	document["version"] = SchemaVersion
	migrated, err := json.MarshalIndent(document, "", "  ")
	return migrated, version, true, err
}

func (s *Store) sessionDirectory(id string, create bool) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid session ID %q", id)
	}
	directory := filepath.Join(s.root, id)
	relative, err := filepath.Rel(s.root, directory)
	if err != nil || relative != id || filepath.IsAbs(relative) {
		return "", fmt.Errorf("session path escapes store root")
	}
	if create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", err
		}
		return directory, nil
	}
	info, err := os.Stat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("session path is not a directory")
	}
	return directory, nil
}

func (s *Store) externalizeTurn(directory string, turn protocol.Turn) (protocol.Turn, error) {
	turn.Parts = append([]protocol.Part(nil), turn.Parts...)
	for index, part := range turn.Parts {
		if part.ToolResult == nil || len([]byte(part.ToolResult.Content)) <= AttachmentThreshold {
			continue
		}
		toolResult := *part.ToolResult
		data := []byte(toolResult.Content)
		if len(data) > MaxAttachmentBytes {
			return protocol.Turn{}, fmt.Errorf("tool result attachment exceeds %d bytes", MaxAttachmentBytes)
		}
		digest := sha256.Sum256(data)
		hexDigest := hex.EncodeToString(digest[:])
		attachmentDirectory := filepath.Join(directory, "attachments")
		if err := os.MkdirAll(attachmentDirectory, 0o700); err != nil {
			return protocol.Turn{}, err
		}
		name := hexDigest + ".txt"
		path := filepath.Join(attachmentDirectory, name)
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, data) {
			if err := writeAtomic(path, data, 0o600); err != nil {
				return protocol.Turn{}, err
			}
		}
		toolResult.Content = attachmentSummary(toolResult.Content)
		toolResult.Metadata = cloneMetadata(toolResult.Metadata)
		toolResult.Metadata["session_attachment"] = AttachmentRef{Path: filepath.ToSlash(filepath.Join("attachments", name)), SHA256: hexDigest, Bytes: len(data)}
		turn.Parts[index].ToolResult = &toolResult
	}
	return turn, nil
}

func (s *Store) hydrateTurn(directory string, turn *protocol.Turn) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	for index, part := range turn.Parts {
		if part.ToolResult == nil || part.ToolResult.Metadata == nil {
			continue
		}
		ref, ok := decodeAttachmentRef(part.ToolResult.Metadata["session_attachment"])
		if !ok {
			continue
		}
		if !validAttachmentRef(ref) {
			diagnostics = append(diagnostics, Diagnostic{Path: ref.Path, Message: "invalid session attachment reference"})
			continue
		}
		path := filepath.Join(directory, filepath.FromSlash(ref.Path))
		relative, relErr := filepath.Rel(directory, path)
		if relErr != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			diagnostics = append(diagnostics, Diagnostic{Path: path, Message: "attachment path escaped session directory"})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > MaxAttachmentBytes {
			diagnostics = append(diagnostics, Diagnostic{Path: path, Message: "attachment unavailable or oversized"})
			continue
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != ref.SHA256 || len(data) != ref.Bytes {
			diagnostics = append(diagnostics, Diagnostic{Path: path, Message: "attachment digest or size mismatch"})
			continue
		}
		toolResult := *part.ToolResult
		toolResult.Content = string(data)
		turn.Parts[index].ToolResult = &toolResult
	}
	return diagnostics
}

func validAttachmentRef(ref AttachmentRef) bool {
	if ref.Bytes <= AttachmentThreshold || ref.Bytes > MaxAttachmentBytes || len(ref.SHA256) != sha256.Size*2 {
		return false
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return false
	}
	expected := filepath.ToSlash(filepath.Join("attachments", ref.SHA256+".txt"))
	return ref.Path == expected
}

func decodeAttachmentRef(value any) (AttachmentRef, bool) {
	switch typed := value.(type) {
	case AttachmentRef:
		return typed, typed.Path != ""
	case map[string]any:
		ref := AttachmentRef{}
		ref.Path, _ = typed["path"].(string)
		ref.SHA256, _ = typed["sha256"].(string)
		if number, ok := typed["bytes"].(float64); ok {
			ref.Bytes = int(number)
		}
		return ref, ref.Path != "" && ref.SHA256 != ""
	default:
		return AttachmentRef{}, false
	}
}

func attachmentSummary(value string) string {
	const kept = 2048
	if len(value) <= kept*2 {
		return value
	}
	start := kept
	for start > 0 && !utf8.RuneStart(value[start]) {
		start--
	}
	end := len(value) - kept
	for end < len(value) && !utf8.RuneStart(value[end]) {
		end++
	}
	return value[:start] + fmt.Sprintf("\n[session attachment: original_bytes=%d]\n", len(value)) + value[end:]
}

func cloneMetadata(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func lastEventSequence(path string) (uint64, error) {
	events, _, err := readEvents(path, "")
	if err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	return events[len(events)-1].Sequence, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) (returnErr error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".session-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporaryName, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// readableSchemaVersion reports whether this build can read a document written by
// another schema version. An older document is readable when it is still in the
// supported range; anything else is refused explicitly instead of being guessed
// at.
func readableSchemaVersion(version int) bool {
	return version >= MinReadableSchemaVersion && version <= SchemaVersion
}

// migratableSchemaVersion reports whether Migrate can upgrade a document. It
// accepts more than the reader does, because upgrading an older session is the
// supported way to bring it forward: the migration writes a backup of the original
// document before it rewrites anything.
func migratableSchemaVersion(version int) bool {
	return version >= 0 && version <= SchemaVersion
}

func schemaError(version int, id string) error {
	return fmt.Errorf("session %q uses unsupported schema version %d (current %d); run `eylu sessions migrate %s`", id, version, SchemaVersion, id)
}

func readEvents(path, expectedSessionID string) ([]Event, []Diagnostic, error) {
	events, diagnostics, _, err := readEventsDetailed(path, expectedSessionID)
	return events, diagnostics, err
}

func readEventsDetailed(path, expectedSessionID string) ([]Event, []Diagnostic, int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 64<<10)
	events := make([]Event, 0)
	diagnostics := make([]Diagnostic, 0)
	lineNumber := 0
	var previous uint64
	var validBytes int64
	for {
		line, complete, eof, readErr := readBoundedEventLine(reader)
		lineNumber++
		if readErr != nil {
			return nil, diagnostics, validBytes, fmt.Errorf("read session event line %d: %w", lineNumber, readErr)
		}
		trimmed := bytes.TrimSpace(line)
		if eof && !complete && len(trimmed) > 0 {
			diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("ignored incomplete event tail at line %d", lineNumber)})
			break
		}
		if len(trimmed) > 0 {
			var header struct {
				Version   int    `json:"version"`
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(trimmed, &header); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("ignored damaged event tail at line %d: %v", lineNumber, err)})
				break
			}
			if !readableSchemaVersion(header.Version) {
				id := expectedSessionID
				if id == "" {
					id = header.SessionID
				}
				return nil, diagnostics, validBytes, schemaError(header.Version, id)
			}
			var event Event
			if err := json.Unmarshal(trimmed, &event); err != nil {
				diagnostics = append(diagnostics, Diagnostic{Path: path, Message: fmt.Sprintf("ignored damaged event tail at line %d: %v", lineNumber, err)})
				break
			}
			if expectedSessionID != "" && event.SessionID != expectedSessionID {
				return nil, diagnostics, validBytes, fmt.Errorf("event line %d belongs to session %q, expected %q", lineNumber, event.SessionID, expectedSessionID)
			}
			if event.Sequence == 0 || (previous > 0 && event.Sequence != previous+1) {
				return nil, diagnostics, validBytes, fmt.Errorf("session event sequence gap at line %d: previous %d, current %d", lineNumber, previous, event.Sequence)
			}
			if previous == 0 && event.Sequence != 1 {
				return nil, diagnostics, validBytes, fmt.Errorf("session event sequence starts at %d", event.Sequence)
			}
			if !validEventType(event.Type) {
				return nil, diagnostics, validBytes, fmt.Errorf("unknown session event type %q at line %d", event.Type, lineNumber)
			}
			previous = event.Sequence
			events = append(events, event)
		}
		if complete {
			validBytes += int64(len(line))
		}
		if eof {
			break
		}
	}
	return events, diagnostics, validBytes, nil
}

func readBoundedEventLine(reader *bufio.Reader) ([]byte, bool, bool, error) {
	var line bytes.Buffer
	for {
		fragment, err := reader.ReadSlice('\n')
		if line.Len()+len(fragment) > maxEventLineBytes {
			return nil, false, false, fmt.Errorf("event line exceeds %d bytes", maxEventLineBytes)
		}
		line.Write(fragment)
		switch {
		case err == nil:
			return line.Bytes(), true, false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return line.Bytes(), false, true, nil
		default:
			return nil, false, false, err
		}
	}
}

func truncateEventTail(path string, validBytes int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() <= validBytes {
		return nil
	}
	if err := file.Truncate(validBytes); err != nil {
		return err
	}
	return file.Sync()
}

func validEventType(eventType EventType) bool {
	switch eventType {
	case EventSessionCreated, EventTurnAppended, EventRuntimeUpdated, EventDriverState,
		EventPromptRecorded, EventSkillActivated, EventContextUpdated, EventAgentTasksUpdated, EventErrorRecorded, EventSessionClosed, EventSessionReopened,
		EventToolExecutionIntent, EventToolCompleted, EventRunReported,
		EventRequestStarted, EventToolPrepared:
		return true
	default:
		return false
	}
}

// recordToolIntent adds one started execution to the pending set.
func recordToolIntent(snapshot *Snapshot, intent ToolIntent) {
	for index, existing := range snapshot.PendingIntents {
		if existing.CallID == intent.CallID {
			snapshot.PendingIntents[index] = intent
			return
		}
	}
	snapshot.PendingIntents = append(snapshot.PendingIntents, intent)
}

// clearToolIntent removes the pending entry of one execution that reached a
// terminal outcome.
func clearToolIntent(snapshot *Snapshot, callID string) {
	if callID == "" || len(snapshot.PendingIntents) == 0 {
		return
	}
	kept := snapshot.PendingIntents[:0]
	for _, intent := range snapshot.PendingIntents {
		if intent.CallID == callID {
			continue
		}
		kept = append(kept, intent)
	}
	snapshot.PendingIntents = kept
}

func applyEvent(snapshot *Snapshot, event Event) {
	snapshot.Version = SchemaVersion
	snapshot.SessionID = event.SessionID
	snapshot.Sequence = event.Sequence
	snapshot.UpdatedAt = event.At
	switch event.Type {
	case EventSessionCreated:
		if snapshot.CreatedAt.IsZero() {
			snapshot.CreatedAt = event.At
		}
		snapshot.Workspace = event.Workspace
		if event.Environment != nil {
			snapshot.Environment = *event.Environment
		}
		snapshot.PermissionMode = event.PermissionMode
		if event.Provider != nil {
			snapshot.Provider = *event.Provider
		}
	case EventTurnAppended:
		if event.Turn != nil {
			snapshot.Turns = append(snapshot.Turns, *event.Turn)
		}
	case EventPromptRecorded:
		if event.Prompt != "" {
			snapshot.PromptHistory = append(snapshot.PromptHistory, event.Prompt)
		}
	case EventRuntimeUpdated:
		if event.Workspace != "" {
			snapshot.Workspace = event.Workspace
		}
		if event.Environment != nil {
			snapshot.Environment = *event.Environment
		}
		if event.PermissionMode != "" {
			snapshot.PermissionMode = event.PermissionMode
		}
		if event.Provider != nil {
			snapshot.Provider = *event.Provider
		}
	case EventRequestStarted, EventToolPrepared:
		// Evidence, not state: both are read from the log when a human asks what
		// happened, and neither changes how a session is rebuilt. Applying them to
		// the snapshot would duplicate what the intent and the completion already
		// say about a call's outcome.
	case EventDriverState:
		snapshot.DriverState = append(snapshot.DriverState[:0], event.DriverState...)
	case EventSkillActivated:
		if event.Skill != nil {
			upsertSkill(&snapshot.Skills, *event.Skill)
		}
	case EventContextUpdated:
		snapshot.SkillCatalog = event.SkillCatalog
		snapshot.Summary = event.Summary
		if event.TodoList != nil {
			snapshot.TodoList = cloneSessionTodoList(*event.TodoList)
		}
		snapshot.OmittedTurnIDs = append([]string(nil), event.OmittedTurnIDs...)
		if event.Ledger != nil {
			snapshot.Ledger = *event.Ledger
		}
	case EventAgentTasksUpdated:
		snapshot.AgentTasks = append([]tool.AgentTask(nil), event.AgentTasks...)
	case EventErrorRecorded:
		snapshot.LastError = event.Error
	case EventSessionClosed:
		closedAt := event.At
		snapshot.ClosedAt = &closedAt
	case EventSessionReopened:
		snapshot.ClosedAt = nil
	case EventToolExecutionIntent:
		if event.Intent != nil {
			recordToolIntent(snapshot, *event.Intent)
		}
	case EventToolCompleted:
		if event.Completion != nil {
			clearToolIntent(snapshot, event.Completion.CallID)
		}
	case EventRunReported:
		if event.Run != nil {
			record := *event.Run
			snapshot.LastRun = &record
		}
	}
}

func cloneSessionTodoList(list protocol.TodoList) protocol.TodoList {
	return protocol.TodoList{Explanation: list.Explanation, Items: append([]protocol.TodoItem(nil), list.Items...)}
}

func upsertSkill(skills *[]SkillState, skill SkillState) {
	for index := range *skills {
		if (*skills)[index].Name == skill.Name {
			(*skills)[index] = skill
			return
		}
	}
	*skills = append(*skills, skill)
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func samePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftPath = filepath.Clean(leftPath)
	rightPath = filepath.Clean(rightPath)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(leftPath, rightPath)
	}
	return leftPath == rightPath
}
