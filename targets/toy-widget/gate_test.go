package main_test

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
	// config is the config's path, config.yaml when empty. Its plans are
	// in plans/ beside it.
	config string
}

const (
	gadgetConfig = "gadget/config.yaml"
	tenantConfig = "tenant/config.yaml"
	grantConfig  = "grant/config.yaml"
	leaseConfig  = "lease/config.yaml"
)

// configs are the toy's configs.
var configs = []string{"config.yaml", gadgetConfig, tenantConfig, grantConfig, leaseConfig}

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

// name is the plan's path from its config's directory, without .json.
func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

func planPath(config, plan string) string {
	return filepath.Join(filepath.Dir(config), "plans", plan+".json")
}

// plans lists the plans beside config, without .json.
func plans(config string) ([]string, error) {
	paths, err := filepath.Glob(planPath(config, "*"))
	for i, path := range paths {
		paths[i] = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return paths, err
}

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line. An empty
// finding means the bug must pass the plan.
var gate = []gateRow{
	{"b1", 1, "Converge: did not stay settled", 0, "changed writes", ""},
	{"b2", 2, "Converge: did not settle", 0, "changed writes", ""},
	{"b4", 4, "Converge: did not settle", 1, "is not ready", ""},
	{"b5", 5, "Converge: did not settle", 0, "is not ready", ""},
	{"b6", 6, "Converge: did not settle", 0, "changed writes", ""},
	{"b7", 7, "Converge: did not settle", 1, "is not ready", ""},
	{"b10", 10, "Converge: did not settle", 2, "is not ready", ""},
	{"b11", 11, "Converge: did not settle", 2, "is not ready", ""},
	{"b11-fault", 11, "Converge: did not settle", 1, "is not ready", ""},
	{"b12", 12, "Crash: the controller exited", 1, "exit status 2", ""},
	{"b12-fault", 12, "Crash: the controller exited", 1, "exit status 2", ""},
	{"b13", 13, "Converge: did not settle", 1, "is being deleted", ""},
	{"kill-delete", 13, "Converge: did not settle", 1, "is being deleted", ""},
	{"b15", 15, "Converge: did not settle", 0, "is not ready", ""},
	{"b16", 16, "Converge: did not settle", 0, "is not ready", ""},
	{"b3", 3, "Clean: left resources behind", 1, "widget-0 (rule 4, slot a)", ""},
	{"b9", 9, "Clean: left resources behind", 1, "widget-1 (rule 4, slot a)", ""},
	{"b8", 8, "Restore: did not restore damaged resources", 1, "widget-0", ""},
	{"b14", 14, "Restore: changed resources", 2, "widget-0", ""},
	{"b17", 17, "Honest: reported ready before seeing its dependency ready", 0, "gadgets.toy.reconciler-fuzzer", gadgetConfig},
	{"b18", 18, "Converge: did not settle", 1, "is not ready", gadgetConfig},
	{"b19", 19, "Honest: stayed ready after seeing its dependency not ready", 1, "seen not ready", gadgetConfig},
	{"b20", 20, "Honest: reported ready with no dependency", 0, "slot a", gadgetConfig},
	{"b21", 21, "Converge: did not settle", 0, "over the budget", gadgetConfig},
	{"b22", 22, "Clean: left resources behind", 1, "gadgets.toy.reconciler-fuzzer", gadgetConfig},
	{"b23", 23, "Converge: did not settle", 1, "is being deleted", gadgetConfig},
	{"foreground-slow-delete", 23, "Converge: did not settle", 1, "is being deleted", gadgetConfig},
	{"b24", 24, "Restore: did not restore damaged resources", 1, "gadgets.toy.reconciler-fuzzer", gadgetConfig},
	{"b25", 25, "Backoff: retried a failing request in a tight loop", 1, "patch gadgets.toy.reconciler-fuzzer", gadgetConfig},
	{"b26", 26, "Clean: left resources behind", 1, "clusterroles.rbac.authorization.k8s.io tenant-tenant (rule 3, slot a)", tenantConfig},
	{"b27", 27, "Clean: left resources behind", 1, ".grant (rule 2, slot a)", grantConfig},
	{"b28", 28, "Clean: left resources behind", 1, "-grant (rule 1, slot a)", grantConfig},
	{"b29", 29, "Converge: did not settle", 1, "is being deleted", tenantConfig},
	{"b29-background", 29, "", 0, "", tenantConfig},
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

func TestEveryPlanLoads(t *testing.T) {
	for _, config := range configs {
		names, err := plans(config)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s has plans %v, %v.", config, names, err)
		}
		for _, name := range names {
			if _, err := plan.Load(planPath(config, name)); err != nil {
				t.Errorf("%v", err)
			}
		}
	}
	for _, g := range gate {
		if _, err := os.Stat(g.planPath()); err != nil {
			t.Errorf("Gate row B%d: %v", g.bug, err)
		}
	}
}
