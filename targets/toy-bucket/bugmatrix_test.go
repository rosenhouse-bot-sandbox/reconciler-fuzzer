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
	section       = "\n## Bucket toy\n"
)

// bugs describes each seeded bug of controller/.
var bugs = map[int]string{
	0:  "The correct controller.",
	30: "Leaks the bucket on CR delete.",
	31: "Retries a create of unknown outcome under a new random name and keeps both buckets.",
	32: "Names its bucket with a per-process nonce, so a restart after the create makes a second one.",
	33: "Reports ready right after the create, while the bucket is CREATING.",
	34: "Never recreates a missing bucket.",
	35: "Retries a 503 every 10 ms, past the rate limiter.",
}

func describe(g gateRow) string {
	switch g.configPath() {
	case haltedConfig:
		return bugs[g.bug] + " With --halt, it stops after a create of unknown outcome."
	case unhaltedConfig:
		return bugs[g.bug] + " With --halt and no primary.halted, it never settles."
	}
	return bugs[g.bug]
}

// renderBugMatrix renders this toy's section: the gate table, plus a B0 row
// for every plan.
func renderBugMatrix(plans []string) string {
	rows := slices.Clone(gate)
	slices.SortStableFunc(rows, func(a, b gateRow) int {
		return cmp.Or(cmp.Compare(a.bug, b.bug), strings.Compare(a.name(), b.name()))
	})
	var b strings.Builder
	b.WriteString(section + "\n" +
		"Each Bucket owns one bucket in the `crudfake` system `cloud` (targets/toy-bucket).\n\n" +
		"| Bug | Description | Plan | Check | Step |\n|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| B0 | %s | all %d committed plans | none | |\n", bugs[0], len(plans))
	for _, g := range rows {
		check, _, _ := strings.Cut(g.finding, ":")
		step := " " + strconv.Itoa(g.step) + " "
		if check == "" {
			check, step = "none", " "
		}
		fmt.Fprintf(&b, "| B%d | %s | %s | %s |%s|\n", g.bug, describe(g), g.name(), check, step)
	}
	return b.String()
}

func TestRenderBugMatrix(t *testing.T) {
	md := renderBugMatrix([]string{"b30", "b0"})
	for _, want := range []string{
		"\n## Bucket toy\n",
		"| B0 | The correct controller. | all 2 committed plans | none | |\n",
		"| B30 | Leaks the bucket on CR delete. | b30 | Clean | 1 |\n",
		"| B0 | The correct controller. With --halt, it stops after a create of unknown outcome. | halted/halt | none | |\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("Want %q in:\n%s", want, md)
		}
	}
	if unique, clean := strings.Index(md, "| b31 | Unique |"), strings.Index(md, "| b31 | Clean |"); unique < 0 || unique > clean {
		t.Errorf("Want B31's Unique row before its Clean row:\n%s", md)
	}
}

// TestBugMatrixIsCurrent owns this toy's section of the file, up to the next
// toy's section.
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
	if err != nil {
		t.Fatal(err)
	}
	updated := spliceSection(string(got), want)
	if *update {
		if err := os.WriteFile(bugMatrixPath, []byte(updated), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if updated != string(got) {
		t.Errorf("docs/bug-matrix.md is stale. Run 'make bug-matrix'.")
	}
}

// spliceSection puts rendered in place of this toy's section of md, or at
// its end.
func spliceSection(md, rendered string) string {
	head, rest, _ := strings.Cut(md, section)
	if i := strings.Index(rest, "\n## "); i >= 0 {
		return head + rendered + rest[i:]
	}
	return head + rendered
}

func TestSpliceSectionKeepsTheOtherToysSections(t *testing.T) {
	const head, other = "# Bug matrix\n\n| B0 |\n", "\n## Other toy\n\n| B101 |\n"
	const old, fresh = section + "\n| B30 old |\n", section + "\n| B30 |\n"
	if got := spliceSection(head+old+other, fresh); got != head+fresh+other {
		t.Errorf("Replacing the section gave %q.", got)
	}
	if got := spliceSection(head+other, fresh); got != head+other+fresh {
		t.Errorf("Adding the section gave %q.", got)
	}
}

func TestEveryGateBugHasADescription(t *testing.T) {
	for _, g := range gate {
		if bugs[g.bug] == "" {
			t.Errorf("B%d has no description.", g.bug)
		}
	}
}
