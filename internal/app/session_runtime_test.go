package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/provider"
	"Eylu/internal/session"
	"Eylu/internal/skill"
)

func TestChatSessionSurvivesRestartWithoutDriverState(t *testing.T) {
	home := isolateUserState(t)
	workspace := t.TempDir()
	t.Setenv("EYLU_API_KEY", "resume-secret")
	var mu sync.Mutex
	requests := make([][]byte, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		encoded, _ := json.Marshal(body)
		mu.Lock()
		requests = append(requests, encoded)
		number := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		answer := "first-answer"
		responseID := "response-one"
		if number == 2 {
			answer = "second-answer"
			responseID = "response-two"
		}
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" + responseID + "\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"" + answer + "\"}]}]}}\n\n"))
	}))
	defer server.Close()
	configPath := filepath.Join(workspace, "config.toml")
	baseArgs := []string{"--config", configPath, "--workspace", workspace, "chat"}

	var stdout, stderr bytes.Buffer
	args := append(append([]string(nil), baseArgs...), "remember-me", "--base-url", server.URL+"/v1", "--model", "test-model", "--mode", "plan", "--session", "durable")
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("first exit=%d stderr=%s", code, stderr.String())
	}
	store, err := session.Open("")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := store.Load("durable")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.DriverState = nil
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	args = append(append([]string(nil), baseArgs...), "what-did-I-say", "--base-url", server.URL+"/v1", "--model", "test-model", "--resume")
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("resume exit=%d stderr=%s", code, stderr.String())
	}
	if stdout.String() != "second-answer\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	mu.Lock()
	if len(requests) != 2 || !bytes.Contains(requests[1], []byte("remember-me")) || !bytes.Contains(requests[1], []byte("first-answer")) {
		t.Fatalf("restored request = %s", requests[1])
	}
	mu.Unlock()
	restored, diagnostics, err := store.Load("durable")
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("load error=%v diagnostics=%#v", err, diagnostics)
	}
	if restored.PermissionMode != "plan" || restored.Provider.Name != "runtime" || restored.Provider.Model != "test-model" || len(restored.Turns) != 4 {
		t.Fatalf("restored = %#v", restored)
	}
	for _, name := range []string{"snapshot.json", "events.jsonl"} {
		data, err := os.ReadFile(filepath.Join(home, "state", "sessions", "durable", name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("resume-secret")) {
			t.Fatalf("credential leaked into %s", name)
		}
	}
}

func TestNewClosesOldSessionAndCreatesIsolatedSession(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	cfg := config.Default(workspace)
	cfg.ActiveProvider = "test"
	cfg.Providers["test"] = config.ProviderConfig{
		Adapter: "openai_responses", BaseURL: "https://example.test/v1", Model: "model",
		Credential: config.CredentialRef{Type: "none"},
	}
	manager, err := provider.NewManager(filepath.Join(workspace, "config.toml"), cfg, func(string, config.Config) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr, credentials: provider.NewCredentialStore(), trustPrompted: make(map[string]bool)}
	opts := chatOptions{}
	conversation, err := runtime.openConversation(manager, &opts)
	if err != nil {
		t.Fatal(err)
	}
	oldID := conversation.SessionID()
	if err := runtime.handleSlashCommand(context.Background(), bufio.NewReader(strings.NewReader("")), "/new", conversation, manager, &opts); err != nil {
		t.Fatal(err)
	}
	newID := conversation.SessionID()
	if oldID == newID || !strings.Contains(stdout.String(), oldID) || !strings.Contains(stdout.String(), newID) {
		t.Fatalf("old=%s new=%s output=%s", oldID, newID, stdout.String())
	}
	store, _ := session.Open("")
	oldSnapshot, _, err := store.Load(oldID)
	if err != nil || oldSnapshot.ClosedAt == nil {
		t.Fatalf("old snapshot=%#v error=%v", oldSnapshot, err)
	}
	newSnapshot, _, err := store.Load(newID)
	if err != nil || newSnapshot.ClosedAt != nil || len(newSnapshot.Turns) != 0 {
		t.Fatalf("new snapshot=%#v error=%v", newSnapshot, err)
	}
	items, err := store.List()
	if err != nil || len(items) != 2 {
		t.Fatalf("sessions=%#v error=%v", items, err)
	}
}

func TestSessionsDeleteRequiresConfirmation(t *testing.T) {
	isolateUserState(t)
	store, _ := session.Open("")
	if _, err := store.Create(session.Snapshot{SessionID: "delete-me", Workspace: t.TempDir(), PermissionMode: "manual"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"sessions", "delete", "delete-me"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), "requires confirmation") {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"sessions", "delete", "delete-me", "--yes"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Deleted session delete-me") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, _, err := store.Load("delete-me"); !os.IsNotExist(err) {
		t.Fatalf("deleted session load error = %v", err)
	}
}

func TestSessionSkillResumeRevalidatesDigest(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(t.TempDir(), "review")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkill := func(body string) {
		t.Helper()
		content := "---\nname: review\ndescription: Review repository changes\n---\n" + body
		if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill("saved instructions")
	parsed, err := skill.ParseDirectory(directory, skill.SourceBuiltin, true)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: home, Builtins: []skill.Skill{parsed}})
	if err != nil {
		t.Fatal(err)
	}
	activatedAt := time.Now().UTC().Add(-time.Hour)
	persisted := session.SkillState{
		Name: parsed.Name, Source: skill.SourceBuiltin.String(), Entry: parsed.Entry, Root: parsed.Root,
		Digest: parsed.Digest, Trigger: "user", ActivatedAt: activatedAt,
	}

	t.Run("unchanged", func(t *testing.T) {
		conversation, err := agent.RestoreConversation(agent.ConversationState{SessionID: "skill-unchanged", PermissionMode: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		controller := &sessionRuntime{snapshot: session.Snapshot{Skills: []session.SkillState{persisted}}}
		diagnostics := controller.RevalidateSkills(registry, skill.NewSession(registry, nil), conversation)
		if len(diagnostics) != 0 {
			t.Fatalf("diagnostics = %#v", diagnostics)
		}
		state := conversation.ExportState()
		if len(state.ProtectedSkills) != 1 || state.ProtectedSkills[0].Digest != parsed.Digest || !strings.Contains(state.ProtectedSkills[0].Content, "saved instructions") || state.ProtectedSkills[0].Trigger != "user" {
			t.Fatalf("protected skills = %#v", state.ProtectedSkills)
		}
	})

	t.Run("changed-after-discovery", func(t *testing.T) {
		writeSkill("changed instructions")
		conversation, err := agent.RestoreConversation(agent.ConversationState{SessionID: "skill-changed", PermissionMode: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		controller := &sessionRuntime{snapshot: session.Snapshot{Skills: []session.SkillState{persisted}}}
		diagnostics := controller.RevalidateSkills(registry, skill.NewSession(registry, nil), conversation)
		if len(diagnostics) != 1 || !strings.Contains(diagnostics[0], "changed while revalidating") {
			t.Fatalf("diagnostics = %#v", diagnostics)
		}
		if len(conversation.ExportState().ProtectedSkills) != 0 {
			t.Fatal("changed skill content entered protected context")
		}
	})
}
