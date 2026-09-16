package driver

import (
	"errors"
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// The interoperability policy is a table, and every row of it is pinned here
// with a literal expectation. The drivers are checked against this function by
// the contract test in package driver_test, so a second mapping cannot appear
// without this table disagreeing with it.
func TestStopKindForImplementsThePolicyTable(t *testing.T) {
	tests := []struct {
		name        string
		reason      StopReason
		hasCalls    bool
		accept      bool
		want        protocol.StopKind
		wantInterop string
		wantErr     string
	}{
		{
			name:   "a tool call request runs its calls",
			reason: StopReasonToolUse, hasCalls: true,
			want: protocol.StopToolUse,
		},
		{
			name:    "a tool call request without calls is refused",
			reason:  StopReasonToolUse,
			wantErr: "without tool calls",
		},
		{
			name:   "a length stop keeps the answer and runs nothing",
			reason: StopReasonLength, want: protocol.StopLength,
		},
		{
			name:   "a length stop with a partial call still runs nothing",
			reason: StopReasonLength, hasCalls: true, want: protocol.StopLength,
		},
		{
			name:   "a completion without calls is a completion",
			reason: StopReasonCompleted, want: protocol.StopCompleted,
		},
		{
			name:   "a completion with calls contradicts itself",
			reason: StopReasonCompleted, hasCalls: true,
			wantErr: "completion while returning tool calls",
		},
		{
			name:   "the relaxation runs the calls and names itself",
			reason: StopReasonCompleted, hasCalls: true, accept: true,
			want: protocol.StopToolUse, wantInterop: InteropNoteToolCallsWithStop,
		},
		{
			name:   "a failure is not relaxed into a tool call",
			reason: StopReasonFailed, hasCalls: true, accept: true,
			want: protocol.StopError,
		},
		{
			name:   "a failure without calls is a failure",
			reason: StopReasonFailed, want: protocol.StopError,
		},
		{
			name:   "a cancellation is a cancellation",
			reason: StopReasonCancelled, want: protocol.StopCancelled,
		},
		{
			name:   "a cancellation is not relaxed into a tool call",
			reason: StopReasonCancelled, hasCalls: true, accept: true,
			want: protocol.StopCancelled,
		},
		{
			name:    "an unrecognized reason is refused",
			reason:  StopReason("queued"),
			wantErr: "unrecognized provider stop reason",
		},
		{
			name:   "an unrecognized reason is refused even with calls",
			reason: StopReason("queued"), hasCalls: true, accept: true,
			wantErr: "unrecognized provider stop reason",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, interop, err := StopKindFor(test.reason, test.hasCalls, test.accept)
			switch {
			case test.wantErr != "":
				var protocolErr *protocol.Error
				if !errors.As(err, &protocolErr) || protocolErr.Code != protocol.ErrProtocol {
					t.Fatalf("err = %v, want a protocol error", err)
				}
				if !strings.Contains(protocolErr.Message, test.wantErr) {
					t.Fatalf("message = %q, want %q", protocolErr.Message, test.wantErr)
				}
				if kind != "" || interop != "" {
					t.Fatalf("a refused row returned kind=%q interop=%q", kind, interop)
				}
				return
			case err != nil:
				t.Fatalf("err = %v", err)
			}
			if kind != test.want {
				t.Fatalf("kind = %q, want %q", kind, test.want)
			}
			if interop != test.wantInterop {
				t.Fatalf("interop = %q, want %q", interop, test.wantInterop)
			}
		})
	}
}
