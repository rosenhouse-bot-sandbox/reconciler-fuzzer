package check_test

import (
	"fmt"
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

// seeBoth has the controller see the Secret and slot b's Database, and then
// gives it stable to hold the Secret.
func seeBoth(r *tt.Script) *tt.Script {
	return r.See(ctl, pw).See(ctl, databaseB).Tick(r.Spec.Stable)
}

func ordered(t *testing.T, r *tt.Script) []check.Finding {
	t.Helper()
	return judgeWith(t, r.EndFaults().Settle(), check.Ordered)
}

func TestOrderedFailsAReleaseWhileASeenCRNamesIt(t *testing.T) {
	for name, before := range map[string]func(r *tt.Script){
		"a live CR": func(*tt.Script) {},
		"a CR being deleted": func(r *tt.Script) {
			r.Update(trace.Harness, databaseB, tt.Deleting(database(false, "pw")))
		},
		"after the object waited in deletion": func(r *tt.Script) {
			r.Update(trace.Harness, pw, tt.Deleting(secret(true)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			before(seeBoth(r))
			r.Delete(ctl, pw)
			requireFinding(t, ordered(t, r), "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

func TestOrderedJudgesOnlyBlockReferences(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	seeBoth(r).Delete(ctl, pw)
	requireNone(t, ordered(t, r))
}

func TestOrderedPassesAReleaseTheControllerHadNoTimeToHold(t *testing.T) {
	for name, see := range map[string]func(r *tt.Script){
		"a CR it never saw":                        func(r *tt.Script) { r.See(ctl, pw).Tick(r.Spec.Stable) },
		"a CR it saw less than stable before":      func(r *tt.Script) { r.See(ctl, pw).Tick(r.Spec.Stable).See(ctl, databaseB) },
		"an object it never saw":                   func(r *tt.Script) { r.See(ctl, databaseB).Tick(r.Spec.Stable) },
		"an object it saw less than stable before": func(r *tt.Script) { r.See(ctl, databaseB).Tick(r.Spec.Stable).See(ctl, pw) },
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			see(r)
			r.Delete(ctl, pw)
			requireNone(t, ordered(t, r))
		})
	}
}

func TestOrderedPassesAReleaseThatNoCRNamesThroughout(t *testing.T) {
	for name, before := range map[string]func(r *tt.Script){
		"a CR gone first":                     func(r *tt.Script) { r.Delete(trace.Harness, databaseB) },
		"a CR that moved its reference first": func(r *tt.Script) { r.Update(trace.Harness, databaseB, database(false, "other")) },
		"a CR that named it again after its deletion started": func(r *tt.Script) {
			r.Update(trace.Harness, databaseB, database(false, "other")).
				Update(trace.Harness, pw, tt.Deleting(secret(true))).
				Update(trace.Harness, databaseB, database(false, "pw"))
			seeBoth(r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			before(seeBoth(r))
			r.Delete(ctl, pw)
			requireNone(t, ordered(t, r))
		})
	}
}

func TestOrderedIgnoresACRCreatedOnceTheDeletionStarted(t *testing.T) {
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{blockRef()}
	r.Begin()
	r.Create(trace.Harness, pw, secret(true))
	r.Update(trace.Harness, pw, tt.Deleting(secret(true)))
	r.Create(trace.Harness, databaseB, database(false, "pw"))
	seeBoth(r.Act("create", "b", &databaseB))
	r.Delete(ctl, pw)
	requireNone(t, ordered(t, r))
}

func TestOrderedJudgesEachInstanceApart(t *testing.T) {
	t.Run("an object made again", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Update(trace.Harness, pw, tt.Deleting(secret(true))).
			Update(trace.Harness, databaseB, database(false, "other")).
			Delete(ctl, pw)
		r.Create(trace.Harness, pw, secret(true))
		seeBoth(r.Update(trace.Harness, databaseB, database(false, "pw")))
		r.Delete(ctl, pw)
		requireFinding(t, ordered(t, r), "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
	})
	t.Run("an object made again that the controller never saw", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Update(trace.Harness, databaseB, database(false, "other")).Delete(ctl, pw)
		r.Create(trace.Harness, pw, secret(true))
		r.Update(trace.Harness, databaseB, database(false, "pw")).See(ctl, databaseB).Tick(r.Spec.Stable)
		r.Delete(ctl, pw)
		requireNone(t, ordered(t, r))
	})
	t.Run("a CR made again that the controller never saw", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Delete(trace.Harness, databaseB)
		r.Create(trace.Harness, databaseB, database(false, "pw"))
		r.Act("create", "b", &databaseB).Tick(r.Spec.Stable)
		r.Delete(ctl, pw)
		requireNone(t, ordered(t, r))
	})
}

// The observer watches each kind apart, so it may record a later version of
// one kind before an earlier version of another. Ordered orders them by
// resourceVersion, as integers.
func TestOrderedOrdersVersionsOfTwoKindsByResourceVersion(t *testing.T) {
	move := func(r *tt.Script, rv string) trace.Event {
		return tt.Version(databaseB, r.UID(databaseB), rv, database(false, "other"))
	}
	release := func(r *tt.Script) trace.Event { return tt.Deleted(pw, r.UID(pw), "100", secret(true)) }
	t.Run("a move before the release, recorded after it", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Add(release(r)).Add(move(r, "99"))
		requireNone(t, ordered(t, r))
	})
	t.Run("a move after the release, recorded before it", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Add(move(r, "101")).Add(release(r))
		requireFinding(t, ordered(t, r), "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
	})
}

// The deleting Cluster reports itself not ready, as toy-fleet's does. After
// a stop, clause 2 judges it as a CR the stop waited for.
func TestACRWaitingInDeletionForACRThatNamesItSettles(t *testing.T) {
	for name, c := range map[string]struct {
		block bool
		after func(r *tt.Script)
		unmet []string
	}{
		"named by a live CR":     {true, func(*tt.Script) {}, nil},
		"named without block":    {false, func(*tt.Script) {}, []string{"clusters.toy ns/a is being deleted"}},
		"after its last CR left": {true, func(r *tt.Script) { r.DeleteCR("b", databaseB) }, []string{"clusters.toy ns/a is being deleted"}},
	} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, stopped %t", name, stop), func(t *testing.T) {
				r := fleetRun(1, clusters, databases)
				r.Spec.References = []trace.Reference{clusterRef(r, c.block)}
				r.Begin()
				r.CreateCR("a", clusterA)
				r.Ready(clusterA)
				r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
				r.Act("create", "b", &databaseB)
				r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", false))).Act("delete", "a", &clusterA)
				c.after(r)
				if stop {
					r.Stop(r.UID(clusterA))
				}
				r.EndFaults()
				if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, c.unmet) {
					t.Errorf("unmet = %q, want %q.", unmet, c.unmet)
				}
			})
		}
	}
}

func TestHonestJudgesAnObjectWaitingInDeletionByItsVersionBefore(t *testing.T) {
	for name, c := range map[string]struct{ block, wasReady, finding bool }{
		"under block, once ready":   {true, true, false},
		"under block, never ready":  {true, false, true},
		"without block, once ready": {false, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{clusterRef(r, c.block)}
			r.Begin()
			r.CreateCR("a", clusterA)
			if c.wasReady {
				r.Ready(clusterA)
			}
			r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
			r.Act("create", "b", &databaseB)
			r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).See(ctl, clusterA)
			r.Update(ctl, databaseB, databaseOn(true, "a"))
			findings := honest(t, r)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
		})
	}
}
