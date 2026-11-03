package check_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

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

// databaseRef is a Database's block reference to the Database that its
// spec.clusterRef names.
func databaseRef(r *tt.Script) trace.Reference {
	ref := clusterRef(r, true)
	ref.To = databases
	return ref
}

// databaseOn is slot b's Database, naming the Cluster cluster.
func databaseOn(ready bool, cluster string) map[string]any {
	return databaseNaming("b", ready, cluster)
}

// databaseNaming is the Database name, whose spec.clusterRef names other.
func databaseNaming(name string, ready bool, other string) map[string]any {
	body := tt.CR(name, ready)
	body["spec"] = map[string]any{"clusterRef": other}
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
			ix := r.Index()
			spec, err := check.SpecFor(nil, ix)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(check.Judge(ix, spec).Findings, func(f check.Finding) bool { return f.Check == "Ordered" }) {
				t.Error("Judge gives no Ordered finding.")
			}
		})
	}
}

// The controller can hold the object once it saw both the object and the CR
// that names it.
func TestOrderedFailsAReleaseStableAfterTheLaterSight(t *testing.T) {
	for name, order := range map[string][]trace.Ref{"of the object": {databaseB, pw}, "of the CR": {pw, databaseB}} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			r.See(ctl, order[0]).See(ctl, order[1]).Tick(r.Spec.Stable-tt.Tick).Delete(ctl, pw)
			requireFinding(t, ordered(t, r), "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

// A controller cannot hold an object while it is down.
func TestOrderedDoesNotCountTimeTheControllerIsDown(t *testing.T) {
	for name, late := range map[string]time.Duration{"before it was up for stable": -tt.Tick, "once it was": 0} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			sight := r.See(ctl, pw).See(ctl, databaseB).Now - tt.Tick
			exit := r.Tick(500 * ms).Now
			r.Add(tt.Exit("exit status 2", false, "panic: nil map"))
			start := r.Tick(2 * s).Now
			r.Add(tt.Start(2))
			findings := ordered(t, r.Tick(sight+r.Spec.Stable+start-exit+late-r.Now).Delete(trace.Harness, pw))
			if late < 0 {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

// secretsFail is a step's fault that fails the controller's requests on
// Secrets.
const secretsFail = "error(500) on resource=secrets"

// A fault failed the controller's update of the Secret, so it retries after
// a backoff that the step's allowance bounds. A sight once the faults ended,
// or stable before they began, gives it only stable. Time the controller is
// down after the faults still does not count toward stable.
func TestOrderedGivesAControllerInRetryBackoffTheStepsAllowance(t *testing.T) {
	// faults runs the step's faults from now for 500ms, failing the
	// controller's update of the Secret if fail is set, and returns when the
	// step's settle excuse starts.
	faults := func(r *tt.Script, fail bool) time.Duration {
		r.Add(tt.Fault(r.Step, 0, false, secretsFail))
		if fail {
			r.Add(injected("update", pw, 500, "error(500)"))
		}
		r.Tick(500 * ms).Add(tt.Fault(r.Step, 0, true, secretsFail))
		return endFaults(r, 2*s)
	}
	for name, c := range map[string]struct {
		// release runs the step and returns when the Secret's release comes.
		release func(r *tt.Script) time.Duration
		finding bool
	}{
		"before the excuse starts": {func(r *tt.Script) time.Duration {
			return faults(r.See(ctl, pw).See(ctl, databaseB), true) - tt.Tick
		}, false},
		"as the excuse starts": {func(r *tt.Script) time.Duration {
			return faults(r.See(ctl, pw).See(ctl, databaseB), true)
		}, true},
		"after it was down from the faults until the excuse started": {func(r *tt.Script) time.Duration {
			excuse := faults(r.See(ctl, pw).See(ctl, databaseB), true)
			return r.Add(tt.Exit("exit status 2", false, "panic: nil map")).Tick(excuse - r.Now).Add(tt.Start(2)).Now
		}, false},
		"stable after a sight once the faults ended": {func(r *tt.Script) time.Duration {
			faults(r.See(ctl, pw).See(ctl, databaseB), true)
			return r.See(ctl, pw).See(ctl, databaseB).Now - tt.Tick + r.Spec.Stable
		}, true},
		"stable after a sight, in faults that altered no request": {func(r *tt.Script) time.Duration {
			sight := r.See(ctl, pw).See(ctl, databaseB).Now - tt.Tick
			faults(r, false)
			return sight + r.Spec.Stable
		}, true},
		"in faults that began as stable passed": {func(r *tt.Script) time.Duration {
			r.See(ctl, pw).See(ctl, databaseB).Tick(r.Spec.Stable - tt.Tick)
			return faults(r, true) - tt.Tick
		}, false},
		"in faults that began once stable passed": {func(r *tt.Script) time.Duration {
			r.See(ctl, pw).See(ctl, databaseB).Tick(r.Spec.Stable)
			return faults(r, true) - tt.Tick
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			release := c.release(r)
			findings := judgeWith(t, r.Tick(release-r.Now).Delete(trace.Harness, pw).Settle(), check.Ordered)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

// A step whose faults never ended, as in an interrupted run's trace, bounds
// no backoff that they caused.
func TestOrderedPassesAReleaseInFaultsThatNeverEnded(t *testing.T) {
	r := secretRun(blockRef(), secret(true))
	r.See(ctl, pw).See(ctl, databaseB).Add(tt.Fault(r.Step, 0, false, secretsFail)).Add(injected("update", pw, 500, "error(500)")).
		Tick(500 * ms).Add(tt.Fault(r.Step, 0, true, secretsFail))
	requireNone(t, judgeWith(t, r.Tick(10*r.Spec.Stable).Delete(trace.Harness, pw), check.Ordered))
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
		"an object it saw only once its deletion started": func(r *tt.Script) {
			r.See(ctl, databaseB).Tick(r.Spec.Stable).Update(trace.Harness, pw, tt.Deleting(secret(true))).See(ctl, pw).Tick(r.Spec.Stable)
		},
		"a CR it saw naming another": func(r *tt.Script) {
			seeBoth(r.Update(trace.Harness, databaseB, database(false, "other"))).Update(trace.Harness, databaseB, database(false, "pw"))
		},
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
	t.Run("a CR made again after the release", func(t *testing.T) {
		r := secretRun(blockRef(), secret(true))
		seeBoth(r).Delete(ctl, pw).Delete(trace.Harness, databaseB)
		r.Create(trace.Harness, databaseB, database(false, "pw"))
		r.Act("create", "b", &databaseB).See(ctl, databaseB)
		findings := ordered(t, r)
		requireFinding(t, findings, "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		if len(findings[0].Details) != 1 {
			t.Errorf("details = %q, want only the first Database's.", findings[0].Details)
		}
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

func TestOrderedPassesACRThatNamesItself(t *testing.T) {
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{databaseRef(r)}
	r.Begin()
	r.Create(trace.Harness, databaseB, databaseOn(true, "b"))
	r.Act("create", "b", &databaseB).See(ctl, databaseB).Tick(r.Spec.Stable)
	r.Delete(ctl, databaseB)
	requireNone(t, ordered(t, r))
}

// Clause 2 needs a cycle of block names among CRs being deleted broken, so
// Ordered passes the release that breaks it. A CR off the cycle still holds
// the object.
func TestOrderedPassesAReleaseThatBreaksACycle(t *testing.T) {
	for name, c := range map[string]struct {
		names    map[string]string
		deleting []string
		holder   string
	}{
		"of CRs being deleted":        {map[string]string{"b": "c", "c": "b"}, []string{"b", "c"}, ""},
		"through a live CR":           {map[string]string{"b": "c", "c": "b"}, []string{"b"}, "slot c"},
		"that a live CR off it names": {map[string]string{"b": "c", "c": "b", "d": "b"}, []string{"b", "c"}, "slot d"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, databases)
			r.Spec.References = []trace.Reference{databaseRef(r)}
			r.Begin()
			for _, slot := range slices.Sorted(maps.Keys(c.names)) {
				ref := tt.Ref("toy/databases", "ns", slot)
				r.Create(trace.Harness, ref, databaseNaming(slot, true, c.names[slot]))
				r.Act("create", slot, &ref).See(ctl, ref)
			}
			r.Tick(r.Spec.Stable)
			for _, slot := range c.deleting {
				r.Update(trace.Harness, tt.Ref("toy/databases", "ns", slot), tt.Deleting(databaseNaming(slot, true, c.names[slot])))
			}
			findings := ordered(t, r.Delete(ctl, databaseB))
			if c.holder == "" {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Ordered", 0, released, "kube/toy/databases", "databases.toy ns/b: "+c.holder+" names it")
			if len(findings[0].Details) != 1 {
				t.Errorf("details = %q, want only %s's.", findings[0].Details, c.holder)
			}
		})
	}
}

// After a relist, the observer records a delete with the resourceVersion of
// the version before it, so the trace does not say when the release came.
func TestOrderedPassesAReleaseRecordedFromARelist(t *testing.T) {
	r := secretRun(blockRef(), secret(true))
	seeBoth(r).Add(tt.Version(pw, r.UID(pw), "100", tt.Deleting(secret(true))))
	r.Add(tt.Version(databaseB, r.UID(databaseB), "105", database(false, "other")))
	r.Add(tt.Deleted(pw, r.UID(pw), "100", tt.Deleting(secret(true))))
	requireNone(t, ordered(t, r))
}

// The controller saw a version of the CR that the observer missed, so the
// trace may lack the move off the object. A version after the release does
// not matter.
func TestOrderedPassesAReleaseAfterACRVersionTheObserverMissed(t *testing.T) {
	for name, c := range map[string]struct {
		missed  string
		finding bool
	}{
		"before the release": {"105", false},
		"after the release":  {"115", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(blockRef(), secret(true))
			seeBoth(r).Add(tt.Deleted(pw, r.UID(pw), "110", secret(true)))
			r.Add(tt.Seen(ctl, databaseB, r.UID(databaseB), c.missed))
			r.Add(tt.Version(databaseB, r.UID(databaseB), "120", database(false, "other")))
			findings := ordered(t, r)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Ordered", 0, released, "kube/secrets", "secrets ns/pw: slot b names it")
		})
	}
}

func TestOrderedReportsAReleaseInItsStepWithEachCRThatNamesIt(t *testing.T) {
	r := secretRun(blockRef(), secret(true))
	body := database(false, "pw")
	body["metadata"] = map[string]any{"name": "c"}
	r.Create(trace.Harness, databaseC, body)
	// The deletion starts in step 0, and the release is in step 1.
	seeBoth(r.Act("create", "c", &databaseC).See(ctl, databaseC)).Update(trace.Harness, pw, tt.Deleting(secret(true))).EndFaults().Settle()
	release := r.Begin().Now
	findings := ordered(t, r.Delete(ctl, pw))
	requireFinding(t, findings, "Ordered", 1, released, "kube/secrets", "secrets ns/pw: slot b names it", "secrets ns/pw: slot c names it")
	if findings[0].At != release {
		t.Errorf("at = %v, want the release at %v.", findings[0].At, release)
	}
}

func TestOrderedReportsAStepsReleasesOfAKindInOneFinding(t *testing.T) {
	r := secretRun(blockRef(), secret(true))
	var objects []trace.Ref
	for n, slot := range []string{"c", "d"} {
		object, db := tt.Ref("secrets", "ns", fmt.Sprint("pw", n+2)), tt.Ref("toy/databases", "ns", slot)
		body := database(false, object.Name)
		body["metadata"] = map[string]any{"name": slot}
		r.Create(trace.Harness, object, secret(true))
		r.Create(trace.Harness, db, body)
		r.Act("create", slot, &db).See(ctl, object).See(ctl, db)
		objects = append(objects, object)
	}
	seeBoth(r)
	first := r.Now
	findings := ordered(t, r.Delete(ctl, objects[0]).Delete(ctl, pw).Delete(ctl, objects[1]))
	requireFinding(t, findings, "Ordered", 0, released, "kube/secrets",
		"secrets ns/pw: slot b names it", "secrets ns/pw2: slot c names it", "secrets ns/pw3: slot d names it")
	if findings[0].At != first {
		t.Errorf("at = %v, want the first release at %v.", findings[0].At, first)
	}
}

// The deleting Cluster reports itself not ready, as toy-fleet's does. After
// a stop, clause 2 judges it as a CR the stop waited for.
func TestACRWaitingInDeletionForACRThatNamesItSettles(t *testing.T) {
	for name, c := range map[string]struct {
		block bool
		after func(r *tt.Script)
		unmet []string
	}{
		"named by a live CR":  {true, func(*tt.Script) {}, nil},
		"named without block": {false, func(*tt.Script) {}, []string{"clusters.toy ns/a is being deleted"}},
		"named by a CR being deleted": {true, func(r *tt.Script) {
			r.Update(trace.Harness, databaseB, tt.Deleting(databaseOn(true, "a")))
		}, []string{"databases.toy ns/b is being deleted"}},
		"after its last CR left": {true, func(r *tt.Script) { r.DeleteCR("b", databaseB) }, []string{"clusters.toy ns/a is being deleted"}},
		"after its CR named another": {true, func(r *tt.Script) {
			r.Update(trace.Harness, databaseB, databaseOn(true, "c"))
		}, []string{"clusters.toy ns/a is being deleted"}},
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
					r.Stop(r.UID(clusterA), r.UID(databaseB))
				}
				r.EndFaults()
				if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, c.unmet) {
					t.Errorf("unmet = %q, want %q.", unmet, c.unmet)
				}
			})
		}
	}
}

// The controller may write the Cluster as it starts to wait, so clause 7
// must see stable after the deletion starts.
func TestACRWaitingInDeletionSettlesStableAfterItsDeletionStarted(t *testing.T) {
	for name, deleteA := range map[string]func(r *tt.Script) (start time.Duration){
		"after its creation": func(r *tt.Script) time.Duration {
			r.CreateCR("a", clusterA)
			r.Ready(clusterA).Tick(5 * s)
			start := r.Now
			r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", true)))
			return start
		},
		"seen first deleting, after another instance": func(r *tt.Script) time.Duration {
			r.CreateCR("a", clusterA)
			r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", true))).Delete(ctl, clusterA).Tick(5 * s)
			start := r.Now
			r.Create(trace.Harness, clusterA, tt.Deleting(tt.CR("a", true)))
			return start
		},
	} {
		for _, stop := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, stopped %t", name, stop), func(t *testing.T) {
				r := fleetRun(1, clusters, databases)
				r.Spec.References = []trace.Reference{clusterRef(r, true)}
				r.Begin()
				r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
				r.Act("create", "b", &databaseB).EndFaults()
				start := deleteA(r)
				r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", false)))
				if stop {
					r.Stop(r.UID(clusterA))
				}
				want := []string{fmt.Sprintf("clusters.toy ns/a waits in deletion, which counts as settled only from %v", start+r.Spec.Stable)}
				if unmet := settledAt(t, r, start+r.Spec.Stable-tt.Tick); !slices.Equal(unmet, want) {
					t.Errorf("Just before stable: unmet = %q, want %q.", unmet, want)
				}
				if unmet := settledAt(t, r, start+r.Spec.Stable); len(unmet) != 0 {
					t.Errorf("At stable: unmet = %q, want none.", unmet)
				}
			})
		}
	}
}

// clustersStatusFail is a step's fault that fails the controller's writes of
// Cluster status.
const clustersStatusFail = "error(500) on group=toy.reconciler-fuzzer resource=clusters subresource=status"

// A fault failed the controller's write of the Cluster as it started to wait,
// so it writes the Cluster again after a backoff, which clause 7 must see.
func TestACRWaitingInDeletionSettlesOnceTheStepsExcuseStarts(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Ready(clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
	r.Act("create", "b", &databaseB)
	r.Add(tt.Fault(r.Step, 0, false, clustersStatusFail))
	r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", true))).Act("delete", "a", &clusterA)
	r.Add(onStatus(injected("update", clusterA, 500, "error(500)")))
	r.Tick(r.Spec.Stable).Add(tt.Fault(r.Step, 0, true, clustersStatusFail))
	excuse := endFaults(r, 2*s)
	want := []string{fmt.Sprintf("clusters.toy ns/a waits in deletion, which counts as settled only from %v", excuse)}
	if unmet := settledAt(t, r, excuse-tt.Tick); !slices.Equal(unmet, want) {
		t.Errorf("Just before the excuse: unmet = %q, want %q.", unmet, want)
	}
	if unmet := settledAt(t, r, excuse); len(unmet) != 0 {
		t.Errorf("At the excuse: unmet = %q, want none.", unmet)
	}
}

// While a step's faults go on, as at the end of an interrupted run's trace,
// the controller may wait in retry backoff for as long as they last.
func TestACRWaitingInDeletionDoesNotSettleBeforeTheStepsFaultsEnd(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Ready(clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
	r.Act("create", "b", &databaseB)
	r.Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", false))).Act("delete", "a", &clusterA)
	want := []string{"clusters.toy ns/a waits in deletion, which counts as settled only once the step's faults end"}
	if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, want) {
		t.Errorf("unmet = %q, want %q.", unmet, want)
	}
}

// CRs being deleted that name each other wait for each other for good. A CR
// on such a cycle still waits for a CR off it that names it.
func TestACRWaitingInDeletionOnACycleDoesNotSettle(t *testing.T) {
	for name, c := range map[string]struct {
		names    map[string]string
		deleting []string
		unmet    []string
	}{
		"a CR that names itself": {map[string]string{"b": "b"}, []string{"b"}, []string{"databases.toy ns/b is being deleted"}},
		"two CRs being deleted that name each other": {map[string]string{"b": "c", "c": "b"}, []string{"b", "c"},
			[]string{"databases.toy ns/b is being deleted", "databases.toy ns/c is being deleted"}},
		"a CR being deleted that names a live CR that names it": {map[string]string{"b": "c", "c": "b"}, []string{"b"}, nil},
		"a cycle through a live CR":                             {map[string]string{"b": "c", "c": "d", "d": "b"}, []string{"b", "d"}, nil},
		"a cycle that a live CR names":                          {map[string]string{"b": "c", "c": "d", "d": "c"}, []string{"c", "d"}, []string{"databases.toy ns/d is being deleted"}},
		"a cycle that a CR being deleted names": {map[string]string{"b": "c", "c": "d", "d": "c"}, []string{"b", "c", "d"},
			[]string{"databases.toy ns/b is being deleted", "databases.toy ns/d is being deleted"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, databases)
			r.Spec.References = []trace.Reference{databaseRef(r)}
			r.Begin()
			for _, slot := range slices.Sorted(maps.Keys(c.names)) {
				ref := tt.Ref("toy/databases", "ns", slot)
				r.Create(trace.Harness, ref, databaseNaming(slot, false, c.names[slot]))
				r.Act("create", slot, &ref).Update(ctl, ref, databaseNaming(slot, true, c.names[slot]))
			}
			for _, slot := range c.deleting {
				r.Update(trace.Harness, tt.Ref("toy/databases", "ns", slot), tt.Deleting(databaseNaming(slot, true, c.names[slot])))
			}
			r.EndFaults()
			unmet := settledAt(t, r, r.Now+5*s)
			slices.Sort(unmet)
			if !slices.Equal(unmet, c.unmet) {
				t.Errorf("unmet = %q, want %q.", unmet, c.unmet)
			}
		})
	}
}

func TestAStepThatEndsBeforeItsCRInUseWaitedStableDidNotSettleByClause2(t *testing.T) {
	r := fleetRun(2, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(true, "a"))
	r.Act("create", "b", &databaseB)
	r.Ready(clusterA).EndFaults().Settle()
	r.Begin().Update(trace.Harness, clusterA, tt.Deleting(tt.CR("a", true))).Act("delete", "a", &clusterA)
	deadline := r.Now + r.Spec.Stable/2
	r.Add(tt.EndFaults(r.Step, deadline, false))
	r.Now = deadline
	r.Add(tt.Phase(r.Step, trace.TimedOut))
	findings, _ := convergeOf(t, r)
	requireFinding(t, findings, "Converge", 1, "did not settle (clause 2)", "kube/toy/clusters", "waits in deletion, which counts as settled only from")
}

func TestHonestJudgesAnObjectWaitingInDeletionByItsLookOrItsVersionBefore(t *testing.T) {
	for name, c := range map[string]struct{ block, wasReady, readyInDeletion, finding bool }{
		"under block, once ready":               {true, true, false, false},
		"under block, never ready":              {true, false, false, true},
		"without block, once ready":             {false, true, false, true},
		"under block, ready only in deletion":   {true, false, true, false},
		"without block, ready only in deletion": {false, false, true, false},
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
			// The look shows the second of two versions that wait in deletion.
			r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).Update(ctl, clusterA, tt.Deleting(tt.CR("a", c.readyInDeletion))).See(ctl, clusterA)
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

func TestHonestJudgesAnObjectCreatedReadyByItsFirstVersion(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.Create(trace.Harness, clusterA, tt.CR("a", true))
	r.Act("create", "a", &clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
	r.Act("create", "b", &databaseB)
	r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).See(ctl, clusterA)
	r.Update(ctl, databaseB, databaseOn(true, "a"))
	requireNone(t, honest(t, r))
}

// Converge counts the same look ready, so it waits for the CR that names
// the object.
func TestSettleWaitsForACRWhileWhatItNamesWaitsInDeletionReady(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
	r.Act("create", "b", &databaseB)
	r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", true))).See(ctl, clusterA).EndFaults()
	if unmet := settledAt(t, r, r.Now+5*s); !waitsForB(unmet) {
		t.Errorf("unmet = %q, want the Database not ready.", unmet)
	}
}

func TestHonestJudgesALookBeforeTheDeletionAsItIs(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
	r.Act("create", "b", &databaseB).See(ctl, clusterA)
	r.Update(ctl, databaseB, databaseOn(true, "a"))
	r.Ready(clusterA).Update(ctl, clusterA, tt.Deleting(tt.CR("a", false)))
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
}

// An object first recorded waiting in deletion has no version before its
// deletion, so Honest judges the look as it is.
func TestHonestJudgesAnObjectFirstRecordedInDeletionAsItIs(t *testing.T) {
	for name, ready := range map[string]bool{"ready": true, "not ready": false} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{clusterRef(r, true)}
			r.Begin()
			r.Create(trace.Harness, clusterA, tt.Deleting(tt.CR("a", ready)))
			r.Act("create", "a", &clusterA)
			r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
			r.Act("create", "b", &databaseB).See(ctl, clusterA)
			r.Update(ctl, databaseB, databaseOn(true, "a"))
			findings := honest(t, r)
			if ready {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
		})
	}
}

func TestHonestUnderBlockFailsACRReadyAfterSeeingWhatItNamesReleased(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{clusterRef(r, true)}
	r.Begin()
	r.CreateCR("a", clusterA)
	r.Ready(clusterA)
	r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
	r.Act("create", "b", &databaseB)
	r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).Delete(ctl, clusterA).SeeDeleted(ctl, clusterA)
	r.Update(ctl, databaseB, databaseOn(true, "a"))
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/clusters", "slot b: clusters.toy ns/a seen missing")
}

// The controller saw the Cluster only while it waited in deletion, and then
// saw it released. A reconcile that read the cache before that wrote the
// Database ready, and the controller corrected the write in time.
func TestHonestExcusesAStaleReadyWriteOnAnObjectThatWaitedInDeletion(t *testing.T) {
	for name, block := range map[string]bool{"under block": true, "without block": false} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{clusterRef(r, block)}
			r.Begin()
			r.CreateCR("a", clusterA)
			r.Ready(clusterA)
			r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
			r.Act("create", "b", &databaseB)
			r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).See(ctl, clusterA)
			r.Delete(ctl, clusterA).SeeDeleted(ctl, clusterA)
			r.UpdateInFlight(ctl, databaseB, databaseOn(true, "a"), 500*ms).Tick(500*ms).Update(ctl, databaseB, databaseOn(false, "a"))
			findings := honest(t, r)
			if block {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, noDependency, "kube/toy/clusters", "slot b: clusters.toy ns/a seen missing")
		})
	}
}

func TestHonestWithTrackPassesACRReadyWhileWhatItNamesWaitsInDeletion(t *testing.T) {
	for name, block := range map[string]bool{"under block": true, "without block": false} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			tracked := clusterRef(r, block)
			tracked.Track = 500 * ms
			r.Spec.References = []trace.Reference{tracked}
			r.Begin()
			r.CreateCR("a", clusterA)
			r.Ready(clusterA).See(ctl, clusterA)
			r.Create(trace.Harness, databaseB, databaseOn(false, "a"))
			r.Act("create", "b", &databaseB)
			r.Update(ctl, databaseB, databaseOn(true, "a"))
			r.Update(ctl, clusterA, tt.Deleting(tt.CR("a", false))).See(ctl, clusterA)
			findings := honest(t, r)
			if block {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, stayedReady, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready at")
		})
	}
}
