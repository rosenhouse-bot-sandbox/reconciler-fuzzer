package main_test

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	databaseConfig = "database/config.yaml"
	orderConfig    = "order/config.yaml"
	// protectConfig needs a model finalizer, which configs cannot yet set.
	protectConfig = "protect/config.yaml"
)

// configs are the toy's configs. B0 passes every plan in plans/ beside each.
var configs = []string{"config.yaml", databaseConfig, orderConfig}

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
	// config is the config's path, config.yaml when empty. Its plans are
	// in plans/ beside it.
	config string
}

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line. An empty
// finding means the bug must pass the plan.
var gate = []gateRow{
	{"slow-delete", 15, "Clean: released a CR before its resources were gone", 1, "cluster-vol-0 (rule 1, slot a)", ""},
	{"slow-delete", 15, "Property: property volumes-go-before-their-cluster does not hold", 1, "is false", orderConfig},
	{"slow-delete", 16, "Finalizers: removed another caller's finalizer", 1, "cluster-vol-0: reconciler-fuzzer.dev/slow-delete", ""},
	{"stuck-delete", 17, "Converge: did not settle (clause 2)", 1, "cluster is being deleted", ""},
	{"slow-delete", 18, "", 0, "", ""},
	{"b0", 0, "", 0, "", protectConfig},
	{"b0", 15, "Clean: released a CR before its resources were gone", 2, "cluster-vol-0 (rule 1, slot a)", protectConfig},
	{"b0", 16, "Finalizers: removed another caller's finalizer", 2, "cluster-vol-0: storage.thirdparty/protect", protectConfig},
}

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

// name is the plan's path from its config's directory, without .json.
func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

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

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, that says detail. The delete-all step's body names it,
// as in "Step 2 (delete all),".
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

// byRun groups the gate's rows by config, plan and bug, in order.
func byRun(rows []gateRow) [][]gateRow {
	var runs [][]gateRow
	for _, g := range rows {
		n := slices.IndexFunc(runs, func(run []gateRow) bool {
			return run[0].configPath() == g.configPath() && run[0].plan == g.plan && run[0].bug == g.bug
		})
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
	for _, g := range gate {
		if _, err := os.Stat(g.planPath()); err != nil {
			t.Errorf("Gate row B%d: %v", g.bug, err)
		}
	}
}
