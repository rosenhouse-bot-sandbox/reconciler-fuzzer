package main_test

import (
	"cmp"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	clusterConfig           = "cluster/config.yaml"
	databaseConfig          = "database/config.yaml"
	orderConfig             = "order/config.yaml"
	protectConfig           = "protect/config.yaml"
	secretConfig            = "secret/config.yaml"
	storageClassConfig      = "storageclass/config.yaml"
	uninstallConfig         = "uninstall/config.yaml"
	upgradeConfig           = "upgrade/config.yaml"
	upgradePropertiesConfig = "upgrade/properties/config.yaml"
)

// configs are the configs whose every plan B0 passes with no notes.
var configs = []string{"config.yaml", databaseConfig, orderConfig, protectConfig, secretConfig, storageClassConfig, upgradeConfig}

type gateRow struct {
	plan    string
	bug     int
	finding string
	step    int
	detail  string
	// config is the config's path, config.yaml when empty. Its plans are
	// in plans/ beside it.
	config string
	// notes are what the run's notes say, in order. Without them the run
	// gives no note.
	notes []string
}

func (g gateRow) configPath() string { return cmp.Or(g.config, "config.yaml") }

// name is the plan's path from its config's directory, without .json.
func (g gateRow) name() string { return filepath.Join(filepath.Dir(g.configPath()), g.plan) }

func (g gateRow) planPath() string { return planPath(g.configPath(), g.plan) }

// gate lists, for each plan, the bug it must catch and the finding
// report.md must hold: its heading, its step and a detail line. An empty
// finding means the bug must pass the plan.
var gate = []gateRow{
	// gc deletes B115's Volumes, which carry ownerReferences, so only the
	// property judges their order.
	{plan: "slow-delete", bug: 115, finding: "Property: property volumes-deleted-before-their-cluster does not hold", step: 1, detail: "is false", config: orderConfig},
	{plan: "slow-delete", bug: 116, finding: "Finalizers: removed another caller's finalizer", step: 1, detail: "cluster-vol-0: reconciler-fuzzer.dev/slow-delete, added by model:volumes.storage.thirdparty, removed by update"},
	{plan: "b0", bug: 116, finding: "Finalizers: removed another caller's finalizer", step: 2, detail: "cluster-vol-0: storage.thirdparty/protect, added by model:volumes.storage.thirdparty, removed by update", config: protectConfig},
	{plan: "delete-cluster-first", bug: 120, finding: "Ordered: released an object that another names", step: 1, detail: "/cluster: slot b names it"},
	{plan: "blocked-delete", bug: 120, finding: "Ordered: released an object that another names", step: 1, detail: "/cluster: slot b names it"},
	// database/config.yaml has no reference to the Cluster, so Converge
	// catches B120 there.
	{plan: "cluster-delete", bug: 120, finding: "Converge: did not settle (clause 1)", step: 1, detail: "/db is not ready", config: databaseConfig},
	{plan: "delete-database-first", bug: 121, finding: "Ordered: released an object that another names", step: 1, detail: "/db: slot c names it"},
	{plan: "user-first", bug: 122, finding: "Converge: did not settle (clause 1)", step: 0, detail: "/alice is not ready", notes: []string{"primaries[2].ready held for no CR version"}},
	{plan: "create-together", bug: 123, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "slot b: clusters.toy.reconciler-fuzzer "},
	{plan: "secret-late", bug: 124, finding: "Honest: reported ready with no dependency", step: 0, detail: "slot b: secrets "},
	{plan: "secret-recreate", bug: 125, finding: "Honest: stayed ready after seeing its dependency not ready", step: 1, detail: "slot b: secrets "},
	// The restarted controller's watch shows the Secret missing.
	{plan: "secret-deleted-while-down", bug: 125, finding: "Honest: stayed ready after seeing its dependency not ready", step: 1, detail: "slot b: secrets "},
	{plan: "stuck-delete", bug: 117, finding: "Converge: did not settle (clause 2)", step: 1, detail: "cluster is being deleted"},
	// gc removes the orphan finalizer that B118's deletes add.
	{plan: "slow-delete", bug: 118},
	{plan: "slow-delete", bug: 119, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: "-cluster (rule 3, slot a)", config: storageClassConfig},
	{plan: "secret-first", bug: 126, finding: "Ordered: released an object that another names", step: 1, detail: "/admin-password: slot a names it", config: secretConfig},
	{plan: "b131", bug: 131, finding: "Honest: reported ready before seeing its dependency ready", step: 0, detail: "/cluster-vol-1 seen not ready"},
	{plan: "b131", bug: 131, finding: "Honest: reported ready before seeing its dependency ready", step: 1, detail: "/cluster-vol-2 seen not ready"},
	// The kill follows the first process's second Volume create. The
	// restarted process makes a second set, and the update deletes only one
	// of its own.
	{plan: "b132", bug: 132, finding: "Unique: duplicate resources", step: 0, detail: `role "cluster-vol-1": `},
	{plan: "b132", bug: 132, finding: "Unique: duplicate resources", step: 1, detail: `role "cluster-vol-0": `},
	{plan: "b133", bug: 133, finding: "Converge: did not settle (clause 1)", step: 0, detail: "cluster is not ready", notes: []string{"primaries[0].ready held for no CR version"}},
	// The restart comes 500 ms after the create, long after the first
	// process makes its Volumes. The second makes its own, and the update
	// deletes only one of them.
	{plan: "b135", bug: 135, finding: "Unique: duplicate resources", step: 0, detail: `role "cluster-vol-1": `},
	{plan: "b135", bug: 135, finding: "Unique: duplicate resources", step: 1, detail: `role "cluster-vol-0": `},
	// The restart has a step of its own, which changes no CR. Restore flags
	// the second process's Volumes whatever their role, so only the Unique
	// row below needs the config's role.
	{plan: "b135-restart", bug: 135, finding: "Restore: replaced resources", step: 1, detail: "cluster-vol-0-"},
	{plan: "b135-restart", bug: 135, finding: "Unique: duplicate resources", step: 1, detail: `role "cluster-vol-0": `},
	// Each plan under upgrade/ restarts the controller as version 2.
	{plan: "delete", bug: 140, finding: "Converge: did not settle (clause 2)", step: 2, detail: "cluster is being deleted", config: upgradeConfig},
	{plan: "update", bug: 141, finding: "Restore: replaced resources", step: 1, detail: "/cluster-data-0 (rule 1, slot a)", config: upgradeConfig},
	{plan: "mid-provisioning", bug: 142, finding: "Converge: did not settle (clause 1)", step: 0, detail: "cluster is not ready", config: upgradeConfig, notes: []string{"primary.ready held for no CR version"}},
	{plan: "upgrade-and-update", bug: 141, finding: "Property: property volumes-go-first does not hold", step: 2, detail: "is false", config: upgradePropertiesConfig},
	// The slowDelete keeps each Volume being deleted for 2 s before its
	// Cluster is gone, so this row does not race the two kinds' watches.
	{plan: "upgrade-and-update", config: upgradePropertiesConfig},
	{plan: "restart-cluster-delete", bug: 150, finding: "Converge: did not settle (clause 1)", step: 1, detail: "db is not ready", config: databaseConfig},
	// B150 strips the Cluster's finalizer at the restart, while slowDelete
	// holds its Volumes.
	{plan: "restart-slow-delete", bug: 150, finding: "Property: property volumes-deleted-before-their-cluster does not hold", step: 1, detail: "is false", config: orderConfig},
	// The kill comes after the update that releases the Cluster, so B151
	// never strips its Volumes' finalizers.
	{plan: "kill-recreate", bug: 151, finding: "Converge: did not settle (clause 1)", step: 1, detail: "cluster is not ready", config: uninstallConfig},
	// The update error keeps B151's Volumes held until the stop.
	{plan: "stop-after-delete", bug: 151, finding: "Clean: left resources behind", step: 1, detail: "/cluster-vol-0 (rule 1, slot a)", config: uninstallConfig},
	{plan: "crd-first-stop", bug: 152, finding: "Converge: did not settle (clause 2)", step: 2, detail: "cluster is being deleted", config: uninstallConfig},
	// The kill follows B160's release of the Cluster and comes before its
	// Volume deletes.
	{plan: "kill-finalize", bug: 160, finding: "Clean: left resources behind", step: 1, detail: "/cluster-vol-0 (rule 2, slot a)"},
	{plan: "kill-finalize", bug: 160, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: "/cluster-vol-0 (rule 2, slot a)"},
	// Without a kill, B160 deletes the Volumes after the release, so only
	// Teardown catches it.
	{plan: "slow-delete", bug: 160, finding: "Teardown: released a CR before deleting its resources", step: 1, detail: "/cluster-vol-0 (rule 2, slot a)"},
	// The restart or kill comes while slowDelete holds the Volumes.
	{plan: "restart-deleting", bug: 161, finding: "Converge: did not settle (clause 2)", step: 1, detail: "cluster is being deleted"},
	{plan: "foreground-kill", bug: 161, finding: "Converge: did not settle (clause 2)", step: 1, detail: "cluster is being deleted"},
	{plan: "slow-delete", bug: 162, finding: "Backoff: repeated a no-op write in a tight loop", step: 1, detail: "delete volumes.storage.thirdparty "},
	// The resize raises each Volume's generation past its Ready condition's.
	{plan: "resize", bug: 172, finding: "Honest: reported ready before seeing its dependency ready", step: 1, detail: "/cluster-vol-0 seen not ready"},
	{plan: "crd-first-stop", config: uninstallConfig},
	{plan: "kill-recreate", config: uninstallConfig},
	// The create waits for the name, the slowDelete for the create, and the
	// plan ends at its create.
	{plan: "recreate", config: uninstallConfig},
	// B0 updates no Volume while it deletes them.
	{plan: "stop-after-delete", config: uninstallConfig, notes: []string{"fault 1 (error(500) on verb=update group=storage.thirdparty resource=volumes) picked no request"}},
	{plan: "stop-first", config: uninstallConfig, notes: []string{"cluster is not judged: the stop did not wait for it"}},
}

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

// byRun groups the gate's rows by config, plan and bug, in order.
func byRun(rows []gateRow) [][]gateRow {
	var runs [][]gateRow
	for _, g := range rows {
		n := slices.IndexFunc(runs, func(run []gateRow) bool {
			return run[0].configPath() == g.configPath() && run[0].plan == g.plan && run[0].bug == g.bug
		})
		if n < 0 {
			runs = append(runs, nil)
			n = len(runs) - 1
		}
		runs[n] = append(runs[n], g)
	}
	return runs
}

// A config outside configs has no run of every plan under B0, so each of
// its plans needs a gate row.
func TestEveryPlanOfAGateConfigHasARow(t *testing.T) {
	for _, g := range gate {
		config := g.configPath()
		if slices.Contains(configs, config) {
			continue
		}
		names, err := plans(config)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if !slices.ContainsFunc(gate, func(r gateRow) bool { return r.configPath() == config && r.plan == name }) {
				t.Errorf("%s has no gate row.", filepath.Join(filepath.Dir(config), name))
			}
		}
	}
}

func TestByRunGroupsRowsOfOneRun(t *testing.T) {
	rows := []gateRow{{plan: "a", bug: 1}, {plan: "b", bug: 1}, {plan: "a", bug: 1, step: 2}, {plan: "a", bug: 0}, {plan: "a", bug: 1, config: "x.yaml"}}
	if got := byRun(rows); len(got) != 4 || len(got[0]) != 2 || got[0][1].step != 2 {
		t.Errorf("byRun = %+v", got)
	}
}

func TestEveryPlanLoadsAndCreatesOnlyListedKinds(t *testing.T) {
	type entry struct{ config, plan string }
	var entries []entry
	for _, path := range configs {
		names, err := plans(path)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s has plans %v, %v.", path, names, err)
		}
		for _, name := range names {
			entries = append(entries, entry{path, name})
		}
	}
	for _, g := range gate {
		entries = append(entries, entry{g.configPath(), g.plan})
	}
	for _, e := range entries {
		c, err := config.Load(e.config)
		if err != nil {
			t.Fatal(err)
		}
		p, err := plan.Load(planPath(e.config, e.plan))
		if err != nil {
			t.Error(err)
			continue
		}
		if err := p.ValidateKinds(c.ListedKinds()); err != nil {
			t.Errorf("%s under %s: %v", e.plan, e.config, err)
		}
	}
}
