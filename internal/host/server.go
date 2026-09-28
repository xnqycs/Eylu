package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"Eylu/internal/buildinfo"
)

type Server struct {
	mu          sync.Mutex
	p           *peer
	initialized bool
	initDigest  string
	negotiated  Initialized
	closing     bool
	sessions    map[string]*conversation
	workers     chan struct{}
	shutdown    chan struct{}
}

// Serve has no configuration discovery or provider registry. Its only network
// boundary is the supplied standard streams, owned by the parent process.
func Serve(ctx context.Context, input io.Reader, output, diagnostics io.Writer) error {
	s := &Server{sessions: map[string]*conversation{}, workers: make(chan struct{}, 64), shutdown: make(chan struct{})}
	s.p = newPeer(ctx, output)
	defer s.p.cancel()
	go s.p.read(input, s.dispatch)
	select {
	case <-s.shutdown:
		return nil
	case err := <-s.p.err:
		s.stopAll("host_unavailable")
		if errors.Is(err, io.EOF) {
			return nil
		}
		fmt.Fprintln(diagnostics, "host protocol connection closed")
		return fault("HOST_UNAVAILABLE")
	case <-ctx.Done():
		s.stopAll("shutdown")
		return ctx.Err()
	}
}
func (s *Server) dispatch(e envelope) {
	if len(e.ID) == 0 {
		s.notification(e)
		return
	}
	select {
	case s.workers <- struct{}{}:
	default:
		_ = s.p.respond(e.ID, nil, fault("SESSION_BUSY"), false)
		return
	}
	go func() {
		defer func() { <-s.workers }()
		result, err := s.handle(e.Method, e.Params)
		flush := e.Method == "shutdown" && err == nil
		if sendErr := s.p.respond(e.ID, result, err, flush); sendErr != nil {
			s.p.fail(sendErr)
		}
		if flush {
			close(s.shutdown)
		}
	}()
}
func (s *Server) handle(method string, raw json.RawMessage) (any, *RPCError) {
	if method == "initialize" {
		var v Initialize
		if decode(raw, &v) != nil {
			return nil, fault("INVALID_PARAMS")
		}
		return s.initialize(v)
	}
	s.mu.Lock()
	initialized, closing := s.initialized, s.closing
	s.mu.Unlock()
	if !initialized {
		return nil, fault("NOT_INITIALIZED")
	}
	if closing {
		return nil, fault("SESSION_BUSY")
	}
	switch method {
	case "conversation.create":
		var v Create
		if decode(raw, &v) != nil || !validCreate(v) {
			return nil, fault("INVALID_PARAMS")
		}
		return s.create(v)
	case "conversation.restore":
		var v Restore
		if decode(raw, &v) != nil {
			return nil, fault("STATE_INCOMPATIBLE")
		}
		return s.restore(v)
	case "conversation.get":
		var v Get
		if decode(raw, &v) != nil || !identifier(v.ConversationID) {
			return nil, fault("INVALID_PARAMS")
		}
		c := s.lookup(v.ConversationID)
		if c == nil {
			return nil, fault("NOT_FOUND")
		}
		return c.view(), nil
	case "run.start":
		var v Start
		if decode(raw, &v) != nil {
			return nil, fault("INVALID_PARAMS")
		}
		if err := validateStart(v); err != nil {
			return nil, err
		}
		c := s.lookup(v.ConversationID)
		if c == nil {
			return nil, fault("NOT_FOUND")
		}
		return c.start(v)
	case "run.interrupt":
		var v Interrupt
		if decode(raw, &v) != nil || !identifier(v.ConversationID) || !identifier(v.RunID) || v.Reason == "" {
			return nil, fault("INVALID_PARAMS")
		}
		c := s.lookup(v.ConversationID)
		if c == nil {
			return nil, fault("NOT_FOUND")
		}
		return c.interrupt(v)
	case "shutdown":
		var v Shutdown
		if decode(raw, &v) != nil || v.Reason == "" || v.WaitMS < 0 || v.WaitMS > 86400000 {
			return nil, fault("INVALID_PARAMS")
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			return nil, fault("SESSION_BUSY")
		}
		s.closing = true
		s.mu.Unlock()
		done := s.stopAll(v.Reason)
		timer := time.NewTimer(time.Duration(v.WaitMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		case <-s.p.ctx.Done():
		}
		return map[string]any{"accepted": true}, nil
	default:
		return nil, fault("METHOD_NOT_FOUND")
	}
}
func (s *Server) initialize(v Initialize) (any, *RPCError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		if s.initDigest != digest(v) {
			return nil, fault("ID_CONFLICT")
		}
		return s.negotiated, nil
	}
	if v.ProtocolVersion != ProtocolVersion {
		return nil, fault("UNSUPPORTED_VERSION")
	}
	if v.HostName == "" || v.HostVersion == "" || v.MaxMessageBytes < 1024 || v.MaxMessageBytes > MaxMessageBytes {
		return nil, fault("INVALID_PARAMS")
	}
	caps := []string{"host_model", "host_tools", "durable_checkpoints", "interrupt"}
	for _, c := range v.RequiredCapabilities {
		if !slices.Contains(append(append([]string{}, caps...), "text_deltas"), c) {
			return nil, fault("UNSUPPORTED_CAPABILITY")
		}
	}
	if slices.Contains(v.RequiredCapabilities, "text_deltas") || slices.Contains(v.OptionalCapabilities, "text_deltas") {
		caps = append(caps, "text_deltas")
	}
	s.negotiated = Initialized{ProtocolVersion: ProtocolVersion, EngineName: "eylu", EngineVersion: buildinfo.String(), MaxMessageBytes: v.MaxMessageBytes, Capabilities: caps, StateSchema: StateSchema}
	s.p.max.Store(int64(v.MaxMessageBytes))
	s.initDigest = digest(v)
	s.initialized = true
	return s.negotiated, nil
}
func (s *Server) lookup(id string) *conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}
func (s *Server) create(v Create) (any, *RPCError) {
	s.mu.Lock()
	if existing := s.sessions[v.ConversationID]; existing != nil {
		s.mu.Unlock()
		existing.mu.Lock()
		defer existing.mu.Unlock()
		if existing.state.CreateDigest != digest(v) {
			return nil, fault("ID_CONFLICT")
		}
		if existing.revision == 0 {
			return nil, fault("SESSION_BUSY")
		}
		return Accepted{ConversationID: v.ConversationID, Revision: 1}, nil
	}
	if s.closing {
		s.mu.Unlock()
		return nil, fault("SESSION_BUSY")
	}
	if len(s.sessions) >= 1024 {
		s.mu.Unlock()
		return nil, fault("LIMIT_EXCEEDED")
	}
	c := newConversation(s, State{ConversationID: v.ConversationID, CreateDigest: digest(v), Instructions: v.Instructions, Context: v.Context, Messages: []StoredMessage{}, Runs: map[string]RunRecord{}, PendingCalls: []PendingCall{}, UsedToolCallIDs: []string{}})
	s.sessions[v.ConversationID] = c
	c.done = make(chan struct{})
	s.mu.Unlock()
	defer close(c.done)
	if err := c.commit("conversation.created", event("conversation.created", "", v)); err != nil {
		return nil, asFault(err, "CHECKPOINT_FAILED")
	}
	return Accepted{ConversationID: v.ConversationID, Revision: 1}, nil
}
func (s *Server) stopAll(reason string) <-chan struct{} {
	s.mu.Lock()
	sessions := make([]*conversation, 0, len(s.sessions))
	for _, c := range s.sessions {
		sessions = append(sessions, c)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range sessions {
		c.stop(reason)
		c.mu.Lock()
		done := c.done
		c.mu.Unlock()
		if done != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-done:
				case <-s.p.ctx.Done():
				}
			}()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return done
}
func (s *Server) notification(e envelope) {
	s.mu.Lock()
	initialized := s.initialized
	deltas := slices.Contains(s.negotiated.Capabilities, "text_deltas")
	s.mu.Unlock()
	if !initialized {
		return
	}
	switch e.Method {
	case "host.model.delta":
		var d Delta
		if !deltas || decode(e.Params, &d) != nil || d.DeltaSeq < 1 {
			return
		}
		if c := s.lookup(d.ConversationID); c != nil {
			c.delta(d)
		}
	case "host.call.progress":
		var p CallProgress
		if decode(e.Params, &p) != nil || !slices.Contains([]string{"awaiting_approval", "awaiting_input", "executing"}, p.Phase) {
			return
		}
		if c := s.lookup(p.ConversationID); c != nil {
			c.progress(p)
		}
	}
}
func asFault(err error, fallback string) *RPCError {
	var e *RPCError
	if errors.As(err, &e) {
		return e
	}
	return fault(fallback)
}
