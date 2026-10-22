package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const databaseConfig = "database/config.yaml"

// configs are the toy's configs. B0 passes every plan in plans/ beside each.
var configs = []string{"config.yaml", databaseConfig}

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
}

// gate lists, for each plan of config.yaml, the bug it must catch and the
// finding report.md must hold: its heading, its step and a detail line.
var gate = []gateRow{
	{"kill-finalize", 60, "Clean: left resources behind", 1, "cluster-vol-0 (rule 2, slot a)"},
	{"restart-deleting", 61, "Converge: did not settle (clause 2)", 1, "cluster is being deleted"},
	{"foreground-kill", 61, "Converge: did not settle (clause 2)", 1, "cluster is being deleted"},
	// The CR goes while its unowned Volumes still terminate under slowDelete.
	{"slow-delete", 60, "Clean: released the CR before its resources were gone", 1, "cluster-vol-0 (rule 2, slot a)"},
	{"slow-delete", 62, "Backoff", 1, "delete volumes.storage.thirdparty"},
}

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, that says detail.
func hasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		at := fmt.Sprintf("Step %d", step)
		if strings.HasPrefix(title, heading) && (strings.HasPrefix(body, at+",") || strings.HasPrefix(body, at+" (")) && strings.Contains(body, detail) {
			return true
		}
	}
	return false
}

// byRun groups the gate's rows by plan and bug, in order.
func byRun(rows []gateRow) [][]gateRow {
	var runs [][]gateRow
	for _, g := range rows {
		n := slices.IndexFunc(runs, func(run []gateRow) bool { return run[0].plan == g.plan && run[0].bug == g.bug })
		if n < 0 {
			runs = append(runs, nil)
			n = len(runs) - 1
		}
		runs[n] = append(runs[n], g)
	}
	return runs
}

// checkOf is the check a finding heading names.
func checkOf(heading string) string {
	check, _, _ := strings.Cut(heading, ":")
	return check
}

// checksOf lists the check of each finding in report.
func checksOf(report string) []string {
	var checks []string
	for _, section := range strings.Split(report, "\n## ")[1:] {
		checks = append(checks, checkOf(section))
	}
	return checks
}

func TestEveryGateRowHasAPlan(t *testing.T) {
	for _, g := range gate {
		if _, err := os.Stat(planPath("config.yaml", g.plan)); err != nil {
			t.Errorf("Gate row B%d: %v", g.bug, err)
		}
	}
}

func planPath(config, plan string) string {
	return filepath.Join(filepath.Dir(config), "plans", plan+".json")
}

// plans lists the plans beside config, without .json.
func plans(config string) ([]string, error) {
	paths, err := filepath.Glob(planPath(config, "*"))
	for i, path := range paths {
		paths[i] = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return paths, err
}

func TestEveryPlanLoads(t *testing.T) {
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s has plans %v, %v.", config, names, err)
		}
		for _, name := range names {
			if _, err := plan.Load(planPath(config, name)); err != nil {
				t.Errorf("%v", err)
			}
		}
	}
}
