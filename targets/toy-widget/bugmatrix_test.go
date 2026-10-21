package main_test

import (
	"cmp"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRenderBugMatrix(t *testing.T) {
	md := renderBugMatrix([]string{"b3", "b0"})
	for _, want := range []string{
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n",
		"| B3 | Leaves child 0 without an owner reference. | b3 | Clean | 1 |\n",
		"| B0 | The correct controller. | all 2 in plans/ and gadget/plans/ | none | |\n",
		"| B17 | Reports its Gadget ready without reading the Gadget's status. | gadget/b17 | Honest | 0 |\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Want %q in:\n%s", want, md)
		}
	}
	if b0, b14 := strings.Index(md, "| B0 "), strings.Index(md, "| B14 "); b0 > b14 {
		t.Errorf("Want B0 before B14:\n%s", md)
	}
	if b2, b11, b11f := strings.Index(md, "| B2 "), strings.Index(md, "| B11 | Never retries a child create the API server refused. | b11 "), strings.Index(md, " | b11-fault | "); !(b2 < b11 && b11 < b11f) {
		t.Errorf("Want rows sorted by bug, then plan:\n%s", md)
	}
}

var update = flag.Bool("update", false, "write ../../docs/bug-matrix.md")

const bugMatrixPath = "../../docs/bug-matrix.md"

func TestBugMatrixIsCurrent(t *testing.T) {
	var all []string
	for _, config := range configs {
		names, err := plans(config)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, names...)
	}
	want := renderBugMatrix(all)
	if *update {
		if err := os.WriteFile(bugMatrixPath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(bugMatrixPath)
	if err != nil || string(got) != want {
		t.Errorf("docs/bug-matrix.md is stale. Run 'make bug-matrix'.")
	}
}

func TestEveryGateBugHasADescription(t *testing.T) {
	for _, g := range gate {
		if bugs[g.bug] == "" {
			t.Errorf("B%d has no description.", g.bug)
		}
	}
}

// bugs describes each seeded bug of controller/widget_controller.go.
var bugs = map[int]string{
	0:  "The correct controller.",
	1:  "Reports children ready before it creates them.",
	2:  "Creates children with generated names, so it duplicates them.",
	3:  "Leaves child 0 without an owner reference.",
	4:  "Reads the child count from the status, not the spec.",
	5:  "Never creates a child that an uncached read cannot find.",
	6:  "Writes a fresh lastSyncTime on every reconcile.",
	7:  "Never deletes surplus children on scale-down.",
	8:  "Does not watch its children, so a deleted one goes unnoticed.",
	9:  "Owns no child and drops the finalizer before deleting children.",
	10: "Keeps in memory which Widgets it created children for, so a restart loses status updates.",
	11: "Never retries a child create the API server refused.",
	12: "Divides by a count of zero and crashes.",
	13: "Never cleans up a deleted Widget.",
	14: "Does not watch the label source, so a changed label reaches no child.",
	15: "Names children after the kind, so two Widgets share them.",
	16: "Writes status with Update, which its RBAC does not allow.",
	17: "Reports its Gadget ready without reading the Gadget's status.",
	18: "Drops a Widget's Gadget events once it saw the Gadget not ready, so it never notices recovery.",
	19: "Never reports its Gadget not ready once it was ready.",
	20: "Creates no Gadget and reports it ready.",
	21: "Updates its Gadget on every reconcile and requeues after 100 ms.",
	22: "Orphans its Gadget on delete instead of deleting it.",
	23: "Gives up on cleanup for good when its Gadget outlives the delete, so the finalizer stays.",
	24: "Never recreates a missing Gadget once it reported it ready.",
	25: "Retries a forbidden Gadget status patch every 10 ms while the Gadget has Ready False.",
}

// renderBugMatrix renders the gate table, plus a B0 row for every plan.
func renderBugMatrix(plans []string) string {
	rows := slices.Clone(gate)
	slices.SortFunc(rows, func(a, b gateRow) int {
		return cmp.Or(cmp.Compare(a.bug, b.bug), strings.Compare(a.name(), b.name()))
	})
	var b strings.Builder
	b.WriteString("# Bug matrix\n\n" +
		"This file is generated from targets/toy-widget/gate_test.go by `make bug-matrix`.\n" +
		"`make test-envtest` proves each row: the plan, run against the bug, reports the check's finding at that step.\n\n" +
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| B0 | %s | all %d in plans/ and gadget/plans/ | none | |\n", bugs[0], len(plans))
	for _, g := range rows {
		check, _, _ := strings.Cut(g.finding, ":")
		fmt.Fprintf(&b, "| B%d | %s | %s | %s | %d |\n", g.bug, bugs[g.bug], g.name(), check, g.step)
	}
	return b.String()
}
