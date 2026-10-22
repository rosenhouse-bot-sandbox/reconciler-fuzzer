//go:build envtest

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func TestMain(m *testing.M) {
	build := exec.Command("go", "build", "-o", "../../bin/toy-fleet", ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Building the toy: %v\n%s", err, out)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestGate(t *testing.T) {
	runGate(t, gate)
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("Found plans %v, %v.", names, err)
		}
		for _, name := range names {
			t.Run(filepath.Join(filepath.Dir(config), name)+" passes B0", func(t *testing.T) {
				t.Parallel()
				if report := runPlan(t, config, planPath(config, name), 0); report != "No findings.\n" {
					t.Errorf("Want no findings and no notes. report.md:\n%s", report)
				}
			})
		}
	}
}

// TestReferencesJudgeTheOrderAcrossKinds runs the gate that the proposed
// references should pass.
func TestReferencesJudgeTheOrderAcrossKinds(t *testing.T) {
	runGate(t, referenceGate)
}

// TestCleanBlamesAChildOnTheCRThatOwnsIt wants Clean to see that Database
// db, which a plan created and which outlives its released Cluster, still
// owns its ConfigMap.
func TestCleanBlamesAChildOnTheCRThatOwnsIt(t *testing.T) {
	report := runPlan(t, "config.yaml", "ordering/gaps/plans/blocked-delete.json", 20)
	if strings.Contains(report, "db-db (rule 4, slot a)") {
		t.Errorf("Clean blamed Database db's ConfigMap on Cluster slot a. report.md:\n%s", report)
	}
}

// runGate runs each row's plan under its bug, and wants the row's findings
// and no other check's.
func runGate(t *testing.T, rows []gateRow) {
	for _, rows := range byRun(rows) {
		g := rows[0]
		verb := "catches"
		if g.finding == "" {
			verb = "passes"
		}
		t.Run(fmt.Sprintf("%s %s B%d", g.name(), verb, g.bug), func(t *testing.T) {
			t.Parallel()
			report := runPlan(t, g.configPath(), g.planPath(), g.bug)
			if g.finding == "" && report != "No findings.\n" {
				t.Errorf("Want no findings and no notes. report.md:\n%s", report)
			}
			want := map[string]bool{}
			for _, r := range rows {
				if r.finding != "" && !hasFinding(report, r.finding, r.step, r.detail) {
					t.Errorf("Want a finding %q at step %d that says %q. report.md:\n%s", r.finding, r.step, r.detail, report)
				}
				want[checkOf(r.finding)] = r.finding != ""
			}
			for _, got := range checksOf(report) {
				if !want[got] {
					t.Errorf("Found an unexpected %s finding. report.md:\n%s", got, report)
				}
			}
		})
	}
}

// runPlan runs the plan at path under bug into a directory it removes
// only if the test passes, and returns report.md.
func runPlan(t *testing.T, config, path string, bug int) string {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-gate-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("The run is in %s.", out)
		} else {
			os.RemoveAll(out)
		}
	})
	o := run.Options{ConfigPath: config, PlanPath: path, Out: out, LaunchArgs: []string{fmt.Sprintf("--bug=%d", bug)}}
	if _, err := run.Run(t.Context(), o); err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}
