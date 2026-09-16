package agent

import (
	"Eylu/internal/protocol"
	"Eylu/internal/tool"
)

// projectToolEvents turns one expanded batch into the batch hooks that report it
// to the host.
//
// The projector only translates: it maps an execution to the events the UI needs
// and never decides the fate of the request. Control state comes from the
// executor, and content aggregation happens separately, so a Web-specific detail
// can never change whether the request continues.
func projectToolEvents(emit func(protocol.ModelEvent) error, expansion toolCallExpansion) tool.BatchHooks {
	if emit == nil {
		return tool.BatchHooks{}
	}
	hooks := tool.BatchHooks{}
	hooks.OnStart = func(call protocol.ToolCall) error {
		if info, ok := expansion.web[call.ID]; ok {
			activity := webActivityForCall(call, info)
			return emit(protocol.ModelEvent{Kind: webStartedEvent(info.kind), WebActivity: &activity})
		}
		return emit(protocol.ModelEvent{Kind: protocol.EventToolStart, ToolCall: &call})
	}
	hooks.OnResult = func(result protocol.ToolResult) error {
		info, isWeb := expansion.web[result.CallID]
		if !isWeb {
			return emit(protocol.ModelEvent{Kind: protocol.EventToolResult, ToolResult: &result})
		}
		for _, part := range webPartsForResult(info, result) {
			switch {
			case part.WebActivity != nil:
				// A provider may report several activities for one execution; each
				// one is announced before its completion.
				if part.WebActivity.CallID != activityCallID(info.executionID, 0) {
					started := *part.WebActivity
					started.Status = protocol.WebStatusRunning
					started.Error = ""
					if err := emit(protocol.ModelEvent{Kind: webStartedEvent(started.Kind), WebActivity: &started}); err != nil {
						return err
					}
				}
				if err := emit(protocol.ModelEvent{Kind: webCompletedEvent(part.WebActivity.Kind), WebActivity: part.WebActivity}); err != nil {
					return err
				}
			case part.Citation != nil:
				if err := emit(protocol.ModelEvent{Kind: protocol.EventCitation, Citation: part.Citation}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return hooks
}
