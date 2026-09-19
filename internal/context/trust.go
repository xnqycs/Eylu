package context

// TrustLevel says who authored a piece of context, and therefore whether a model
// may treat it as an instruction.
//
// The categories in this package were already an enumeration of where context
// comes from; they were not, until now, an answer to whether it came from the
// person running Eylu, from Eylu itself, or from somewhere neither of them
// controls. Eylu reads files, runs commands and calls remote servers, so the
// third kind reaches the model in the same request as the first and has to be
// distinguishable from it.
type TrustLevel string

const (
	// TrustHost is context Eylu wrote itself: its own instructions, its own
	// schemas, its own bookkeeping.
	TrustHost TrustLevel = "host"
	// TrustUser is what the person running Eylu asked for. It is the only
	// context that is an instruction in its own right.
	TrustUser TrustLevel = "user"
	// TrustDerived is text the host wrote about content it did not author: the
	// agent's own turns, the compaction summary, the project map, the driver's
	// state. It is the host's account of untrusted material, so it is neither an
	// instruction nor a verbatim copy of one.
	TrustDerived TrustLevel = "derived"
	// TrustExternal is content read from a source the host does not control: a
	// file, a command's output, a tool's result, a remote server, a distributed
	// skill. It is data, never an instruction.
	TrustExternal TrustLevel = "external"
)

// trustLevels is the grading of every category. A category that is not listed
// here is reported as external, because an unclassified source of context is not
// a trusted one; TestEveryCategoryIsGraded proves that no declared category is
// missing from this table.
var trustLevels = map[Category]TrustLevel{
	CategorySystemPrompt:  TrustHost,
	CategoryTaskState:     TrustHost,
	CategoryOutputReserve: TrustHost,

	CategoryBuiltinToolSchema: TrustHost,

	CategoryUserMessage: TrustUser,

	CategoryAgentMessage:   TrustDerived,
	CategoryProjectContext: TrustDerived,
	CategorySummary:        TrustDerived,
	CategoryDriverState:    TrustDerived,

	CategorySkillCatalog:      TrustExternal,
	CategorySkillBody:         TrustExternal,
	CategorySkillResource:     TrustExternal,
	CategoryMCPInstructions:   TrustExternal,
	CategoryMCPToolSchema:     TrustExternal,
	CategoryMCPResource:       TrustExternal,
	CategoryMCPToolResult:     TrustExternal,
	CategoryBuiltinToolResult: TrustExternal,
	CategoryCodeSlice:         TrustExternal,
}

// Trust reports who authored the content of a category.
//
// The grading is deliberately about authorship rather than about risk. A skill
// body is external even though a person installed the skill, because installing
// a skill is not the same as writing every file in it, and a distributed skill
// can carry text its installer never read.
func (c Category) Trust() TrustLevel {
	if level, ok := trustLevels[c]; ok {
		return level
	}
	return TrustExternal
}

// GradedCategories reports the categories that carry an explicit grading, which
// is what lets a test prove that every declared category has one rather than
// falling through to the default.
func GradedCategories() map[Category]TrustLevel {
	graded := make(map[Category]TrustLevel, len(trustLevels))
	for category, level := range trustLevels {
		graded[category] = level
	}
	return graded
}

// Untrusted reports whether a category is content the model must read as data.
func (c Category) Untrusted() bool {
	return c.Trust() == TrustExternal
}
