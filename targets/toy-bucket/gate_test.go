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

const (
	haltedConfig   = "halted/config.yaml"
	unhaltedConfig = "halted/unhalted.yaml"
)

// configs are the toy's configs.
var configs = []string{"config.yaml", haltedConfig}

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

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line. An empty
// finding means the bug must pass the plan.
var gate = []gateRow{
	{"b30", 30, "Clean: left resources behind", 1, "cloud buckets buckets-1 (rule 2, slot a)", ""},
	{"b31", 31, "Unique: duplicate resources", 0, "cloud buckets buckets-1 (rule 2, slot a), cloud buckets buckets-2", ""},
	{"b31", 31, "Clean: left resources behind", 2, "cloud buckets buckets-1 (rule 2, slot a)", ""},
	{"b31", 31, "Honest: reported ready before seeing its dependency ready", 0, "slot a: cloud buckets buckets-1 not seen", ""},
	{"b32", 32, "Unique: duplicate resources", 0, "cloud buckets buckets-1 (rule 2, slot a), cloud buckets buckets-2", ""},
	{"b32", 32, "Clean: left resources behind", 2, "cloud buckets buckets-1 (rule 2, slot a)", ""},
	{"b32", 32, "Honest: reported ready before seeing its dependency ready", 0, "slot a: cloud buckets buckets-1 not seen", ""},
	{"b33", 33, "Honest: reported ready before seeing its dependency ready", 0, "cloud buckets buckets-1 seen not ready", ""},
	{"b34", 34, "Restore: did not restore damaged resources", 1, "cloud buckets buckets-1 (rule 2, slot a)", ""},
	{"b35", 35, "Backoff: retried a failing request in a tight loop", 0, "create cloud buckets b-", ""},
	{"halt", 0, "", 0, "", haltedConfig},
	{"halt", 0, "Converge: did not settle", 0, "is not ready", unhaltedConfig},
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

func TestByRunGroupsRowsOfOneRun(t *testing.T) {
	rows := []gateRow{{plan: "a", bug: 1}, {plan: "b", bug: 1}, {plan: "a", bug: 1, step: 2}, {plan: "a", bug: 0}, {plan: "a", bug: 1, config: "x.yaml"}}
	if got := byRun(rows); len(got) != 4 || len(got[0]) != 2 || got[0][1].step != 2 {
		t.Errorf("byRun = %+v", got)
	}
}

func TestChecksOfListsEachFindingsCheck(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2.\n\n## Unique: duplicate resources\n\nStep 0.\n"
	if got := checksOf(report); !slices.Equal(got, []string{"Clean", "Unique"}) {
		t.Errorf("checksOf = %q", got)
	}
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

func TestHasFindingMatchesTheStep(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2 (delete all), at 1s.\n\n```\nbuckets b\n```\n"
	if !hasFinding(report, "Clean", 2, "buckets b") {
		t.Error("missed step 2 (delete all)")
	}
	for _, step := range []int{1, 3, 12} {
		if hasFinding(report, "Clean", step, "buckets b") {
			t.Errorf("matched step %d", step)
		}
	}
}
