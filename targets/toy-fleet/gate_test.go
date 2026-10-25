package main_test

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	clusterConfig   = "cluster/config.yaml"
	databaseConfig  = "database/config.yaml"
	uninstallConfig = "uninstall/config.yaml"
)

// configs are the configs whose every plan B0 passes with no notes.
var configs = []string{"config.yaml", databaseConfig}

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
	// config is the config's path, config.yaml when empty. Its plans are
	// in plans/ beside it.
	config string
	// note is what the run's one note must say. Without it the run gives
	// no note.
	note string
}

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

// name is the plan's path from its config's directory, without .json.
func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line. An empty
// finding means the bug must pass the plan.
var gate = []gateRow{
	{plan: "restart-cluster-delete", bug: 150, finding: "Converge: did not settle (clause 1)", step: 1, detail: "db is not ready", config: databaseConfig},
	// The update error keeps B151's Volumes held until the stop.
	{plan: "stop-after-delete", bug: 151, finding: "Clean: left resources behind", step: 1, detail: "/cluster-vol-0 (rule 1, slot a)", config: uninstallConfig},
	{plan: "crd-first", bug: 152, finding: "Converge: did not settle (clause 2)", step: 2, detail: "cluster is being deleted", config: uninstallConfig},
	// B0 crashes there too: the toy cannot start without the Database CRD.
	{plan: "crd-first", bug: 152, finding: "Crash: the controller exited", step: 4, detail: `no matches for kind "Database"`, config: uninstallConfig},
	// The create waits for the name, the slowDelete for the create, and the
	// plan ends at its create.
	{plan: "recreate", config: uninstallConfig},
	// B0 updates no Volume while it deletes them.
	{plan: "stop-after-delete", config: uninstallConfig, note: "fault 1 (error(500) on verb=update group=storage.thirdparty resource=volumes) picked no request"},
	{plan: "stop-first", config: uninstallConfig, note: "cluster is not judged: the plan deleted it after the stop"},
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

// notesMatch says whether notes are one note that says note, or none when
// note is empty.
func notesMatch(notes []string, note string) bool {
	if note == "" {
		return len(notes) == 0
	}
	return len(notes) == 1 && strings.Contains(notes[0], note)
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
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2.\n\n## Converge: did not settle (clause 2)\n\nStep 0.\n"
	if got := checksOf(report); !slices.Equal(got, []string{"Clean", "Converge"}) {
		t.Errorf("checksOf = %q", got)
	}
}

func TestHasFindingMatchesTheStep(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2 (delete all), at 1s.\n\n```\nvolumes v\n```\n"
	if !hasFinding(report, "Clean", 2, "volumes v") {
		t.Error("missed step 2 (delete all)")
	}
	for _, step := range []int{1, 3, 12} {
		if hasFinding(report, "Clean", step, "volumes v") {
			t.Errorf("matched step %d", step)
		}
	}
}

func TestNotesMatchWantsTheOneNoteOrNone(t *testing.T) {
	for _, c := range []struct {
		notes []string
		note  string
		want  bool
	}{
		{nil, "", true},
		{[]string{"x"}, "", false},
		{[]string{"a CR is not judged"}, "not judged", true},
		{[]string{"a CR is not judged", "another"}, "not judged", false},
		{[]string{"another"}, "not judged", false},
		{nil, "not judged", false},
	} {
		if got := notesMatch(c.notes, c.note); got != c.want {
			t.Errorf("notesMatch(%q, %q) = %v", c.notes, c.note, got)
		}
	}
}

func TestEveryPlanLoadsAndCreatesOnlyListedKinds(t *testing.T) {
	type entry struct{ config, plan string }
	var entries []entry
	for _, path := range configs {
		names, err := plans(path)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s has plans %v, %v.", path, names, err)
		}
		for _, name := range names {
			entries = append(entries, entry{path, name})
		}
	}
	for _, g := range gate {
		entries = append(entries, entry{g.configPath(), g.plan})
	}
	for _, e := range entries {
		c, err := config.Load(e.config)
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.Load(planPath(e.config, e.plan))
		if err != nil {
			t.Error(err)
			continue
		}
		if err := p.ValidateKinds(c.ListedKinds()); err != nil {
			t.Errorf("%s under %s: %v", e.plan, e.config, err)
		}
	}
}
