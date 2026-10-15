//go:build envtest

package run_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// Under --resync the toy writes its unchanged status on every tick. Ticks
// 900ms apart put at most three in the toy's 2s quiet window, so a quiet of 3
// admits them and the default of 0 does not.
func TestAQuietThresholdAdmitsATimer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	runAllowing := func(t *testing.T, quiet int) run.Result {
		t.Helper()
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, "--resync=900ms")
		toy.Thresholds.Quiet = quiet
		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		return result
	}

	t.Run("declared", func(t *testing.T) {
		if result := runAllowing(t, 3); result.Violation != nil {
			t.Errorf("The run reported %v, and quiet: 3 admits three ticks.", result.Violation)
		}
	})

	t.Run("undeclared", func(t *testing.T) {
		if result := runAllowing(t, 0); result.Violation == nil || result.Violation.ID != "G1" {
			t.Errorf("The run reported %v, want the G1 of a tick in a quiet window.", result.Violation)
		}
	})
}
