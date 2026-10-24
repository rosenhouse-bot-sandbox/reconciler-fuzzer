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
	r := tt.NewScript(clusterA, planSteps, 1*s)
	r.Spec.Primaries = nil
	for _, k := range kinds {
		r.Spec.Primaries = append(r.Spec.Primaries, trace.Primary{Kind: k, Ready: "has(self.status) && self.status.ready == true"})
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
	t.Skip("enabled by the judge package")
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
	t.Skip("enabled by the judge package")
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
	t.Skip("enabled by the judge package")
	requireNone(t, judgeWith(t, unlistedOwner(false), check.Clean))
}

func TestCleanFailsAChildWhoseUnlistedOwnerIsGone(t *testing.T) {
	requireFinding(t, judgeWith(t, unlistedOwner(true), check.Clean), "Clean", 1, "left resources behind", "kube/configmaps",
		"configmaps ns/db-db (rule 4, slot a)")
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
	t.Skip("enabled by the judge package")
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
	t.Skip("enabled by the judge package")
	r := fleetRun(1, clusters, databases)
	r.Spec.Properties = []trace.Property{{Name: "both", Expr: "crs.size() == 2", When: "step"}}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	r.Ready(clusterA).Ready(databaseB).EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Property))
}

func TestHonestJudgesACROfEveryListedKind(t *testing.T) {
	t.Skip("enabled by the judge package")
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

func TestConvergeNotesWhereTheStepsRecordEnds(t *testing.T) {
	t.Skip("enabled by the judge package")
	confirmed := createStep()
	confirmed.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed)).
		At(9 * s).Add(tt.Call(ctl, "get", widgetA, 200, ""))
	longer := spec
	longer.Stable = 3 * s
	requireNote(t, confirmed, longer, "Step 0 is not judged: it settled at 4.5s, but its confirm window ends at 5.5s, before 7.5s.")

	cut := createStep()
	cut.At(10 * s).Add(tt.Phase(0, trace.TimedOut)).
		At(10 * s).Add(tt.Phase(1, trace.FaultPeriod))
	slow := spec
	slow.Stable = 8 * s
	requireNote(t, cut, slow, "Step 0 is not judged: it settled at 9.5s, but the next step starts at 10s, before 17.5s.")
}
