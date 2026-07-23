package agent

import (
	"strings"

	"Eylu/internal/policy"
)

const ModelInherit = "inherit"

type Profile struct {
	Name           string
	Description    string
	PermissionMode string
	Model          string
	MaxTurns       int
	Isolated       bool
	SystemPrompt   func() string
	AllowTool      func(string, policy.Risk) bool
}

func (p Profile) AllowsTool(name string, risk policy.Risk) bool {
	if p.AllowTool == nil {
		return true
	}
	return p.AllowTool(name, risk)
}

func (p Profile) LimitTurns(configured int) int {
	if configured <= 0 {
		configured = 20
	}
	if p.MaxTurns > 0 && p.MaxTurns < configured {
		return p.MaxTurns
	}
	return configured
}

func ProfileForMode(mode string) Profile {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "plan":
		return planProfile()
	default:
		return executionProfile(mode)
	}
}

func executionProfile(mode string) Profile {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "manual"
	}
	return Profile{
		Name:           "execution",
		Description:    "General-purpose repository agent.",
		PermissionMode: mode,
		Model:          ModelInherit,
		SystemPrompt: func() string {
			base := SystemPrompt + "\nCurrent permission mode: " + mode + ". Local policy decisions are final."
			switch mode {
			case "auto":
				return base + " Workspace edits run automatically. Allowlisted commands run automatically; other commands request confirmation."
			case "full":
				return base + " Ordinary workspace tools and commands run automatically. Dangerous operations always request a prominent confirmation."
			default:
				return base + " Reads run automatically. Writes and commands request confirmation; dangerous operations require two confirmations."
			}
		},
	}
}

func planProfile() Profile {
	return Profile{
		Name:           "plan",
		Description:    "Read-only software architecture planner.",
		PermissionMode: "plan",
		Model:          ModelInherit,
		MaxTurns:       20,
		Isolated:       true,
		SystemPrompt: func() string {
			return `You are Eylu's software architecture planner running in an isolated child context. Produce implementation-ready plans for the local repository.

Explore the repository before deciding. Use only read-only tools and commands, inherit the parent's model and relevant conversation context, and keep every recommendation grounded in inspected code. Do not modify files. Do not spawn another agent. Ask a concise clarification only when a product decision cannot be derived from the repository.

Emit independent read-only tool calls together in the same response. Keep calls with data, file, or state dependencies in separate rounds.

The final response must be a decision-complete plan with a title, summary, implementation changes, important interface or compatibility changes, test scenarios, and explicit assumptions. Mention concrete files only when they disambiguate ownership. Local permission policy is authoritative.`
		},
		AllowTool: func(name string, risk policy.Risk) bool {
			return risk == policy.RiskRead || name == "bash" || name == "ask" || name == "agent" || name == "task_output" || name == "task_stop"
		},
	}
}

func SearchProfile(maxTurns int) Profile {
	if maxTurns <= 0 {
		maxTurns = 8
	}
	return Profile{
		Name: "search", Description: "Read-only repository search specialist.", PermissionMode: "plan",
		Model: ModelInherit, MaxTurns: maxTurns, Isolated: true,
		SystemPrompt: func() string {
			return `You are Eylu's repository search subagent. Locate the smallest set of source locations that answers the delegated question.

Use search_code to narrow candidates, read_file with exact line ranges to verify them, and list_directory only when repository shape matters. Emit independent reads together. Keep dependent searches and reads in separate rounds. You cannot edit files, run commands, or delegate another agent.

Return only one JSON object with this schema:
{"summary":"concise answer","findings":[{"path":"workspace/relative/path","start_line":1,"end_line":1,"symbol":"optional symbol","reason":"why this location matters","confidence":0.0,"file_hash":"hash from read/search"}],"follow_up":["optional unresolved question"]}

Every finding must be grounded in tool output. Use an empty findings array when no verified location exists.`
		},
		AllowTool: func(name string, risk policy.Risk) bool {
			if risk != policy.RiskRead {
				return false
			}
			switch name {
			case "read_file", "search_code", "list_directory":
				return true
			default:
				return false
			}
		},
	}
}

func GeneralSubagentProfile(mode string, maxTurns int) Profile {
	base := executionProfile(mode)
	base.Name = "general"
	base.Description = "General-purpose background repository subagent."
	base.MaxTurns = maxTurns
	base.Isolated = true
	base.SystemPrompt = func() string {
		return executionProfile(mode).SystemPrompt() + ` You are running as a child agent in a shared workspace. Complete the delegated task independently and keep the final response concise. Re-read a file immediately before editing it, use edit_file for existing files, and use write_file only for new files. You cannot delegate another agent or ask the user questions. The parent agent receives your final response and transcript.`
	}
	base.AllowTool = func(name string, _ policy.Risk) bool {
		switch name {
		case "agent", "task_output", "task_stop", "ask", "activate_skill":
			return false
		default:
			return true
		}
	}
	return base
}
