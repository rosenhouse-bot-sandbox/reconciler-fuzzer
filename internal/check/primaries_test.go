package check_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	clusters  = trace.Kind{Group: "toy", Version: "v1", Kind: "Cluster", Resource: "clusters"}
	databases = trace.Kind{Group: "toy", Version: "v1", Kind: "Database", Resource: "databases"}
	gadgets   = trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}
	clusterA  = tt.Ref("toy/clusters", "ns", "a")
	databaseB = tt.Ref("toy/databases", "ns", "b")
	databaseC = tt.Ref("toy/databases", "ns", "c")
)

// fleetRun starts a run whose primaries list kinds, each ready as newRun's
// CRs are.
func fleetRun(planSteps int, kinds ...trace.Kind) *tt.Script {
	r := tt.NewScript(kinds[0], planSteps, 1*s)
	for _, k := range kinds[1:] {
		r.Spec.Primaries = append(r.Spec.Primaries, trace.Primary{Kind: k, Ready: r.Spec.Primaries[0].Ready})
	}
	return r
}

func settledAt(t *testing.T, r *tt.Script, at time.Duration) []string {
	t.Helper()
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	return check.Settled(ix, at, spec)
}

func TestSettleWaitsOnACROfEveryListedKind(t *testing.T) {
	for name, create := range map[string]func(r *tt.Script){
		"the harness's":    func(r *tt.Script) { r.CreateCR("b", databaseB) },
		"the controller's": func(r *tt.Script) { r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", false), r.UID(clusterA), true)) },
	} {
		r := fleetRun(1, clusters, databases)
		r.Begin()
		r.CreateCR("a", clusterA)
		create(r)
		r.Ready(clusterA).EndFaults()
		if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, []string{"databases.toy ns/b is not ready"}) {
			t.Errorf("%s Database: unmet = %q, want only that it is not ready.", name, unmet)
		}
	}
}

func TestClause7BudgetsCountCRsOfEveryListedKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	r.Ready(clusterA).Ready(databaseB).EndFaults().Tick(2 * s)
	for range 8 {
		r.NoOp(ctl, clusterA)
	}
	if unmet := settledAt(t, r, r.Now); len(unmet) != 0 {
		t.Errorf("8 writes with 2 CRs: unmet = %q, want none.", unmet)
	}
	r.NoOp(ctl, clusterA)
	if unmet := settledAt(t, r, r.Now); len(unmet) != 1 {
		t.Errorf("9 writes with 2 CRs: unmet = %q, want the write budget.", unmet)
	}
}

// unlistedOwner settles a step in which the harness creates Database db,
// which primaries does not list, and Cluster a, and the controller gives db
// a ConfigMap. The next step deletes Cluster a.
func unlistedOwner(deleteDB bool) *tt.Script {
	db := tt.Ref("toy/databases", "ns", "db")
	r := fleetRun(2, clusters)
	r.Begin()
	uid := r.Create(trace.Harness, db, tt.CR("db", true))
	r.CreateCR("a", clusterA)
	r.Create(ctl, tt.Ref("configmaps", "ns", "db-db"), tt.OwnedBy(tt.Object("db-db"), uid, true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().DeleteCR("a", clusterA)
	if deleteDB {
		r.Delete(trace.Harness, db)
	}
	return r.EndFaults().Settle()
}

func TestCleanPassesAChildOfALiveUnlistedOwner(t *testing.T) {
	requireNone(t, judgeWith(t, unlistedOwner(false), check.Clean))
}

func TestCleanFailsAChildWhoseUnlistedOwnerIsGone(t *testing.T) {
	requireFinding(t, judgeWith(t, unlistedOwner(true), check.Clean), "Clean", 1, "left resources behind", "kube/configmaps",
		"configmaps ns/db-db (rule 4, slot a)")
}

func TestRestoreNamesTheUnlistedOwnerOfAChangedResource(t *testing.T) {
	db, child := tt.Ref("toy/databases", "ns", "db"), tt.Ref("configmaps", "ns", "db-db")
	r := fleetRun(2, clusters)
	r.Begin()
	uid := r.Create(trace.Harness, db, tt.CR("db", true))
	r.CreateCR("a", clusterA)
	r.Create(ctl, child, tt.OwnedBy(tt.Object("db-db"), uid, true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Restart()
	r.Update(ctl, child, tt.OwnedBy(named("db-db", map[string]any{"data": "new"}), uid, true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps",
		"configmaps ns/db-db (rule 1, unlisted owner databases.toy ns/db): data")
}

func TestCleanFailsACRTheControllerCreatedThatOutlivesItsOwner(t *testing.T) {
	r := fleetRun(2, clusters, databases)
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), r.UID(clusterA), true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().DeleteCR("a", clusterA).EndFaults().Settle()
	requireFinding(t, judgeWith(t, r, check.Clean), "Clean", 1, "left resources behind", "kube/toy/databases",
		"databases.toy ns/b (rule 1, slot a)")
}

func TestUniqueGroupsByACROfAnyListedKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Begin()
	r.CreateCR("a", clusterA)
	for _, db := range []trace.Ref{databaseB, databaseC} {
		uid := r.CreateCR(db.Name, db)
		r.Create(ctl, tt.Ref("configmaps", "ns", db.Name+"-x1"), tt.OwnedBy(tt.Generated("db-", db.Name+"-x1"), uid, true))
		r.Ready(db)
	}
	r.Ready(clusterA).EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Unique))
}

func TestPropertySeesCRsOfEveryListedKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Properties = []trace.Property{{Name: "both", Expr: "crs.size() == 2", When: "step"}}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	r.Ready(clusterA).Ready(databaseB).EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Property))
}

func TestHonestJudgesACROfEveryListedKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Models = []trace.Model{{Kind: gadgets, Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{}}}
	r.Begin()
	uid := r.CreateCR("b", databaseB)
	r.Create(ctl, gadgetA, tt.OwnedBy(tt.ReadyCondition("a", 1, false), uid, true))
	r.Ready(databaseB)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "slot b", "gadgets.toy ns/a")
}

func TestHonestRequiresADependencyOnlyOfTheFirstListedKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Models = []trace.Model{{Kind: gadgets, Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Required: true}}}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	r.Ready(clusterA).Ready(databaseB)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/clusters", "slot a")
}
