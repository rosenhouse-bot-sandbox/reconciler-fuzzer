//go:build envtest

package main_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const uninstallConfig = "uninstall/config.yaml"

// uninstallRows say, for each plan and bug, the finding report.md must
// hold: its heading, its step and a detail line. An empty finding means no
// findings and no notes.
var uninstallRows = []struct {
	config, plan string
	bug          int
	finding      string
	step         int
	detail       string
}{
	{uninstallConfig, "crd-first", 52, "Converge: did not settle (clause 2)", 2, "cluster is being deleted"},
	{databaseConfig, "restart-cluster-delete", 50, "Converge: did not settle (clause 1)", 1, "db is not ready"},
	// The rows below need a stop action, a final Clean after it, CRDs from
	// crds: that changeFixture can delete, and a check of finalizer order.
	{uninstallConfig, "crd-first-stop", 0, "", 0, ""},
	{uninstallConfig, "uninstall", 0, "", 0, ""},
	{uninstallConfig, "stop-after-delete", 0, "", 0, ""},
	{uninstallConfig, "stop-after-delete", 51, "Clean: left resources behind", 1, "storage.thirdparty volumes"},
	{uninstallConfig, "stop-first", 0, "Converge: did not settle (clause 2)", 2, "cluster is being deleted"},
	{"config.yaml", "restart-slow-delete", 50, "Clean: released its finalizer before its resources were gone", 1, "storage.thirdparty volumes"},
}

func TestUninstall(t *testing.T) {
	for _, r := range uninstallRows {
		t.Run(fmt.Sprintf("%s B%d", filepath.Join(filepath.Dir(r.config), r.plan), r.bug), func(t *testing.T) {
			t.Parallel()
			report := runPlan(t, r.config, r.plan, r.bug)
			if r.finding == "" && report != "No findings.\n" {
				t.Errorf("Want no findings and no notes. report.md:\n%s", report)
			}
			if r.finding != "" && !hasFinding(report, r.finding, r.step, r.detail) {
				t.Errorf("Want %q at step %d saying %q. report.md:\n%s", r.finding, r.step, r.detail, report)
			}
		})
	}
}

// hasFinding says whether report holds a finding whose heading starts with
// heading, at step, that says detail.
func hasFinding(report, heading string, step int, detail string) bool {
	for _, section := range strings.Split(report, "\n## ")[1:] {
		title, body, _ := strings.Cut(section, "\n\n")
		at := fmt.Sprintf("Step %d", step)
		if strings.HasPrefix(title, heading) && (strings.HasPrefix(body, at+",") || strings.HasPrefix(body, at+" (")) && strings.Contains(body, detail) {
			return true
		}
	}
	return false
}
