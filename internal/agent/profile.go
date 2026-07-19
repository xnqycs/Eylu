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

The final response must be a decision-complete plan with a title, summary, implementation changes, important interface or compatibility changes, test scenarios, and explicit assumptions. Mention concrete files only when they disambiguate ownership. Local permission policy is authoritative.`
		},
		AllowTool: func(name string, risk policy.Risk) bool {
			return risk == policy.RiskRead || name == "bash" || name == "ask"
		},
	}
}
