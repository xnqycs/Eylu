package agent

import (
	"fmt"
	"strconv"
)

// executionAllocator hands out host-owned execution identities.
//
// Three identities are kept apart on purpose:
//
//   - the model call ID is what the provider protocol pairs a result with, and
//     the model controls it;
//   - the execution ID names one host-side execution and is never built from a
//     model call ID, so a model that returns both "a" and "a:1" cannot make two
//     executions collide;
//   - the parent call ID records which model call produced an execution, so the
//     parent relation is explicit instead of being parsed from a string.
//
// Every ID reserved for the turn is skipped, so an injected or adversarial model
// ID cannot capture an execution identity either.
type executionAllocator struct {
	prefix   string
	next     uint64
	reserved map[string]struct{}
}

func newExecutionAllocator(modelCallIDs []string) *executionAllocator {
	allocator := &executionAllocator{prefix: "exec-", reserved: make(map[string]struct{}, len(modelCallIDs))}
	for _, id := range modelCallIDs {
		if id != "" {
			allocator.reserved[id] = struct{}{}
		}
	}
	return allocator
}

// allocate returns a fresh execution identity that no model call and no earlier
// execution of this turn has claimed.
func (a *executionAllocator) allocate() string {
	for {
		a.next++
		candidate := a.prefix + strconv.FormatUint(a.next, 10)
		if _, taken := a.reserved[candidate]; taken {
			continue
		}
		a.reserved[candidate] = struct{}{}
		return candidate
	}
}

// activityCallID names one web activity inside one execution.
//
// It is derived from the host-owned execution identity rather than from a model
// call ID, and it is deterministic so the start projection, the completion
// projection and the collapsed activity list all agree on one name.
func activityCallID(executionID string, index int) string {
	if index <= 0 {
		return executionID
	}
	return fmt.Sprintf("%s#%d", executionID, index+1)
}
