package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"Eylu/internal/driver"
	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/skill"
	"Eylu/internal/tool"
)

type skillLoopDriver struct {
	t       *testing.T
	request int
	script  string
}

func (d *skillLoopDriver) Name() string { return "skill-loop" }
func (d *skillLoopDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{ToolCalling: true}
}
func (d *skillLoopDriver) Generate(_ context.Context, request driver.Request, _ driver.EmitFunc) (protocol.ModelResponse, error) {
	d.request++
	allText := requestText(request.Model.Turns)
	switch d.request {
	case 1:
		if !strings.Contains(allText, "demo-skill") || strings.Contains(allText, "SECRET INSTRUCTION") || len(request.Model.Tools) != 3 {
			d.t.Fatalf("initial request leaked instructions or omitted catalog/tools: %s %#v", allText, request.Model.Tools)
		}
		call := protocol.ToolCall{ID: "activate-1", Name: "activate_skill", Arguments: json.RawMessage(`{"name":"demo-skill"}`)}
		return toolCallResponse("activate-turn", call), nil
	case 2:
		if strings.Count(allText, "SECRET INSTRUCTION") != 1 || !strings.Contains(allText, "references/guide.md") {
			d.t.Fatalf("protected skill context = %s", allText)
		}
		call := protocol.ToolCall{ID: "resource-1", Name: "read_skill_resource", Arguments: json.RawMessage(`{"skill":"demo-skill","path":"references/guide.md"}`)}
		return toolCallResponse("resource-turn", call), nil
	case 3:
		if strings.Count(allText, "SECRET INSTRUCTION") != 1 || !strings.Contains(allText, "REFERENCE CONTENT") {
			d.t.Fatalf("resource request context = %s", allText)
		}
		call := protocol.ToolCall{ID: "script-1", Name: "bash", Arguments: mustRawJSON(d.t, map[string]string{"command": "bash \"" + filepath.ToSlash(d.script) + "\""})}
		return toolCallResponse("script-turn", call), nil
	case 4:
		if strings.Count(allText, "SECRET INSTRUCTION") != 1 || !strings.Contains(allText, "SKILL_SCRIPT_OK") {
			d.t.Fatalf("script result context = %s", allText)
		}
		return protocol.ModelResponse{Turn: protocol.Turn{ID: "final", Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartText, Text: "skill complete"}}}, Stop: protocol.StopCompleted}, nil
	default:
		d.t.Fatalf("unexpected request %d", d.request)
		return protocol.ModelResponse{}, nil
	}
}

func TestAgentSkillActivationResourceAndProtectedContext(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	directory := filepath.Join(home, ".eylu", "skills", "demo-skill")
	if err := os.MkdirAll(filepath.Join(directory, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	entry := "---\nname: demo-skill\ndescription: Use demo instructions for demo requests.\n---\n\nSECRET INSTRUCTION\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "references", "guide.md"), []byte("REFERENCE CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(directory, "scripts", "check.sh")
	if err := os.WriteFile(scriptPath, []byte("printf SKILL_SCRIPT_OK"), 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.DiscoveryOptions{Workspace: workspace, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	session := skill.NewSession(registry, nil)
	bashTool, err := tool.NewBash(workspace, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &tool.Executor{Registry: tool.NewRegistry(tool.NewActivateSkill(registry, session), tool.NewReadSkillResource(session), bashTool), Policy: policy.AllowAllChecker{}}
	model := &skillLoopDriver{t: t, script: scriptPath}
	runtime := testRuntime(model, 1)
	runtime.SkillCatalog = registry.Catalog()
	conversation := NewConversation()
	response, err := conversation.Run(context.Background(), "use the demo skill", runtime, executor, LoopOptions{MaxTurns: 5, MaxTotalTokens: 1000}, false, nil)
	if err != nil || response.Turn.Parts[0].Text != "skill complete" || model.request != 4 {
		t.Fatalf("response=%#v requests=%d err=%v", response, model.request, err)
	}
	digests := conversation.ActivatedSkillDigests()
	if len(digests) != 1 || digests["demo-skill"] == "" {
		t.Fatalf("digests = %#v", digests)
	}
	report := conversation.ContextReport()
	var catalogTokens, bodyTokens int
	for _, category := range report.Categories {
		switch category.Category {
		case "skill_catalog":
			catalogTokens = category.Tokens
		case "skill_body":
			bodyTokens = category.Tokens
		}
	}
	if catalogTokens == 0 || bodyTokens == 0 {
		t.Fatalf("context report = %#v", report)
	}
	conversation.NewSession()
	if len(conversation.ActivatedSkillDigests()) != 0 {
		t.Fatal("new session retained activated skills")
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func requestText(turns []protocol.Turn) string {
	var text strings.Builder
	for _, turn := range turns {
		for _, part := range turn.Parts {
			if part.Kind == protocol.PartText {
				text.WriteString(part.Text)
				text.WriteByte('\n')
			}
			if part.Kind == protocol.PartToolResult && part.ToolResult != nil {
				text.WriteString(part.ToolResult.Content)
				text.WriteByte('\n')
			}
		}
	}
	return text.String()
}

func toolCallResponse(id string, call protocol.ToolCall) protocol.ModelResponse {
	return protocol.ModelResponse{Turn: protocol.Turn{ID: id, Role: protocol.RoleAgent, Parts: []protocol.Part{{Kind: protocol.PartToolCall, ToolCall: &call}}}, Stop: protocol.StopToolUse}
}
