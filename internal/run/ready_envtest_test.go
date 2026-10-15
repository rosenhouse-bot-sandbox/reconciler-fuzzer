//go:build envtest

package run_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// A G4 on op 0 has three usual causes, and the run says which.
func TestAnExpiredWaitSaysWhy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	runOn := func(t *testing.T, toy *target.Target) (run.Result, error) {
		t.Helper()
		return run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})
	}

	t.Run("a ready that names a field the CR lacks", func(t *testing.T) {
		result, err := runOn(t, withReady(t, binary, "status.readyy == spec.count"))

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		want := `ready never held: evaluating ready "status.readyy == spec.count": no such key: readyy`
		if result.Violation == nil || result.Violation.ID != "G4" || !strings.Contains(result.Violation.Statement, want) {
			t.Fatalf("The run reported %v, want a G4 saying %q.", result.Violation, want)
		}
		if ready := result.Violation.Ready; ready == nil || ready.Status["ready"] == nil {
			t.Errorf("The violation quotes %+v, want the status the toy wrote.", ready)
		}
	})

	t.Run("a ready that yields no bool", func(t *testing.T) {
		result, err := runOn(t, withReady(t, binary, "status.ready"))

		if !errors.Is(err, target.ErrNotBool) || !strings.Contains(err.Error(), `"status.ready"`) {
			t.Errorf("The run returned the error %v, want one that names the expression and says it yields no bool.", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %v against the toy, and the declaration is at fault.", result.Violation)
		}
	})

	t.Run("a target that holds ready and keeps writing", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, "--bug=6")

		result, err := runOn(t, toy)

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		want := "but the namespace never held still for 2s (timeouts.stable)"
		if result.Violation == nil || result.Violation.ID != "G4" ||
			!strings.Contains(result.Violation.Statement, "ready held from") || !strings.Contains(result.Violation.Statement, want) {
			t.Errorf("The run reported %v, want a G4 saying ready held and %q.", result.Violation, want)
		}
	})
}

// withReady is the toy with its ready replaced, loaded as target.yaml declares
// one.
func withReady(t *testing.T, binary, ready string) *target.Target {
	t.Helper()
	sample, err := filepath.Abs(repoRoot + "/targets/toy-widget/widget.yaml")
	if err != nil {
		t.Fatal(err)
	}
	declared := fmt.Sprintf("name: toy-widget\nprimary: toy.reconciler-fuzzer/v1/Widget\nsample: %s\nready: '%s'\nlaunch: {binary: %s}\n",
		sample, ready, binary)
	path := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(path, []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Loading a ready of %q failed: %v", ready, err)
	}
	toy := loadTarget(t, binary)
	toy.Ready, toy.ReadyExpr = loaded.Ready, loaded.ReadyExpr
	return toy
}
