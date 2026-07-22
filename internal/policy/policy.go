package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type PermissionMode int

const (
	ModeManual PermissionMode = iota
	ModePlan
	ModeAuto
	ModeFull
)

func (m PermissionMode) String() string {
	switch m {
	case ModePlan:
		return "plan"
	case ModeAuto:
		return "auto"
	case ModeFull:
		return "full"
	default:
		return "manual"
	}
}

func ParseMode(value string) (PermissionMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "manual":
		return ModeManual, nil
	case "plan":
		return ModePlan, nil
	case "auto":
		return ModeAuto, nil
	case "full":
		return ModeFull, nil
	default:
		return ModeManual, fmt.Errorf("unknown permission mode %q", value)
	}
}

type Risk string

const (
	RiskRead    Risk = "read"
	RiskWrite   Risk = "write"
	RiskExec    Risk = "exec"
	RiskHigh    Risk = "high"
	RiskSession Risk = "session"
	RiskNetwork Risk = "network"
)

type Decision string

const (
	DecisionAllow   Decision = "allow"
	DecisionConfirm Decision = "confirm"
	DecisionDeny    Decision = "deny"
)

type Request struct {
	Tool              string
	Input             json.RawMessage
	Workspace         string
	Risk              Risk
	ConfirmationStep  int
	ConfirmationTotal int
}

type CommandClass string

const (
	CommandNotApplicable CommandClass = "not_applicable"
	CommandReadOnly      CommandClass = "read_only"
	CommandAutoAllowed   CommandClass = "auto_allowed"
	CommandUnknown       CommandClass = "unknown"
	CommandDangerous     CommandClass = "dangerous"
	CommandBlocked       CommandClass = "blocked"
)

type Outcome struct {
	Decision       Decision
	Reason         string
	Risk           Risk
	Mode           PermissionMode
	Classification CommandClass
	Confirmations  int
	Warning        bool
}

type Checker interface {
	Check(context.Context, Request) Outcome
}

type Config struct {
	Mode              PermissionMode
	ReadOnlyCommands  []string
	AutoAllowCommands []string
	DangerousPatterns []string
	BlockedPatterns   []string
}

func DefaultConfig(mode PermissionMode) Config {
	return Config{
		Mode: mode,
		ReadOnlyCommands: []string{
			"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files",
		},
		AutoAllowCommands: []string{
			"ls", "dir", "pwd", "find", "rg", "grep", "git status", "git diff", "git log", "git show", "git grep", "git branch", "git rev-parse", "git ls-files",
			"go test", "go vet", "go build", "go list", "go env", "go version", "gofmt", "go fmt",
		},
		DangerousPatterns: []string{
			"rm -rf", "git reset --hard", "git clean -fd", "git push --force", "mkfs", "diskpart", "format ", "remove-item -recurse", "del /s", "rd /s",
		},
	}
}

type LocalChecker struct {
	config Config
}

func NewChecker(config Config) *LocalChecker {
	defaults := DefaultConfig(config.Mode)
	if len(config.ReadOnlyCommands) == 0 {
		config.ReadOnlyCommands = defaults.ReadOnlyCommands
	}
	if len(config.AutoAllowCommands) == 0 {
		config.AutoAllowCommands = defaults.AutoAllowCommands
	}
	if len(config.DangerousPatterns) == 0 {
		config.DangerousPatterns = defaults.DangerousPatterns
	}
	return &LocalChecker{config: config}
}

type BaselineChecker struct{}

func (BaselineChecker) Check(_ context.Context, request Request) Outcome {
	return NewChecker(DefaultConfig(ModeManual)).Check(context.Background(), request)
}

type AllowAllChecker struct{}

func (AllowAllChecker) Check(_ context.Context, request Request) Outcome {
	return Outcome{Decision: DecisionAllow, Risk: request.Risk, Mode: ModeFull, Classification: CommandNotApplicable, Reason: "explicit test or application approval"}
}

func (c *LocalChecker) Check(_ context.Context, request Request) Outcome {
	outcome := Outcome{Risk: request.Risk, Mode: c.config.Mode, Classification: CommandNotApplicable}
	if request.Tool == "bash" || request.Risk == RiskExec {
		command := commandFromInput(request.Input)
		outcome.Classification = ClassifyCommand(command, c.config)
		if outcome.Classification == CommandDangerous {
			outcome.Risk = RiskHigh
		}
		if outcome.Classification == CommandBlocked {
			outcome.Decision = DecisionDeny
			outcome.Reason = "command matches a configured blocked pattern"
			return outcome
		}
	}
	if request.Risk == RiskRead || request.Risk == RiskSession {
		outcome.Decision = DecisionAllow
		if request.Risk == RiskSession {
			outcome.Reason = "session-local operation"
		} else {
			outcome.Reason = "read-only workspace operation"
		}
		return outcome
	}
	switch c.config.Mode {
	case ModePlan:
		if outcome.Classification == CommandReadOnly {
			outcome.Decision = DecisionAllow
			outcome.Reason = "plan mode permits classified read-only commands"
			return outcome
		}
		outcome.Decision = DecisionDeny
		outcome.Reason = "plan mode permits exploration and read-only commands"
	case ModeAuto:
		switch outcome.Classification {
		case CommandReadOnly, CommandAutoAllowed:
			outcome.Decision = DecisionAllow
			outcome.Reason = "command is in the auto-mode allowlist"
		case CommandDangerous:
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 2
			outcome.Warning = true
			outcome.Reason = "dangerous command requires two confirmations"
		case CommandUnknown:
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 1
			outcome.Reason = "command is outside the auto-mode allowlist"
		default:
			outcome.Decision = DecisionAllow
			outcome.Reason = "auto mode permits workspace edits"
		}
	case ModeFull:
		if outcome.Classification == CommandDangerous || request.Risk == RiskHigh {
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 1
			outcome.Warning = true
			outcome.Reason = "dangerous operation requires an explicit warning confirmation"
		} else {
			outcome.Decision = DecisionAllow
			outcome.Reason = "full mode permits this operation"
		}
	default:
		outcome.Decision = DecisionConfirm
		outcome.Confirmations = 1
		outcome.Reason = "manual mode requires confirmation for writes and commands"
		if outcome.Classification == CommandDangerous || request.Risk == RiskHigh {
			outcome.Confirmations = 2
			outcome.Warning = true
			outcome.Reason = "dangerous operation requires two confirmations in manual mode"
		}
	}
	return outcome
}

func ClassifyCommand(command string, config Config) CommandClass {
	normalized := strings.ToLower(strings.TrimSpace(command))
	if normalized == "" {
		return CommandUnknown
	}
	for _, pattern := range config.BlockedPatterns {
		if patternMatch(normalized, pattern) {
			return CommandBlocked
		}
	}
	for _, pattern := range config.DangerousPatterns {
		if patternMatch(normalized, pattern) {
			return CommandDangerous
		}
	}
	if hasActiveShellSyntax(normalized) {
		return CommandUnknown
	}
	segments := splitShellCommands(normalized)
	if len(segments) == 0 {
		return CommandUnknown
	}
	readOnly, autoAllowed := true, true
	for _, segment := range segments {
		if !matchesCommandList(segment, config.ReadOnlyCommands) {
			readOnly = false
		}
		if !matchesCommandList(segment, config.AutoAllowCommands) {
			autoAllowed = false
		}
	}
	if readOnly {
		return CommandReadOnly
	}
	if autoAllowed {
		return CommandAutoAllowed
	}
	return CommandUnknown
}

func hasActiveShellSyntax(command string) bool {
	var quote rune
	escaped := false
	runes := []rune(command)
	for index, current := range runes {
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if current == '\'' {
			if quote == 0 {
				quote = current
			} else if quote == current {
				quote = 0
			}
			continue
		}
		if current == '"' {
			if quote == 0 {
				quote = current
			} else if quote == current {
				quote = 0
			}
			continue
		}
		if quote != '\'' {
			if current == '`' || current == '>' || current == '<' {
				return true
			}
			if current == '$' && index+1 < len(runes) && runes[index+1] == '(' {
				return true
			}
		}
	}
	return false
}

func commandFromInput(input json.RawMessage) string {
	var value struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &value)
	return value.Command
}

func patternMatch(command, pattern string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	return pattern != "" && strings.Contains(command, pattern)
}

func matchesCommandList(command string, commands []string) bool {
	command = strings.TrimSpace(command)
	for _, allowed := range commands {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if command == allowed || strings.HasPrefix(command, allowed+" ") {
			return true
		}
	}
	return false
}

func splitShellCommands(command string) []string {
	result := make([]string, 0)
	start := 0
	var quote rune
	escaped := false
	runes := []rune(command)
	for index, current := range runes {
		if escaped {
			escaped = false
			continue
		}
		if current == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if current == '\'' || current == '"' {
			if quote == 0 {
				quote = current
			} else if quote == current {
				quote = 0
			}
			continue
		}
		if quote == 0 && (current == ';' || current == '\n' || current == '|' || current == '&') {
			if segment := strings.TrimSpace(string(runes[start:index])); segment != "" {
				result = append(result, segment)
			}
			start = index + 1
		}
	}
	if segment := strings.TrimSpace(string(runes[start:])); segment != "" {
		result = append(result, segment)
	}
	return result
}
