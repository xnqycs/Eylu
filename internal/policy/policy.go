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

// DecisionSource identifies the layer that produced a decision.
type DecisionSource string

const (
	// SourceWorkspace is the operator configuration and the session mode.
	SourceWorkspace DecisionSource = "workspace"
	// SourceTool is a tool-level policy, which may only decide inside the
	// domain the operator granted it.
	SourceTool DecisionSource = "tool"
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
	// Rule identifies the rule that produced the decision, for audit.
	Rule string
	// Source identifies the layer that produced the decision.
	Source DecisionSource
	// HardDeny marks a prohibition that a tool-level policy cannot relax.
	HardDeny bool
	// Override describes a tool-level decision that was ignored because the
	// workspace policy returned a non-relaxable prohibition.
	Override string
}

// Normalize returns an outcome whose decision is always one of the three defined
// values. Anything unrecognized fails closed as a denial: a zero or unknown
// decision must never authorize execution.
func (o Outcome) Normalize() Outcome {
	switch o.Decision {
	case DecisionAllow, DecisionConfirm, DecisionDeny:
	default:
		o.Decision = DecisionDeny
		o.HardDeny = true
		o.Rule = "unrecognized_decision"
		o.Reason = joinReason(o.Reason, "policy returned an unrecognized decision")
	}
	if o.Confirmations < 0 {
		o.Confirmations = 0
	}
	return o
}

// Compose applies an optional tool-level decision on top of the workspace
// decision.
//
// The layers stay separate: the workspace layer owns the mode, the risk and any
// non-relaxable prohibition, while a tool may only decide inside its own domain
// (for example the independently configured web permission). A hard denial is
// never replaced by a tool-level allow, and an unrecognized decision fails
// closed.
func Compose(base Outcome, override *Outcome) Outcome {
	result := base
	if override != nil {
		if base.HardDeny {
			result.Override = describeOutcome(*override)
			result.Reason = joinReason(base.Reason, "tool-level decision was ignored because the workspace policy returned a non-relaxable denial")
		} else {
			result = *override
			result.Source = SourceTool
			// The session mode is a workspace-layer concept and is never taken
			// from the tool, otherwise audit would misreport the active mode.
			result.Mode = base.Mode
			if result.Risk == "" {
				result.Risk = base.Risk
			}
			if result.Classification == "" {
				result.Classification = base.Classification
			}
			if result.Rule == "" {
				result.Rule = "tool_domain"
			}
		}
	}
	if result.Source == "" {
		result.Source = SourceWorkspace
	}
	return result.Normalize()
}

func describeOutcome(outcome Outcome) string {
	source := string(outcome.Source)
	if source == "" {
		source = string(SourceTool)
	}
	if outcome.Rule == "" {
		return source
	}
	return source + ":" + outcome.Rule
}

func joinReason(existing, added string) string {
	if existing == "" {
		return added
	}
	return existing + "; " + added
}

type Checker interface {
	Check(context.Context, Request) Outcome
}

// ShellDialect names how a shell reads a command line.
//
// The classifier reads the line the same way the shell that will run it does,
// because the two readings have to agree: a construct the classifier believes is
// inert text but the shell treats as a separator is a second command that was never
// classified. The dialect is an input rather than a guess for exactly that reason.
type ShellDialect string

const (
	// ShellPOSIX is the dialect of sh and of the POSIX-compatible shells the tool
	// uses on Unix and in git-bash: single and double quotes group, a backslash
	// escapes, and `&`, `|`, `;`, `<` and `>` separate or redirect outside quotes.
	// It is the zero value, so every caller that does not name a dialect gets the
	// historical reading.
	ShellPOSIX ShellDialect = ""
	// ShellCommandPrompt is the dialect of the Windows command interpreter. Single
	// quotes are ordinary characters, `%` expands, `^` escapes, and `&`, `|`, `<`
	// and `>` are active wherever they are not inside double quotes.
	ShellCommandPrompt ShellDialect = "cmd"
)

type Config struct {
	Mode              PermissionMode
	ReadOnlyCommands  []string
	AutoAllowCommands []string
	DangerousPatterns []string
	BlockedPatterns   []string
	// Shell names the reading of a command line. An empty value is ShellPOSIX.
	Shell ShellDialect
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
	return Outcome{Decision: DecisionAllow, Risk: request.Risk, Mode: ModeFull, Classification: CommandNotApplicable, Source: SourceWorkspace, Rule: "allow_all", Reason: "explicit test or application approval"}
}

func (c *LocalChecker) Check(_ context.Context, request Request) Outcome {
	outcome := Outcome{Risk: request.Risk, Mode: c.config.Mode, Classification: CommandNotApplicable, Source: SourceWorkspace}
	if request.Tool == "bash" || request.Risk == RiskExec {
		command := commandFromInput(request.Input)
		outcome.Classification = ClassifyCommand(command, c.config)
		if outcome.Classification == CommandBlocked {
			outcome.Decision = DecisionDeny
			outcome.HardDeny = true
			outcome.Rule = "blocked_pattern"
			outcome.Reason = "command matches a configured blocked pattern"
			return outcome
		}
		if outcome.Classification == CommandDangerous {
			outcome.Risk = RiskHigh
		}
	}
	if request.Risk == RiskRead || request.Risk == RiskSession {
		outcome.Decision = DecisionAllow
		if request.Risk == RiskSession {
			outcome.Rule = "session_tool"
			outcome.Reason = "session-local operation"
		} else {
			outcome.Rule = "read_only_tool"
			outcome.Reason = "read-only workspace operation"
		}
		return outcome
	}
	switch c.config.Mode {
	case ModePlan:
		if outcome.Classification == CommandReadOnly {
			outcome.Decision = DecisionAllow
			outcome.Rule = "plan:read_only_command"
			outcome.Reason = "plan mode permits classified read-only commands"
			return outcome
		}
		outcome.Decision = DecisionDeny
		outcome.Rule = "plan:default_deny"
		outcome.Reason = "plan mode permits exploration and read-only commands"
	case ModeAuto:
		switch outcome.Classification {
		case CommandReadOnly, CommandAutoAllowed:
			outcome.Decision = DecisionAllow
			outcome.Rule = "auto:allowlist"
			outcome.Reason = "command is in the auto-mode allowlist"
		case CommandDangerous:
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 2
			outcome.Warning = true
			outcome.Rule = "auto:dangerous_command"
			outcome.Reason = "dangerous command requires two confirmations"
		case CommandUnknown:
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 1
			outcome.Rule = "auto:unknown_command"
			outcome.Reason = "command is outside the auto-mode allowlist"
		default:
			outcome.Decision = DecisionAllow
			outcome.Rule = "auto:workspace_write"
			outcome.Reason = "auto mode permits workspace edits"
		}
	case ModeFull:
		if outcome.Classification == CommandDangerous || request.Risk == RiskHigh {
			outcome.Decision = DecisionConfirm
			outcome.Confirmations = 1
			outcome.Warning = true
			outcome.Rule = "full:dangerous_operation"
			outcome.Reason = "dangerous operation requires an explicit warning confirmation"
		} else {
			outcome.Decision = DecisionAllow
			outcome.Rule = "full:allow"
			outcome.Reason = "full mode permits this operation"
		}
	default:
		outcome.Decision = DecisionConfirm
		outcome.Confirmations = 1
		outcome.Rule = "manual:confirm"
		outcome.Reason = "manual mode requires confirmation for writes and commands"
		if outcome.Classification == CommandDangerous || request.Risk == RiskHigh {
			outcome.Confirmations = 2
			outcome.Warning = true
			outcome.Rule = "manual:dangerous_operation"
			outcome.Reason = "dangerous operation requires two confirmations in manual mode"
		}
	}
	return outcome
}

// ClassifyCommand classifies one shell command line.
//
// Read-only classification is only granted when every segment of the line is
// provably free of side effects. Argument forms that delete, move, execute
// another program, or write a file are reported as dangerous, and anything that
// cannot be interpreted unambiguously falls back to unknown. An operator can
// therefore never widen the read-only set by configuring a command prefix.
func ClassifyCommand(command string, config Config) CommandClass {
	trimmed := strings.TrimSpace(command)
	normalized := strings.ToLower(trimmed)
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
	if hasActiveShellSyntax(trimmed, config.Shell) {
		return CommandUnknown
	}
	// Segments keep their original case: short options are case sensitive, and
	// only the pattern lists and the auto-allow list are compared lowercased.
	segments := Segments(trimmed, config)
	if len(segments) == 0 {
		return CommandUnknown
	}
	// rank grows as the line becomes less provable: 0 read-only, 1 auto-allowed,
	// 2 unknown. A dangerous segment short-circuits immediately.
	rank := 0
	for _, segment := range segments {
		switch classifySegment(segment, config) {
		case CommandDangerous:
			return CommandDangerous
		case CommandReadOnly:
		case CommandAutoAllowed:
			if rank < 1 {
				rank = 1
			}
		default:
			rank = 2
		}
	}
	switch rank {
	case 0:
		return CommandReadOnly
	case 1:
		return CommandAutoAllowed
	default:
		return CommandUnknown
	}
}

// Segments reports the invocations the classifier believes one command line
// contains, read with the dialect of the shell that will run it.
//
// It is exported because the reading is the thing that has to match the shell: a
// test can put the same line to the classifier and to the shell and compare how
// many commands each of them sees. A line whose reading cannot be proven is
// reported as a single segment, and ClassifyCommand refuses it separately through
// hasActiveShellSyntax.
func Segments(command string, config Config) []string {
	return splitShellCommands(strings.TrimSpace(command), config.Shell)
}

// ReadsInertText reports whether the classifier reads a line as one command with no
// active shell syntax at all.
//
// It is the claim a caller acts on when it auto-approves a line, so it is stated as
// its own question: a test can put the same line to the classifier and to the shell
// and require the two readings to agree about whether anything is active.
func ReadsInertText(command string, config Config) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}
	return !hasActiveShellSyntax(trimmed, config.Shell) && len(Segments(trimmed, config)) <= 1
}

// hasActiveShellSyntax reports whether a line contains syntax the classifier
// cannot prove inert under the named dialect.
//
// The two dialects differ in the dangerous direction: a construct that a POSIX
// shell treats as quoted text - an ampersand inside single quotes, for example - is
// a command separator to the Windows command interpreter. Reading a line with the
// wrong rules is what would let a read-only classification run a second command, so
// each dialect is read on its own terms and anything unprovable fails closed.
func hasActiveShellSyntax(command string, dialect ShellDialect) bool {
	if dialect == ShellCommandPrompt {
		return hasActiveCommandPromptSyntax(command)
	}
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

// hasActiveCommandPromptSyntax reads a line the way the Windows command
// interpreter does, and refuses anything it cannot prove inert.
//
// The interpreter has no single-quote grouping at all, so a single-quoted region is
// not a region: separators inside it are separators. Only double quotes group, and
// even there `%` keeps expanding. `^` escapes the next character outside double
// quotes, parentheses, a backtick and `$(` are refused because their effect depends
// on context this reader does not model.
func hasActiveCommandPromptSyntax(command string) bool {
	quoted := false
	runes := []rune(command)
	for index, current := range runes {
		if current == '"' {
			quoted = !quoted
			continue
		}
		if current == '^' && !quoted {
			// An escaped character is the interpreter's own quoting, which this
			// reader does not interpret: it fails closed rather than guessing.
			return true
		}
		if current == '%' {
			// Variable expansion changes the argument after classification, inside
			// or outside double quotes.
			return true
		}
		if quoted {
			continue
		}
		switch current {
		case '&', '|', '<', '>', '(', ')', '`':
			return true
		}
		if current == '$' && index+1 < len(runes) && runes[index+1] == '(' {
			return true
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

// splitShellCommands splits a line into the invocations the named dialect reads.
//
// The Windows command interpreter has no single-quote grouping, so a single-quoted
// region does not protect a separator: reading it as one would report one command
// where the shell runs two.
func splitShellCommands(command string, dialect ShellDialect) []string {
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
		if dialect == ShellCommandPrompt {
			if current == '^' {
				// The interpreter's own escape: the next character is literal, and
				// hasActiveCommandPromptSyntax has already refused the line.
				escaped = true
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
			if quote == 0 && (current == '&' || current == '|' || current == '\n') {
				if segment := strings.TrimSpace(string(runes[start:index])); segment != "" {
					result = append(result, segment)
				}
				start = index + 1
			}
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
