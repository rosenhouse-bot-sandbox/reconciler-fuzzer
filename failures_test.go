package reconcilerfuzzer_test

import (
	"os"
	"regexp"
	"testing"
)

func TestFailuresHasASectionForEveryCheck(t *testing.T) {
	doc, err := os.ReadFile("docs/failures.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range judgeChecks(t) {
		if !regexp.MustCompile(`(?m)^### ` + check + `\b`).Match(doc) {
			t.Errorf("docs/failures.md has no section that starts with ### %s.", check)
		}
	}
}
