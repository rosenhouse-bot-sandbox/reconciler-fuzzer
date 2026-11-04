package checktest_test

import (
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check/checktest"
)

func TestHasFindingMatchesTheStep(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2 (delete all), at 1s.\n\n```\nbuckets b\n```\n\n## Restore: changed resources\n\nStep 4, at 2s.\n\n```\nbuckets c\n```\n"
	if !checktest.HasFinding(report, "Clean", 2, "buckets b") || !checktest.HasFinding(report, "Restore", 4, "buckets c") {
		t.Error("missed a step")
	}
	for _, step := range []int{1, 3, 12, 40} {
		if checktest.HasFinding(report, "Clean", step, "buckets b") || checktest.HasFinding(report, "Restore", step, "buckets c") {
			t.Errorf("matched step %d", step)
		}
	}
}

func TestHasFindingMatchesOnlyTheDetails(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 1, at 1s.\nActions: damage buckets b-0.\n\n```\nbuckets b-1\n```\n\n# Next steps\n\nReplay buckets b-2.\n"
	if !checktest.HasFinding(report, "Clean", 1, "buckets b-1") {
		t.Error("missed the detail line")
	}
	if checktest.HasFinding(report, "Restore", 1, "buckets b-1") {
		t.Error("matched another heading")
	}
	for _, elsewhere := range []string{"buckets b-0", "buckets b-2"} {
		if checktest.HasFinding(report, "Clean", 1, elsewhere) {
			t.Errorf("matched %q outside the details", elsewhere)
		}
	}
}

func TestNotesMatch(t *testing.T) {
	for _, c := range []struct {
		notes, want, may []string
		match            bool
	}{
		{nil, nil, nil, true},
		{[]string{"a note"}, nil, nil, false},
		{nil, []string{"note"}, nil, false},
		{[]string{"a note"}, []string{"note"}, nil, true},
		{[]string{"a note"}, []string{"other"}, nil, false},
		{[]string{"first", "second"}, []string{"first", "second"}, nil, true},
		{[]string{"first", "second"}, []string{"second", "first"}, nil, false},
		{[]string{"first", "second"}, []string{"first"}, nil, false},
		{nil, nil, []string{"race"}, true},
		{[]string{"first", "a race", "second"}, []string{"first", "second"}, []string{"race"}, true},
		{[]string{"a race", "a lap"}, nil, []string{"race", "lap"}, true},
		{[]string{"a note"}, nil, []string{"race"}, false},
	} {
		if got := checktest.NotesMatch(c.notes, c.want, c.may...); got != c.match {
			t.Errorf("NotesMatch(%q, %q, %q) = %v", c.notes, c.want, c.may, got)
		}
	}
}

func TestChecksOfListsEachFindingsCheck(t *testing.T) {
	const report = "# Findings\n\n## Clean: left resources behind\n\nStep 2.\n\n## Converge: did not settle (clause 2)\n\nStep 0.\n"
	if got := checktest.ChecksOf(report); !slices.Equal(got, []string{"Clean", "Converge"}) {
		t.Errorf("ChecksOf = %q", got)
	}
}
