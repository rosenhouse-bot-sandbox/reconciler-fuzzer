package check

import (
	"strings"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Unique fails a settled step whose end holds two live controller instances
// of one system, kind, namespace, owning CR and role.
func Unique(ix *trace.Index, s Spec) []Finding {
	v := newView(ix, s)
	var findings []Finding
	for _, step := range v.steps {
		end := step.ConfirmEnd
		duplicates := map[string][]string{}
		for g, instances := range v.groups(end) {
			if len(instances) > 1 {
				duplicates[kind(g.kind)] = append(duplicates[kind(g.kind)], strings.Join(v.describeAll(instances, end), ", "))
			}
		}
		findings = append(findings, byKind("Unique", step.Step, end, "duplicate resources", sorted(duplicates))...)
	}
	return findings
}
