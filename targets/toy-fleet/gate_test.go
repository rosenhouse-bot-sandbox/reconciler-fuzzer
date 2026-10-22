package main_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	databaseConfig = "database/config.yaml"
	userConfig     = "user/config.yaml"
	orderingConfig = "ordering/config.yaml"
	// referencesConfig is orderingConfig with the proposed references in
	// place of its properties.
	referencesConfig = "ordering/gaps/references.yaml"
)

// configs are the toy's configs. B0 passes every plan in plans/ beside each.
var configs = []string{"config.yaml", databaseConfig, userConfig, orderingConfig}

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

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line.
var gate = []gateRow{
	{"cluster-delete", 20, "Converge: did not settle (clause 1)", 1, "databases.toy.reconciler-fuzzer rf-", databaseConfig},
	{"user-first", 22, "Converge: did not settle (clause 1)", 0, "/alice is not ready", userConfig},
	{"delete-cluster-first", 20, "Property: property databases-have-a-cluster does not hold", 1, "is false", orderingConfig},
	{"delete-cluster-first", 20, "Property: property ready-databases-have-a-ready-cluster does not hold", 1, "is false", orderingConfig},
	{"delete-database-first", 21, "Property: property users-have-a-database does not hold", 1, "is false", orderingConfig},
	{"create-together", 23, "Property: property ready-databases-have-a-ready-cluster does not hold", 0, "is false", orderingConfig},
}

// referenceGate is the gate the proposed references should pass: the
// bugs above without properties, and blocked deletions under B0.
var referenceGate = []gateRow{
	{"blocked-delete", 0, "", 0, "", referencesConfig},
	{"../../plans/create-together", 0, "", 0, "", referencesConfig},
	{"../../plans/delete-cluster-first", 0, "", 0, "", referencesConfig},
	{"../../plans/delete-database-first", 0, "", 0, "", referencesConfig},
	{"../../plans/delete-cluster-first", 20, "Ordered: released a CR that another names", 1, "clusters.toy.reconciler-fuzzer", referencesConfig},
	{"../../plans/delete-database-first", 21, "Ordered: released a CR that another names", 1, "databases.toy.reconciler-fuzzer", referencesConfig},
	{"../../plans/create-together", 23, "Honest: reported ready before seeing its dependency ready", 0, "databases.toy.reconciler-fuzzer", referencesConfig},
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
	for _, g := range append(gate, referenceGate...) {
		if _, err := plan.Load(g.planPath()); err != nil {
			t.Errorf("Gate row B%d: %v", g.bug, err)
		}
	}
}

func TestHasFindingMatchesTheStep(t *testing.T) {
	const report = "# Findings\n\n## Converge: did not settle (clause 1)\n\nStep 1, at 1s.\n\n```\nusers u is not ready\n```\n"
	if !hasFinding(report, "Converge", 1, "users u") {
		t.Error("missed step 1")
	}
	if hasFinding(report, "Converge", 0, "users u") || hasFinding(report, "Converge", 1, "databases") {
		t.Error("matched another step or detail")
	}
}

func TestByRunGroupsRowsOfOneRun(t *testing.T) {
	rows := []gateRow{{plan: "a", bug: 1}, {plan: "b", bug: 1}, {plan: "a", bug: 1, step: 2}, {plan: "a", bug: 1, config: "x.yaml"}}
	if got := byRun(rows); len(got) != 3 || len(got[0]) != 2 || got[0][1].step != 2 {
		t.Errorf("byRun = %+v", got)
	}
}

func TestChecksOfListsEachFindingsCheck(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2.\n\n## Property: property p does not hold\n\nStep 0.\n"
	if got := checksOf(report); !slices.Equal(got, []string{"Clean", "Property"}) {
		t.Errorf("checksOf = %q", got)
	}
}

// TestReferencesConfigLoads wants the proposed references: which field of
// one kind names a CR of another, and what makes that CR ready.
func TestReferencesConfigLoads(t *testing.T) {
	if _, err := os.Stat(referencesConfig); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(referencesConfig); err != nil {
		t.Error(err)
	}
}

// TestGeneratedPlansCreateAndDeleteReferencingCRs wants generated plans to
// create and delete the kinds the references name, so that seeds reach the
// ordering across kinds.
func TestGeneratedPlansCreateAndDeleteReferencingCRs(t *testing.T) {
	c, err := config.Load(referencesConfig)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(c)
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]bool{}
	for seed := range uint64(50) {
		p, err := g.Plan(seed + 1)
		if err != nil {
			t.Fatal(err)
		}
		slots := map[string]string{}
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				var o struct{ Kind string }
				if a.Do == plan.Create && json.Unmarshal(a.Object, &o) == nil && o.Kind != "" {
					slots[a.Slot] = o.Kind
				}
				made[string(a.Do)+" "+slots[a.Slot]] = true
			}
		}
	}
	for _, want := range []string{"create Database", "delete Database", "create User", "delete User"} {
		if !made[want] {
			t.Errorf("No plan of seeds 1 to 50 has a %s.", want)
		}
	}
}
