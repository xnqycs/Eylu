package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
)

type ConfirmFunc func(context.Context, policy.Request, policy.Outcome) (bool, error)

type AuditRecord struct {
	RequestID      string              `json:"request_id"`
	CallID         string              `json:"call_id"`
	Tool           string              `json:"tool"`
	Risk           policy.Risk         `json:"risk"`
	Decision       policy.Decision     `json:"decision"`
	Reason         string              `json:"reason"`
	Confirmed      bool                `json:"confirmed"`
	DurationMS     int64               `json:"duration_ms"`
	IsError        bool                `json:"is_error"`
	Truncated      bool                `json:"truncated"`
	InputBytes     int                 `json:"input_bytes"`
	OutputBytes    int                 `json:"output_bytes"`
	ExitCode       int                 `json:"exit_code,omitempty"`
	Mode           string              `json:"mode"`
	Classification policy.CommandClass `json:"classification"`
	Confirmations  int                 `json:"confirmations"`
	Warning        bool                `json:"warning"`
	SkillName      string              `json:"skill_name,omitempty"`
	SkillSource    string              `json:"skill_source,omitempty"`
	SkillDigest    string              `json:"skill_digest,omitempty"`
	SkillTrigger   string              `json:"skill_trigger,omitempty"`
	SkillActivated string              `json:"skill_activated_at,omitempty"`
	AllowedTools   string              `json:"allowed_tools,omitempty"`
	SkillResource  string              `json:"skill_resource,omitempty"`
	ResourceBytes  int                 `json:"resource_bytes,omitempty"`
}

type AuditSink interface {
	Record(AuditRecord)
}

type Executor struct {
	Registry       *Registry
	Policy         policy.Checker
	Confirm        ConfirmFunc
	Audit          AuditSink
	Workspace      string
	Timeout        time.Duration
	MaxOutputBytes int
}

func (e *Executor) Definitions() []protocol.ToolDefinition {
	if e == nil || e.Registry == nil {
		return nil
	}
	return e.Registry.Definitions()
}

func (e *Executor) Execute(ctx context.Context, requestID string, call protocol.ToolCall) protocol.ToolResult {
	started := time.Now()
	result := protocol.ToolResult{CallID: call.ID}
	record := AuditRecord{RequestID: requestID, CallID: call.ID, Tool: call.Name, InputBytes: len(call.Arguments)}
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
	if e == nil || e.Registry == nil {
		result.IsError = true
		result.Content = "tool executor is unavailable"
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
			allowed, err := e.Confirm(ctx, policyRequest, outcome)
			if err != nil {
				result.IsError = true
				result.Content = "confirmation failed: " + err.Error()
				return result
			}
			if !allowed {
				result.IsError = true
				result.Content = fmt.Sprintf("confirmation rejected at step %d of %d", step, confirmations)
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
	result = item.Execute(toolCtx, call.Arguments)
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
