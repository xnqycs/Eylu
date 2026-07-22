package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type Confirmation struct {
	Approved        bool
	RejectionReason string
}

type ConfirmFunc func(context.Context, policy.Request, policy.Outcome) (Confirmation, error)

type AuditRecord struct {
	Timestamp           time.Time           `json:"timestamp"`
	RequestID           string              `json:"request_id"`
	SessionID           string              `json:"session_id,omitempty"`
	ProviderName        string              `json:"provider_name,omitempty"`
	ProviderGeneration  uint64              `json:"provider_generation,omitempty"`
	Model               string              `json:"model,omitempty"`
	CallID              string              `json:"call_id"`
	BatchID             string              `json:"batch_id,omitempty"`
	BatchIndex          int                 `json:"batch_index"`
	Tool                string              `json:"tool"`
	Risk                policy.Risk         `json:"risk"`
	Decision            policy.Decision     `json:"decision"`
	Reason              string              `json:"reason"`
	Confirmed           bool                `json:"confirmed"`
	DurationMS          int64               `json:"duration_ms"`
	QueueDurationMS     int64               `json:"queue_duration_ms,omitempty"`
	ExecutionDurationMS int64               `json:"execution_duration_ms,omitempty"`
	ConcurrencyMode     string              `json:"concurrency_mode,omitempty"`
	ResourceClaims      []ResourceClaim     `json:"resource_claims,omitempty"`
	IsError             bool                `json:"is_error"`
	Truncated           bool                `json:"truncated"`
	InputBytes          int                 `json:"input_bytes"`
	OutputBytes         int                 `json:"output_bytes"`
	ExitCode            int                 `json:"exit_code,omitempty"`
	Mode                string              `json:"mode"`
	Classification      policy.CommandClass `json:"classification"`
	Confirmations       int                 `json:"confirmations"`
	Warning             bool                `json:"warning"`
	SkillName           string              `json:"skill_name,omitempty"`
	SkillSource         string              `json:"skill_source,omitempty"`
	SkillDigest         string              `json:"skill_digest,omitempty"`
	SkillTrigger        string              `json:"skill_trigger,omitempty"`
	SkillActivated      string              `json:"skill_activated_at,omitempty"`
	AllowedTools        string              `json:"allowed_tools,omitempty"`
	SkillResource       string              `json:"skill_resource,omitempty"`
	ResourceBytes       int                 `json:"resource_bytes,omitempty"`
	WebBackend          string              `json:"web_backend,omitempty"`
	WebTarget           string              `json:"web_target,omitempty"`
	WebStatus           string              `json:"web_status,omitempty"`
	WebSources          int                 `json:"web_sources,omitempty"`
	WebInputTokens      int                 `json:"web_input_tokens,omitempty"`
	WebOutputTokens     int                 `json:"web_output_tokens,omitempty"`
	WebCostUSD          float64             `json:"web_cost_usd,omitempty"`
	UntrustedWebContent bool                `json:"untrusted_web_content,omitempty"`
}

type AuditSink interface {
	Record(AuditRecord)
}

type Executor struct {
	Registry           *Registry
	Policy             policy.Checker
	Confirm            ConfirmFunc
	Audit              AuditSink
	Workspace          string
	Timeout            time.Duration
	MaxOutputBytes     int
	SessionID          string
	ProviderName       string
	ProviderGeneration uint64
	Model              string
	MaxParallelTools   int
}

type BatchHooks struct {
	OnStart  func(protocol.ToolCall) error
	OnResult func(protocol.ToolResult) error
}

type preparedCall struct {
	call             protocol.ToolCall
	item             Tool
	input            json.RawMessage
	outcome          policy.Outcome
	spec             ConcurrencySpec
	terminal         *protocol.ToolResult
	queuedAt         time.Time
	executionStarted time.Time
	record           AuditRecord
	auditOnce        sync.Once
	running          bool
	done             bool
}

type batchCompletion struct {
	index  int
	result protocol.ToolResult
}

func (e *Executor) Definitions() []protocol.ToolDefinition {
	if e == nil || e.Registry == nil {
		return nil
	}
	definitions := e.Registry.Definitions()
	for index := range definitions {
		item, ok := e.Registry.Get(definitions[index].Name)
		if ok && item.Risk() != policy.RiskRead && item.Risk() != policy.RiskSession {
			definitions[index] = withApprovalReason(definitions[index])
		}
	}
	return definitions
}

func (e *Executor) CanExecuteConcurrently(call protocol.ToolCall) bool {
	if e == nil || e.Registry == nil {
		return false
	}
	item, ok := e.Registry.Get(call.Name)
	if !ok {
		return false
	}
	if classifier, ok := item.(ConcurrencyClassifier); ok {
		return normalizeConcurrencySpec(classifier.ClassifyConcurrency(call.Arguments, policy.Outcome{})).Mode != ConcurrencyExclusive
	}
	safe, ok := item.(ParallelSafe)
	return ok && safe.ParallelSafe()
}

func (e *Executor) ExecuteConcurrent(ctx context.Context, requestID string, calls []protocol.ToolCall) []protocol.ToolResult {
	results, _ := e.ExecuteBatch(ctx, requestID, calls, BatchHooks{})
	return results
}

func (e *Executor) ParallelLimit() int {
	if e == nil || e.MaxParallelTools <= 0 {
		return 4
	}
	return e.MaxParallelTools
}

func (e *Executor) Execute(ctx context.Context, requestID string, call protocol.ToolCall) protocol.ToolResult {
	prepared := e.prepareCall(ctx, requestID, "", 0, call, time.Now())
	if prepared.terminal != nil {
		return e.finishPrepared(prepared, *prepared.terminal, 0)
	}
	return e.executePrepared(ctx, prepared)
}

func (e *Executor) ExecuteBatch(ctx context.Context, requestID string, calls []protocol.ToolCall, hooks BatchHooks) ([]protocol.ToolResult, error) {
	results := make([]protocol.ToolResult, len(calls))
	if len(calls) == 0 {
		return results, nil
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	queuedAt := time.Now()
	batchID := uuid.NewString()
	prepared := make([]*preparedCall, len(calls))
	interrupt := false
	var batchErr error
	for index, call := range calls {
		if interrupt || batchErr != nil {
			prepared[index] = e.cancelledPrepared(requestID, batchID, index, call, queuedAt, "tool execution cancelled by batch preflight")
			continue
		}
		prepared[index] = e.prepareCall(batchCtx, requestID, batchID, index, call, queuedAt)
		if prepared[index].terminal != nil && prepared[index].terminal.Metadata != nil && prepared[index].terminal.Metadata["interrupt_request"] == true {
			interrupt = true
		}
		if err := ctx.Err(); err != nil {
			batchErr = err
		}
	}
	if interrupt || batchErr != nil {
		message := "tool execution cancelled by batch preflight"
		if batchErr != nil {
			message = "tool execution cancelled"
		}
		for index, item := range prepared {
			if item == nil {
				prepared[index] = e.cancelledPrepared(requestID, batchID, index, calls[index], queuedAt, message)
			} else if item.terminal == nil {
				result := protocol.ToolResult{CallID: item.call.ID, Content: message, IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
				item.terminal = &result
			}
		}
	}

	completed := 0
	for index, item := range prepared {
		if item.terminal == nil {
			continue
		}
		result, hookErr := e.deliverTerminal(item, *item.terminal, hooks)
		results[index] = result
		item.done = true
		completed++
		if hookErr != nil {
			if batchErr == nil {
				batchErr = hookErr
				cancel()
			}
			hooks = BatchHooks{}
		}
	}
	if completed == len(prepared) {
		return results, batchErr
	}

	limit := e.ParallelLimit()
	if limit > len(prepared) {
		limit = len(prepared)
	}
	active := make(map[int]*preparedCall)
	completion := make(chan batchCompletion, len(prepared))
	contextDone := batchCtx.Done()
	for completed < len(prepared) {
		if batchErr == nil && batchCtx.Err() == nil {
			for len(active) < limit {
				index := nextRunnable(prepared, active)
				if index < 0 {
					break
				}
				item := prepared[index]
				if hooks.OnStart != nil {
					if err := hooks.OnStart(item.call); err != nil {
						batchErr = err
						cancel()
						hooks = BatchHooks{}
						break
					}
				}
				item.running = true
				active[index] = item
				go func(index int, item *preparedCall) {
					completion <- batchCompletion{index: index, result: e.executePrepared(batchCtx, item)}
				}(index, item)
				if item.spec.Mode == ConcurrencyExclusive {
					break
				}
			}
		}
		if batchCtx.Err() != nil {
			for index, item := range prepared {
				if item.done || item.running {
					continue
				}
				result := protocol.ToolResult{CallID: item.call.ID, Content: "tool execution cancelled", IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
				result, hookErr := e.deliverTerminal(item, result, hooks)
				results[index] = result
				item.done = true
				completed++
				if hookErr != nil {
					if batchErr == nil {
						batchErr = hookErr
					}
					hooks = BatchHooks{}
				}
			}
			contextDone = nil
		}
		if completed == len(prepared) {
			break
		}
		if len(active) == 0 {
			if batchErr == nil && batchCtx.Err() == nil {
				batchErr = fmt.Errorf("tool scheduler could not make progress")
				cancel()
				continue
			}
			continue
		}
		select {
		case finished := <-completion:
			item := prepared[finished.index]
			delete(active, finished.index)
			item.running = false
			item.done = true
			results[finished.index] = finished.result
			completed++
			if hooks.OnResult != nil {
				if err := hooks.OnResult(finished.result); err != nil {
					if batchErr == nil {
						batchErr = err
						cancel()
					}
					hooks = BatchHooks{}
				}
			}
		case <-contextDone:
			if batchErr == nil {
				batchErr = ctx.Err()
				if batchErr == nil {
					batchErr = batchCtx.Err()
				}
			}
			contextDone = nil
		}
	}
	return results, batchErr
}

func (e *Executor) prepareCall(ctx context.Context, requestID, batchID string, batchIndex int, call protocol.ToolCall, queuedAt time.Time) (prepared *preparedCall) {
	prepared = &preparedCall{call: call, queuedAt: queuedAt, spec: ConcurrencySpec{Mode: ConcurrencyExclusive}}
	prepared.record = AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, BatchID: batchID, BatchIndex: batchIndex, CallID: call.ID, Tool: call.Name, InputBytes: len(call.Arguments), ConcurrencyMode: string(ConcurrencyExclusive)}
	if e != nil {
		prepared.record.SessionID, prepared.record.ProviderName = e.SessionID, e.ProviderName
		prepared.record.ProviderGeneration, prepared.record.Model = e.ProviderGeneration, e.Model
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			result := protocol.ToolResult{CallID: call.ID, Content: fmt.Sprintf("tool preparation panicked: %v", recovered), IsError: true}
			prepared.terminal = &result
		}
	}()
	if e == nil || e.Registry == nil {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool executor is unavailable", IsError: true}
		prepared.terminal = &result
		return prepared
	}
	if ctx.Err() != nil {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool execution cancelled", IsError: true}
		prepared.terminal = &result
		return prepared
	}
	item, ok := e.Registry.Get(call.Name)
	if !ok {
		result := protocol.ToolResult{CallID: call.ID, Content: fmt.Sprintf("unknown tool %q", call.Name), IsError: true}
		prepared.terminal = &result
		return prepared
	}
	prepared.item = item
	if !json.Valid(call.Arguments) {
		result := protocol.ToolResult{CallID: call.ID, Content: "tool input is invalid JSON", IsError: true}
		prepared.terminal = &result
		return prepared
	}
	checker := e.Policy
	if checker == nil {
		checker = policy.BaselineChecker{}
	}
	policyRequest := policy.Request{Tool: call.Name, Input: call.Arguments, Workspace: e.Workspace, Risk: item.Risk()}
	outcome := checker.Check(ctx, policyRequest)
	if override, ok := item.(PolicyOverride); ok {
		if domainOutcome, applied := override.OverridePolicy(call.Arguments); applied {
			outcome = domainOutcome
		}
	}
	prepared.outcome = outcome
	prepared.record.Risk, prepared.record.Decision, prepared.record.Reason = outcome.Risk, outcome.Decision, outcome.Reason
	prepared.record.Mode, prepared.record.Classification, prepared.record.Warning = outcome.Mode.String(), outcome.Classification, outcome.Warning
	switch outcome.Decision {
	case policy.DecisionDeny:
		result := protocol.ToolResult{CallID: call.ID, Content: "permission denied: " + outcome.Reason, IsError: true}
		prepared.terminal = &result
		return prepared
	case policy.DecisionConfirm:
		if e.Confirm == nil {
			result := protocol.ToolResult{CallID: call.ID, Content: "confirmation required: " + outcome.Reason, IsError: true}
			prepared.terminal = &result
			return prepared
		}
		confirmations := outcome.Confirmations
		if confirmations <= 0 {
			confirmations = 1
		}
		for step := 1; step <= confirmations; step++ {
			policyRequest.ConfirmationStep = step
			policyRequest.ConfirmationTotal = confirmations
			confirmation, err := e.Confirm(ctx, policyRequest, outcome)
			if err != nil {
				result := protocol.ToolResult{CallID: call.ID, Content: "confirmation failed: " + err.Error(), IsError: true}
				prepared.terminal = &result
				return prepared
			}
			if !confirmation.Approved {
				result := protocol.ToolResult{CallID: call.ID, Content: "approval rejected", IsError: true, Metadata: map[string]any{"approval_rejected": true}}
				if reason := strings.TrimSpace(confirmation.RejectionReason); reason != "" {
					result.Content += ": " + reason
					result.Metadata["rejection_reason"] = reason
				} else {
					result.Metadata["interrupt_request"] = true
				}
				prepared.terminal = &result
				return prepared
			}
			prepared.record.Confirmations++
		}
		prepared.record.Confirmed = true
	}
	prepared.input = call.Arguments
	if !schemaHasProperty(item.Definition().InputSchema, "reason") {
		prepared.input = withoutJSONField(prepared.input, "reason")
	}
	prepared.spec = concurrencySpec(item, prepared.input, outcome)
	prepared.record.ConcurrencyMode = string(prepared.spec.Mode)
	prepared.record.ResourceClaims = append([]ResourceClaim(nil), prepared.spec.Claims...)
	return prepared
}

func (e *Executor) cancelledPrepared(requestID, batchID string, batchIndex int, call protocol.ToolCall, queuedAt time.Time, message string) *preparedCall {
	prepared := &preparedCall{call: call, queuedAt: queuedAt, spec: ConcurrencySpec{Mode: ConcurrencyExclusive}}
	prepared.record = AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, BatchID: batchID, BatchIndex: batchIndex, CallID: call.ID, Tool: call.Name, InputBytes: len(call.Arguments), ConcurrencyMode: string(ConcurrencyExclusive)}
	if e != nil {
		prepared.record.SessionID, prepared.record.ProviderName = e.SessionID, e.ProviderName
		prepared.record.ProviderGeneration, prepared.record.Model = e.ProviderGeneration, e.Model
	}
	result := protocol.ToolResult{CallID: call.ID, Content: message, IsError: true, Metadata: map[string]any{"batch_cancelled": true}}
	prepared.terminal = &result
	return prepared
}

func (e *Executor) executePrepared(ctx context.Context, prepared *preparedCall) (result protocol.ToolResult) {
	prepared.executionStarted = time.Now()
	executionStarted := prepared.executionStarted
	defer func() {
		if recovered := recover(); recovered != nil {
			result = protocol.ToolResult{CallID: prepared.call.ID, Content: fmt.Sprintf("tool execution panicked: %v", recovered), IsError: true}
		}
		result.CallID = prepared.call.ID
		result = e.finishPrepared(prepared, result, time.Since(executionStarted))
	}()
	toolCtx := ctx
	cancel := func() {}
	useTimeout := true
	if timeoutPolicy, ok := prepared.item.(ExecutorTimeoutPolicy); ok {
		useTimeout = timeoutPolicy.UseExecutorTimeout()
	}
	if useTimeout {
		timeout := e.Timeout
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		toolCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	result = prepared.item.Execute(toolCtx, prepared.input)
	if useTimeout && toolCtx.Err() == context.DeadlineExceeded {
		result.IsError = true
		result.Content = "tool execution timed out"
	} else if toolCtx.Err() == context.Canceled {
		result.IsError = true
		result.Content = "tool execution cancelled"
	}
	maxOutput := e.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = 64 << 10
	}
	content, truncated := truncateUTF8(result.Content, maxOutput)
	result.Content = content
	result.Truncated = result.Truncated || truncated
	return result
}

func (e *Executor) finishPrepared(prepared *preparedCall, result protocol.ToolResult, executionDuration time.Duration) protocol.ToolResult {
	result.CallID = prepared.call.ID
	prepared.auditOnce.Do(func() {
		prepared.record.DurationMS = time.Since(prepared.queuedAt).Milliseconds()
		if !prepared.executionStarted.IsZero() {
			prepared.record.QueueDurationMS = prepared.executionStarted.Sub(prepared.queuedAt).Milliseconds()
		} else {
			prepared.record.QueueDurationMS = prepared.record.DurationMS
		}
		prepared.record.ExecutionDurationMS = executionDuration.Milliseconds()
		prepared.record.IsError = result.IsError
		prepared.record.Truncated = result.Truncated
		prepared.record.OutputBytes = len([]byte(result.Content))
		if result.Metadata != nil {
			if exitCode, ok := result.Metadata["exit_code"].(int); ok {
				prepared.record.ExitCode = exitCode
			}
			prepared.record.SkillName, _ = result.Metadata["skill_name"].(string)
			prepared.record.SkillSource, _ = result.Metadata["skill_source"].(string)
			prepared.record.SkillDigest, _ = result.Metadata["skill_digest"].(string)
			prepared.record.SkillTrigger, _ = result.Metadata["trigger"].(string)
			prepared.record.SkillActivated, _ = result.Metadata["activated_at"].(string)
			prepared.record.AllowedTools, _ = result.Metadata["allowed_tools"].(string)
			prepared.record.SkillResource, _ = result.Metadata["resource"].(string)
			prepared.record.ResourceBytes, _ = result.Metadata["bytes"].(int)
			prepared.record.WebBackend, _ = result.Metadata["web_backend"].(string)
			prepared.record.WebTarget, _ = result.Metadata["web_target"].(string)
			prepared.record.WebStatus, _ = result.Metadata["web_status"].(string)
			prepared.record.WebSources, _ = result.Metadata["citation_count"].(int)
			prepared.record.WebInputTokens, _ = result.Metadata["web_input_tokens"].(int)
			prepared.record.WebOutputTokens, _ = result.Metadata["web_output_tokens"].(int)
			prepared.record.WebCostUSD, _ = result.Metadata["web_cost_usd"].(float64)
			prepared.record.UntrustedWebContent, _ = result.Metadata["untrusted_web_content"].(bool)
		}
		if e != nil && e.Audit != nil {
			e.Audit.Record(prepared.record)
		}
	})
	return result
}

func (e *Executor) deliverTerminal(prepared *preparedCall, result protocol.ToolResult, hooks BatchHooks) (protocol.ToolResult, error) {
	if hooks.OnStart != nil {
		if err := hooks.OnStart(prepared.call); err != nil {
			return e.finishPrepared(prepared, result, 0), err
		}
	}
	result = e.finishPrepared(prepared, result, 0)
	if hooks.OnResult != nil {
		if err := hooks.OnResult(result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func concurrencySpec(item Tool, input json.RawMessage, outcome policy.Outcome) ConcurrencySpec {
	if classifier, ok := item.(ConcurrencyClassifier); ok {
		return normalizeConcurrencySpec(classifier.ClassifyConcurrency(input, outcome))
	}
	if safe, ok := item.(ParallelSafe); ok && safe.ParallelSafe() {
		return ConcurrencySpec{Mode: ConcurrencyShared}
	}
	return ConcurrencySpec{Mode: ConcurrencyExclusive}
}

func normalizeConcurrencySpec(spec ConcurrencySpec) ConcurrencySpec {
	switch spec.Mode {
	case ConcurrencyShared:
		return ConcurrencySpec{Mode: ConcurrencyShared}
	case ConcurrencyClaimed:
		claims := make([]ResourceClaim, 0, len(spec.Claims))
		for _, claim := range spec.Claims {
			claim.Path = strings.ReplaceAll(strings.TrimSpace(claim.Path), "\\", "/")
			if claim.Path != "/" && !strings.HasSuffix(claim.Path, ":/") {
				claim.Path = strings.TrimSuffix(claim.Path, "/")
			}
			if claim.Path == "" || claim.Kind != ResourceFile && claim.Kind != ResourceTree || claim.Access != ResourceRead && claim.Access != ResourceWrite {
				return ConcurrencySpec{Mode: ConcurrencyExclusive}
			}
			claims = append(claims, claim)
		}
		if len(claims) == 0 {
			return ConcurrencySpec{Mode: ConcurrencyExclusive}
		}
		return ConcurrencySpec{Mode: ConcurrencyClaimed, Claims: claims}
	default:
		return ConcurrencySpec{Mode: ConcurrencyExclusive}
	}
}

func nextRunnable(prepared []*preparedCall, active map[int]*preparedCall) int {
	for index, item := range prepared {
		if item.done || item.running || item.terminal != nil {
			continue
		}
		if canStartCall(prepared, active, index) {
			return index
		}
	}
	return -1
}

func canStartCall(prepared []*preparedCall, active map[int]*preparedCall, index int) bool {
	item := prepared[index]
	if item.spec.Mode == ConcurrencyExclusive {
		if len(active) > 0 {
			return false
		}
		for earlier := 0; earlier < index; earlier++ {
			if !prepared[earlier].done {
				return false
			}
		}
		return true
	}
	for earlier := 0; earlier < index; earlier++ {
		other := prepared[earlier]
		if other.done {
			continue
		}
		if other.spec.Mode == ConcurrencyExclusive || concurrencyConflicts(item.spec, other.spec) {
			return false
		}
	}
	for _, other := range active {
		if other.spec.Mode == ConcurrencyExclusive || concurrencyConflicts(item.spec, other.spec) {
			return false
		}
	}
	return true
}

func concurrencyConflicts(left, right ConcurrencySpec) bool {
	if left.Mode == ConcurrencyExclusive || right.Mode == ConcurrencyExclusive {
		return true
	}
	if left.Mode != ConcurrencyClaimed || right.Mode != ConcurrencyClaimed {
		return false
	}
	for _, first := range left.Claims {
		for _, second := range right.Claims {
			if first.Access == ResourceRead && second.Access == ResourceRead {
				continue
			}
			if resourceClaimsOverlap(first, second) {
				return true
			}
		}
	}
	return false
}

func resourceClaimsOverlap(left, right ResourceClaim) bool {
	switch {
	case left.Kind == ResourceFile && right.Kind == ResourceFile:
		return left.Path == right.Path
	case left.Kind == ResourceTree && right.Kind == ResourceFile:
		return resourceTreeContains(left.Path, right.Path)
	case left.Kind == ResourceFile && right.Kind == ResourceTree:
		return resourceTreeContains(right.Path, left.Path)
	default:
		return resourceTreeContains(left.Path, right.Path) || resourceTreeContains(right.Path, left.Path)
	}
}

func resourceTreeContains(tree, candidate string) bool {
	prefix := tree
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return candidate == tree || strings.HasPrefix(candidate, prefix)
}

func withApprovalReason(definition protocol.ToolDefinition) protocol.ToolDefinition {
	if schemaHasProperty(definition.InputSchema, "reason") {
		return definition
	}
	var schema map[string]any
	if json.Unmarshal(definition.InputSchema, &schema) != nil || schema["type"] != "object" {
		return definition
	}
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
		schema["properties"] = properties
	}
	properties["reason"] = map[string]any{"type": "string", "minLength": 1, "description": "User-facing reason"}
	required, _ := schema["required"].([]any)
	schema["required"] = append(required, "reason")
	encoded, err := json.Marshal(schema)
	if err == nil {
		definition.InputSchema = encoded
	}
	return definition
}

func schemaHasProperty(schema json.RawMessage, name string) bool {
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &decoded) != nil {
		return false
	}
	_, exists := decoded.Properties[name]
	return exists
}

func withoutJSONField(input json.RawMessage, name string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(input, &fields) != nil {
		return input
	}
	if _, exists := fields[name]; !exists {
		return input
	}
	delete(fields, name)
	encoded, err := json.Marshal(fields)
	if err != nil {
		return input
	}
	return encoded
}

func truncateUTF8(value string, limit int) (string, bool) {
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	marker := "\n[output truncated]"
	if limit <= len(marker) {
		return marker[:limit], true
	}
	end := limit - len(marker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + marker, true
}
