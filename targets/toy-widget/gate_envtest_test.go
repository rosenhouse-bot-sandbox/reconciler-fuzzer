//go:build envtest

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func TestMain(m *testing.M) {
	build := exec.Command("go", "build", "-o", "../../bin/toy-widget", ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Building the toy: %v\n%s", err, out)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestGate(t *testing.T) {
	for _, g := range gate {
		verb := "catches"
		if g.finding == "" {
			verb = "passes"
		}
		t.Run(fmt.Sprintf("%s %s B%d", g.name(), verb, g.bug), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, g.configPath(), run.Options{PlanPath: g.planPath()}, g.bug)
			if g.finding == "" && len(report.Findings) != 0 {
				t.Errorf("Found %d, want no findings. report.md:\n%s", len(report.Findings), text)
			}
			if g.finding != "" && !hasFinding(text, g.finding, g.step, g.detail) {
				t.Errorf("Want a finding %q at step %d that says %q. report.md:\n%s", g.finding, g.step, g.detail, text)
			}
			if !g.notesMatch(report.Notes) {
				t.Errorf("Found notes %q, want notes that say %q. report.md:\n%s", report.Notes, g.notes, text)
			}
		})
	}
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("Found plans %v, %v.", names, err)
		}
		for _, name := range names {
			t.Run(filepath.Join(filepath.Dir(config), name)+" passes B0", func(t *testing.T) {
				t.Parallel()
				report, text := run1(t, config, run.Options{PlanPath: planPath(config, name)}, 0)
				if text != "No findings.\n" {
					t.Errorf("Found %d, want no findings and no notes. report.md:\n%s", len(report.Findings), text)
				}
			})
		}
	}
}

// run1 runs o with config under bug into a directory it removes only if
// the test passes.
func run1(t *testing.T, config string, o run.Options, bug int) (check.Report, string) {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			os.RemoveAll(out)
		}
	})
	o.ConfigPath, o.Out, o.LaunchArgs = config, out, []string{fmt.Sprintf("--bug=%d", bug)}
	report, err := run.Run(t.Context(), o)
	if err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	if t.Failed() || len(report.Findings) > 0 {
		t.Logf("The run is in %s.", out)
	}
	return report, string(text)
}

func TestConvergeSuggestsModellingAnUnmodelledGadget(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
	_, report := run1(t, "gadget/unmodelled/config.yaml", run.Options{Plan: p}, 0)
	want := "add to models: - {kind: toy.reconciler-fuzzer/v1/Gadget, stages: readyCondition, after: 200ms}"
	if !hasFinding(report, "Converge: did not settle", 0, want) {
		t.Errorf("Want a Converge finding at step 0 that says %q. report.md:\n%s", want, report)
	}
}
