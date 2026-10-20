//go:build envtest

package toywidget_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/run"
)

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line.
var gate = []struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
}{
	{"b1", 1, "Converge: did not stay settled", 0, "changed writes"},
	{"b2", 2, "Converge: did not settle", 0, "changed writes"},
	{"b4", 4, "Converge: did not settle", 1, "is not ready"},
	{"b5", 5, "Converge: did not settle", 0, "is not ready"},
	{"b6", 6, "Converge: did not settle", 0, "changed writes"},
	{"b7", 7, "Converge: did not settle", 1, "is not ready"},
	{"b10", 10, "Converge: did not settle", 2, "is not ready"},
	{"b11", 11, "Converge: did not settle", 2, "is not ready"},
	{"b11-fault", 11, "Converge: did not settle", 1, "is not ready"},
	{"b12", 12, "Crash: the controller exited", 1, "exit status 2"},
	{"b12-fault", 12, "Crash: the controller exited", 1, "exit status 2"},
	{"b13", 13, "Converge: did not settle", 1, "is being deleted"},
	{"b15", 15, "Converge: did not settle", 0, "is not ready"},
	{"b16", 16, "Converge: did not settle", 0, "is not ready"},
	{"b3", 3, "Clean: left resources behind", 1, "widget-0 (rule 4, slot a)"},
	{"b9", 9, "Clean: left resources behind", 1, "widget-1 (rule 4, slot a)"},
	{"b8", 8, "Restore: did not restore damaged resources", 1, "widget-0"},
	{"b14", 14, "Restore: changed resources", 2, "widget-0"},
}

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, that says detail.
func hasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		if strings.HasPrefix(title, heading) && strings.HasPrefix(body, fmt.Sprintf("Step %d,", step)) && strings.Contains(body, detail) {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	build := exec.Command("go", "build", "-o", "../../../bin/toy-widget", "../../../targets/toy-widget")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "Building the toy: %v\n%s", err, out)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func TestGate(t *testing.T) {
	for _, g := range gate {
		t.Run(fmt.Sprintf("%s catches B%d", g.plan, g.bug), func(t *testing.T) {
			t.Parallel()
			_, report := runPlan(t, g.plan, g.bug)
			if !hasFinding(report, g.finding, g.step, g.detail) {
				t.Errorf("Want a finding %q at step %d that says %q. report.md:\n%s", g.finding, g.step, g.detail, report)
			}
		})
	}
	plans, err := filepath.Glob("plans/*.json")
	if err != nil || len(plans) < len(gate) {
		t.Fatalf("Found plans %v, %v.", plans, err)
	}
	for _, path := range plans {
		plan := strings.TrimSuffix(filepath.Base(path), ".json")
		t.Run(plan+" passes B0", func(t *testing.T) {
			t.Parallel()
			if findings, report := runPlan(t, plan, 0); findings != 0 || report != "No findings.\n" {
				t.Errorf("Found %d, want no findings and no notes. report.md:\n%s", findings, report)
			}
		})
	}
}

func runPlan(t *testing.T, plan string, bug int) (int, string) {
	report, text := run1(t, run.Options{PlanPath: filepath.Join("plans", plan+".json")}, bug)
	if strings.Contains(text, "# Notes") {
		t.Errorf("report.md has notes:\n%s", text)
	}
	return len(report.Findings), text
}

// run1 runs o under bug into a directory it removes only if the test passes.
func run1(t *testing.T, o run.Options, bug int) (check.Report, string) {
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
	o.ConfigPath, o.Out, o.LaunchArgs = "config.yaml", out, []string{fmt.Sprintf("--bug=%d", bug)}
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
