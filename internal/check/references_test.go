package check_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	secrets = trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}
	pw      = tt.Ref("secrets", "ns", "pw")
)

// secretRef is a Database's reference to the Secret its
// spec.passwordSecretRef.name names, which is ready once it has data.
func secretRef() trace.Reference {
	return trace.Reference{From: databases, To: secrets, Namespaced: true, Name: "spec.passwordSecretRef.name", Ready: "has(self.data)", Required: true}
}

// database is slot b's Database, naming the Secret secret.
func database(ready bool, secret string) map[string]any {
	body := tt.CR("b", ready)
	body["spec"] = map[string]any{"passwordSecretRef": map[string]any{"name": secret}}
	return body
}

func secret(ready bool) map[string]any {
	body := tt.Object("pw")
	if ready {
		body["data"] = map[string]any{"password": "eA=="}
	}
	return body
}

// secretRun starts step 0 of a run of Databases with ref, in which the
// harness creates the Secret pw with body, unless body is nil, and then
// slot b's Database, not ready, naming pw.
func secretRun(ref trace.Reference, body map[string]any) *tt.Script {
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{ref}
	r.Begin()
	if body != nil {
		r.Create(trace.Harness, pw, body)
	}
	r.Create(trace.Harness, databaseB, database(false, "pw"))
	return r.Act("create", "b", &databaseB)
}

func readyB(r *tt.Script) *tt.Script { return r.Update(ctl, databaseB, database(true, "pw")) }

func TestHonestPassesACRReadyAfterSeeingWhatItNamesReady(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(r.See(ctl, pw))
	requireNone(t, honest(t, r))
}

func TestHonestFailsACRReadyBeforeSeeingWhatItNamesReady(t *testing.T) {
	for name, c := range map[string]struct {
		secret map[string]any
		look   func(r *tt.Script)
		detail string
	}{
		"not seen":       {secret(true), func(*tt.Script) {}, "slot b: secrets ns/pw not seen"},
		"seen not ready": {secret(false), func(r *tt.Script) { r.See(ctl, pw) }, "slot b: secrets ns/pw seen not ready"},
		"seen ready, then not ready": {secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(false)).See(ctl, pw)
		}, "slot b: secrets ns/pw seen not ready"},
		"read opaquely, then seen not ready": {secret(false), func(r *tt.Script) {
			r.Add(opaque(tt.Ref("secrets", "ns", ""), 0)).See(ctl, pw)
		}, "slot b: secrets ns/pw seen not ready"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), c.secret)
			c.look(r)
			readyB(r)
			requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", c.detail)
		})
	}
}

func TestHonestFailsACRReadyWhileWhatItRequiresIsMissing(t *testing.T) {
	for name, c := range map[string]struct {
		secret map[string]any
		look   func(r *tt.Script)
		detail string
	}{
		"never created":     {nil, func(*tt.Script) {}, "slot b: secrets ns/pw not seen"},
		"a 404":             {nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, "slot b: secrets ns/pw seen missing"},
		"a list without it": {nil, func(r *tt.Script) { r.Call(ctl, "list", tt.Ref("secrets", "ns", ""), 200) }, "slot b: secrets ns/pw seen missing"},
		"seen deleted": {secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Delete(trace.Harness, pw).SeeDeleted(ctl, pw)
		}, "slot b: secrets ns/pw seen missing"},
		"seen missing, then created unseen": {nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(true))
		}, "slot b: secrets ns/pw seen missing"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), c.secret)
			c.look(r)
			readyB(r)
			requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", c.detail)
		})
	}
}

func TestHonestPassesACRReadyWhileWhatItNamesWithoutRequiredIsMissing(t *testing.T) {
	optional := secretRef()
	optional.Required = false
	for name, look := range map[string]func(r *tt.Script){
		"never seen": func(*tt.Script) {},
		"a 404":      func(r *tt.Script) { r.Call(ctl, "get", pw, 404) },
	} {
		r := secretRun(optional, nil)
		look(r)
		readyB(r)
		if findings := honest(t, r); len(findings) != 0 {
			t.Errorf("%s: findings = %+v, want none.", name, findings)
		}
	}
	r := secretRun(optional, secret(true))
	readyB(r)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/pw not seen")
}

func TestHonestJudgesAReadyWriteByWhatTheControllerSawBeforeSendingIt(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	r.UpdateInFlight(ctl, databaseB, database(true, "pw"), 50*ms).See(ctl, pw)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/pw not seen")
}

func TestHonestPassesACRReadyWhileTheControllerLastReadWhatItNamesOpaquely(t *testing.T) {
	r := secretRun(secretRef(), secret(false))
	readyB(r.See(ctl, pw).Add(opaque(tt.Ref("secrets", "ns", ""), 0)))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesWhatEachValueNames(t *testing.T) {
	r := secretRun(secretRef(), secret(false))
	r.Update(ctl, databaseB, database(true, ""))
	requireNone(t, honest(t, r))

	keys := secretRef()
	keys.Name = "spec.keys[*].name"
	r = secretRun(keys, secret(true))
	r.Create(trace.Harness, tt.Ref("secrets", "ns", "other"), tt.Object("other"))
	body := tt.CR("b", true)
	body["spec"] = map[string]any{"keys": []any{map[string]any{"name": "pw"}, map[string]any{"name": "other"}}}
	r.See(ctl, pw).Update(ctl, databaseB, body)
	findings := honest(t, r)
	requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/other not seen")
	if len(findings[0].Details) != 1 {
		t.Errorf("details = %q, want only the Secret other.", findings[0].Details)
	}

	elsewhere := secretRef()
	elsewhere.Namespace = "spec.passwordSecretRef.namespace"
	r = secretRun(elsewhere, secret(true))
	body = database(true, "pw")
	body["spec"].(map[string]any)["passwordSecretRef"].(map[string]any)["namespace"] = "elsewhere"
	r.See(ctl, pw).Update(ctl, databaseB, body)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", "slot b: secrets elsewhere/pw not seen")
}

func TestHonestJudgesAClusterScopedCRsReferenceByItsNamespaceValue(t *testing.T) {
	tenants := trace.Kind{Group: "toy", Version: "v1", Kind: "Tenant", Resource: "tenants"}
	tenantB := tt.Ref("toy/tenants", "", "b")
	for _, namespace := range []string{"", "ns"} {
		r := fleetRun(1, tenants)
		r.Spec.References = []trace.Reference{{From: tenants, To: secrets, Namespaced: true,
			Name: "spec.secretRef.name", Namespace: "spec.secretRef.namespace", Ready: "true", Required: true}}
		r.Begin()
		tenant := func(ready bool) map[string]any {
			body := tt.CR("b", ready)
			body["spec"] = map[string]any{"secretRef": map[string]any{"name": "pw", "namespace": namespace}}
			return body
		}
		r.Create(trace.Harness, tenantB, tenant(false))
		r.Act("create", "b", &tenantB).Update(ctl, tenantB, tenant(true))
		if namespace == "" {
			requireNone(t, honest(t, r))
		} else {
			requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", "slot b: secrets ns/pw not seen")
		}
	}
}

func TestHonestJudgesAReferenceToAListedKindByItsReady(t *testing.T) {
	for name, c := range map[string]struct {
		look    func(r *tt.Script)
		finding bool
	}{
		"seen not ready":                     {func(r *tt.Script) { r.See(ctl, clusterA) }, true},
		"made ready and seen":                {func(r *tt.Script) { r.Ready(clusterA).See(ctl, clusterA) }, false},
		"seen ready before the next write":   {func(r *tt.Script) { r.Ready(clusterA).See(ctl, clusterA).Update(ctl, clusterA, tt.CR("a", true)) }, false},
		"seen not ready after a ready write": {func(r *tt.Script) { r.See(ctl, clusterA).Ready(clusterA) }, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{{From: databases, To: clusters, Namespaced: true, Name: "spec.clusterRef", Ready: r.Spec.Primaries[0].Ready, Required: true}}
			r.Begin()
			r.CreateCR("a", clusterA)
			body := tt.CR("b", false)
			body["spec"] = map[string]any{"clusterRef": "a"}
			r.Create(trace.Harness, databaseB, body)
			r.Act("create", "b", &databaseB)
			c.look(r)
			body = tt.CR("b", true)
			body["spec"] = map[string]any{"clusterRef": "a"}
			r.Update(ctl, databaseB, body)
			findings := honest(t, r)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
		})
	}
}

func TestHonestJudgesAReferenceOnlyForCRsOfItsFromKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{secretRef()}
	r.Begin()
	cluster := func(ready bool) map[string]any {
		body := tt.CR("a", ready)
		body["spec"] = map[string]any{"passwordSecretRef": map[string]any{"name": "pw"}}
		return body
	}
	r.Create(trace.Harness, clusterA, cluster(false))
	r.Act("create", "a", &clusterA).Update(ctl, clusterA, cluster(true))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesAReferenceToAFakesObject(t *testing.T) {
	for name, c := range map[string]struct {
		state           string
		look            bool
		summary, detail string
	}{
		"seen ready": {state: "READY", look: true},
		"not seen":   {state: "READY", summary: beforeSeeing, detail: "slot b: cloud buckets b1 not seen"},
		"gone":       {state: "DELETING", summary: noDependency, detail: "slot b: cloud buckets b1 not seen"},
		"seen gone":  {state: "DELETING", look: true, summary: noDependency, detail: "slot b: cloud buckets b1 seen missing"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, databases)
			r.Spec.Systems = []trace.System{{Name: "cloud", API: "crud", Gone: cloudSystem.Gone}}
			r.Spec.References = []trace.Reference{{From: databases, System: "cloud", To: trace.Kind{Resource: "buckets"},
				Name: "spec.bucket", Ready: cloudSystem.Ready, Required: true}}
			r.Begin()
			r.Create(trace.Harness, bucketA, map[string]any{"id": "b1", "state": c.state})
			if c.look {
				r.See(ctl, bucketA)
			}
			database := func(ready bool) map[string]any {
				body := tt.CR("b", ready)
				body["spec"] = map[string]any{"bucket": "b1"}
				return body
			}
			r.Create(trace.Harness, databaseB, database(false))
			r.Act("create", "b", &databaseB).Update(ctl, databaseB, database(true))
			findings := honest(t, r)
			if c.summary == "" {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, c.summary, "cloud/buckets", c.detail)
		})
	}
}

func TestHonestJudgesAVersionTheObserverMissedByTheReferencesReady(t *testing.T) {
	anyObject := secretRef()
	anyObject.Ready = "true"
	r := secretRun(anyObject, secret(false))
	readyB(r.Add(tt.Seen(ctl, pw, r.UID(pw), "missed")))
	requireNone(t, honest(t, r))
}

// secretGoes has the controller make slot b ready after seeing pw ready,
// and then has change change pw.
func secretGoes(ref trace.Reference, change func(r *tt.Script)) *tt.Script {
	r := secretRun(ref, secret(true))
	readyB(r.See(ctl, pw))
	change(r)
	return r
}

func notReadySeen(r *tt.Script) { r.Update(trace.Harness, pw, secret(false)).See(ctl, pw) }

func deletedSeen(r *tt.Script) { r.Delete(trace.Harness, pw).SeeDeleted(ctl, pw) }

func TestHonestWithTrackFailsACRThatStaysReadyNamingWhatHoldsItBack(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	for name, c := range map[string]struct {
		change func(r *tt.Script)
		detail string
	}{
		"seen not ready": {notReadySeen, "slot b: secrets ns/pw seen not ready at"},
		"seen deleted":   {deletedSeen, "slot b: secrets ns/pw seen missing at"},
		"a 404":          {func(r *tt.Script) { r.Delete(trace.Harness, pw).Call(ctl, "get", pw, 404) }, "slot b: secrets ns/pw seen missing at"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretGoes(tracked, c.change)
			look := r.Now - tt.Tick
			findings := honest(t, r)
			requireFinding(t, findings, "Honest", 0, stayedReady, "kube/secrets", c.detail)
			if findings[0].At != look+500*ms {
				t.Errorf("at = %v, want %v", findings[0].At, look+500*ms)
			}
		})
	}
	requireNone(t, honest(t, secretGoes(secretRef(), notReadySeen)))
}

func TestHonestWithTrackPassesACRThatReactsInTime(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	optional := tracked
	optional.Required = false
	for name, c := range map[string]struct {
		ref    trace.Reference
		change func(r *tt.Script)
	}{
		"the CR goes not ready in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(ctl, databaseB, database(false, "pw"))
		}},
		"the controller sees it ready in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(trace.Harness, pw, secret(true)).See(ctl, pw)
		}},
		"the CR stops naming it in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(trace.Harness, databaseB, database(true, ""))
		}},
		"the controller restarts in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400 * ms).Restart()
		}},
		"an opaque read follows": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Add(opaque(tt.Ref("secrets", "ns", ""), 0))
		}},
		"what goes is not required": {optional, deletedSeen},
		"the CR is deleted in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Delete(trace.Harness, databaseB)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			requireNone(t, honest(t, secretGoes(c.ref, c.change)))
		})
	}
}

func TestHonestWithTrackFailsOnceAtTheFirstLookThatHeldTheCRBack(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	var first time.Duration
	r := secretGoes(tracked, func(r *tt.Script) {
		notReadySeen(r)
		first = r.Now - tt.Tick
		r.Tick(100*ms).See(ctl, pw).Tick(500*ms).Update(trace.Harness, pw, secret(true)).See(ctl, pw)
	})
	findings := honest(t, r)
	requireFinding(t, findings, "Honest", 0, stayedReady, "kube/secrets", fmt.Sprintf("slot b: secrets ns/pw seen not ready at %v", first))
	if f := findings[0]; len(f.Details) != 1 || f.At != first+500*ms {
		t.Errorf("finding = %+v, want one detail, at %v.", f, first+500*ms)
	}
}

func TestHonestWithTrackFailsACRThatReactsTooLate(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	r := secretGoes(tracked, func(r *tt.Script) {
		notReadySeen(r)
		r.Tick(600*ms).Update(ctl, databaseB, database(false, "pw"))
	})
	requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "kube/secrets", "slot b: secrets ns/pw seen not ready at")
}

func TestHonestWithTrackJudgesOnlyLooksWhileTheCRLives(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{tracked}
	r.Begin()
	r.Create(trace.Harness, pw, secret(false))
	r.See(ctl, pw).Create(trace.Harness, databaseB, database(true, "pw"))
	r.Act("create", "b", &databaseB)
	requireNone(t, honest(t, r))
}

func TestHonestWithTrackJudgesNoLookWhoseTrackOutlastsTheTrace(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 5 * s
	requireNone(t, honest(t, secretGoes(tracked, notReadySeen)))
}

func TestSettleExcusesACRNotReadyWhileAReferenceHoldsItBack(t *testing.T) {
	optional := secretRef()
	optional.Required = false
	for name, c := range map[string]struct {
		ref     trace.Reference
		secret  map[string]any
		look    func(r *tt.Script)
		excused bool
	}{
		"missing and never seen": {secretRef(), nil, func(*tt.Script) {}, true},
		"seen missing":           {secretRef(), nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, true},
		"seen not ready":         {secretRef(), secret(false), func(r *tt.Script) { r.See(ctl, pw) }, true},
		"seen missing, then created not ready": {secretRef(), nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(false))
		}, true},
		"seen not ready, then changed": {secretRef(), secret(false), func(r *tt.Script) {
			changed := secret(false)
			changed["type"] = "Opaque"
			r.See(ctl, pw).Update(trace.Harness, pw, changed)
		}, true},
		"missing until after t": {secretRef(), nil, func(r *tt.Script) {
			r.At(r.Now+time.Minute, tt.Version(pw, "u-pw", "99", secret(true)))
		}, true},
		"seen missing, then created ready": {secretRef(), nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(true))
		}, false},
		"seen not ready, then made ready": {secretRef(), secret(false), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(true))
		}, false},
		"read opaquely": {secretRef(), secret(true), func(r *tt.Script) { r.Add(opaque(tt.Ref("secrets", "ns", ""), 0)) }, true},
		"seen ready":    {secretRef(), secret(true), func(r *tt.Script) { r.See(ctl, pw) }, false},
		"seen deleted":  {secretRef(), secret(true), func(r *tt.Script) { deletedSeen(r.See(ctl, pw)) }, true},
		"seen ready, then made not ready": {secretRef(), secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(false))
		}, false},
		"live and never seen":           {secretRef(), secret(true), func(*tt.Script) {}, false},
		"not ready and never seen":      {secretRef(), secret(false), func(*tt.Script) {}, false},
		"missing without required":      {optional, nil, func(*tt.Script) {}, false},
		"seen missing without required": {optional, nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, false},
	} {
		r := secretRun(c.ref, c.secret)
		c.look(r)
		r.EndFaults()
		unmet := settledAt(t, r, r.Now+5*s)
		if waits := slices.Contains(unmet, "databases.toy ns/b is not ready"); waits == c.excused {
			t.Errorf("%s: unmet = %q, want the Database excused: %v.", name, unmet, c.excused)
		}
	}
}

// The controller reacts to a held-back CR's create within stable, so the
// step must not settle before it does.
func TestSettleExcusesAHeldBackCROnlyStableAfterTheFaultsEnd(t *testing.T) {
	r := secretRun(secretRef(), nil).Tick(5 * s)
	if !slices.Contains(settledAt(t, r, r.Now), "databases.toy ns/b is not ready") {
		t.Error("The Database is excused before the faults end.")
	}
	r.EndFaults()
	endFaults := r.Now - tt.Tick
	for at, excused := range map[time.Duration]bool{endFaults + r.Spec.Stable - time.Millisecond: false, endFaults + r.Spec.Stable: true} {
		if waits := slices.Contains(settledAt(t, r, at), "databases.toy ns/b is not ready"); waits == excused {
			t.Errorf("At %v, the Database is excused: %v, want %v.", at, !waits, excused)
		}
	}
}

func TestSettleStillWaitsForACRNoReferenceHoldsBack(t *testing.T) {
	r := secretRun(secretRef(), nil)
	namesNothing := database(false, "")
	namesNothing["metadata"] = map[string]any{"name": "c"}
	r.Create(trace.Harness, databaseC, namesNothing)
	r.Act("create", "c", &databaseC).Call(ctl, "get", databaseC, 200).EndFaults()
	if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, []string{"databases.toy ns/c is not ready"}) {
		t.Errorf("unmet = %q, want only that Database c, which names nothing, is not ready.", unmet)
	}
}

func TestClause5HoldsTheControllerToLookingAtACreatedFixture(t *testing.T) {
	requireUnmet(t, damagedAt1200ms("createFixture"), 3200*ms, "has not looked at configmaps ns/c since the createFixture at 1.2s")
}

func TestSpecForReadsTheRunEventsReferences(t *testing.T) {
	run := tt.Run("toy", "widgets")
	run.Run.Primaries[0].Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "self.ok", 4*s, 3, 5
	run.Run.References = []trace.Reference{
		{From: widgets, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "has(self.data)", Required: true, Track: s},
		{From: widgets, System: "cloud", To: trace.Kind{Resource: "buckets"}, Name: "spec.bucket", Ready: "self.ok"},
	}
	var b tt.Builder
	got, err := check.SpecFor(nil, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 2 {
		t.Fatalf("references = %+v, want 2.", got.References)
	}
	toSecret, toBucket := got.References[0], got.References[1]
	if toSecret.From != widgets || toSecret.To != (trace.Ref{System: "kube", Resource: "secrets"}) || !toSecret.Namespaced ||
		toSecret.Name != "spec.secretName" || !toSecret.Required || toSecret.Track != s {
		t.Errorf("the Secret reference = %+v", toSecret)
	}
	if toBucket.To != (trace.Ref{System: "cloud", Resource: "buckets"}) || toBucket.Namespaced || toBucket.Required || toBucket.Name != "spec.bucket" {
		t.Errorf("the bucket reference = %+v", toBucket)
	}
	if ok, err := toSecret.Ready(map[string]any{"data": map[string]any{}}); !ok || err != nil {
		t.Errorf("Ready = %v, %v; want the run event's.", ok, err)
	}
	run.Run.References[0].Ready = "self.("
	b = tt.Builder{}
	if _, err := check.SpecFor(nil, b.Add(run).Index()); err == nil {
		t.Error("SpecFor accepts a reference whose ready does not compile.")
	}
}

func TestSpecForTakesReferencesFromTheConfig(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.References = []config.Reference{{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Namespaced: true,
		Name: "spec.secretName", Ready: "has(self.data)", Required: true, Track: s}}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{{From: run.Run.Primaries[0].Kind, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "true"}}
	var b tt.Builder
	ix := b.Add(run).Index()
	got, err := check.SpecFor(c, ix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 1 || got.References[0].To != (trace.Ref{System: "kube", Resource: "secrets"}) || !got.References[0].Required || got.References[0].Track != s {
		t.Fatalf("references = %+v, want the config's.", got.References)
	}
	if ok, _ := got.References[0].Ready(map[string]any{}); ok {
		t.Error("Ready is the run event's, want the config's.")
	}
	c.References[0].To = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	if _, err := check.SpecFor(c, ix); err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("SpecFor = %v, want an error that names ConfigMap, whose seens the trace did not record.", err)
	}
}

func TestSpecForRefusesAConfigReferenceToAKindTheTraceDidNotSee(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{
		{From: run.Run.Primaries[0].Kind, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "true"},
		{From: run.Run.Primaries[0].Kind, System: "vault", To: trace.Kind{Resource: "secret"}, Name: "spec.path", Ready: "true"},
	}
	var b tt.Builder
	ix := b.Add(run).Index()
	for _, want := range []struct {
		to      config.Reference
		refused string
	}{
		{config.Reference{To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Namespaced: true}, ""},
		{config.Reference{System: "vault", Collection: "other"}, ""},
		{config.Reference{To: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespaced: true}, "v1/ConfigMap"},
		{config.Reference{To: schema.GroupVersionKind{Group: "other.io", Version: "v1", Kind: "Secret"}, Namespaced: true}, "other.io/v1/Secret"},
		{config.Reference{System: "cloud", Collection: "secret"}, "cloud:secret"},
	} {
		r := want.to
		r.From, r.Name, r.Ready = c.Primaries[0].Kind, "spec.name", "true"
		c.References = []config.Reference{r}
		_, err := check.SpecFor(c, ix)
		switch {
		case want.refused == "" && err != nil:
			t.Errorf("SpecFor = %v, want no error, as the run event names %+v.", err, want.to)
		case want.refused != "" && (err == nil || !strings.Contains(err.Error(), want.refused)):
			t.Errorf("SpecFor = %v, want an error that names %s.", err, want.refused)
		}
	}
	c.References[0] = config.Reference{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Name: "spec.name", Ready: "self.("}
	if _, err := check.SpecFor(c, ix); err == nil {
		t.Error("SpecFor accepts a config reference whose ready does not compile.")
	}
}

func TestSpecForTakesAConfigReferencesResourceFromTheRunEvent(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.References = []config.Reference{{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Group: "zoo.io", Version: "v1", Kind: "Moose"}, Namespaced: true, Name: "spec.moose", Ready: "true"}}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{{From: run.Run.Primaries[0].Kind, To: trace.Kind{Group: "zoo.io", Version: "v1", Kind: "Moose", Resource: "moose"}, Namespaced: true, Name: "spec.moose", Ready: "true"}}
	var b tt.Builder
	got, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 1 || got.References[0].To != (trace.Ref{System: "kube", Group: "zoo.io", Resource: "moose"}) {
		t.Errorf("references = %+v, want one to moose, the run event's resource for Moose.", got.References)
	}
}
