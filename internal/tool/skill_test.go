package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/policy"
	"Eylu/internal/skill"
)

func TestActivateAndReadSkillResourceTools(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(home, ".eylu", "skills", "demo-skill")
	writeToolSkill(t, directory, "---\nname: demo-skill\ndescription: Demo instructions when requested.\nallowed-tools: write_file bash\n---\n\nDEMO BODY\n")
	if err := os.MkdirAll(filepath.Join(directory, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "guide.md"), []byte("GUIDE CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	session := skill.NewSession(registry, nil)
	activate := NewActivateSkill(registry, session)
	definition := activate.Definition()
	if !strings.Contains(string(definition.InputSchema), `"demo-skill"`) {
		t.Fatalf("schema = %s", definition.InputSchema)
	}
	result := activate.Execute(context.Background(), json.RawMessage(`{"name":"demo-skill"}`))
	if result.IsError || result.Metadata["skill_activation"] != true || !strings.Contains(result.Metadata["protected_content"].(string), "DEMO BODY") || strings.Contains(result.Content, "DEMO BODY") {
		t.Fatalf("activation result = %#v", result)
	}
	duplicate := activate.Execute(context.Background(), json.RawMessage(`{"name":"demo-skill"}`))
	if duplicate.IsError || duplicate.Metadata["duplicate"] != true {
		t.Fatalf("duplicate = %#v", duplicate)
	}
	if _, exists := duplicate.Metadata["protected_content"]; exists {
		t.Fatal("duplicate activation returned protected content")
	}
	read := NewReadSkillResource(session)
	resource := read.Execute(context.Background(), json.RawMessage(`{"skill":"demo-skill","path":"references/guide.md"}`))
	if resource.IsError || resource.Content != "GUIDE CONTENT" || resource.Metadata["skill_name"] != "demo-skill" {
		t.Fatalf("resource = %#v", resource)
	}
}

func TestAllowedToolsNeverRaisePolicyCeiling(t *testing.T) {
	request := policy.Request{Tool: "write_file", Risk: policy.RiskWrite, Input: json.RawMessage(`{}`)}
	want := map[policy.PermissionMode]policy.Decision{
		policy.ModeManual: policy.DecisionConfirm,
		policy.ModePlan:   policy.DecisionDeny,
		policy.ModeAuto:   policy.DecisionAllow,
		policy.ModeFull:   policy.DecisionAllow,
	}
	for mode, decision := range want {
		outcome := policy.NewChecker(policy.DefaultConfig(mode)).Check(context.Background(), request)
		if outcome.Decision != decision {
			t.Fatalf("mode=%s outcome=%#v", mode, outcome)
		}
	}
}

func writeToolSkill(t *testing.T, directory, content string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
