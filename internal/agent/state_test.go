package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"Eylu/internal/environment"
	"Eylu/internal/protocol"
	"Eylu/internal/provider"
)

func TestConversationStateRoundTripAndDeepClone(t *testing.T) {
	model := &scriptedDriver{}
	environmentContext := environment.Context{WorkingDirectory: "C:/workspace", Platform: "windows", Today: "2026-07-19"}
	conversation := NewConversationWithEnvironment(environmentContext)
	runtime := testRuntime(model, 7)
	runtime.Provider.Config.CatalogProvider = "openai"
	runtime.Provider.Limits = provider.ModelLimits{ContextWindow: 24000, MaxOutputTokens: 4096, Source: provider.LimitSourceModelsDev, ObservedAt: time.Now().UTC(), Cached: true}
	runtime.Provider.EffectiveContextWindow = 24000
	runtime.Workspace = t.TempDir()
	if _, err := conversation.Send(context.Background(), "remember state", runtime, false, nil); err != nil {
		t.Fatal(err)
	}
	conversation.mu.Lock()
	conversation.summary = "summary-marker"
	conversation.omittedTurnIDs[conversation.turns[0].ID] = struct{}{}
	conversation.protectedSkills["demo"] = ProtectedSkill{Name: "demo", Source: "user_eylu", Entry: "SKILL.md", Root: "skills/demo", Digest: "digest", Content: "body", Trigger: "model", ActivatedAt: time.Now().UTC()}
	conversation.mu.Unlock()
	state := conversation.ExportState()
	if state.Provider.Generation != 7 || state.Provider.Model != "test-model" || state.Provider.CatalogProvider != "openai" || state.Provider.DetectedContextWindow != 24000 || state.Provider.EffectiveContextWindow != 24000 || state.Provider.LimitSource != string(provider.LimitSourceModelsDev) || !state.Provider.LimitCached || state.Environment != environmentContext || state.Summary != "summary-marker" || len(state.ProtectedSkills) != 1 || len(state.DriverState) == 0 {
		t.Fatalf("state = %#v", state)
	}
	state.Turns[0].Parts[0].Text = "mutated"
	if conversation.Transcript()[0].Parts[0].Text != "remember state" {
		t.Fatal("exported state shares turn memory")
	}
	state = conversation.ExportState()
	restored, err := RestoreConversation(state)
	if err != nil {
		t.Fatal(err)
	}
	restoredState := restored.ExportState()
	if restored.SessionID() != conversation.SessionID() || len(restored.Transcript()) != 2 || restoredState.Provider.CatalogProvider != "openai" || restoredState.Provider.DetectedContextWindow != 24000 || restoredState.Provider.EffectiveContextWindow != 24000 || restoredState.Environment != environmentContext || restoredState.Summary != "summary-marker" || restoredState.ProtectedSkills[0].Digest != "digest" || !json.Valid(restoredState.DriverState) {
		t.Fatalf("restored = %#v", restoredState)
	}
	if report := restored.ContextReport(); report.LastUsage.InputTokens != conversation.ContextReport().LastUsage.InputTokens {
		t.Fatalf("report = %#v", report)
	}
}

func TestRestoreConversationRejectsInvalidTranscript(t *testing.T) {
	turn := protocol.Turn{ID: "duplicate", Role: protocol.RoleUser, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "x"}}}
	_, err := RestoreConversation(ConversationState{SessionID: "session", Turns: []protocol.Turn{turn, turn}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("error = %v", err)
	}
}
