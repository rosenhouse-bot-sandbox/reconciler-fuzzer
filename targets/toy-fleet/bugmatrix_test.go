package main_test

import (
	"flag"
	"fmt"
	"os"
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
	123: "Readies a Database on a Cluster that is not Ready.",
	124: "Readies a Database without checking its Secret.",
	125: "Keeps a Ready Database ready after its Secret goes.",
}

func TestEveryReferenceRowBugHasADescription(t *testing.T) {
	for _, r := range referenceRows {
		if bugs[r.bug] == "" {
			t.Errorf("B%d has no description.", r.bug)
		}
	}
}

// renderBugMatrix renders this toy's section: a B0 row that counts the
// committed plans, then a row for each of referenceRows.
func renderBugMatrix(plans []string) string {
	var b strings.Builder
	b.WriteString(section + "\n" +
		"Each Cluster owns Volumes of a third party's kind, which a model sets ready. " +
		"A Database names its Cluster and maybe a Secret, and a User its Database (targets/toy-fleet).\n\n" +
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| B0 | %s | all %d committed plans | none | |\n", bugs[0], len(plans))
	for _, r := range referenceRows {
		fmt.Fprintf(&b, "| B%d | %s | %s | Honest | %d |\n", r.bug, bugs[r.bug], r.plan, r.step)
	}
	return b.String()
}

func TestRenderBugMatrix(t *testing.T) {
	md := renderBugMatrix([]string{"b0", "stuck"})
	for _, want := range []string{
		"\n## Fleet toy\n",
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n",
		"| B0 | The correct controller. | all 2 committed plans | none | |\n",
		"| B125 | Keeps a Ready Database ready after its Secret goes. | secret-recreate | Honest | 1 |\n",
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
