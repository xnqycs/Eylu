package policy

import "strings"

// argumentVerdict is the result of validating one command invocation.
type argumentVerdict int

const (
	// verdictReadOnly means the invocation is provably free of side effects.
	verdictReadOnly argumentVerdict = iota
	// verdictUnknown means the invocation cannot be proven side-effect free, so
	// callers must not treat it as read-only.
	verdictUnknown
	// verdictDangerous means the invocation carries a known destructive,
	// executing, or file-writing argument form.
	verdictDangerous
)

// readOnlyValidation describes how the arguments of one configured read-only
// command are checked.
//
// A configured command prefix alone never proves that every argument form is
// safe: `find` can delete, `git branch` can create and remove refs, and `git -c`
// can inject configuration that spawns external programs. Options are therefore
// only accepted when they can be shown to be free of side effects. Where the
// option space is small it is whitelisted; where it is large it is kept open and
// the executable or file-writing families are rejected explicitly. Anything not
// covered fails closed.
type readOnlyValidation struct {
	// allowedOptions, when non-nil, is the complete set of accepted option
	// names. A nil map keeps the option space open.
	allowedOptions map[string]struct{}
	// allowedOptionPrefixes accepts options that carry an attached value, such
	// as `-uno` or `--porcelain=v2`.
	allowedOptionPrefixes []string
	// dangerousOptions and dangerousOptionPrefixes mark option families that
	// execute another program, mutate repository state, or write a file.
	dangerousOptions        map[string]struct{}
	dangerousOptionPrefixes []string
	// unknownOptions and unknownOptionPrefixes mark option families that cannot
	// be proven safe without being known dangerous.
	unknownOptions        map[string]struct{}
	unknownOptionPrefixes []string
	// listOptions records the options that select a listing form, so that
	// commands whose bare operands mutate state can require them.
	listOptions map[string]struct{}
	// allowOperands accepts non-option arguments in general.
	allowOperands bool
}

// evaluate validates one argument list.
func (v readOnlyValidation) evaluate(args []string) argumentVerdict {
	listing := false
	operands := false
	optionsDone := false
	for _, arg := range args {
		if !optionsDone && arg == "--" {
			optionsDone = true
			continue
		}
		if !optionsDone && strings.HasPrefix(arg, "-") && arg != "-" {
			name, _ := splitOption(arg)
			switch {
			case optionIn(name, arg, v.dangerousOptions) || hasOptionPrefix(arg, v.dangerousOptionPrefixes):
				return verdictDangerous
			case optionIn(name, arg, v.unknownOptions) || hasOptionPrefix(arg, v.unknownOptionPrefixes):
				return verdictUnknown
			case !v.optionAllowed(name, arg):
				return verdictUnknown
			}
			if _, ok := v.listOptions[name]; ok {
				listing = true
			}
			continue
		}
		operands = true
	}
	if !operands || v.allowOperands {
		return verdictReadOnly
	}
	if listing {
		return verdictReadOnly
	}
	return verdictUnknown
}

func (v readOnlyValidation) optionAllowed(name, arg string) bool {
	if v.allowedOptions == nil {
		return true
	}
	if _, ok := v.allowedOptions[name]; ok {
		return true
	}
	return hasOptionPrefix(arg, v.allowedOptionPrefixes)
}

// readOnlyArgumentRules validates the arguments of the read-only commands that
// ship in the default configuration. A configured command without a rule is
// read-only only in its bare form, because no argument can be proven safe.
var readOnlyArgumentRules = map[string]func([]string) argumentVerdict{
	"pwd":  validate(readOnlyValidation{allowedOptions: optionSet("-L", "-P", "--logical", "--physical")}),
	"ls":   validate(readOnlyValidation{allowOperands: true}),
	"dir":  validate(readOnlyValidation{allowOperands: true}),
	"grep": validate(readOnlyValidation{allowOperands: true}),
	"rg": validate(readOnlyValidation{
		dangerousOptionPrefixes: []string{"--pre"},
		allowOperands:           true,
	}),
	"find": validate(readOnlyValidation{
		dangerousOptions:        optionSet("-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls"),
		dangerousOptionPrefixes: []string{"-fprint"},
		allowOperands:           true,
	}),
	"git status": validate(readOnlyValidation{
		allowedOptions: optionSet(
			"-s", "--short", "-b", "--branch", "--porcelain", "--long", "-v", "--verbose", "-z", "--null",
			"--column", "--no-column", "--ahead-behind", "--no-ahead-behind", "--renames", "--no-renames",
			"--find-renames", "--show-stash", "--untracked-files", "--ignored", "--ignore-submodules",
		),
		allowedOptionPrefixes: []string{"-u", "--porcelain=", "--untracked-files=", "--ignored=", "--column=", "--ignore-submodules=", "--find-renames="},
		allowOperands:         true,
	}),
	"git diff":      gitQueryArguments(gitQueryOptions),
	"git log":       gitQueryArguments(gitQueryOptions),
	"git show":      gitQueryArguments(gitQueryOptions),
	"git rev-parse": gitQueryArguments(gitQueryOptions),
	"git ls-files":  gitQueryArguments(gitQueryOptions),
	"git grep":      gitQueryArguments(gitQueryOptions),
	"git branch":    gitBranchArguments,
}

// gitQueryOptions lists the git options that spawn an external program or write
// a file: external diff drivers, textconv filters, signature verification, a
// forced pager, and `git diff --output`. The query option space of these
// subcommands is large and only affects revision selection and formatting, so it
// is kept open and these families are rejected instead.
var gitQueryOptions = optionSet(
	"--ext-diff", "--textconv", "--output", "--show-signature", "--open-files-in-pager", "-O",
)

// gitUnprovableOptions cannot be shown to avoid executing a helper program.
var gitUnprovableOptions = optionSet("--help")

// gitBranchQueryOptions lists the options that only select or format the branch
// listing. Anything outside this set is rejected, so that creation, deletion and
// renaming forms (`git branch -D`, `git branch -m`, `git branch -c`) are never
// classified read-only.
var gitBranchQueryOptions = optionSet(
	"-a", "--all", "-r", "--remotes", "-v", "--verbose", "-l", "--list", "--show-current",
	"--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort",
	"--column", "--no-column", "--color", "--no-color", "--abbrev", "--no-abbrev",
	"--ignore-case", "--no-ignore-case", "--omit-empty", "--no-omit-empty",
)

// gitBranchMutationOptions lists the options that create, move, delete or
// re-point a branch.
var gitBranchMutationOptions = optionSet(
	"-d", "-D", "--delete", "-m", "-M", "--move", "-c", "-C", "--copy", "--edit-description",
	"-f", "--force", "-t", "--track", "--no-track", "-u", "--set-upstream-to", "--unset-upstream",
	"--create-reflog",
)

// gitBranchListingOptions lists the options that put `git branch` into listing
// mode. Operands are only meaningful as patterns alongside one of them; without
// one, `git branch <name>` creates a branch.
var gitBranchListingOptions = optionSet(
	"-a", "--all", "-r", "--remotes", "-v", "--verbose", "-l", "--list", "--show-current",
	"--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort",
)

// gitGlobalOptionsReadOnly lists the git options accepted before a subcommand.
// `-c`, `--config-env`, `--exec-path` and `--paginate` can inject configuration
// or spawn a program, while `-C`, `--git-dir` and `--work-tree` cannot be proven
// to stay inside the workspace.
var gitGlobalOptionsReadOnly = optionSet(
	"--no-pager", "-P", "--no-optional-locks", "--literal-pathspecs", "--glob-pathspecs",
	"--noglob-pathspecs", "--icase-pathspecs", "--no-replace-objects",
)

// gitGlobalOptionsDangerous lists the git options that execute another program
// or override repository configuration.
var gitGlobalOptionsDangerous = optionSet("-c", "--config-env", "--exec-path", "-p", "--paginate", "--help")

// gitBranchArguments validates a `git branch` invocation.
func gitBranchArguments(args []string) argumentVerdict {
	listing := false
	operands := false
	optionsDone := false
	for _, arg := range args {
		if !optionsDone && arg == "--" {
			optionsDone = true
			continue
		}
		if !optionsDone && strings.HasPrefix(arg, "-") && arg != "-" {
			name, _ := splitOption(arg)
			switch {
			case optionIn(name, arg, gitBranchMutationOptions):
				return verdictDangerous
			case !optionIn(name, arg, gitBranchQueryOptions):
				return verdictUnknown
			}
			if optionIn(name, arg, gitBranchListingOptions) {
				listing = true
			}
			continue
		}
		operands = true
	}
	if !operands || listing {
		return verdictReadOnly
	}
	// A bare operand names a branch to create, which is a repository mutation.
	return verdictUnknown
}

// gitQueryArguments builds the rule shared by the git query subcommands.
func gitQueryArguments(dangerous map[string]struct{}) func([]string) argumentVerdict {
	return validate(readOnlyValidation{
		dangerousOptions: dangerous,
		unknownOptions:   gitUnprovableOptions,
		allowOperands:    true,
	})
}

func validate(rule readOnlyValidation) func([]string) argumentVerdict {
	return rule.evaluate
}

func optionSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// optionIn reports whether an option name, or a bundled short-option form such
// as `-vv`, is a member of the set.
func optionIn(name, arg string, set map[string]struct{}) bool {
	if len(set) == 0 {
		return false
	}
	if _, ok := set[name]; ok {
		return true
	}
	return shortOptionCluster(arg, set)
}

// shortOptionCluster accepts bundled single-letter options when every bundled
// letter is individually allowed.
func shortOptionCluster(arg string, set map[string]struct{}) bool {
	if len(arg) < 3 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") {
		return false
	}
	for _, letter := range arg[1:] {
		if _, ok := set["-"+string(letter)]; !ok {
			return false
		}
	}
	return true
}

func hasOptionPrefix(arg string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// splitOption separates an option name from an attached value.
func splitOption(arg string) (string, string) {
	if index := strings.IndexByte(arg, '='); index >= 0 {
		return arg[:index], arg[index+1:]
	}
	return arg, ""
}

// tokenizeSegment splits one shell segment into an argument vector. It fails
// closed whenever quoting or expansion cannot be interpreted unambiguously, so
// that an invocation is never classified read-only based on a guess.
func tokenizeSegment(segment string) ([]string, bool) {
	tokens := make([]string, 0, 8)
	builder := strings.Builder{}
	started := false
	quote := rune(0)
	escaped := false
	flush := func() {
		if started {
			tokens = append(tokens, builder.String())
			builder.Reset()
			started = false
		}
	}
	for _, char := range segment {
		switch {
		case escaped:
			builder.WriteRune(char)
			started = true
			escaped = false
		case char == '\\' && quote != '\'':
			escaped = true
			started = true
		case char == '\'':
			if quote == 0 {
				quote = '\''
			} else if quote == '\'' {
				quote = 0
			} else {
				builder.WriteRune(char)
			}
			started = true
		case char == '"':
			if quote == 0 {
				quote = '"'
			} else if quote == '"' {
				quote = 0
			} else {
				builder.WriteRune(char)
			}
			started = true
		case quote == 0 && (char == ' ' || char == '\t' || char == '\r' || char == '\n'):
			flush()
		default:
			builder.WriteRune(char)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, false
	}
	flush()
	for _, token := range tokens {
		// Variable, arithmetic and command expansion change the argument after
		// classification, so the invocation is never treated as proven safe.
		if strings.ContainsAny(token, "$`") {
			return nil, false
		}
	}
	return tokens, true
}

// classifySegment returns the class of one shell segment.
//
// The segment keeps its original case because short options are case sensitive
// (`git grep -O` is not `-o`, `pwd -P` is not `-p`). Command and subcommand
// names are compared case-insensitively instead.
func classifySegment(segment string, config Config) CommandClass {
	tokens, ok := tokenizeSegment(segment)
	if !ok || len(tokens) == 0 {
		return CommandUnknown
	}
	name, args, verdict := resolveReadOnlyCommand(tokens, config.ReadOnlyCommands)
	switch verdict {
	case verdictDangerous:
		return CommandDangerous
	case verdictUnknown:
		return CommandUnknown
	}
	if name != "" {
		return classForVerdict(validateReadOnlyArguments(name, args))
	}
	if matchesCommandList(strings.ToLower(segment), config.AutoAllowCommands) {
		return CommandAutoAllowed
	}
	return CommandUnknown
}

// resolveReadOnlyCommand reports the configured read-only command named by one
// invocation together with the arguments that follow it. An empty name means no
// configured read-only command matched.
func resolveReadOnlyCommand(tokens []string, configured []string) (string, []string, argumentVerdict) {
	if strings.EqualFold(tokens[0], "git") {
		name, args, verdict := gitSubcommand(tokens)
		if verdict != verdictReadOnly {
			return "", nil, verdict
		}
		if !commandConfigured(name, configured) {
			return "", nil, verdictReadOnly
		}
		return name, args, verdictReadOnly
	}
	name, args, ok := configuredCommand(tokens, configured)
	if !ok {
		return "", nil, verdictReadOnly
	}
	return name, args, verdictReadOnly
}

// gitSubcommand resolves the `git <subcommand>` name while validating the global
// options that precede it. A global option that cannot be proven safe makes the
// whole invocation unprovable or dangerous.
func gitSubcommand(tokens []string) (string, []string, argumentVerdict) {
	index := 1
	for index < len(tokens) && strings.HasPrefix(tokens[index], "-") {
		option := tokens[index]
		name, _ := splitOption(option)
		switch {
		case optionIn(name, option, gitGlobalOptionsDangerous):
			return "", nil, verdictDangerous
		case !optionIn(name, option, gitGlobalOptionsReadOnly):
			return "", nil, verdictUnknown
		}
		index++
	}
	if index >= len(tokens) {
		return "", nil, verdictUnknown
	}
	return "git " + strings.ToLower(tokens[index]), tokens[index+1:], verdictReadOnly
}

// configuredCommand matches the longest configured command prefix and returns
// the remaining arguments. Whole tokens are compared so that `git status` never
// matches an unrelated invocation that merely starts with the same letters.
func configuredCommand(tokens []string, configured []string) (string, []string, bool) {
	best := ""
	bestCount := 0
	for _, candidate := range configured {
		fields := normalizedFields(candidate)
		if len(fields) == 0 || len(fields) <= bestCount || len(fields) > len(tokens) {
			continue
		}
		matched := true
		for index, field := range fields {
			if !strings.EqualFold(tokens[index], field) {
				matched = false
				break
			}
		}
		if matched {
			best, bestCount = strings.Join(fields, " "), len(fields)
		}
	}
	if best == "" {
		return "", nil, false
	}
	return best, tokens[bestCount:], true
}

func commandConfigured(name string, configured []string) bool {
	if name == "" {
		return false
	}
	for _, candidate := range configured {
		if strings.Join(normalizedFields(candidate), " ") == name {
			return true
		}
	}
	return false
}

func normalizedFields(value string) []string {
	return strings.Fields(strings.ToLower(strings.TrimSpace(value)))
}

// validateReadOnlyArguments applies the argument rules for one configured
// read-only command. A command without rules is read-only only in its bare form.
func validateReadOnlyArguments(name string, args []string) argumentVerdict {
	if rule, ok := readOnlyArgumentRules[name]; ok {
		return rule(args)
	}
	if len(args) == 0 {
		return verdictReadOnly
	}
	return verdictUnknown
}

func classForVerdict(verdict argumentVerdict) CommandClass {
	switch verdict {
	case verdictReadOnly:
		return CommandReadOnly
	case verdictDangerous:
		return CommandDangerous
	default:
		return CommandUnknown
	}
}
