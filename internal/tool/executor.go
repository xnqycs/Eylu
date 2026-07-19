package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type Confirmation struct {
	Approved        bool
	RejectionReason string
}

type ConfirmFunc func(context.Context, policy.Request, policy.Outcome) (Confirmation, error)

type AuditRecord struct {
	Timestamp          time.Time           `json:"timestamp"`
	RequestID          string              `json:"request_id"`
	SessionID          string              `json:"session_id,omitempty"`
	ProviderName       string              `json:"provider_name,omitempty"`
	ProviderGeneration uint64              `json:"provider_generation,omitempty"`
	Model              string              `json:"model,omitempty"`
	CallID             string              `json:"call_id"`
	Tool               string              `json:"tool"`
	Risk               policy.Risk         `json:"risk"`
	Decision           policy.Decision     `json:"decision"`
	Reason             string              `json:"reason"`
	Confirmed          bool                `json:"confirmed"`
	DurationMS         int64               `json:"duration_ms"`
	IsError            bool                `json:"is_error"`
	Truncated          bool                `json:"truncated"`
	InputBytes         int                 `json:"input_bytes"`
	OutputBytes        int                 `json:"output_bytes"`
	ExitCode           int                 `json:"exit_code,omitempty"`
	Mode               string              `json:"mode"`
	Classification     policy.CommandClass `json:"classification"`
	Confirmations      int                 `json:"confirmations"`
	Warning            bool                `json:"warning"`
	SkillName          string              `json:"skill_name,omitempty"`
	SkillSource        string              `json:"skill_source,omitempty"`
	SkillDigest        string              `json:"skill_digest,omitempty"`
	SkillTrigger       string              `json:"skill_trigger,omitempty"`
	SkillActivated     string              `json:"skill_activated_at,omitempty"`
	AllowedTools       string              `json:"allowed_tools,omitempty"`
	SkillResource      string              `json:"skill_resource,omitempty"`
	ResourceBytes      int                 `json:"resource_bytes,omitempty"`
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

func (e *Executor) Definitions() []protocol.ToolDefinition {
	if e == nil || e.Registry == nil {
		return nil
	}
	definitions := e.Registry.Definitions()
	for index := range definitions {
		item, ok := e.Registry.Get(definitions[index].Name)
		if ok && item.Risk() != policy.RiskRead {
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
	safe, ok := item.(ParallelSafe)
	return ok && safe.ParallelSafe()
}

func (e *Executor) ExecuteConcurrent(ctx context.Context, requestID string, calls []protocol.ToolCall) []protocol.ToolResult {
	results := make([]protocol.ToolResult, len(calls))
	if len(calls) == 0 {
		return results
	}
	for _, call := range calls {
		if !e.CanExecuteConcurrently(call) {
			for index, sequential := range calls {
				results[index] = e.Execute(ctx, requestID, sequential)
			}
			return results
		}
	}
	limit := e.MaxParallelTools
	if limit <= 0 {
		limit = 4
	}
	if limit > len(calls) {
		limit = len(calls)
	}
	semaphore := make(chan struct{}, limit)
	var wait sync.WaitGroup
	for index, call := range calls {
		index, call := index, call
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
				results[index] = e.Execute(ctx, requestID, call)
			case <-ctx.Done():
				results[index] = e.Execute(ctx, requestID, call)
			}
		}()
	}
	wait.Wait()
	return results
}

func (e *Executor) Execute(ctx context.Context, requestID string, call protocol.ToolCall) (result protocol.ToolResult) {
	started := time.Now()
	result = protocol.ToolResult{CallID: call.ID}
	record := AuditRecord{Timestamp: time.Now().UTC(), RequestID: requestID, CallID: call.ID, Tool: call.Name, InputBytes: len(call.Arguments)}
	if e != nil {
		record.SessionID, record.ProviderName = e.SessionID, e.ProviderName
		record.ProviderGeneration, record.Model = e.ProviderGeneration, e.Model
	}
	defer func() {
		record.DurationMS = time.Since(started).Milliseconds()
		record.IsError = result.IsError
		record.Truncated = result.Truncated
		record.OutputBytes = len([]byte(result.Content))
		if result.Metadata != nil {
			if exitCode, ok := result.Metadata["exit_code"].(int); ok {
				record.ExitCode = exitCode
			}
			record.SkillName, _ = result.Metadata["skill_name"].(string)
			record.SkillSource, _ = result.Metadata["skill_source"].(string)
			record.SkillDigest, _ = result.Metadata["skill_digest"].(string)
			record.SkillTrigger, _ = result.Metadata["trigger"].(string)
			record.SkillActivated, _ = result.Metadata["activated_at"].(string)
			record.AllowedTools, _ = result.Metadata["allowed_tools"].(string)
			record.SkillResource, _ = result.Metadata["resource"].(string)
			record.ResourceBytes, _ = result.Metadata["bytes"].(int)
		}
		if e != nil && e.Audit != nil {
			e.Audit.Record(record)
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			result.CallID = call.ID
			result.IsError = true
			result.Content = fmt.Sprintf("tool execution panicked: %v", recovered)
		}
	}()
	if e == nil || e.Registry == nil {
		result.IsError = true
		result.Content = "tool executor is unavailable"
		return result
	}
	if ctx.Err() != nil {
		result.IsError = true
		result.Content = "tool execution cancelled"
		return result
	}
	item, ok := e.Registry.Get(call.Name)
	if !ok {
		result.IsError = true
		result.Content = fmt.Sprintf("unknown tool %q", call.Name)
		return result
	}
	if !json.Valid(call.Arguments) {
		result.IsError = true
		result.Content = "tool input is invalid JSON"
		return result
	}
	checker := e.Policy
	if checker == nil {
		checker = policy.BaselineChecker{}
	}
	policyRequest := policy.Request{Tool: call.Name, Input: call.Arguments, Workspace: e.Workspace, Risk: item.Risk()}
	outcome := checker.Check(ctx, policyRequest)
	record.Risk, record.Decision, record.Reason = outcome.Risk, outcome.Decision, outcome.Reason
	record.Mode, record.Classification, record.Warning = outcome.Mode.String(), outcome.Classification, outcome.Warning
	switch outcome.Decision {
	case policy.DecisionDeny:
		result.IsError = true
		result.Content = "permission denied: " + outcome.Reason
		return result
	case policy.DecisionConfirm:
		if e.Confirm == nil {
			result.IsError = true
			result.Content = "confirmation required: " + outcome.Reason
			return result
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
				result.IsError = true
				result.Content = "confirmation failed: " + err.Error()
				return result
			}
			if !confirmation.Approved {
				result.IsError = true
				result.Content = "approval rejected"
				result.Metadata = map[string]any{"approval_rejected": true}
				if reason := strings.TrimSpace(confirmation.RejectionReason); reason != "" {
					result.Content += ": " + reason
					result.Metadata["rejection_reason"] = reason
				} else {
					result.Metadata["interrupt_request"] = true
				}
				return result
			}
			record.Confirmations++
		}
		record.Confirmed = true
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	executionInput := call.Arguments
	if !schemaHasProperty(item.Definition().InputSchema, "reason") {
		executionInput = withoutJSONField(executionInput, "reason")
	}
	result = item.Execute(toolCtx, executionInput)
	result.CallID = call.ID
	if toolCtx.Err() == context.DeadlineExceeded {
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
