package reconcilerfuzzer_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDesignStaysUnder530Lines(t *testing.T) {
	if lines := bytes.Count(design(t), []byte("\n")); lines >= 530 {
		t.Errorf("DESIGN.md has %d lines, want under 530.", lines)
	}
}

func TestDesignNamesEveryPackage(t *testing.T) {
	dirs, err := filepath.Glob("internal/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("found no packages: %v", err)
	}
	for _, problem := range packageTableProblems(string(design(t)), dirs) {
		t.Error(problem)
	}
}

func TestPackageTableProblemsCatchesMistakes(t *testing.T) {
	doc := "| Package | Job |\n| --- | --- |\n| `trace` | Events. |\n| `gone` | Nothing. |\n\n| `other` | Not a package row. |\n"
	got := packageTableProblems(doc, []string{"internal/trace", "internal/hunt"})
	want := []string{"DESIGN.md §2 has no row for internal/hunt.", "DESIGN.md §2 names gone, which internal/ lacks."}
	if !slices.Equal(got, want) {
		t.Errorf("packageTableProblems = %q, want %q.", got, want)
	}
}

var packageRow = regexp.MustCompile("^\\| `([a-z0-9]+)` \\|")

// packageTableProblems compares the rows of DESIGN.md's package table with
// the directories under internal/.
func packageTableProblems(doc string, dirs []string) []string {
	_, table, _ := strings.Cut(doc, "| Package | Job |\n")
	table, _, _ = strings.Cut(table, "\n\n")
	var rows []string
	for _, line := range strings.Split(table, "\n") {
		if m := packageRow.FindStringSubmatch(line); m != nil {
			rows = append(rows, m[1])
		}
	}
	var problems []string
	for _, dir := range dirs {
		if !slices.Contains(rows, filepath.Base(dir)) {
			problems = append(problems, "DESIGN.md §2 has no row for "+dir+".")
		}
	}
	for _, row := range rows {
		if !slices.Contains(dirs, "internal/"+row) {
			problems = append(problems, "DESIGN.md §2 names "+row+", which internal/ lacks.")
		}
	}
	return problems
}

func design(t *testing.T) []byte {
	doc, err := os.ReadFile("DESIGN.md")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
