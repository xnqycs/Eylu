package agent

import (
	"fmt"
	"strings"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
	"Eylu/internal/webtool"
)

func recordHostedWebActivities(executor *tool.Executor, runtime Runtime, requestID string, turn protocol.Turn, plan webtool.ResolvedWebToolPlan, budget *webtool.UsageBudget) error {
	limits := make(map[protocol.ToolKind]int, len(plan.Hosted))
	for _, hosted := range plan.Hosted {
		limits[hosted.Definition.Kind] = hosted.Definition.MaxUses
	}
	citations := make(map[string]int)
	for _, part := range turn.Parts {
		if part.Kind == protocol.PartCitation && part.Citation != nil {
			citations[part.Citation.CallID]++
		}
	}
	for _, part := range turn.Parts {
		if part.Kind != protocol.PartWebActivity || part.WebActivity == nil {
			continue
		}
		activity := part.WebActivity
		if !budget.Record(activity.Kind, limits[activity.Kind]) {
			return &protocol.Error{Code: protocol.ErrTool, Message: fmt.Sprintf("%s max_uses exceeded", activity.Kind)}
		}
		if executor.Audit == nil {
			continue
		}
		inputBytes := webActivityInputBytes(activity)
		executor.Audit.Record(tool.AuditRecord{
			SchemaVersion: tool.AuditSchemaVersion,
			Timestamp:     time.Now().UTC(), RequestID: requestID, SessionID: executor.SessionID,
			ProviderName: runtime.Provider.Name, ProviderGeneration: runtime.Provider.Generation, Model: runtime.Provider.Config.Model,
			CallID: activity.CallID, Tool: string(activity.Kind), Risk: policy.RiskNetwork, Decision: policy.DecisionAllow,
			Reason: "hosted web execution", Confirmed: runtime.Provider.Config.WebTools.Permission == "ask", DurationMS: activity.DurationMS,
			ExecutionDurationMS: activity.DurationMS, IsError: activity.Status == protocol.WebStatusError, InputBytes: inputBytes,
			Mode: runtime.PermissionMode, Classification: policy.CommandNotApplicable, WebBackend: string(protocol.ExecutionHosted),
			WebStatus: string(activity.Status), WebSources: max(len(activity.Sources), citations[activity.CallID]),
			WebInputTokens: activity.Usage.InputTokens, WebOutputTokens: activity.Usage.OutputTokens, WebCostUSD: activity.Usage.CostUSD,
			UntrustedWebContent: true,
		})
	}
	return nil
}

func webActivityInputBytes(activity *protocol.WebActivity) int {
	if activity == nil {
		return 0
	}
	values := append([]string(nil), activity.Queries...)
	query := strings.TrimSpace(activity.Query)
	found := false
	for _, value := range values {
		if strings.TrimSpace(value) == query {
			found = true
			break
		}
	}
	if query != "" && !found {
		values = append(values, query)
	}
	return len([]byte(strings.Join(values, ""))) + len([]byte(activity.URL))
}

// refreshMCPRuntime reads the live MCP catalog and applies it.
//
// The host callback that supplies the catalog runs outside the state lock, so it
