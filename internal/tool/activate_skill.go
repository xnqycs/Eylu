package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/skill"
)

type ActivateSkill struct {
	registry *skill.Registry
	session  *skill.Session
}

func NewActivateSkill(registry *skill.Registry, session *skill.Session) *ActivateSkill {
	return &ActivateSkill{registry: registry, session: session}
}

func (a *ActivateSkill) Definition() protocol.ToolDefinition {
	names := make([]string, 0)
	for _, item := range a.registry.Active() {
		names = append(names, item.Name)
	}
	schema, _ := json.Marshal(map[string]any{
		"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "enum": names}},
		"required": []string{"name"}, "additionalProperties": false,
	})
	return protocol.ToolDefinition{
		Name:        "activate_skill",
		Description: "Activate one discovered Agent Skill by name. Returns its digest, root, allowed-tools hint, and bounded resource listing. Full instructions are inserted into protected context exactly once per digest. Use when a task matches a skill catalog description.",
		InputSchema: schema,
	}
}

func (a *ActivateSkill) Risk() policy.Risk { return policy.RiskRead }

func (a *ActivateSkill) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid activate_skill input: " + err.Error())
	}
	activation, err := a.session.Activate(input.Name, "model")
	if err != nil {
		return toolError(err.Error())
	}
	content := fmt.Sprintf("activated skill %s\nsource: %s\ndigest: %s\nroot: %s\nallowed_tools: %s\nresources:\n%s", activation.Name, activation.Source.String(), activation.Digest, activation.Root, activation.AllowedTools, strings.Join(activation.Resources, "\n"))
	metadata := map[string]any{
		"skill_activation": true, "skill_name": activation.Name, "skill_source": activation.Source.String(),
		"skill_entry": activation.Entry, "skill_root": activation.Root, "skill_digest": activation.Digest,
		"allowed_tools": activation.AllowedTools, "trigger": activation.Trigger, "duplicate": activation.Duplicate,
		"activated_at": activation.ActivatedAt.Format(time.RFC3339Nano),
	}
	if !activation.Duplicate {
		metadata["protected_content"] = wrapSkillContent(activation)
	}
	return protocol.ToolResult{Content: content, Metadata: metadata}
}

func wrapSkillContent(activation skill.Activation) string {
	var content strings.Builder
	fmt.Fprintf(&content, "<skill_content name=%q digest=%q source=%q>\n", activation.Name, activation.Digest, activation.Source.String())
	content.WriteString(activation.Body)
	fmt.Fprintf(&content, "\n\nSkill directory: %s\nRelative paths resolve from this directory.\n", activation.Root)
	if activation.AllowedTools != "" {
		fmt.Fprintf(&content, "Allowed-tools hint: %s. Local Eylu policy remains authoritative.\n", activation.AllowedTools)
	}
	if len(activation.Resources) > 0 {
		content.WriteString("<skill_resources>\n")
		for _, resource := range activation.Resources {
			fmt.Fprintf(&content, "  <file>%s</file>\n", resource)
		}
		content.WriteString("</skill_resources>\n")
	}
	content.WriteString("</skill_content>")
	return content.String()
}
