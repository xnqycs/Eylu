package agent

import (
	"encoding/json"

	"Eylu/internal/protocol"
	"Eylu/internal/webtool"
)

type expandedWebCall struct {
	parentIndex int
	childIndex  int
	kind        protocol.ToolKind
	value       string
	// executionID is the host-owned identity of this execution.
	executionID string
	// parentCallID is the model call ID of the request that produced it.
	parentCallID string
}

type toolCallExpansion struct {
	calls          []protocol.ToolCall
	parentChildren [][]int
	web            map[string]expandedWebCall
}

// expandToolCalls turns one model response into the calls the executor runs.
//
// A call the host expands into several executions gets a host-owned execution ID;
// the model call ID is only kept as the explicit parent relation and as the ID the
// collapsed result is reported under. Calls the host does not expand are already
// provider-level calls, so their model ID is their execution identity.
func expandToolCalls(calls []protocol.ToolCall, plan webtool.ResolvedWebToolPlan, allocator *executionAllocator) toolCallExpansion {
	expansion := toolCallExpansion{
		calls: make([]protocol.ToolCall, 0, len(calls)), parentChildren: make([][]int, len(calls)),
		web: make(map[string]expandedWebCall),
	}
	local := make(map[string]webtool.ResolvedTool, len(plan.Local))
	for _, resolved := range plan.Local {
		local[resolved.Definition.Name] = resolved
	}
	for parentIndex, call := range calls {
		resolved, isWeb := local[call.Name]
		values, err := webtool.InputValues(resolved.Definition.Kind, call.Arguments)
		if !isWeb || err != nil {
			index := len(expansion.calls)
			expansion.calls = append(expansion.calls, call)
			expansion.parentChildren[parentIndex] = append(expansion.parentChildren[parentIndex], index)
			if isWeb {
				expansion.web[call.ID] = expandedWebCall{parentIndex: parentIndex, kind: resolved.Definition.Kind, executionID: call.ID, parentCallID: call.ID}
			}
			continue
		}
		field := "query"
		if resolved.Definition.Kind == protocol.ToolWebFetch {
			field = "url"
		}
		for childIndex, value := range values {
			executionID := allocator.allocate()
			arguments, _ := json.Marshal(map[string]any{field: value, "_eylu_batch_id": call.ID})
			child := protocol.ToolCall{ID: executionID, Name: call.Name, Arguments: arguments, ParentCallID: call.ID}
			index := len(expansion.calls)
			expansion.calls = append(expansion.calls, child)
			expansion.parentChildren[parentIndex] = append(expansion.parentChildren[parentIndex], index)
			expansion.web[executionID] = expandedWebCall{
				parentIndex: parentIndex, childIndex: childIndex, kind: resolved.Definition.Kind, value: value,
				executionID: executionID, parentCallID: call.ID,
			}
		}
	}
	return expansion
}
