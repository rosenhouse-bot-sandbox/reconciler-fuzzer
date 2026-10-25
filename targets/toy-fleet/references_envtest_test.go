//go:build envtest

package main_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func TestReferencesJudgeTheOrderAcrossKinds(t *testing.T) {
	for _, row := range referenceRows {
		t.Run(fmt.Sprintf("%s catches B%d", row.plan, row.bug), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, "config.yaml", run.Options{PlanPath: planPath("config.yaml", row.plan)}, row.bug)
			found := slices.ContainsFunc(report.Findings, func(f check.Finding) bool {
				return f.Check == "Honest" && f.Summary == row.summary && f.Kind == row.kind && f.Step == row.step &&
					slices.ContainsFunc(f.Details, func(d string) bool { return strings.HasPrefix(d, row.detail) })
			})
			if !found || len(report.Findings) != 1 {
				t.Errorf("Want only the Honest finding %q on %s at step %d that says %q. report.md:\n%s", row.summary, row.kind, row.step, row.detail, text)
			}
		})
	}
}
