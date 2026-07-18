package tool

import (
	"context"
	"encoding/json"

	"Eylu/internal/policy"
	"Eylu/internal/protocol"
	"Eylu/internal/skill"
)

type ReadSkillResource struct {
	session *skill.Session
}

func NewReadSkillResource(session *skill.Session) *ReadSkillResource {
	return &ReadSkillResource{session: session}
}

func (r *ReadSkillResource) Definition() protocol.ToolDefinition {
	return protocol.ToolDefinition{
		Name:        "read_skill_resource",
		Description: "Read one UTF-8 resource from an already activated Agent Skill. The path must remain within that skill root and pass size, symlink, and open-time identity checks. Use for referenced scripts or documentation only after activate_skill.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"skill":{"type":"string"},"path":{"type":"string"}},"required":["skill","path"],"additionalProperties":false}`),
	}
}

func (r *ReadSkillResource) Risk() policy.Risk { return policy.RiskRead }

func (r *ReadSkillResource) Execute(_ context.Context, raw json.RawMessage) protocol.ToolResult {
	var input struct {
		Skill string `json:"skill"`
		Path  string `json:"path"`
	}
	if err := decodeStrict(raw, &input); err != nil {
		return toolError("invalid read_skill_resource input: " + err.Error())
	}
	content, metadata, err := r.session.ReadResource(input.Skill, input.Path)
	if err != nil {
		return toolError(err.Error())
	}
	return protocol.ToolResult{Content: content, Metadata: metadata}
}
