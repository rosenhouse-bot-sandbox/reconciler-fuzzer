package main_test

import (
	"cmp"
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
	// notes are what the run's notes say, in order. Without them the run
	// gives no note.
	notes []string
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
	{plan: "b30", bug: 30, finding: "Clean: left resources behind", step: 1, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b30", bug: 30, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b31", bug: 31, finding: "Unique: duplicate resources", step: 0, detail: "cloud buckets buckets-1 (rule 2, slot a), cloud buckets buckets-2"},
	{plan: "b31", bug: 31, finding: "Clean: left resources behind", step: 2, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b31", bug: 31, finding: "Teardown: released a CR before deleting its resources", step: 2, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b31", bug: 31, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "slot a: cloud buckets buckets-1 not seen"},
	{plan: "b32", bug: 32, finding: "Unique: duplicate resources", step: 0, detail: "cloud buckets buckets-1 (rule 2, slot a), cloud buckets buckets-2"},
	{plan: "b32", bug: 32, finding: "Clean: left resources behind", step: 2, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b32", bug: 32, finding: "Teardown: released a CR before deleting its resources", step: 2, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b32", bug: 32, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "slot a: cloud buckets buckets-1 not seen"},
	{plan: "b33", bug: 33, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "cloud buckets buckets-1 seen not ready"},
	{plan: "b34", bug: 34, finding: "Restore: did not restore damaged resources", step: 1, detail: "cloud buckets buckets-1 (rule 2, slot a)"},
	{plan: "b35", bug: 35, finding: "Backoff: retried a failing request in a tight loop", step: 0, detail: "create cloud buckets b-"},
	{plan: "halt", bug: 0, config: haltedConfig},
	{plan: "halt", bug: 0, finding: "Converge: did not settle", step: 0, detail: "is not ready", config: unhaltedConfig, notes: []string{"primary.ready held for no CR version"}},
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

func TestByRunGroupsRowsOfOneRun(t *testing.T) {
	rows := []gateRow{{plan: "a", bug: 1}, {plan: "b", bug: 1}, {plan: "a", bug: 1, step: 2}, {plan: "a", bug: 0}, {plan: "a", bug: 1, config: "x.yaml"}}
	if got := byRun(rows); len(got) != 4 || len(got[0]) != 2 || got[0][1].step != 2 {
		t.Errorf("byRun = %+v", got)
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
