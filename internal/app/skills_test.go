package app

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/agent"
	"Eylu/internal/config"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

func TestProjectSkillRequiresExplicitTrust(t *testing.T) {
	home := isolateUserState(t)
	workspace := t.TempDir()
	createAppSkill(t, filepath.Join(workspace, ".agents", "skills", "project-skill"), "project-skill", "Project workflow when requested.", "PROJECT BODY")
	cfg := config.Default(workspace)
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, credentials: nil, trustPrompted: make(map[string]bool)}
	conversation := agent.NewConversation()
	registry, _, err := runtime.loadSkillRuntime(cfg, chatOptions{}, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("project-skill"); ok {
		t.Fatal("untrusted project skill became active")
	}
	foundUntrusted := false
	for _, record := range registry.Records() {
		foundUntrusted = foundUntrusted || record.Status == skill.StatusUntrusted
	}
	if !foundUntrusted {
		t.Fatalf("records = %#v", registry.Records())
	}
	registry, _, err = runtime.loadSkillRuntime(cfg, chatOptions{trustSkills: true}, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if item, ok := registry.Get("project-skill"); !ok || !item.Trusted {
		t.Fatalf("trusted skill = %#v, %v", item, ok)
	}
	store, err := skill.OpenTrustStore(filepath.Join(home, "state", "trust.json"))
	if err != nil || !store.IsTrusted(workspace) {
		t.Fatalf("store=%#v err=%v", store, err)
	}
}

func TestToolAuditSeparatesSkillActivationAndResourceSummary(t *testing.T) {
	var output bytes.Buffer
	writer := &toolAuditWriter{writer: &output}
	writer.Record(tool.AuditRecord{Tool: "activate_skill", SkillName: "demo", SkillSource: "user_eylu", SkillDigest: "abc", SkillTrigger: "model", SkillActivated: "2026-07-19T00:00:00Z"})
	writer.Record(tool.AuditRecord{Tool: "read_skill_resource", SkillName: "demo", SkillDigest: "abc", SkillResource: "references/guide.md", ResourceBytes: 42})
	text := output.String()
	if !strings.Contains(text, "[skill] name=demo source=user_eylu") || !strings.Contains(text, "[skill-resource] name=demo digest=abc path=references/guide.md bytes=42") {
		t.Fatalf("audit output = %q", text)
	}
}

func TestToolAuditJSONLIsStructured(t *testing.T) {
	var output bytes.Buffer
	writer := &toolAuditWriter{writer: &output, jsonl: true}
	writer.Record(tool.AuditRecord{RequestID: "request", CallID: "call", Tool: "bash", DurationMS: 12})
	var envelope struct {
		Type  string           `json:"type"`
		Audit tool.AuditRecord `json:"audit"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || envelope.Type != "tool_audit" || envelope.Audit.CallID != "call" {
		t.Fatalf("envelope=%#v err=%v output=%q", envelope, err, output.String())
	}
}

func TestSkillsCLIValidateListAndExplicitActivation(t *testing.T) {
	home := isolateUserState(t)
	workspace := t.TempDir()
	directory := filepath.Join(home, ".eylu", "skills", "user-skill")
	createAppSkill(t, directory, "user-skill", "User workflow when explicitly requested.", "USER SKILL BODY")
	configPath := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	code := Execute(context.Background(), []string{"--config", configPath, "--workspace", workspace, "skills", "validate", directory}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "valid skill user-skill") {
		t.Fatalf("validate exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"--config", configPath, "--workspace", workspace, "skills", "list"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "user-skill\tactive\tuser_eylu") {
		t.Fatalf("list exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"--config", configPath, "--workspace", workspace, "--output", "json", "skills", "diagnose"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || strings.Contains(stdout.String(), "USER SKILL BODY") || !strings.Contains(stdout.String(), `"source":"user_eylu"`) || !strings.Contains(stdout.String(), `"active_skills":200`) {
		t.Fatalf("diagnose exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	invalid := filepath.Join(t.TempDir(), "invalid")
	createAppSkill(t, invalid, "WrongName", "bad", "body")
	stdout.Reset()
	stderr.Reset()
	code = Execute(context.Background(), []string{"--config", configPath, "--workspace", workspace, "skills", "validate", invalid}, strings.NewReader(""), &stdout, &stderr)
	if code != exitConfig || !strings.Contains(stderr.String(), "config_error") {
		t.Fatalf("invalid exit=%d stderr=%q", code, stderr.String())
	}

	var slashOut, slashErr bytes.Buffer
	runtime := &runtime{stdin: strings.NewReader(""), stdout: &slashOut, stderr: &slashErr, credentials: nil, trustPrompted: make(map[string]bool)}
	conversation := agent.NewConversation()
	if err := runtime.activateSkillSlash(conversation, config.Default(workspace), chatOptions{}, "user-skill"); err != nil {
		t.Fatal(err)
	}
	if conversation.ActivatedSkillDigests()["user-skill"] == "" || !strings.Contains(slashOut.String(), "activated skill user-skill") || !strings.Contains(slashErr.String(), "trigger=user") {
		t.Fatalf("digests=%#v stdout=%q stderr=%q", conversation.ActivatedSkillDigests(), slashOut.String(), slashErr.String())
	}
}

func TestSkillRegistryCLIConfiguration(t *testing.T) {
	isolateUserState(t)
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "config.toml")
	publicKey := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
	var stdout, stderr bytes.Buffer
	args := []string{
		"--config", configPath, "--workspace", workspace, "skills", "registries", "add", "team",
		"--index-url", "https://registry.example/index.json", "--public-key", "release=" + publicKey, "--token-env", "TEAM_TOKEN",
	}
	if code := Execute(context.Background(), args, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("add exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err := config.Load(config.LoadOptions{ExplicitPath: configPath, Workspace: workspace, Environ: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	registry := loaded.Config.SkillRegistries["team"]
	if registry.IndexURL != "https://registry.example/index.json" || registry.TokenEnvironment != "TEAM_TOKEN" || registry.PublicKeys["release"] != publicKey {
		t.Fatalf("registry = %#v", registry)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Execute(context.Background(), []string{"--config", configPath, "--workspace", workspace, "skills", "registries", "delete", "team"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("delete exit=%d stderr=%s", code, stderr.String())
	}
	loaded, err = config.Load(config.LoadOptions{ExplicitPath: configPath, Workspace: workspace, Environ: os.Environ()})
	if err != nil || len(loaded.Config.SkillRegistries) != 0 {
		t.Fatalf("registries=%#v error=%v", loaded.Config.SkillRegistries, err)
	}
}

func createAppSkill(t *testing.T, directory, name, description, body string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
