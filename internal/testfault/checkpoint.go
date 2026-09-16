package testfault

import (
	"sync"

	"Eylu/internal/tool"
)

// CheckpointFaults wraps a tool.CheckpointSink and fails either half on
// schedule. Every lifecycle record is kept, including the ones whose delivery
// failed, so a test can compare what the executor tried to record with what the
// store actually holds.
type CheckpointFaults struct {
	Delegate        tool.CheckpointSink
	IntentFault     *Fault
	CompletionFault *Fault

	mu          sync.Mutex
	intents     []tool.Intent
	completions []tool.Completion
}

var _ tool.CheckpointSink = (*CheckpointFaults)(nil)

// RecordIntent keeps the intent and fails it on schedule. A failed intent means
// the operation must not start at all.
func (c *CheckpointFaults) RecordIntent(intent tool.Intent) error {
	c.mu.Lock()
	c.intents = append(c.intents, intent)
	c.mu.Unlock()
	if err := c.IntentFault.Fail(); err != nil {
		return err
	}
	if c.Delegate == nil {
		return nil
	}
	return c.Delegate.RecordIntent(intent)
}

// RecordCompletion keeps the completion and fails it on schedule. A failed
// completion means the operation may already have happened.
func (c *CheckpointFaults) RecordCompletion(completion tool.Completion) error {
	c.mu.Lock()
	c.completions = append(c.completions, completion)
	c.mu.Unlock()
	if err := c.CompletionFault.Fail(); err != nil {
		return err
	}
	if c.Delegate == nil {
		return nil
	}
	return c.Delegate.RecordCompletion(completion)
}

// Intents returns every intent the executor produced, in order.
func (c *CheckpointFaults) Intents() []tool.Intent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tool.Intent(nil), c.intents...)
}

// Completions returns every completion the executor produced, in order.
func (c *CheckpointFaults) Completions() []tool.Completion {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tool.Completion(nil), c.completions...)
}
