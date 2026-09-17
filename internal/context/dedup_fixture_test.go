package context

import "strings"

// bodyLargerThanAReference returns a body comfortably larger than the reference that
// would stand for it.
//
// A repeat is only replaced by a reference when the reference is smaller, because
// otherwise the "saving" grows the request - see
// TestASmallBodyKeepsItsContent in internal/agent and the block metadata those cases
// assert. Cases about the deduplication rule
// must therefore use a body whose size is not the reason for the answer. That applies
// to the cases asserting a body is *kept* as much as to the ones asserting it is
// replaced: with a tiny fixture, a kept body would be kept for the wrong reason and
// the assertion would stop discriminating.
func bodyLargerThanAReference(label string) string {
	return label + ": " + strings.Repeat("content of the file ", 12)
}
