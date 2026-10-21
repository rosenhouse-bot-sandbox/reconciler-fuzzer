//go:build envtest

package main_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func TestMain(m *testing.M) {
	for _, build := range [][]string{
		{"go", "build", "-o", "../../bin/toy-bucket", "."},
		{"go", "build", "-o", "../../bin/crudfake", "../../cmd/crudfake"},
	} {
		if out, err := exec.Command(build[0], build[1:]...).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "%v: %v\n%s", build, err, out)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

func TestGate(t *testing.T) {
	for _, rows := range byRun(gate) {
		g := rows[0]
		verb := "catches"
		if g.finding == "" {
			verb = "passes"
		}
		t.Run(fmt.Sprintf("%s %s B%d", g.name(), verb, g.bug), func(t *testing.T) {
			t.Parallel()
			findings, report := runPlan(t, g.configPath(), g.plan, g.bug)
			if g.finding == "" && findings != 0 {
				t.Errorf("Found %d, want no findings. report.md:\n%s", findings, report)
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
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("Found plans %v, %v.", names, err)
		}
		for _, name := range names {
			t.Run(filepath.Join(filepath.Dir(config), name)+" passes B0", func(t *testing.T) {
				t.Parallel()
				if findings, report := runPlan(t, config, name, 0); findings != 0 || report != "No findings.\n" {
					t.Errorf("Found %d, want no findings and no notes. report.md:\n%s", findings, report)
				}
			})
		}
	}
}

func runPlan(t *testing.T, config, name string, bug int) (int, string) {
	report, text := run1(t, config, run.Options{PlanPath: planPath(config, name)}, bug)
	if strings.Contains(text, "# Notes") {
		t.Errorf("report.md has notes:\n%s", text)
	}
	return len(report.Findings), text
}

// run1 runs o with config under bug into a directory it removes only if
// the test passes.
func run1(t *testing.T, config string, o run.Options, bug int) (check.Report, string) {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-bucket-")
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
