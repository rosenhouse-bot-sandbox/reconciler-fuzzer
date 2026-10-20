package main_test

import (
	"fmt"
	"strings"
)

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
}

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line.
var gate = []gateRow{
	{"b1", 1, "Converge: did not stay settled", 0, "changed writes"},
	{"b2", 2, "Converge: did not settle", 0, "changed writes"},
	{"b4", 4, "Converge: did not settle", 1, "is not ready"},
	{"b5", 5, "Converge: did not settle", 0, "is not ready"},
	{"b6", 6, "Converge: did not settle", 0, "changed writes"},
	{"b7", 7, "Converge: did not settle", 1, "is not ready"},
	{"b10", 10, "Converge: did not settle", 2, "is not ready"},
	{"b11", 11, "Converge: did not settle", 2, "is not ready"},
	{"b11-fault", 11, "Converge: did not settle", 1, "is not ready"},
	{"b12", 12, "Crash: the controller exited", 1, "exit status 2"},
	{"b12-fault", 12, "Crash: the controller exited", 1, "exit status 2"},
	{"b13", 13, "Converge: did not settle", 1, "is being deleted"},
	{"b15", 15, "Converge: did not settle", 0, "is not ready"},
	{"b16", 16, "Converge: did not settle", 0, "is not ready"},
	{"b3", 3, "Clean: left resources behind", 1, "widget-0 (rule 4, slot a)"},
	{"b9", 9, "Clean: left resources behind", 1, "widget-1 (rule 4, slot a)"},
	{"b8", 8, "Restore: did not restore damaged resources", 1, "widget-0"},
	{"b14", 14, "Restore: changed resources", 2, "widget-0"},
}

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, that says detail.
func hasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		if strings.HasPrefix(title, heading) && strings.HasPrefix(body, fmt.Sprintf("Step %d,", step)) && strings.Contains(body, detail) {
			return true
		}
	}
	return false
}
