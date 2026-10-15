//go:build envtest

package run_test

import (
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// The README says the baseline catches a controller that does not watch a kind
// it manages, or that ignores a spec change. Its toy's baseline passes the toy
// with no bug, and catches each.
func TestTheToysBaselineCatchesWhatTheREADMESays(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	g, err := generate.New(loadTarget(t, binary), generate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sequence, err := g.Baseline()
	if err != nil {
		t.Fatal(err)
	}
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)

	for _, test := range []struct {
		name, bug, check, says string
	}{
		{name: "the toy with no bug", bug: "--bug=0"},
		{name: "a toy that does not watch its ConfigMaps", bug: "--bug=8", check: "G7", says: "op 1 (deleteManaged)"},
		{name: "a toy that keeps the ConfigMaps a lower count drops", bug: "--bug=7", check: "G4", says: "op 2 (update)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			toy := loadTarget(t, binary)
			toy.Launch.Args = append(toy.Launch.Args, test.bug)

			result, err := run.Run(t.Context(), toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			got := result.Violation
			switch {
			case test.check == "" && got != nil:
				t.Errorf("The run reported %s: %s", got.ID, got.Statement)
			case test.check == "":
			case got == nil:
				t.Errorf("The run passed, want %s.", test.check)
			case got.ID != test.check || !strings.Contains(got.Statement, test.says):
				t.Errorf("The run reported %s: %s\nwant %s saying %q.", got.ID, got.Statement, test.check, test.says)
			}
		})
	}
}
