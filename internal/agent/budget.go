package agent

import (
	"errors"

	"Eylu/internal/protocol"
)

// RunUsage is the accumulated usage of one Run.
//
// It is deliberately separate from the usage of the final ModelResponse, which
// describes only the last model call. Reporting the last call as if it were the
// whole request would understate what the request cost.
type RunUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
	// CachedInputTokens is the part of InputTokens the providers served from their
	// own cache. It is a subset of InputTokens, never an addition, so the budget
	// total is unchanged by it; it is reported so cost accounting can tell a cache
	// hit from a miss.
	CachedInputTokens int `json:"cached_input_tokens,omitempty"`
	// ModelCalls counts the main model calls of the request.
	ModelCalls int `json:"model_calls"`
	// SummaryCalls counts the context-compaction summary calls the request made.
	SummaryCalls int `json:"summary_calls"`
	// RetryCalls counts the context-recovery retries the request made.
	RetryCalls int `json:"retry_calls"`
	// Exact reports whether every contribution came from provider-reported usage.
	Exact bool `json:"exact"`
	// Estimated reports that at least one contribution had to be estimated, so
	// the totals are a lower bound rather than an exact figure.
	Estimated bool `json:"estimated"`
}

// Total returns the tokens counted against the budget. Reasoning tokens are not
// added: every supported adapter already includes them in its output tokens, so
// adding them again would double count them.
func (u RunUsage) Total() int { return u.InputTokens + u.OutputTokens }

// BudgetTracker accounts for everything one request spends.
//
// The budget covers the main model calls, the context-compaction summaries and
// the context-recovery retries, because they all belong to the same request. A
// subagent is not included: it runs its own request and may outlive the parent,
// so its cost is reported separately.
//
// The budget is soft. Eylu stops before starting another model call once the
// limit is reached, but the current drivers do not forward a remaining output
// token limit to the provider, so a single call may still overshoot.
type BudgetTracker struct {
	limit int
	usage RunUsage
}

type callKind int

const (
	callMain callKind = iota
	callSummary
	callRetry
)

func newBudgetTracker(limit int) *BudgetTracker {
	return &BudgetTracker{limit: limit, usage: RunUsage{Exact: true}}
}

func (b *BudgetTracker) add(kind callKind, usage protocol.Usage) {
	if b == nil {
		return
	}
	switch kind {
	case callSummary:
		b.usage.SummaryCalls++
	case callRetry:
		b.usage.RetryCalls++
	default:
		b.usage.ModelCalls++
	}
	b.usage.InputTokens += usage.InputTokens
	b.usage.OutputTokens += usage.OutputTokens
	b.usage.ReasoningTokens += usage.ReasoningTokens
	// A cached input token is already inside the input tokens of the same call, so
	// it is accumulated separately and never added to the total.
	b.usage.CachedInputTokens += usage.CachedInputTokens
	if !usage.Exact {
		// The provider did not report usage, so the contribution is an estimate
		// and the totals are a lower bound.
		b.usage.Exact = false
		b.usage.Estimated = true
	}
}

// exceeded reports whether the request has already spent more than it was
// allowed to.
func (b *BudgetTracker) exceeded() bool {
	return b != nil && b.limit > 0 && b.usage.Total() > b.limit
}

// admits reports whether one more model call fits in the remaining budget, given
// the estimated input of the prepared request and the output the call may
// produce. It is the pre-request admission check.
func (b *BudgetTracker) admits(estimatedInput, outputReserve int) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	return b.usage.Total()+estimatedInput+outputReserve <= b.limit
}

// remaining returns the tokens still available, or 0 when no budget is set.
func (b *BudgetTracker) remaining() int {
	if b == nil || b.limit <= 0 {
		return 0
	}
	return max(0, b.limit-b.usage.Total())
}

// report returns the accumulated usage of the request.
func (b *BudgetTracker) report() RunUsage {
	if b == nil {
		return RunUsage{}
	}
	return b.usage
}

// ErrTokenBudget marks a request that could not continue within its token budget.
//
// It is a sentinel rather than a separate error type so the request still fails
// with the protocol error callers already classify, while the loop can tell a
// budget stop from any other failure and report token_budget as the reason - both
// when a call overspent and when the admission check refused to start one.
var ErrTokenBudget = errors.New("agent token budget")

// budgetError reports that the request reached its token budget.
func budgetError(limit int, closing bool) error {
	message := "agent token budget exceeded"
	if closing {
		message = "agent token budget exhausted before the next model call"
	}
	return &protocol.Error{Code: protocol.ErrProtocol, Message: message, ContextLimit: limit, Cause: ErrTokenBudget}
}
