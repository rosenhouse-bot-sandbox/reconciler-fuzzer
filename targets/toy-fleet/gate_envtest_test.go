//go:build envtest

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("Found plans %v, %v.", names, err)
		}
		for _, name := range names {
			t.Run(filepath.Join(filepath.Dir(config), name)+" passes B0", func(t *testing.T) {
				t.Parallel()
				if report := runPlan(t, config, name, 0); report != "No findings.\n" {
					t.Errorf("Want no findings and no notes. report.md:\n%s", report)
				}
			})
		}
	}
}

// runPlan runs a plan under bug into a directory it removes only if the
// test passes, and returns report.md.
func runPlan(t *testing.T, config, name string, bug int) string {
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
	o := run.Options{ConfigPath: config, PlanPath: planPath(config, name), Out: out, LaunchArgs: []string{fmt.Sprintf("--bug=%d", bug)}}
	if _, err := run.Run(t.Context(), o); err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}
