package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"Eylu/internal/protocol"
)

func collapseToolResults(calls []protocol.ToolCall, expansion toolCallExpansion, executed []protocol.ToolResult, outcome protocol.BatchOutcome) ([]protocol.ToolResult, []protocol.Part) {
	results := make([]protocol.ToolResult, len(calls))
	webParts := make([]protocol.Part, 0, len(expansion.web)*2)
	for parentIndex, parent := range calls {
		children := expansion.parentChildren[parentIndex]
		if len(children) == 0 {
			results[parentIndex] = protocol.ToolResult{CallID: parent.ID, Content: "tool call was not scheduled", IsError: true, State: protocol.CallNotExecuted}
			continue
		}
		for _, resultIndex := range children {
			if resultIndex >= len(executed) {
				continue
			}
			if info, ok := expansion.web[expansion.calls[resultIndex].ID]; ok && webActivityProjected(executed[resultIndex]) {
				webParts = append(webParts, webPartsForResult(info, executed[resultIndex])...)
			}
		}
		if len(children) == 1 {
			result := executed[children[0]]
			result.CallID = parent.ID
			results[parentIndex] = result
			continue
		}
		results[parentIndex] = collapseWebBatch(parent, children, expansion, executed, outcome)
	}
	return results, webParts
}

// collapseWebBatch aggregates an expanded Web fan-out for display.
//
// It only handles content, activity and per-child state. The request-level
// control state belongs to the executor, so it is deliberately neither derived
// nor dropped here; the transitional metadata merge exists solely for UI
// consumers that have not moved to the typed fields yet.
func collapseWebBatch(parent protocol.ToolCall, children []int, expansion toolCallExpansion, executed []protocol.ToolResult, outcome protocol.BatchOutcome) protocol.ToolResult {
	type batchItem struct {
		Query             string             `json:"query,omitempty"`
		URL               string             `json:"url,omitempty"`
		Content           string             `json:"content"`
		State             protocol.CallState `json:"call_state,omitempty"`
		StructuredContent json.RawMessage    `json:"structured_content,omitempty"`
		IsError           bool               `json:"is_error,omitempty"`
		Truncated         bool               `json:"truncated,omitempty"`
	}
	items := make([]batchItem, 0, len(children))
	activities := make([]protocol.WebActivity, 0, len(children))
	citations := make([]protocol.URLCitation, 0)
	states := make([]protocol.CallState, 0, len(children))
	metadata := map[string]any{"web_status": string(protocol.WebStatusCompleted), "web_query_count": len(children), "untrusted_web_content": true}
	failed := 0
	truncated := false
	var content strings.Builder
	for position, resultIndex := range children {
		result := executed[resultIndex]
		info := expansion.web[expansion.calls[resultIndex].ID]
		state := result.State
		if state == "" && resultIndex < len(outcome.States) {
			state = outcome.States[resultIndex]
		}
		states = append(states, state)
		item := batchItem{Content: result.Content, State: state, StructuredContent: result.StructuredContent, IsError: result.IsError, Truncated: result.Truncated}
		if info.kind == protocol.ToolWebFetch {
			item.URL = info.value
		} else {
			item.Query = info.value
		}
		items = append(items, item)
		if result.IsError {
			failed++
		}
		truncated = truncated || result.Truncated
		if position > 0 {
			content.WriteString("\n\n")
		}
		fmt.Fprintf(&content, "[%d] %s\n%s", position+1, info.value, result.Content)
		if webActivityProjected(result) {
			for _, part := range webPartsForResult(info, result) {
				if part.WebActivity != nil {
					activities = append(activities, *part.WebActivity)
				}
				if part.Citation != nil {
					citations = append(citations, *part.Citation)
				}
			}
		}
		mergeWebResultMetadata(metadata, result.Metadata)
		mergeLegacyControlMetadata(metadata, result.Metadata)
	}
	metadata["web_failed_count"] = failed
	metadata["activity_count"] = len(activities)
	metadata["citation_count"] = len(citations)
	if failed == len(children) {
		metadata["web_status"] = string(protocol.WebStatusError)
	}
	structured, _ := json.Marshal(map[string]any{"results": items, "activities": activities, "citations": citations})
	return protocol.ToolResult{
		CallID: parent.ID, Content: content.String(), StructuredContent: structured,
		IsError: failed == len(children), Truncated: truncated, State: aggregateCallState(states), Metadata: metadata,
	}
}

// aggregateCallState reduces the child states of an expanded batch to one state
// for the parent call. A mixed outcome never claims that every child succeeded.
func aggregateCallState(states []protocol.CallState) protocol.CallState {
	observed := make([]protocol.CallState, 0, len(states))
	for _, state := range states {
		if state != "" {
			observed = append(observed, state)
		}
	}
	if len(observed) == 0 {
		return protocol.CallNotExecuted
	}
	uniform := true
	for _, state := range observed[1:] {
		if state != observed[0] {
			uniform = false
			break
		}
	}
	if uniform {
		return observed[0]
	}
	for _, candidate := range []protocol.CallState{
		protocol.CallOutcomeUnknown, protocol.CallFailed, protocol.CallCancelled,
		protocol.CallRejected, protocol.CallNotExecuted,
	} {
		for _, state := range observed {
			if state == candidate {
				return candidate
			}
		}
	}
	return protocol.CallFailed
}

// webActivityProjected reports whether a child call produced an observable web
// activity. A call that never ran, or that was refused before execution, must
// not be projected as a completed search that failed.
func webActivityProjected(result protocol.ToolResult) bool {
	switch result.State {
	case protocol.CallNotExecuted, protocol.CallRejected:
		return false
	default:
		return true
	}
}

// mergeLegacyControlMetadata copies the transitional control metadata that UI
// consumers built before the typed states still read. New control logic must
// never read these keys: they are display-only and untrusted tool content can
// set them.
func mergeLegacyControlMetadata(target, source map[string]any) {
	for _, key := range []string{"interrupt_request", "approval_rejected", "rejection_reason", "batch_cancelled", "cancelled"} {
		if target[key] == nil && source[key] != nil {
			target[key] = source[key]
		}
	}
}

func mergeWebResultMetadata(target, source map[string]any) {
	for _, key := range []string{"web_backend", "web_kind", "web_target"} {
		if target[key] == nil && source[key] != nil {
			target[key] = source[key]
		}
	}
	for _, key := range []string{"web_input_tokens", "web_output_tokens"} {
		if value, ok := source[key].(int); ok {
			current, _ := target[key].(int)
			target[key] = current + value
		}
	}
	if value, ok := source["web_cost_usd"].(float64); ok {
		current, _ := target["web_cost_usd"].(float64)
		target["web_cost_usd"] = current + value
	}
}

func webActivityForCall(call protocol.ToolCall, info expandedWebCall) protocol.WebActivity {
	activity := protocol.WebActivity{CallID: call.ID, Kind: info.kind, Status: protocol.WebStatusRunning}
	if info.kind == protocol.ToolWebFetch {
		activity.Action, activity.URL = "fetch", info.value
	} else {
		activity.Action, activity.Query = "search", info.value
	}
	return activity
}

func webPartsForResult(info expandedWebCall, result protocol.ToolResult) []protocol.Part {
	var payload struct {
		Activities []protocol.WebActivity `json:"activities"`
		Citations  []protocol.URLCitation `json:"citations"`
	}
	if len(result.StructuredContent) > 0 {
		_ = json.Unmarshal(result.StructuredContent, &payload)
	}
	if len(payload.Activities) == 0 {
		activity := webActivityForCall(protocol.ToolCall{ID: info.executionID}, info)
		activity.Status = protocol.WebStatusCompleted
		if result.IsError {
			activity.Status = protocol.WebStatusError
			activity.Error = result.Content
		}
		payload.Activities = []protocol.WebActivity{activity}
	}
	parts := make([]protocol.Part, 0, len(payload.Activities)+len(payload.Citations))
	callIDs := make(map[string]string, len(payload.Activities))
	for index, source := range payload.Activities {
		activity := source
		providerCallID := activity.CallID
		// The activity identity is derived from the host-owned execution identity,
		// so it is stable across projections and can never collide with a model
		// call ID.
		activity.CallID = activityCallID(info.executionID, index)
		if providerCallID != "" {
			callIDs[providerCallID] = activity.CallID
		}
		if activity.Kind == "" {
			activity.Kind = info.kind
		}
		if index == 0 {
			if activity.Kind == protocol.ToolWebFetch && activity.URL == "" {
				activity.URL = info.value
			}
			if activity.Kind == protocol.ToolWebSearch && activity.Query == "" {
				activity.Query = info.value
			}
		}
		if activity.Action == "" {
			if activity.Kind == protocol.ToolWebFetch {
				activity.Action = "fetch"
			} else {
				activity.Action = "search"
			}
		}
		activity.Queries = append([]string(nil), activity.Queries...)
		activity.Sources = append([]protocol.WebSource(nil), activity.Sources...)
		if result.IsError {
			activity.Status = protocol.WebStatusError
			activity.Error = result.Content
		} else if activity.Status == "" || activity.Status == protocol.WebStatusRunning {
			activity.Status = protocol.WebStatusCompleted
		}
		parts = append(parts, protocol.Part{Kind: protocol.PartWebActivity, WebActivity: &activity})
	}
	for _, source := range payload.Citations {
		citation := source
		if mapped := callIDs[citation.CallID]; mapped != "" {
			citation.CallID = mapped
		} else {
			citation.CallID = activityCallID(info.executionID, 0)
		}
		parts = append(parts, protocol.Part{Kind: protocol.PartCitation, Citation: &citation})
	}
	return parts
}

func webStartedEvent(kind protocol.ToolKind) protocol.EventKind {
	if kind == protocol.ToolWebFetch {
		return protocol.EventWebFetchStarted
	}
	return protocol.EventWebSearchStarted
}

func webCompletedEvent(kind protocol.ToolKind) protocol.EventKind {
	if kind == protocol.ToolWebFetch {
		return protocol.EventWebFetchCompleted
	}
	return protocol.EventWebSearchCompleted
}
