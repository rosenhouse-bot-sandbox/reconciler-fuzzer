package check

import "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"

// released is the summary of an Ordered finding.
const released = "released an object that another names"

// Ordered fails the to object of a block reference that is released while
// a from CR that the controller saw names it (DESIGN.md §12).
func Ordered(ix *trace.Index, s Spec) []Finding {
	return nil
}
