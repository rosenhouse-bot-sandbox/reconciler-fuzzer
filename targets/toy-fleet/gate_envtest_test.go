//go:build envtest

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
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
				if name == "secondary-update" || name == "damage-volume" {
					t.Skip("enabled by the toy-fleet package")
				}
				t.Parallel()
				_, report := run1(t, config, run.Options{PlanPath: planPath(config, name)}, 0)
				if report != "No findings.\n" {
					t.Errorf("Want no findings and no notes. report.md:\n%s", report)
				}
			})
		}
	}
}

// run1 runs o with config under bug into a directory it removes only if
// the test passes.
func run1(t *testing.T, config string, o run.Options, bug int) (check.Report, string) {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-fleet-")
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
	o.ConfigPath, o.Out, o.LaunchArgs = config, out, []string{fmt.Sprintf("--bug=%d", bug)}
	report, err := run.Run(t.Context(), o)
	if err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	return report, string(text)
}
