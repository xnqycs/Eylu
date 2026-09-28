package host

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func restoreRequest(c Commit, rs ...Reconciliation) Restore {
	if rs == nil {
		rs = []Reconciliation{}
	}
	return Restore{ConversationID: c.ConversationID, Revision: c.State.Revision, EventSeq: c.State.EventSeq, StateSchema: c.StateSchema, State: c.State, Reconciliations: rs}
}
func (m *mockHost) seed(c Commit) { m.mu.Lock(); m.commits[c.ConversationID] = clone(c); m.mu.Unlock() }

func TestC06ApprovalTimeoutCannotBeRevivedByLateResult(t *testing.T) {
	m := newMock(t)
	entered := make(chan ToolRequest, 1)
	release := make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	lateSent := make(chan struct{})
	m.responseSent = func(method string) {
		if method == "host.tool.execute" {
			close(lateSent)
		}
	}
	var executions atomic.Int32
	m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		switch method {
		case "host.model.generate":
			var r ModelRequest
			_ = decode(raw, &r)
			return calls(r, "approval"), nil
		case "host.tool.execute":
			var r ToolRequest
			_ = decode(raw, &r)
			executions.Add(1)
			p, _ := json.Marshal(CallProgress{ConversationID: "c", RunID: "r", RequestID: r.RequestID, Phase: "awaiting_approval"})
			m.send(envelope{JSONRPC: "2.0", Method: "host.call.progress", Params: p})
			entered <- r
			<-release
			return success(r), nil
		case "host.calls.cancel":
			return map[string]bool{"accepted": true}, nil
		}
		return nil, fault("METHOD_NOT_FOUND")
	}
	m.create()
	v := startRequest()
	v.ToolCatalog.Tools = []ToolDefinition{toolDefinition()}
	v.Limits.CallbackTimeoutMS = 35
	v.Limits.StopGraceMS = 20
	m.must("run.start", v, nil)
	<-entered
	s := m.waitFinished()
	if s.LastRun.Reason != "callback_timeout" || len(s.PendingCalls) != 1 || executions.Load() != 1 {
		t.Fatalf("approval timeout: %+v", s.LastRun)
	}
	released.Do(func() { close(release) })
	select {
	case <-lateSent:
	case <-time.After(time.Second):
		t.Fatal("late approval response was not delivered")
	}
	var view View
	m.must("conversation.get", Get{"c"}, &view)
	if view.Revision != s.Revision || view.ActiveRun != nil || view.LastRun.Reason != "callback_timeout" || len(view.BlockingRequestIDs) != 1 || executions.Load() != 1 {
		t.Fatal("late approval revived the timed-out call")
	}
}

func TestC12CommittedHistorySurvivesLostNotifications(t *testing.T) {
	m := newMock(t)
	m.dropNotifications.Store(true)
	m.handle = func(_ string, raw json.RawMessage) (any, *RPCError) {
		var r ModelRequest
		_ = decode(raw, &r)
		return answer(r, "durable answer"), nil
	}
	m.create()
	m.must("run.start", startRequest(), nil)
	s := m.waitFinished()
	var view View
	m.must("conversation.get", Get{"c"}, &view)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.notifications) != 0 || view.EventSeq != int64(len(m.events["c"])) || view.EventSeq != s.EventSeq {
		t.Fatal("history depends on transient notifications")
	}
	var messages []Message
	for _, event := range m.events["c"] {
		if event.Type == "message.committed" {
			var committed StoredMessage
			if err := json.Unmarshal(event.Data, &committed); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, committed.Message)
		}
	}
	if len(messages) != 2 || messages[0].Parts[0].Text != startRequest().Input.Text || messages[1].Parts[0].Text != "durable answer" {
		t.Fatal("committed events cannot reconstruct the conversation")
	}
}

func TestC06DeniedResultIsPreserved(t *testing.T) {
	m := newMock(t)
	var n atomic.Int32
	m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		if method == "host.model.generate" {
			var r ModelRequest
			_ = decode(raw, &r)
			if n.Add(1) == 1 {
				return calls(r, "denied"), nil
			}
			return answer(r, "approval was denied"), nil
		}
		var r ToolRequest
		_ = decode(raw, &r)
		result := success(r)
		result.Status = "denied"
		result.Error = &SafeError{Code: "DENIED", Message: "Host refused approval"}
		return result, nil
	}
	m.create()
	v := startRequest()
	v.ToolCatalog.Tools = []ToolDefinition{toolDefinition()}
	m.must("run.start", v, nil)
	s := m.waitFinished()
	if s.LastRun.Status != "completed" || len(s.LastRun.KnownEffects) != 0 {
		t.Fatal(s.LastRun)
	}
	if s.Messages[2].Message.Parts[0].Result.Status != "denied" {
		t.Fatal("denial became failure")
	}
}

func TestC09ConcurrentStartsHaveOneWriter(t *testing.T) {
	m := newMock(t)
	release := make(chan struct{})
	defer close(release)
	var modelCalls atomic.Int32
	m.handle = func(_ string, raw json.RawMessage) (any, *RPCError) {
		var r ModelRequest
		_ = decode(raw, &r)
		modelCalls.Add(1)
		<-release
		return answer(r, "done"), nil
	}
	m.create()
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			v := startRequest()
			v.RunID = id
			_, err := m.request("run.start", v)
			if err == nil {
				accepted.Add(1)
			} else if err.Data.Code != "SESSION_BUSY" && err.Data.Code != "STALE_REVISION" {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d concurrent runs", accepted.Load())
	}
}

func TestC10CommitAcknowledgementLossReusesIDAndEvents(t *testing.T) {
	m := newMock(t)
	var lost atomic.Bool
	m.checkpoint = func(c Commit) (bool, *RPCError) {
		if c.Reason == "call.prepared" && !lost.Swap(true) {
			_, err := m.save(c)
			if err != nil {
				t.Error(err)
			}
			return true, nil
		}
		return false, nil
	}
	m.handle = func(_ string, raw json.RawMessage) (any, *RPCError) {
		var r ModelRequest
		_ = decode(raw, &r)
		return answer(r, "done"), nil
	}
	m.create()
	v := startRequest()
	v.Limits.CallbackTimeoutMS = 25
	m.must("run.start", v, nil)
	s := m.waitFinished()
	m.mu.Lock()
	defer m.mu.Unlock()
	prepared := 0
	seen := map[string]bool{}
	for _, e := range m.events["c"] {
		if seen[e.EventID] {
			t.Fatal("duplicate event")
		}
		seen[e.EventID] = true
		if e.Type == "call.prepared" {
			prepared++
		}
	}
	if prepared != 1 || int64(len(m.events["c"])) != s.EventSeq || int64(len(m.checkpointIDs)) != s.Revision {
		t.Fatal("commit retry repeated versions or events")
	}
}

func TestC11C12RestorePreparedModelWithoutReplay(t *testing.T) {
	first := newMock(t)
	inflight := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	first.handle = func(_ string, raw json.RawMessage) (any, *RPCError) {
		close(inflight)
		<-release
		var r ModelRequest
		_ = decode(raw, &r)
		return answer(r, "late"), nil
	}
	first.create()
	v := startRequest()
	first.must("run.start", v, nil)
	<-inflight
	checkpoint := first.snapshot()
	first.cancel()
	if len(checkpoint.State.Messages) != 1 || checkpoint.State.Messages[0].Message.Parts[0].Text != v.Input.Text {
		t.Fatal("lost accepted user input")
	}
	second := newMock(t)
	second.seed(checkpoint)
	second.init()
	var calls atomic.Int32
	second.handle = func(string, json.RawMessage) (any, *RPCError) { calls.Add(1); return nil, fault("INVALID_REQUEST") }
	var view View
	second.must("conversation.restore", restoreRequest(checkpoint), &view)
	if view.ActiveRun == nil || view.ActiveRun.Status != "stopping" || len(view.BlockingRequestIDs) != 1 || view.ActiveRun.BlockingReason != "reconciliation_required" {
		t.Fatalf("unsafe restore: %+v", view)
	}
	blocked := second.snapshot()
	id := blocked.State.PendingCalls[0].RequestID
	second.must("conversation.restore", restoreRequest(blocked, Reconciliation{RequestID: id, Status: "not_dispatched"}), &view)
	if view.ActiveRun != nil || view.LastRun.Status != "interrupted" || calls.Load() != 0 {
		t.Fatal("old model request replayed")
	}
	if view.EventSeq <= checkpoint.State.EventSeq || view.Revision <= checkpoint.State.Revision {
		t.Fatal("event sequence did not advance")
	}
	var acceptance Accepted
	second.must("run.start", v, &acceptance)
	if acceptance.Revision != 2 {
		t.Fatal("restart forgot run idempotency")
	}
}

func TestC11RestoreToolResultFromLedgerAndNeverReexecute(t *testing.T) {
	first := newMock(t)
	entered := make(chan ToolRequest, 1)
	release := make(chan struct{})
	defer close(release)
	first.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		if method == "host.model.generate" {
			var r ModelRequest
			_ = decode(raw, &r)
			return calls(r, "a", "b"), nil
		}
		var r ToolRequest
		_ = decode(raw, &r)
		entered <- r
		<-release
		return success(r), nil
	}
	first.create()
	v := startRequest()
	v.ToolCatalog.Tools = []ToolDefinition{toolDefinition()}
	first.must("run.start", v, nil)
	request := <-entered
	checkpoint := first.snapshot()
	first.cancel()
	second := newMock(t)
	second.seed(checkpoint)
	second.init()
	var attempts atomic.Int32
	second.handle = func(string, json.RawMessage) (any, *RPCError) { attempts.Add(1); return nil, fault("INVALID_REQUEST") }
	result := success(request)
	result.Effect = "applied"
	op := "op-1"
	result.OperationID = &op
	var view View
	second.must("conversation.restore", restoreRequest(checkpoint, Reconciliation{RequestID: request.RequestID, Status: "settled", ToolResult: &result}), &view)
	s := second.snapshot().State
	if view.ActiveRun != nil || len(s.PendingCalls) != 0 || attempts.Load() != 0 || len(view.LastRun.KnownEffects) != 1 || !hasToolResult(s.Messages, "a") || !hasToolResult(s.Messages, "b") {
		t.Fatalf("unsafe tool recovery: %+v", view)
	}
	if err := validateState(restoreRequest(second.snapshot())); err != nil {
		t.Fatal(err)
	}
	bad := restoreRequest(second.snapshot())
	bad.StateSchema = "eylu-host-state/999"
	if _, err := second.request("conversation.restore", bad); err == nil || err.Data.Code != "STATE_INCOMPATIBLE" {
		t.Fatal(err)
	}
}

func TestC13C15BudgetsTruncationAndUnknownUsage(t *testing.T) {
	for _, scenario := range []string{"unknown", "length", "blocked", "call_limit", "output_limit", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			m := newMock(t)
			var models, tools atomic.Int32
			m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
				if method == "host.tool.execute" {
					tools.Add(1)
					var r ToolRequest
					_ = decode(raw, &r)
					return success(r), nil
				}
				var r ModelRequest
				_ = decode(raw, &r)
				models.Add(1)
				if scenario == "revoked" {
					return nil, fault("MODEL_REQUEST_FAILED")
				}
				rtn := calls(r, "a")
				switch scenario {
				case "unknown":
					rtn.Usage.Source = "unknown"
				case "length":
					rtn.FinishReason = "length"
				case "blocked":
					rtn.FinishReason = "blocked"
				case "output_limit":
					rtn.Usage.OutputTokens = 4000
				}
				return rtn, nil
			}
			m.create()
			v := startRequest()
			v.ToolCatalog.Tools = []ToolDefinition{toolDefinition()}
			if scenario == "call_limit" {
				v.Limits.MaxModelCalls = 1
			}
			m.must("run.start", v, nil)
			s := m.waitFinished()
			if models.Load() != 1 {
				t.Fatal("paid model was retried")
			}
			if scenario != "call_limit" && tools.Load() != 0 {
				t.Fatal("incomplete or unmetered model output executed tools")
			}
			if scenario == "unknown" && s.LastRun.Reason != "budget_unknown" {
				t.Fatal(s.LastRun)
			}
			if scenario == "revoked" && len(s.PendingCalls) != 1 {
				t.Fatal("lost unknown model request")
			}
			if s.LastRun.Status == "completed" {
				t.Fatal("limit or unknown reported completed")
			}
		})
	}
}

func TestC13ElapsedLimitCancelsOriginalCall(t *testing.T) {
	m := newMock(t)
	cancelled := make(chan struct{})
	var once sync.Once
	m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		if method == "host.calls.cancel" {
			once.Do(func() { close(cancelled) })
			return map[string]bool{"accepted": true}, nil
		}
		<-cancelled
		return nil, fault("MODEL_REQUEST_FAILED")
	}
	m.create()
	v := startRequest()
	v.Limits.MaxElapsedMS = 35
	m.must("run.start", v, nil)
	s := m.waitFinished()
	if s.LastRun.Reason != "elapsed_limit" || s.LastRun.Status != "interrupted" {
		t.Fatal(s.LastRun)
	}
}

func TestC07InterruptWhileInputCommitOrModelIsWaiting(t *testing.T) {
	for _, stage := range []string{"input_commit", "model"} {
		t.Run(stage, func(t *testing.T) {
			m := newMock(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			var released sync.Once
			defer released.Do(func() { close(release) })
			var models atomic.Int32
			if stage == "input_commit" {
				m.checkpoint = func(c Commit) (bool, *RPCError) {
					if c.Reason == "run.started" {
						close(entered)
						<-release
					}
					return false, nil
				}
			}
			m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
				if method == "host.calls.cancel" {
					released.Do(func() { close(release) })
					return map[string]bool{"accepted": true}, nil
				}
				models.Add(1)
				close(entered)
				<-release
				return nil, fault("MODEL_REQUEST_FAILED")
			}
			m.create()
			accepted := make(chan struct{})
			go func() {
				_, err := m.request("run.start", startRequest())
				if err != nil {
					t.Error(err)
				}
				close(accepted)
			}()
			<-entered
			m.must("run.interrupt", Interrupt{ConversationID: "c", RunID: "r", Reason: "operator_takeover"}, nil)
			released.Do(func() { close(release) })
			<-accepted
			state := m.waitFinished()
			if state.LastRun.Status != "interrupted" || stage == "input_commit" && models.Load() != 0 {
				t.Fatalf("unexpected stop: %+v", state.LastRun)
			}
		})
	}
}

func TestC10TerminalCommitFailureDoesNotExposeCommittedCompletion(t *testing.T) {
	m := newMock(t)
	m.handle = func(_ string, raw json.RawMessage) (any, *RPCError) {
		var r ModelRequest
		_ = decode(raw, &r)
		return answer(r, "done"), nil
	}
	m.checkpoint = func(c Commit) (bool, *RPCError) {
		if c.Reason == "run.finished" {
			return false, fault("CHECKPOINT_FAILED")
		}
		return false, nil
	}
	m.create()
	m.must("run.start", startRequest(), nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var v View
		m.must("conversation.get", Get{"c"}, &v)
		if v.ActiveRun != nil && v.ActiveRun.BlockingReason == "checkpoint_failed" {
			if v.LastRun != nil || !v.HasUncommittedProgress || v.ActiveRun.Status != "stopping" {
				t.Fatal("uncommitted completion exposed")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no checkpoint fault")
}

func TestC14NegotiationAndUnsupportedContentFailClosed(t *testing.T) {
	m := newMock(t)
	v := Initialize{ProtocolVersion: "bastion-host/2.0", HostName: "test", HostVersion: "1", MaxMessageBytes: MaxMessageBytes, RequiredCapabilities: []string{}, OptionalCapabilities: []string{}}
	if _, err := m.request("initialize", v); err == nil || err.Data.Code != "UNSUPPORTED_VERSION" {
		t.Fatal(err)
	}
	v.ProtocolVersion = ProtocolVersion
	v.RequiredCapabilities = []string{"vision"}
	if _, err := m.request("initialize", v); err == nil || err.Data.Code != "UNSUPPORTED_CAPABILITY" {
		t.Fatal(err)
	}
	m.create()
	start := startRequest()
	start.ToolCatalog.Tools = []ToolDefinition{{Name: "unsupported", Description: "unsupported", InputSchema: json.RawMessage(`{"type":"object","$ref":"https://invalid.example/schema"}`)}}
	if _, err := m.request("run.start", start); err == nil || err.Data.Code != "UNSUPPORTED_CAPABILITY" {
		t.Fatal(err)
	}
}

func TestC05LengthRetainsTextAndDiscardsPartialToolArguments(t *testing.T) {
	m := newMock(t)
	var tools atomic.Int32
	m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		if method != "host.model.generate" {
			tools.Add(1)
			return nil, fault("INVALID_REQUEST")
		}
		var r ModelRequest
		_ = decode(raw, &r)
		result := answer(r, "usable partial answer")
		result.FinishReason = "length"
		result.Message.Parts = append(result.Message.Parts, Part{Type: "tool_call", ToolCallID: "unfinished", Name: "remote.inspect", Arguments: json.RawMessage(`"{unfinished"`)})
		return result, nil
	}
	m.create()
	v := startRequest()
	v.ToolCatalog.Tools = []ToolDefinition{toolDefinition()}
	m.must("run.start", v, nil)
	s := m.waitFinished()
	if tools.Load() != 0 || s.LastRun.Reason != "output_limit" || len(s.Messages[1].Message.Parts) != 1 || s.Messages[1].Message.Parts[0].Text != "usable partial answer" {
		t.Fatal("lost text or executed partial parameters")
	}
}

func TestC13CompactionUsesHostAndCountsBudget(t *testing.T) {
	m := newMock(t)
	// Persisting a checkpoint precedes the engine processing its acknowledgement.
	// Make that interval observable instead of relying on local scheduling speed.
	m.checkpoint = func(c Commit) (bool, *RPCError) {
		if c.Reason == "run.finished" {
			if _, err := m.save(c); err != nil {
				return false, err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false, nil
	}
	var summaries atomic.Int32
	m.handle = func(method string, raw json.RawMessage) (any, *RPCError) {
		if method != "host.model.generate" {
			return nil, fault("METHOD_NOT_FOUND")
		}
		var r ModelRequest
		_ = decode(raw, &r)
		if r.Purpose == "compaction" {
			summaries.Add(1)
			return answer(r, "<conversation_summary>\nUser goals:\nSupport\nConstraints and decisions:\nKeep binding\nCompleted modifications:\nNone\nUnfinished tasks:\nInspect\nFailed attempts:\nNone\nValidation results:\nNone\nKey files:\nNone\n</conversation_summary>"), nil
		}
		return answer(r, strings.Repeat("context fact ", 220)), nil
	}
	m.create()
	revision := int64(1)
	for i := 0; i < 12; i++ {
		v := startRequest()
		v.RunID = fmt.Sprintf("r%d", i)
		v.ExpectedRevision = revision
		if i == 11 {
			v.ModelBinding.ContextWindowTokens = 9000
			v.ModelBinding.MaxOutputTokens = 512
			v.Limits.MaxModelCalls = 3
		}
		// Re-send the same business request while the preceding committed run is
		// still settling. Never replace its ID or revision to bypass a conflict.
		startDeadline := time.Now().Add(3 * time.Second)
		for {
			_, err := m.request("run.start", v)
			if err == nil {
				break
			}
			if (err.Data.Code != "SESSION_BUSY" && err.Data.Code != "STALE_REVISION") || time.Now().After(startDeadline) {
				t.Fatalf("run %d did not become ready: %v", i, err)
			}
			time.Sleep(time.Millisecond)
		}
		deadline := time.Now().Add(3 * time.Second)
		var state State
		for time.Now().Before(deadline) {
			state = m.snapshot().State
			if state.LastRun != nil && state.LastRun.RunID == v.RunID && state.ActiveRun == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if state.LastRun == nil || state.LastRun.RunID != v.RunID || state.LastRun.Status != "completed" {
			t.Fatalf("run %d: %+v", i, state.LastRun)
		}
		revision = state.Revision
	}
	s := m.snapshot().State
	if summaries.Load() == 0 || s.Summary == "" || s.LastRun.ModelCalls < 2 {
		t.Fatalf("no durable budgeted host compaction: summaries=%d run=%+v", summaries.Load(), s.LastRun)
	}
}
