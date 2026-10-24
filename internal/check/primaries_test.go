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

var (
	unlistedDB = tt.Ref("toy/databases", "ns", "db")
	dbChild    = tt.Ref("configmaps", "ns", "db-db")
)

// withUnlistedOwner settles a step in which the harness creates Database db,
// which primaries does not list, and Cluster a, and the controller gives db
// a ConfigMap. Step 1 is the delete-all step.
func withUnlistedOwner() *tt.Script {
	r := fleetRun(1, clusters)
	r.Begin()
	r.Create(trace.Harness, unlistedDB, tt.CR("db", true))
	r.CreateCR("a", clusterA)
	r.Create(ctl, dbChild, ownedByDB(r, tt.Object("db-db")))
	return r.Ready(clusterA).EndFaults().Settle()
}

func ownedByDB(r *tt.Script, body map[string]any) map[string]any {
	return tt.OwnedBy(body, r.UID(unlistedDB), true)
}

// unlistedOwner is withUnlistedOwner, its delete-all step and its restart
// step. deleteDB has the delete-all step also delete db.
func unlistedOwner(deleteDB bool) *tt.Script {
	r := withUnlistedOwner()
	r.Begin().DeleteCR("a", clusterA)
	if deleteDB {
		r.Delete(trace.Harness, unlistedDB)
	}
	return r.EndFaults().Settle().Begin().Restart().EndFaults().Settle()
}

func TestCleanPassesAChildOfALiveUnlistedOwner(t *testing.T) {
	requireNone(t, judgeWith(t, unlistedOwner(false), check.Clean))
}

func TestCleanFailsAChildWhoseUnlistedOwnerIsGone(t *testing.T) {
	requireFinding(t, judgeWith(t, unlistedOwner(true), check.Clean), "Clean", 1, "left resources behind", "kube/configmaps",
		"configmaps ns/db-db (rule 4, slot a)")
}

func TestRestoreNamesTheUnlistedOwnerOfAChangedResource(t *testing.T) {
	r := withUnlistedOwner()
	r.Begin().Restart()
	r.Update(ctl, dbChild, ownedByDB(r, named("db-db", map[string]any{"data": "new"})))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps",
		"configmaps ns/db-db (rule 1, unlisted owner databases.toy ns/db): data")
}

func TestRestorePassesAResourceRecreatedBeforeItsDamageIsRecorded(t *testing.T) {
	r := withUnlistedOwner()
	r.Begin().Delete(trace.Harness, dbChild)
	r.Create(ctl, dbChild, ownedByDB(r, tt.Object("db-db")))
	r.Act("damage", "", &dbChild).Call(ctl, "get", dbChild, 200).EndFaults().Settle()
	requireNone(t, restore(t, r))
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

// databaseReady is a Database's ready, which phaseReady satisfies and
// fleetRun's ready does not.
const databaseReady = "self.status.phase == 'Ready'"

func phaseReady(name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"phase": "Ready"}}
}

func TestSettleJudgesEachCRByItsKindsReady(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Primaries[1].Ready = databaseReady
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	r.Ready(clusterA).Update(ctl, databaseB, phaseReady("b")).EndFaults()
	if unmet := settledAt(t, r, r.Now+5*s); len(unmet) != 0 {
		t.Errorf("unmet = %q, want none.", unmet)
	}
}

func TestSettleCountsACRHaltedByItsKindsHalted(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Primaries[1].Halted = "self.status.halted == true"
	r.Begin()
	r.CreateCR("a", clusterA)
	r.CreateCR("b", databaseB)
	halted := tt.CR("b", false)
	halted["status"].(map[string]any)["halted"] = true
	dropped := tt.Call(ctl, "update", databaseB, 200, "")
	dropped.Call.Injected = trace.DropResponse
	r.Ready(clusterA).Update(ctl, databaseB, halted).Add(dropped).EndFaults()
	if unmet := settledAt(t, r, r.Now+5*s); len(unmet) != 0 {
		t.Errorf("unmet = %q, want none.", unmet)
	}
}

func TestCleanAllowsWhatACRsKindRetains(t *testing.T) {
	r := fleetRun(2, clusters, databases)
	r.Spec.Primaries[1].Retain = "true"
	r.Begin()
	for _, cr := range []trace.Ref{clusterA, databaseB} {
		uid := r.CreateCR(cr.Name, cr)
		r.Create(ctl, tt.Ref("configmaps", "ns", cr.Name+"-0"), tt.OwnedBy(tt.Object(cr.Name+"-0"), uid, true))
		r.Ready(cr)
	}
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", clusterA).DeleteCR("b", databaseB).EndFaults().Settle()
	findings := judgeWith(t, r, check.Clean)
	requireFinding(t, findings, "Clean", 1, "left resources behind", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a)")
	if len(findings[0].Details) != 1 {
		t.Errorf("details = %q, want only Cluster a's ConfigMap.", findings[0].Details)
	}
}

func TestPeriodicExemptsARefreshOfAnyListedKind(t *testing.T) {
	databaseA := tt.Ref("toy/databases", "ns", "a")
	r := fleetRun(0, clusters, databases)
	r.Spec.Primaries[1].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"status.refreshTime"}}
	r.Begin()
	r.CreateCR("a", databaseA)
	r.Update(ctl, databaseA, refreshed(1, "t1", nil))
	r.EndFaults().Tick(2 * s)
	r.Update(ctl, databaseA, refreshed(2, "t2", nil))
	if unmet := changedWrites(t, r); len(unmet) != 0 {
		t.Errorf("unmet = %q, want none.", unmet)
	}
}

func TestHonestJudgesEachCRByItsKindsReady(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Primaries[1].Ready = databaseReady
	r.Spec.Models = []trace.Model{{Kind: gadgets, Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{}}}
	r.Begin()
	uid := r.CreateCR("b", databaseB)
	r.Create(ctl, gadgetA, tt.OwnedBy(tt.ReadyCondition("a", 1, false), uid, true))
	r.Update(ctl, databaseB, phaseReady("b"))
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "slot b")
}

func TestHonestTracksEachCRByItsKindsReady(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.Primaries[1].Ready = databaseReady
	r.Spec.Models = []trace.Model{{Kind: gadgets, Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Track: 500 * ms}}}
	r.Begin()
	uid := r.CreateCR("b", databaseB)
	gadget := func(ready bool) map[string]any { return tt.OwnedBy(tt.ReadyCondition("a", 1, ready), uid, true) }
	r.Create(ctl, gadgetA, gadget(false))
	r.Update(gadgetModel, gadgetA, gadget(true)).See(ctl, gadgetA)
	r.Update(ctl, databaseB, phaseReady("b"))
	r.Update(gadgetModel, gadgetA, gadget(false)).See(ctl, gadgetA)
	requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "kube/toy/gadgets", "slot b")
}

func TestBackoffRunBreaksOnANewGenerationOfACROfAnyListedKind(t *testing.T) {
	database := func(generation int) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": "b", "generation": generation}}
	}
	r := fleetRun(1, clusters, databases)
	r.Begin()
	r.Create(trace.Harness, databaseB, database(1))
	for range 8 {
		r.Call(ctl, "get", gadgetA, 500)
	}
	r.Update(trace.Harness, databaseB, database(2))
	for range 7 {
		r.Call(ctl, "get", gadgetA, 500)
	}
	requireNone(t, judgeWith(t, r, check.Backoff))
}

func TestPropertySeesAnEmptyKindThatAnyEntryOwns(t *testing.T) {
	r := fleetRun(0, clusters, databases)
	r.Spec.Primaries[1].Owns = []trace.Owns{{Kind: trace.Kind{Group: grants.Group, Version: "v1", Kind: "Grant", Resource: grants.Resource}, Name: "self.metadata.name"}}
	r.Spec.Properties = []trace.Property{{Name: "no-grants", Expr: "size(world.kube['grants.toy.reconciler-fuzzer']) == 0", When: "always"}}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Ready(clusterA).EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Property))
}

func TestRestoreIgnoresARefreshOfACRTheControllerCreated(t *testing.T) {
	databaseA := tt.Ref("toy/databases", "ns", "a")
	r := fleetRun(2, clusters, databases)
	r.Spec.Primaries[1].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"status.refreshTime"}}
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.Create(ctl, databaseA, tt.OwnedBy(refreshed(1, "t1", nil), uid, true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Restart()
	r.Update(ctl, databaseA, tt.OwnedBy(refreshed(2, "t2", nil), uid, true))
	r.EndFaults().Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreIgnoresPeriodicPathsWithTheOtherVolatileFields(t *testing.T) {
	databaseA := tt.Ref("toy/databases", "ns", "a")
	body := func(n int, at, tier, message string) map[string]any {
		b := refreshed(n, "t1", map[string]any{"message": message})
		metadata := b["metadata"].(map[string]any)
		if at != "" {
			metadata["annotations"] = map[string]any{"refreshed": at}
		}
		metadata["labels"] = map[string]any{"tier": tier}
		return b
	}
	for name, tc := range map[string]struct{ before, tier, want string }{
		"a refresh":                    {"t1", "x", ""},
		"a first refresh":              {"", "x", ""},
		"a refresh and a label change": {"t1", "y", "databases.toy ns/a (rule 1, slot a): metadata.labels"},
	} {
		r := fleetRun(2, clusters, databases)
		r.Spec.Primaries[1].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"metadata.annotations.refreshed"}}
		r.Spec.Models = []trace.Model{{Kind: databases, Ready: config.ReadyConditionCEL}}
		r.Begin()
		uid := r.CreateCR("a", clusterA)
		r.Create(ctl, databaseA, tt.OwnedBy(body(1, tc.before, "x", ""), uid, true))
		r.Ready(clusterA).EndFaults().Settle()
		r.Begin().Restart()
		r.Update(trace.ModelCaller("toy", "databases"), databaseA, tt.OwnedBy(body(2, tc.before, "x", "synced"), uid, true))
		r.Update(ctl, databaseA, tt.OwnedBy(body(3, "t2", tc.tier, "synced"), uid, true))
		r.EndFaults().Settle()
		findings := restore(t, r)
		switch {
		case tc.want == "" && len(findings) > 0:
			t.Errorf("%s: findings = %+v, want none", name, findings)
		case tc.want != "":
			requireFinding(t, findings, "Restore", 1, "changed resources", "kube/toy/databases", tc.want)
		}
	}
}

func TestCleanFailsALeakedChildOfAnOwnerTheControllerMade(t *testing.T) {
	registry, entry := tt.Ref("configmaps", "ns", "registry"), tt.Ref("configmaps", "ns", "a-entry")
	r := newRun(2)
	r.Begin()
	r.Create(ctl, registry, tt.Object("registry"))
	r.CreateCR("a", widgetA)
	r.Create(ctl, entry, tt.OwnedBy(tt.Object("a-entry"), r.UID(registry), true))
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
	requireFinding(t, judgeWith(t, r, check.Clean), "Clean", 1, "left resources behind", "kube/configmaps", "configmaps ns/a-entry (rule 4, slot a)")
}

func TestHonestNamesACRWithoutASlotByItsKindAndName(t *testing.T) {
	gadget := func(r *tt.Script, ready bool) map[string]any {
		return tt.OwnedBy(tt.ReadyCondition("a", 1, ready), r.UID(databaseB), true)
	}
	for summary, c := range map[string]struct {
		dependency   trace.Dependency
		script       func(r *tt.Script, readyB func())
		kind, detail string
	}{
		beforeSeeing: {trace.Dependency{}, func(r *tt.Script, readyB func()) {
			r.Create(ctl, gadgetA, gadget(r, false))
			readyB()
		}, "kube/toy/gadgets", "CR databases.toy ns/b: gadgets.toy ns/a"},
		noDependency: {trace.Dependency{Required: true}, func(_ *tt.Script, readyB func()) { readyB() },
			"kube/toy/databases", "CR databases.toy ns/b"},
		stayedReady: {trace.Dependency{Track: 500 * ms}, func(r *tt.Script, readyB func()) {
			r.Create(ctl, gadgetA, gadget(r, false))
			r.Update(gadgetModel, gadgetA, gadget(r, true)).See(ctl, gadgetA)
			readyB()
			r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
		}, "kube/toy/gadgets", "CR databases.toy ns/b: gadgets.toy ns/a seen not ready"},
	} {
		t.Run(summary, func(t *testing.T) {
			r := fleetRun(1, databases, clusters)
			r.Spec.Models = []trace.Model{{Kind: gadgets, Ready: config.ReadyConditionCEL, Dependency: &c.dependency}}
			r.Begin()
			uid := r.CreateCR("a", clusterA)
			r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", false), uid, true))
			c.script(r, func() { r.Update(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true)) })
			requireFinding(t, honest(t, r), "Honest", 0, summary, c.kind, c.detail)
		})
	}
}
