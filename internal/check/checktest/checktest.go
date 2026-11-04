// Package checktest reads a run's report.md and notes in tests.
package checktest

import (
	"fmt"
	"slices"
	"strings"
)

// HasFinding says whether report holds a finding whose heading starts with
// heading, at step, whose details say detail. The header may name the step,
// as in "Step 2 (delete all),".
func HasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		header, details, _ := strings.Cut(body, "\n\n")
		details, _, _ = strings.Cut(details, "\n# ")
		at := fmt.Sprintf("Step %d", step)
		if strings.HasPrefix(title, heading) && (strings.HasPrefix(header, at+",") || strings.HasPrefix(header, at+" (")) && strings.Contains(details, detail) {
			return true
		}
	}
	return false
}

// NotesMatch drops the notes that say one of may, and says whether each
// other note says the string at its index in want.
func NotesMatch(notes, want []string, may ...string) bool {
	notes = slices.DeleteFunc(slices.Clone(notes), func(note string) bool {
		return slices.ContainsFunc(may, func(m string) bool { return strings.Contains(note, m) })
	})
	return slices.EqualFunc(notes, want, strings.Contains)
}

// CheckOf is the check a finding heading names.
func CheckOf(heading string) string {
	check, _, _ := strings.Cut(heading, ":")
	return check
}

// ChecksOf lists the check of each finding in report.
func ChecksOf(report string) []string {
	var checks []string
	for _, section := range strings.Split(report, "\n## ")[1:] {
		checks = append(checks, CheckOf(section))
	}
	return checks
}
