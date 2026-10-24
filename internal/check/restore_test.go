package check_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	childA = tt.Ref("configmaps", "ns", "a-0")
	childB = tt.Ref("configmaps", "ns", "b-0")
)

// twoCRs settles a step that creates slots a and b, each with its child.
func twoCRs() *tt.Script {
	r := newRun(3)
	r.Begin()
	withChild(r, "a", true)
	withChild(r, "b", true)
	return r.EndFaults().Settle()
}

// damageA starts a step that deletes slot a's child behind the
// controller's back.
func damageA(r *tt.Script) *tt.Script {
	return r.Begin().Delete(trace.Harness, childA).Act("damage", "a", &childA).EndFaults()
}

func restore(t *testing.T, r *tt.Script) []check.Finding { return judgeWith(t, r, check.Restore) }

func TestRestoreFailsADamagedResourceThatStaysGone(t *testing.T) {
	r := damageA(twoCRs())
	r.Call(ctl, "get", childA, 404).Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a)")
}

func TestRestoreJudgesADamagedCROnExistenceOnly(t *testing.T) {
	r := damageA(twoCRs())
	r.Create(ctl, childA, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-0"}, "data": "new"}, r.UID(widgetA), true))
	r.Create(ctl, tt.Ref("configmaps", "ns", "a-x1"), tt.OwnedBy(tt.Generated("a-", "a-x1"), r.UID(widgetA), true))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreFailsAnUndamagedCRsReplacedResource(t *testing.T) {
	r := damageA(twoCRs())
	r.Create(ctl, childA, tt.OwnedBy(tt.Object("a-0"), r.UID(widgetA), true))
	r.Delete(ctl, childB).Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(widgetB), true))
	r.Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "replaced resources", "kube/configmaps", "configmaps ns/b-0 (rule 1, slot b)")
}

func TestRestoreFailsANewResource(t *testing.T) {
	r := twoCRs()
	r.Begin().Restart()
	r.Create(ctl, tt.Ref("configmaps", "ns", "a-1"), tt.OwnedBy(tt.Object("a-1"), r.UID(widgetA), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "replaced resources", "kube/configmaps", "configmaps ns/a-1")
}

func TestRestoreFailsAChangedResource(t *testing.T) {
	r := twoCRs()
	r.Begin().Restart()
	r.Update(ctl, childA, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-0"}, "data": map[string]any{"label": "blue"}}, r.UID(widgetA), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a): data")
}

func TestRestoreIgnoresMetadataEveryWriteChanges(t *testing.T) {
	r := twoCRs()
	r.Begin().Restart()
	body := tt.OwnedBy(tt.Object("a-0"), r.UID(widgetA), true)
	metadata := body["metadata"].(map[string]any)
	metadata["resourceVersion"], metadata["generation"], metadata["managedFields"] = "99", 2, []any{"x"}
	r.Update(ctl, childA, body)
	r.EndFaults().Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreSkipsAStepThatChangesWhatItCompares(t *testing.T) {
	for _, do := range []string{"create", "update", "delete", "changeFixture"} {
		r := twoCRs()
		r.Begin().Restart().Act(do, "a", &widgetA)
		r.Update(ctl, childA, map[string]any{"data": "changed"})
		r.EndFaults().Settle()
		if findings := restore(t, r); len(findings) != 0 {
			t.Errorf("with %s: findings = %+v", do, findings)
		}
	}
}

func TestRestoreNeedsBothStepsSettled(t *testing.T) {
	unsettled := twoCRs()
	unsettled.Begin().Restart().EndFaults().At(unsettled.Now, tt.Phase(1, trace.Settled))
	unsettled.Tick(500*ms).Update(ctl, childA, map[string]any{"data": "changed"}).Tick(500*ms).At(unsettled.Now, tt.Phase(1, trace.Confirmed))
	unsettled.Begin().Restart().EndFaults().Settle()
	requireNone(t, restore(t, unsettled))

	r := newRun(1)
	r.Begin().Restart()
	r.Create(ctl, cm, tt.Object("c"))
	r.EndFaults().Settle()
	requireNone(t, restore(t, r))
}

// reissue settles a step that creates certificate a with its request a-1
// and Secret a-tls, then starts one that damages the Secret.
func reissue() (r *tt.Script, request, secret trace.Ref) {
	r = newRun(2)
	request, secret = tt.Ref("cert-manager.io/certificaterequests", "ns", "a-1"), tt.Ref("secrets", "ns", "a-tls")
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	r.Create(ctl, request, tt.OwnedBy(tt.Object("a-1"), uid, true))
	r.Create(ctl, secret, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-tls"}, "data": "key-1"}, uid, true))
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, secret).Act("damage", "a", &secret).EndFaults()
	return r, request, secret
}

func TestRestoreLetsADamagedCRRenameItsOtherResources(t *testing.T) {
	r, request, secret := reissue()
	uid := r.UID(widgetA)
	r.Create(ctl, tt.Ref("cert-manager.io/certificaterequests", "ns", "a-2"), tt.OwnedBy(tt.Object("a-2"), uid, true))
	r.Delete(ctl, request)
	r.Create(ctl, secret, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-tls"}, "data": "key-2"}, uid, true))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreFailsADamagedCRThatLosesAKind(t *testing.T) {
	r, request, secret := reissue()
	r.Delete(ctl, request)
	r.Create(ctl, secret, tt.OwnedBy(tt.Object("a-tls"), r.UID(widgetA), true))
	r.Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/cert-manager.io/certificaterequests", "certificaterequests.cert-manager.io ns/a-1")
}

func TestRestoreFailsADamagedResourceWhoseKindHasOthers(t *testing.T) {
	r := newRun(2)
	r.Begin()
	withChild(r, "a", true)
	r.Create(ctl, tt.Ref("configmaps", "ns", "a-1"), tt.OwnedBy(tt.Object("a-1"), r.UID(widgetA), true))
	r.EndFaults().Settle()
	damageA(r).Call(ctl, "get", childA, 404).Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/configmaps", "configmaps ns/a-0")
}

func TestRestoreJudgesADamagedSharedResourceOnExistenceOnly(t *testing.T) {
	r := newRun(2)
	webhook := tt.Ref("secrets", "ns", "webhook-cert")
	r.Create(ctl, webhook, tt.Object("webhook-cert"))
	r.Begin().Restart().EndFaults().Settle()
	r.Begin().Delete(trace.Harness, webhook).Act("damage", "", &webhook).EndFaults()
	r.Call(ctl, "get", webhook, 404).Create(ctl, webhook, tt.Object("webhook-cert"))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreFailsADamagedSharedResourceWhoseKindHasOthers(t *testing.T) {
	r := newRun(2)
	webhook := tt.Ref("secrets", "ns", "webhook-cert")
	r.Create(ctl, webhook, tt.Object("webhook-cert"))
	r.Create(ctl, tt.Ref("secrets", "ns", "metrics-cert"), tt.Object("metrics-cert"))
	r.Begin().Restart().EndFaults().Settle()
	r.Begin().Delete(trace.Harness, webhook).Act("damage", "", &webhook).EndFaults()
	r.Call(ctl, "get", webhook, 404).Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/secrets", "secrets ns/webhook-cert (shared)")
}

func TestRestoreJudgesAFailedDamageAsNoDamage(t *testing.T) {
	r := twoCRs()
	r.Begin()
	r.Add(trace.Event{Action: &trace.Action{Step: 1, Do: "damage", Slot: "a", Ref: &childA, Error: "conflict"}})
	r.Delete(ctl, childA).Create(ctl, childA, tt.OwnedBy(tt.Object("a-0"), r.UID(widgetA), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "replaced resources", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a)")
}

func TestRestoreIgnoresAModelKindsStatus(t *testing.T) {
	for name, c := range map[string]struct {
		body func(r *tt.Script) map[string]any
		want string
	}{
		"status": {func(r *tt.Script) map[string]any {
			body := gadget(r, true)
			body["status"].(map[string]any)["message"] = "fault 1 ended"
			return body
		}, ""},
		"spec": {func(r *tt.Script) map[string]any {
			body := gadget(r, true)
			body["spec"] = map[string]any{"size": 2}
			return body
		}, "gadgets.toy ns/a (rule 1, slot a): spec"},
	} {
		r := gadgetRun(trace.Dependency{})
		r.See(ctl, gadgetA).Ready(widgetA).EndFaults().Settle()
		r.Begin().Restart().Update(gadgetModel, gadgetA, c.body(r)).EndFaults().Settle()
		findings := restore(t, r)
		if c.want == "" {
			if len(findings) != 0 {
				t.Errorf("%s: findings = %+v", name, findings)
			}
			continue
		}
		requireFinding(t, findings, "Restore", 1, "changed resources", "kube/toy/gadgets", c.want)
	}
}

func TestRestoreComparesAnotherKindsStatus(t *testing.T) {
	r := twoCRs()
	r.Begin().Restart()
	r.Update(ctl, childA, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-0"}, "status": "new"}, r.UID(widgetA), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a): status")
}

// madeDatabase settles a step in which the harness creates Cluster a, and
// the controller makes Database b, a CR of a listed kind, for a, and the
// ConfigMap b-0 for b.
func madeDatabase() *tt.Script {
	r := fleetRun(2, clusters, databases)
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	return r.Ready(clusterA).EndFaults().Settle()
}

func TestRestoreLetsACRTheControllerMadeChangeWhenADamageTouchesIt(t *testing.T) {
	r := madeDatabase()
	r.Begin().Delete(trace.Harness, childB).Act("damage", "a", &childB).EndFaults()
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Update(ctl, databaseB, tt.OwnedBy(named("b", map[string]any{"status": map[string]any{"ready": true, "since": "t2"}}), r.UID(clusterA), true))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreLetsACRTheControllerMadeForAFixtureChangeWhenADamageTouchesIt(t *testing.T) {
	r := fleetRun(2, clusters, databases)
	r.Begin()
	r.CreateCR("a", clusterA)
	fixture := r.Create(trace.Harness, tt.Ref("configmaps", "ns", "f"), tt.Object("f"))
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), fixture, true))
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, childB).Act("damage", "", &childB).EndFaults()
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Update(ctl, databaseB, tt.OwnedBy(named("b", map[string]any{"status": map[string]any{"ready": true, "since": "t2"}}), fixture, true))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreJudgesTheResourcesOfACRTheControllerMadeAgain(t *testing.T) {
	for name, remade := range map[string]bool{"remade": true, "not remade": false} {
		t.Run(name, func(t *testing.T) {
			r := madeDatabase()
			r.Begin().Delete(trace.Harness, databaseB).Act("damage", "a", &databaseB).EndFaults()
			collect(r, childB)
			r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), r.UID(clusterA), true))
			if remade {
				r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
			}
			r.Settle()
			if remade {
				requireNone(t, restore(t, r))
			} else {
				requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/configmaps", "configmaps ns/b-0 (rule 1, CR databases.toy ns/b)")
			}
		})
	}
}

func TestRestorePassesADamagedResourceWhoseOwnerTheControllerRemade(t *testing.T) {
	r := madeDatabase()
	r.Begin().Delete(trace.Harness, childB).Act("damage", "a", &childB).EndFaults()
	r.Delete(ctl, databaseB)
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), r.UID(clusterA), true))
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreFailsADamagedResourceWhoseRemadeOwnerDoesNotRemakeIt(t *testing.T) {
	r := fleetRun(2, clusters, databases)
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.Create(ctl, childA, tt.OwnedBy(tt.Object("a-0"), uid, true))
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, childB).Act("damage", "a", &childB).EndFaults()
	r.Call(ctl, "get", childB, 404)
	r.Delete(ctl, databaseB)
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/configmaps", "configmaps ns/b-0 (rule 1, CR databases.toy ns/b)")
}

func TestRestorePassesAnOwnerAndItsResourceThatTheControllerMadeAgain(t *testing.T) {
	role := tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "a-reader")
	binding := tt.Ref("rbac.authorization.k8s.io/clusterrolebindings", "", "a-reader")
	makeBoth := func(r *tt.Script) {
		r.Create(ctl, role, tt.Object("a-reader"))
		r.Create(ctl, binding, tt.OwnedBy(tt.Object("a-reader"), r.UID(role), true))
	}
	r := newRun(2)
	r.Begin()
	r.CreateCR("a", widgetA)
	makeBoth(r)
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, role).Act("damage", "a", &role).EndFaults()
	collect(r, binding)
	makeBoth(r)
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreNamesACRWithoutASlotByItsKindAndName(t *testing.T) {
	r := madeDatabase()
	r.Begin().Restart()
	r.Update(ctl, childB, tt.OwnedBy(named("b-0", map[string]any{"data": "new"}), r.UID(databaseB), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps", "configmaps ns/b-0 (rule 1, CR databases.toy ns/b): data")
}

func TestRestoreLetsARule4ResourceOfADamagedRootChange(t *testing.T) {
	clusterC, shared := tt.Ref("toy/clusters", "ns", "c"), tt.Ref("configmaps", "ns", "x")
	r := fleetRun(2, clusters, databases)
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.CreateCR("c", clusterC)
	r.Create(ctl, shared, tt.Object("x"))
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Ready(clusterA).Ready(clusterC).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, childB).Act("damage", "a", &childB).EndFaults()
	r.Create(ctl, childB, tt.OwnedBy(tt.Object("b-0"), r.UID(databaseB), true))
	r.Update(ctl, shared, named("x", map[string]any{"data": "new"}))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestorePassesARule4ResourceRemadeAfterTheControllerMadeACR(t *testing.T) {
	role := tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "a-reader")
	r := fleetRun(2, clusters, databases)
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.Create(ctl, role, tt.Object("a-reader"))
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, role).Act("damage", "a", &role).EndFaults()
	r.Create(ctl, role, tt.Object("a-reader"))
	r.Settle()
	requireNone(t, restore(t, r))
}

func TestRestoreFailsADamagedResourceWhileASiblingCRKeepsOneOfItsRole(t *testing.T) {
	cmB, cmC := tt.Ref("configmaps", "ns", "db-b1"), tt.Ref("configmaps", "ns", "db-c1")
	r := fleetRun(2, clusters, databases)
	r.Begin()
	uid := r.CreateCR("a", clusterA)
	r.Create(ctl, databaseB, tt.OwnedBy(tt.CR("b", true), uid, true))
	r.Create(ctl, databaseC, tt.OwnedBy(tt.CR("c", true), uid, true))
	r.Create(ctl, cmB, tt.OwnedBy(tt.Generated("db-", "db-b1"), r.UID(databaseB), true))
	r.Create(ctl, cmC, tt.OwnedBy(tt.Generated("db-", "db-c1"), r.UID(databaseC), true))
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Delete(trace.Harness, cmB).Act("damage", "a", &cmB).EndFaults()
	r.Call(ctl, "get", cmB, 404).Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "did not restore damaged resources", "kube/configmaps", "configmaps ns/db-b1 (rule 1, CR databases.toy ns/b)")
}
