//go:build envtest

package main_test

import (
	"testing"
)

// TestGapsPassB0 runs plans that B0 should pass, but that the fuzzer
// cannot yet judge right.
func TestGapsPassB0(t *testing.T) {
	for _, name := range []string{
		// A step that only changes a Database settles before the controller acts.
		"secondary-update",
		// A Cluster's deletion waits, by design, for a Database a later step deletes.
		"blocked-cluster-delete",
		// A held Cluster watch keeps the recreate waiting, and the plan's last step never runs.
		"hold-recreate",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if report := runPlan(t, "gaps/config.yaml", name, 0); report != "No findings.\n" {
				t.Errorf("Want no findings and no notes. report.md:\n%s", report)
			}
		})
	}
}
