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
	// notes are what the run's notes say, in order. Without them the run
	// gives no note.
	notes []string
}

const (
	gadgetConfig = "gadget/config.yaml"
	tenantConfig = "tenant/config.yaml"
	grantConfig  = "grant/config.yaml"
	leaseConfig  = "lease/config.yaml"
)

// configs are the toy's configs.
var configs = []string{"config.yaml", gadgetConfig, tenantConfig, grantConfig, leaseConfig}

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
	{plan: "b1", bug: 1, finding: "Converge: did not stay settled", step: 0, detail: "changed writes"},
	{plan: "b2", bug: 2, finding: "Converge: did not settle", step: 0, detail: "changed writes"},
	{plan: "b4", bug: 4, finding: "Converge: did not settle", step: 1, detail: "is not ready"},
	{plan: "b5", bug: 5, finding: "Converge: did not settle", step: 0, detail: "is not ready", notes: []string{"primary.ready held for no CR version"}},
	{plan: "b6", bug: 6, finding: "Converge: did not settle", step: 0, detail: "changed writes"},
	{plan: "b7", bug: 7, finding: "Converge: did not settle", step: 1, detail: "is not ready"},
	{plan: "b10", bug: 10, finding: "Converge: did not settle", step: 2, detail: "is not ready"},
	{plan: "b11", bug: 11, finding: "Converge: did not settle", step: 2, detail: "is not ready"},
	{plan: "b11-fault", bug: 11, finding: "Converge: did not settle", step: 1, detail: "is not ready"},
	{plan: "b12", bug: 12, finding: "Crash: the controller exited", step: 1, detail: "exit status 2"},
	{plan: "b12-fault", bug: 12, finding: "Crash: the controller exited", step: 1, detail: "exit status 2"},
	{plan: "b13", bug: 13, finding: "Converge: did not settle", step: 1, detail: "is being deleted"},
	{plan: "kill-delete", bug: 13, finding: "Converge: did not settle", step: 1, detail: "is being deleted"},
	{plan: "b15", bug: 15, finding: "Converge: did not settle", step: 0, detail: "is not ready"},
	{plan: "b16", bug: 16, finding: "Converge: did not settle", step: 0, detail: "is not ready", notes: []string{"primary.ready held for no CR version",
		"kube: launch.rbac forbids the controller's update on the status subresource of widgets.toy.reconciler-fuzzer, "}},
	{plan: "b3", bug: 3, finding: "Clean: left resources behind", step: 1, detail: "widget-0 (rule 4, slot a)"},
	{plan: "b9", bug: 9, finding: "Clean: left resources behind", step: 1, detail: "widget-1 (rule 4, slot a)", notes: []string{"primary.ready held for no CR version"}},
	{plan: "b8", bug: 8, finding: "Restore: did not restore damaged resources", step: 1, detail: "widget-0"},
	{plan: "b14", bug: 14, finding: "Restore: changed resources", step: 2, detail: "widget-0"},
	{plan: "b17", bug: 17, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "gadgets.toy.reconciler-fuzzer", config: gadgetConfig},
	{plan: "b18", bug: 18, finding: "Converge: did not settle", step: 1, detail: "is not ready", config: gadgetConfig},
	{plan: "b19", bug: 19, finding: "Honest: stayed ready after seeing its dependency not ready", step: 1, detail: "seen not ready", config: gadgetConfig},
	{plan: "b20", bug: 20, finding: "Honest: reported ready with no dependency", step: 0, detail: "slot a", config: gadgetConfig},
	{plan: "b21", bug: 21, finding: "Converge: did not settle", step: 0, detail: "over the budget", config: gadgetConfig},
	{plan: "b22", bug: 22, finding: "Clean: left resources behind", step: 1, detail: "gadgets.toy.reconciler-fuzzer", config: gadgetConfig},
	{plan: "b23", bug: 23, finding: "Converge: did not settle", step: 1, detail: "is being deleted, held by finalizer widget.reconciler-fuzzer/cleanup", config: gadgetConfig},
	{plan: "foreground-slow-delete", bug: 23, finding: "Converge: did not settle", step: 1, detail: "is being deleted, held by finalizer widget.reconciler-fuzzer/cleanup", config: gadgetConfig},
	{plan: "b24", bug: 24, finding: "Restore: did not restore damaged resources", step: 1, detail: "gadgets.toy.reconciler-fuzzer", config: gadgetConfig},
	{plan: "b25", bug: 25, finding: "Backoff: retried a failing request in a tight loop", step: 1, detail: "patch gadgets.toy.reconciler-fuzzer", config: gadgetConfig,
		notes: []string{"kube: launch.rbac forbids the controller's patch on the status subresource of gadgets.toy.reconciler-fuzzer, "}},
	{plan: "b26", bug: 26, finding: "Clean: left resources behind", step: 1, detail: "clusterroles.rbac.authorization.k8s.io tenant-tenant (rule 3, slot a)", config: tenantConfig},
	{plan: "b27", bug: 27, finding: "Clean: left resources behind", step: 1, detail: ".grant (rule 2, slot a)", config: grantConfig},
	{plan: "b27", bug: 27, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: ".grant (rule 2, slot a)", config: grantConfig},
	{plan: "b28", bug: 28, finding: "Clean: left resources behind", step: 1, detail: "-grant (rule 1, slot a)", config: grantConfig},
	{plan: "b28", bug: 28, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: "-grant (rule 1, slot a)", config: grantConfig},
	{plan: "b29", bug: 29, finding: "Converge: did not settle", step: 1, detail: "is being deleted", config: tenantConfig},
	{plan: "b29-background", bug: 29, config: tenantConfig},
}

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, whose details say detail.
func hasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		header, details, _ := strings.Cut(body, "\n\n")
		details, _, _ = strings.Cut(details, "\n# ")
		if strings.HasPrefix(title, heading) && strings.HasPrefix(header, fmt.Sprintf("Step %d,", step)) && strings.Contains(details, detail) {
			return true
		}
	}
	return false
}

// notesMatch says whether each note says the string at its index in want.
func notesMatch(notes, want []string) bool {
	return slices.EqualFunc(notes, want, strings.Contains)
}

func TestNotesMatch(t *testing.T) {
	for _, c := range []struct {
		notes, want []string
		match       bool
	}{
		{nil, nil, true},
		{[]string{"a note"}, nil, false},
		{nil, []string{"note"}, false},
		{[]string{"a note"}, []string{"note"}, true},
		{[]string{"a note"}, []string{"other"}, false},
		{[]string{"first", "second"}, []string{"first", "second"}, true},
		{[]string{"first", "second"}, []string{"second", "first"}, false},
		{[]string{"first", "second"}, []string{"first"}, false},
	} {
		if got := notesMatch(c.notes, c.want); got != c.match {
			t.Errorf("notesMatch(%q, %q) = %v", c.notes, c.want, got)
		}
	}
}

func TestHasFindingMatchesOnlyTheDetails(t *testing.T) {
	const report = "# Findings\n\n## Restore: did not restore damaged resources\n\nStep 1, at 1s.\nActions: damage configmaps ns/widget-0.\n\n```\nconfigmaps ns/widget-1\n```\n\n# Next steps\n\nReplay widget-2.\n"
	if !hasFinding(report, "Restore", 1, "widget-1") {
		t.Error("missed the detail line")
	}
	for _, elsewhere := range []string{"widget-0", "widget-2"} {
		if hasFinding(report, "Restore", 1, elsewhere) {
			t.Errorf("matched %q outside the details", elsewhere)
		}
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
