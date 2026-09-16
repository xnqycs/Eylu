package driver

import (
	"fmt"

	"Eylu/internal/protocol"
)

// StopReason is a provider's stopping condition expressed in the one vocabulary
// the interoperability policy is written in.
//
// Every driver translates its own dialect into these values and then asks
// StopKindFor what to do, so the policy table has exactly one implementation and
// a new dialect cannot quietly invent a second one. A value this client cannot
// explain is carried through unchanged, so the policy can reject it by name.
type StopReason string

const (
	// StopReasonToolUse: the provider stopped so that the tool calls it returned
	// could run.
	StopReasonToolUse StopReason = "tool_use"
	// StopReasonLength: the provider stopped at a limit of its own (an output
	// cap, a content filter). A partial call may be carried, and it is closed
	// without being executed.
	StopReasonLength StopReason = "length"
	// StopReasonCompleted: the provider finished and asked for nothing.
	StopReasonCompleted StopReason = "completed"
	// StopReasonFailed: the provider reported the response as failed.
	StopReasonFailed StopReason = "failed"
	// StopReasonCancelled: the provider reported the response as cancelled.
	StopReasonCancelled StopReason = "cancelled"
)

// InteropNoteToolCallsWithStop names the relaxation that lets a response which
// reports completion while returning tool calls be executed anyway. It is
// recorded on the response, in the run report and in the audit trail, so a
// relaxed request is never indistinguishable from a conforming one.
const InteropNoteToolCallsWithStop = "accept_tool_calls_with_stop"

// StopKindFor is the single mapping between a provider's stopping condition and
// the protocol's stop kind.
//
// The default is strict: a condition this client cannot explain, or one that
// contradicts the content of the response, is refused instead of being guessed
// at. The only relaxation is acceptToolCallsWithStop, and it covers exactly one
// row of the table - a provider that says "completed" while returning tool
// calls. When it applies, the returned note names the relaxation so the caller
// can record it.
//
//	| provider behaviour                       | default        | relaxation |
//	|------------------------------------------|----------------|------------|
//	| tool_calls / function_call               | tool_use       | none       |
//	| length / content_filter                  | length         | none       |
//	| completed, no calls                      | completed      | none       |
//	| completed, with calls                    | protocol error | accept_tool_calls_with_stop -> tool_use |
//	| failed / cancelled                       | error / cancelled | none    |
//	| tool_use without calls                   | protocol error | none       |
//	| unrecognized value                       | protocol error | none       |
func StopKindFor(reason StopReason, hasCalls, acceptToolCallsWithStop bool) (kind protocol.StopKind, interop string, err error) {
	switch reason {
	case StopReasonToolUse:
		if !hasCalls {
			return "", "", &protocol.Error{Code: protocol.ErrProtocol, Message: "model stopped for tool use without tool calls"}
		}
		return protocol.StopToolUse, "", nil
	case StopReasonLength:
		// A truncated or filtered answer may carry a partial call. The caller
		// keeps the answer and closes the call without executing it, so this row
		// is never an error.
		return protocol.StopLength, "", nil
	case StopReasonFailed:
		return protocol.StopError, "", nil
	case StopReasonCancelled:
		return protocol.StopCancelled, "", nil
	case StopReasonCompleted:
		if !hasCalls {
			return protocol.StopCompleted, "", nil
		}
		if !acceptToolCallsWithStop {
			return "", "", &protocol.Error{Code: protocol.ErrProtocol, Message: "model reported completion while returning tool calls"}
		}
		return protocol.StopToolUse, InteropNoteToolCallsWithStop, nil
	default:
		return "", "", &protocol.Error{Code: protocol.ErrProtocol, Message: fmt.Sprintf("unrecognized provider stop reason %q", string(reason))}
	}
}
