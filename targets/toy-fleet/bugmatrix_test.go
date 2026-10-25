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
	116: "Removes every finalizer from a deleting Volume.",
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
		"| B0 | The correct controller. | all 2 plans of config.yaml, database/config.yaml, protect/config.yaml | none | |\n",
		"| B116 | Removes every finalizer from a deleting Volume. | protect/b0 | Finalizers | 2 |\n" +
			"| B116 | Removes every finalizer from a deleting Volume. | slow-delete | Finalizers | 1 |\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Want %q in:\n%s", want, md)
		}
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
