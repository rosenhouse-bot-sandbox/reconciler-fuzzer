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

const databaseConfig = "database/config.yaml"

// configs are the toy's configs. B0 passes every plan in plans/ beside each.
var configs = []string{"config.yaml", databaseConfig}

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
	// config is the config's path, config.yaml when empty.
	config string
}

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

// name is the plan's path from its config's directory, without .json.
func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line.
var gate = []gateRow{
	{"b31", 31, "Honest: reported ready before seeing its dependency ready", 0, "cluster-vol-1 seen not ready", ""},
	{"b31", 31, "Honest: reported ready before seeing its dependency ready", 1, "cluster-vol-2 seen not ready", ""},
	{"b32", 32, "Unique: duplicate resources", 0, "cluster-vol-1-", ""},
	{"b32", 32, "Unique: duplicate resources", 1, "cluster-vol-0-", ""},
	{"b33", 33, "Converge: did not settle", 0, "is not ready", ""},
}

// gaps lists bugs the fuzzer should catch with some finding and misses.
// detail says what it should see.
var gaps = []gateRow{
	{plan: "b30", bug: 30, detail: "a Ready Cluster fell back to Provisioning with no new generation, fault or Volume change"},
	{plan: "b34", bug: 34, detail: "the Cluster reports generation 2 observed and Ready, but no Volume changed after the update"},
	{plan: "b35", bug: 35, detail: "after the restart, the Cluster owns two Volumes per index, cluster-vol-<i>-<suffix>"},
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
	for _, g := range append(gate, gaps...) {
		if _, err := os.Stat(g.planPath()); err != nil {
			t.Errorf("Gate row B%d: %v", g.bug, err)
		}
	}
}
