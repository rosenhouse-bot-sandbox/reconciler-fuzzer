//go:build envtest

package main_test

import (
	"fmt"
	"strings"
	"testing"
)

// gateRows pair each caught bug with a plan and the finding it gives.
var gateRows = []struct {
	plan    string
	bug     int
	finding string
	detail  string
}{
	{"resize", 72, "## Honest: reported ready before seeing its dependency ready\n\nStep 1,", "cluster-vol-0 seen not ready"},
}

func TestGateCatchesEachBug(t *testing.T) {
	for _, g := range gateRows {
		t.Run(fmt.Sprintf("%s catches B%d", g.plan, g.bug), func(t *testing.T) {
			t.Parallel()
			if report := runPlan(t, "config.yaml", g.plan, g.bug); !strings.Contains(report, g.finding) || !strings.Contains(report, g.detail) {
				t.Errorf("Want %q with %q. report.md:\n%s", g.finding, g.detail, report)
			}
		})
	}
}
