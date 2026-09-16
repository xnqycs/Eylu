package agent

import (
	"strings"
	"testing"

	"Eylu/internal/protocol"
)

// A duplicate turn ID is the one transcript failure a human can act on, so the
// message says which session, which turn, and where to look. Before this the
// message named neither the session nor a way forward, which is what made an old
// log look like an unreadable session.
func TestDuplicateTurnDiagnosticNamesTheSessionAndTheWayOut(t *testing.T) {
	state := NewConversation().ExportState()
	state.SessionID = "legacy-session"
	state.Turns = []protocol.Turn{
		{ID: "turn-1", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "one"}}},
		{ID: "turn-1", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "two"}}},
	}
	_, err := RestoreConversation(state)
	if err == nil {
		t.Fatal("a transcript with a duplicate turn ID was accepted")
	}
	for _, want := range []string{"legacy-session", "turn-1", "LoadRecovering", "event log"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message = %q, missing %q", err.Error(), want)
		}
	}
}

// A turn without an ID is still refused, and it also names its session.
func TestTurnWithoutAnIDNamesItsSession(t *testing.T) {
	state := NewConversation().ExportState()
	state.SessionID = "nameless"
	state.Turns = []protocol.Turn{{Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "one"}}}}
	if _, err := RestoreConversation(state); err == nil || !strings.Contains(err.Error(), "nameless") {
		t.Fatalf("err = %v", err)
	}
}
