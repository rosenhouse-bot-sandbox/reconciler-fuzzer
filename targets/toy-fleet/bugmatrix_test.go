package main_test

import (
	"cmp"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "write this toy's section of ../../docs/bug-matrix.md")

const (
	bugMatrixPath = "../../docs/bug-matrix.md"
	section       = "\n## Fleet toy\n"
)

// bugs describes each seeded bug of controller/.
var bugs = map[int]string{
	0:   "The correct controller.",
	140: "Under version 2, keeps the version 1 finalizer, so a deleted Cluster stays.",
	141: "Under version 2, names its Volumes <cluster>-data-<i> and sees only those, so it makes new ones beside version 1's.",
	142: "Under version 2, leaves a Cluster that version 1 was provisioning in the phase Provisioning.",
	150: "Strips its finalizers from deleting CRs when it shuts down.",
	151: "Puts a finalizer on each Volume, and releases a deleted Cluster before its Volumes, whose finalizers it strips only afterwards.",
	152: "Errors when a dependent kind is not served, as after its CRD is deleted, so the parent's deletion never ends.",
}

func TestEveryGateBugHasADescription(t *testing.T) {
	for _, g := range gate {
		if bugs[g.bug] == "" {
			t.Errorf("B%d has no description.", g.bug)
		}
	}
}

// renderBugMatrix renders this toy's section: a B0 row that counts the
// plans of configs, then the gate table.
func renderBugMatrix(plans []string) string {
	rows := slices.Clone(gate)
	slices.SortStableFunc(rows, func(a, b gateRow) int {
		return cmp.Or(cmp.Compare(a.bug, b.bug), strings.Compare(a.name(), b.name()))
	})
	var b strings.Builder
	b.WriteString(section + "\n" +
		"Each Cluster owns Volumes of a third party's kind, which a model sets ready. " +
		"A Database names its Cluster, and a User its Database (targets/toy-fleet).\n\n" +
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| B0 | %s | all %d plans of %s | none | |\n", bugs[0], len(plans), strings.Join(configs, ", "))
	for _, g := range rows {
		check, step := checkOf(g.finding), " "+strconv.Itoa(g.step)+" "
		if check == "" {
			check, step = "none", " "
		}
		fmt.Fprintf(&b, "| B%d | %s | %s | %s |%s|\n", g.bug, bugs[g.bug], g.name(), check, step)
	}
	return b.String()
}

func TestRenderBugMatrix(t *testing.T) {
	md := renderBugMatrix([]string{"b0", "stuck"})
	for _, want := range []string{
		"\n## Fleet toy\n",
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n",
		"| B0 | The correct controller. | all 2 plans of config.yaml, database/config.yaml, protect/config.yaml, upgrade/config.yaml | none | |\n",
		"| B0 | The correct controller. | uninstall/stop-first | none | |\n",
		"| B150 | Strips its finalizers from deleting CRs when it shuts down. | database/restart-cluster-delete | Converge | 1 |\n",
		"| B152 | Errors when a dependent kind is not served, as after its CRD is deleted, so the parent's deletion never ends. | uninstall/crd-first-stop | Converge | 2 |\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Want %q in:\n%s", want, md)
		}
	}
	if b0, b150 := strings.Index(md, "| B0 | The correct controller. | uninstall/recreate |"), strings.Index(md, "| B150 |"); b0 < 0 || b0 > b150 {
		t.Errorf("Want the B0 rows before the B150 row:\n%s", md)
	}
}

// TestBugMatrixIsCurrent owns this toy's section of the file, up to the next
// toy's section. A missing file reads as empty, so -update can rebuild it.
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
	got, err := os.ReadFile(bugMatrixPath)
	updated := spliceSection(string(got), want)
	if *update {
		if err := os.WriteFile(bugMatrixPath, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil || updated != string(got) {
		t.Errorf("docs/bug-matrix.md is stale. Run 'make bug-matrix'.")
	}
}

// spliceSection puts rendered in place of this toy's section of md, or
// appends it if md has none.
func spliceSection(md, rendered string) string {
	head, rest, _ := strings.Cut(md, section)
	if i := strings.Index(rest, "\n## "); i >= 0 {
		return head + rendered + rest[i:]
	}
	return head + rendered
}

func TestSpliceSectionKeepsTheOtherToysSections(t *testing.T) {
	const head, other = "# Bug matrix\n\n| B0 |\n", "\n## Other toy\n\n| B30 |\n"
	const old, fresh = section + "\n| B0 old |\n", section + "\n| B0 |\n"
	if got := spliceSection(head+old+other, fresh); got != head+fresh+other {
		t.Errorf("Replacing the section gave %q.", got)
	}
	if got := spliceSection(head+other, fresh); got != head+other+fresh {
		t.Errorf("Adding the section gave %q.", got)
	}
}
