//go:build envtest

package main_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// upgradeRow runs plan under bug and wants report.md to hold finding, or
// no findings when finding is empty.
type upgradeRow struct {
	config, plan string
	bug          int
	finding      string
}

// upgradeScript upgrades through upgrade/toy-fleet.sh, whose first restart
// starts --version=2.
var upgradeScript = []upgradeRow{
	{"upgrade/config.yaml", "upgrade/plans/delete.json", 0, ""},
	{"upgrade/config.yaml", "upgrade/plans/delete.json", 40, "## Converge: did not settle (clause 2)"},
	{"upgrade/config.yaml", "upgrade/plans/update.json", 0, ""},
	{"upgrade/config.yaml", "upgrade/plans/update.json", 41, "## Restore: replaced resources"},
	{"upgrade/config.yaml", "upgrade/plans/mid-provisioning.json", 0, ""},
	{"upgrade/config.yaml", "upgrade/plans/mid-provisioning.json", 42, "## Converge: did not settle (clause 1)"},
	{"upgrade/properties.yaml", "upgrade/plans/upgrade-and-update.json", 0, ""},
	{"upgrade/properties.yaml", "upgrade/plans/upgrade-and-update.json", 41, "## Property: property volumes-go-first does not hold"},
}

// upgradeAction upgrades with a restart that names the new args, on the
// plain config.yaml.
var upgradeAction = []upgradeRow{
	{"config.yaml", "upgrade/proposed/delete.json", 0, ""},
	{"config.yaml", "upgrade/proposed/delete.json", 40, "## Converge: did not settle (clause 2)"},
	{"config.yaml", "upgrade/proposed/update.json", 41, "## Restore: replaced resources"},
	{"config.yaml", "upgrade/proposed/mid-provisioning.json", 42, "## Converge: did not settle (clause 1)"},
}

func TestUpgradeThroughALaunchScript(t *testing.T) { testUpgrades(t, upgradeScript) }

func TestUpgradeThroughARestartAction(t *testing.T) { testUpgrades(t, upgradeAction) }

func testUpgrades(t *testing.T, rows []upgradeRow) {
	for _, r := range rows {
		t.Run(fmt.Sprintf("%s %s B%d", r.config, r.plan, r.bug), func(t *testing.T) {
			t.Parallel()
			report, _ := runUpgrade(t, r.config, r.plan, r.bug)
			if r.finding == "" && report != "No findings.\n" {
				t.Errorf("Want no findings. report.md:\n%s", report)
			}
			if r.finding != "" && !strings.Contains(report, r.finding+"\n") {
				t.Errorf("Want %q. report.md:\n%s", r.finding, report)
			}
		})
	}
}

// TestARestartActionsArgsOutliveIt wants the controller that the appended
// restart starts to keep the upgrade's args.
func TestARestartActionsArgsOutliveIt(t *testing.T) {
	_, out := runUpgrade(t, "config.yaml", "upgrade/proposed/delete.json", 0)
	log, err := os.ReadFile(filepath.Join(out, "controller.log"))
	if err != nil {
		t.Fatal(err)
	}
	starts := strings.Count(string(log), `"starting"`)
	if v2 := strings.Count(string(log), `"version": 2}`); starts != 3 || v2 != 2 {
		t.Errorf("The controller started %d times, %d of them as version 2; want 3 and 2.", starts, v2)
	}
}

// TestARestartActionInstallsNewCRDs wants a restart's crds applied before
// the controller starts, so that the server defaults the new field
// spec.tier on the v1 Cluster.
func TestARestartActionInstallsNewCRDs(t *testing.T) {
	_, out := runUpgrade(t, "config.yaml", "upgrade/proposed/crds.json", 0)
	f, err := os.Open(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	lines.Buffer(nil, 1<<24)
	for lines.Scan() {
		var e struct {
			Version *struct {
				Ref  struct{ Resource string }
				Body struct{ Spec map[string]any }
			}
		}
		if err := json.Unmarshal(lines.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Version != nil && e.Version.Ref.Resource == "clusters" && e.Version.Body.Spec["tier"] == "standard" {
			return
		}
	}
	t.Errorf("No Cluster version has spec.tier standard. The run is in %s.", out)
}

// runUpgrade runs a plan under bug into a directory it removes only if the
// test passes, and returns report.md and the directory.
func runUpgrade(t *testing.T, config, plan string, bug int) (string, string) {
	t.Helper()
	out, err := os.MkdirTemp("", "rf-upgrade-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("The run is in %s.", out)
		} else {
			os.RemoveAll(out)
		}
	})
	o := run.Options{ConfigPath: config, PlanPath: plan, Out: out, LaunchArgs: []string{fmt.Sprintf("--bug=%d", bug)}}
	if _, err := run.Run(t.Context(), o); err != nil {
		t.Fatalf("%v; the run is in %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(out, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(text), out
}
