package check_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const released = "released an object that another names"

// blockRef is secretRef under onDelete: block.
func blockRef() trace.Reference {
	ref := secretRef()
	ref.Block = true
	return ref
}

// clusterRef is a Database's reference to the Cluster its spec.clusterRef
// names.
func clusterRef(r *tt.Script, block bool) trace.Reference {
	return trace.Reference{From: databases, To: clusters, Namespaced: true, Name: "spec.clusterRef", Ready: r.Spec.Primaries[0].Ready, Required: true, Block: block}
}

// databaseOn is slot b's Database, naming the Cluster cluster.
func databaseOn(ready bool, cluster string) map[string]any {
	body := tt.CR("b", ready)
	body["spec"] = map[string]any{"clusterRef": cluster}
	return body
}

// deletingBody is body once its deletion started.
func deletingBody(body map[string]any) map[string]any {
	body = maps.Clone(body)
	metadata := maps.Clone(body["metadata"].(map[string]any))
	metadata["deletionTimestamp"] = "2026-01-01T00:00:00Z"
	body["metadata"] = metadata
	return body
}

func ordered(t *testing.T, r *tt.Script) []check.Finding {
	t.Helper()
	return judgeWith(t, r.EndFaults().Settle(), check.Ordered)
}

func TestOrderedFailsAReleaseWhileASeenCRNamesIt(t *testing.T) {
	t.Skip("enabled by the judge package")
	for name, before := range map[string]func(r *tt.Script){
		"a live CR": func(*tt.Script) {},
		"a CR being deleted": func(r *tt.Script) {
			r.Update(trace.Harness, databaseB, deletingBody(database(false, "pw")))
		},
		"after the object waited in deletion": func(r *tt.Script) {
			r.Update(trace.Harness, pw, deletingBody(secret(true)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			r.See(ctl, databaseB)
			before(r)
			r.Delete(ctl, pw)
			requireFinding(t, ordered(t, r), "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

func TestOrderedPassesAReleaseThatNoSeenCRNamesThen(t *testing.T) {
	t.Skip("enabled by the judge package")
	for name, c := range map[string]struct {
		ref    trace.Reference
		before func(r *tt.Script)
	}{
		"without block":                 {secretRef(), func(r *tt.Script) { r.See(ctl, databaseB) }},
		"a CR the controller never saw": {blockRef(), func(*tt.Script) {}},
		"a CR gone first": {blockRef(), func(r *tt.Script) {
			r.See(ctl, databaseB).Delete(trace.Harness, databaseB)
		}},
		"a CR that moved its reference first": {blockRef(), func(r *tt.Script) {
			r.See(ctl, databaseB).Update(trace.Harness, databaseB, database(false, "other"))
		}},
		"a CR that named it again once its deletion started": {blockRef(), func(r *tt.Script) {
			r.See(ctl, databaseB).Update(trace.Harness, databaseB, database(false, "other")).
				Update(trace.Harness, pw, deletingBody(secret(true))).
				Update(trace.Harness, databaseB, database(false, "pw")).See(ctl, databaseB)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(c.ref, secret(true))
			c.before(r)
			r.Delete(ctl, pw)
			requireNone(t, ordered(t, r))
		})
	}
}

func TestOrderedIgnoresACRCreatedOnceTheDeletionStarted(t *testing.T) {
	t.Skip("enabled by the judge package")
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{blockRef()}
	r.Begin()
	r.Create(trace.Harness, pw, secret(true))
	r.Update(trace.Harness, pw, deletingBody(secret(true)))
	r.Create(trace.Harness, databaseB, database(false, "pw"))
	r.Act("create", "b", &databaseB).See(ctl, databaseB)
	r.Delete(ctl, pw)
	requireNone(t, ordered(t, r))
}

// The observer watches each kind apart, so it may record a later version of
// one kind before an earlier version of another. Ordered orders them by
// resourceVersion, as integers.
func TestOrderedOrdersVersionsOfTwoKindsByResourceVersion(t *testing.T) {
	t.Skip("enabled by the judge package")
	for name, c := range map[string]struct {
		moved, release string
		finding        bool
	}{
		"a move before the release, recorded after it": {"99", "100", false},
		"a move after the release, recorded before it": {"101", "100", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			r.See(ctl, databaseB)
			r.Add(tt.Version(databaseB, r.UID(databaseB), c.moved, database(false, "other")))
			r.Add(tt.Deleted(pw, r.UID(pw), c.release, secret(true)))
			findings := ordered(t, r)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

func TestACRWaitingInDeletionForACRThatNamesItMeetsClause2(t *testing.T) {
	t.Skip("enabled by the judge package")
	for name, c := range map[string]struct {
		block bool
		after func(r *tt.Script)
		unmet []string
	}{
		"named by a live CR":     {true, func(*tt.Script) {}, nil},
		"named without block":    {false, func(*tt.Script) {}, []string{"clusters.toy ns/a is being deleted"}},
		"after its last CR left": {true, func(r *tt.Script) { r.DeleteCR("b", databaseB) }, []string{"clusters.toy ns/a is being deleted"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{clusterRef(r, c.block)}
			r.Begin()
			r.CreateCR("a", clusterA)
			r.Ready(clusterA)
			r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
			r.Act("create", "b", &databaseB)
			r.Update(trace.Harness, clusterA, deletingBody(tt.CR("a", true))).Act("delete", "a", &clusterA)
			c.after(r)
			r.EndFaults()
			if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, c.unmet) {
				t.Errorf("unmet = %q, want %q.", unmet, c.unmet)
			}
		})
	}
}

func TestHonestCountsAnObjectWaitingInDeletionUnderBlockAsReady(t *testing.T) {
	t.Skip("enabled by the judge package")
	for name, block := range map[string]bool{"under block": true, "without block": false} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{clusterRef(r, block)}
			r.Begin()
			r.CreateCR("a", clusterA)
			r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
			r.Act("create", "b", &databaseB)
			r.Update(trace.Harness, clusterA, deletingBody(tt.CR("a", false))).See(ctl, clusterA)
			r.Update(ctl, databaseB, databaseOn(true, "a"))
			findings := honest(t, r)
			if block {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
		})
	}
}
