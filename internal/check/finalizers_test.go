package check_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const (
	protect = "storage.example/protect"
	own     = "toy/cleanup"
	removed = "removed another caller's finalizer"
)

// objectBody is a body named name with finalizers, and a deletionTimestamp
// when deleting.
func objectBody(name string, deleting bool, finalizers ...string) map[string]any {
	metadata := map[string]any{"name": name, "finalizers": finalizers}
	if deleting {
		metadata["deletionTimestamp"] = "2026-10-20T00:00:00Z"
	}
	return map[string]any{"metadata": metadata}
}

// gadgetBody is the body of slot a's Gadget.
func gadgetBody(deleting bool, finalizers ...string) map[string]any {
	return objectBody("a", deleting, finalizers...)
}

// write is a write by caller of ref whose answer carries rv and finalizers,
// as a kube listener records it.
func write(caller trace.Caller, verb string, ref trace.Ref, rv string, finalizers ...string) trace.Event {
	e := tt.Call(caller, verb, ref, 200, rv)
	e.Call.ResultFinalizers = &finalizers
	return e
}

// sent is e sent latency before its answer.
func sent(e trace.Event, latency time.Duration) trace.Event {
	e.Call.Latency = latency
	return e
}

// heldGadget starts step 0, in which the controller creates slot a's
// Gadget and its model puts protect on it at resourceVersion "10".
func heldGadget() (*tt.Script, string) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, gadgetBody(false))
	r.Add(write(gadgetModel, "patch", gadgetA, "10", protect), tt.Version(gadgetA, uid, "10", gadgetBody(false, protect)))
	return r, uid
}

// release has the controller delete slot a's Gadget, which protect holds,
// and writer write it with no finalizers. The API server deletes it instead:
// its answer carries the last resourceVersion, and its deleted version the
// last body.
func release(r *tt.Script, uid string, writer trace.Caller) {
	r.Add(write(ctl, "delete", gadgetA, "d1", protect), tt.Version(gadgetA, uid, "d1", gadgetBody(true, protect)))
	r.Add(write(writer, "update", gadgetA, "d1"), tt.Deleted(gadgetA, uid, "d2", gadgetBody(true, protect)))
}

func finalizerFindings(t *testing.T, r *tt.Script) []check.Finding {
	t.Helper()
	return judgeWith(t, r, check.Finalizers)
}

func TestFinalizersFailsAControllerWriteThatDropsAModelsFinalizer(t *testing.T) {
	for name, c := range map[string]struct {
		drop func(r *tt.Script, uid string)
		verb string
	}{
		"an update": {func(r *tt.Script, uid string) {
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
		}, "update"},
		"a patch": {func(r *tt.Script, uid string) {
			r.Add(write(ctl, "patch", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
		}, "patch"},
		// The observer may record a write's version before its answer.
		"a patch answered after its version": {func(r *tt.Script, uid string) {
			r.Add(tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
			r.Add(sent(write(ctl, "patch", gadgetA, "c1"), 2*tt.Tick))
		}, "patch"},
		"an update that releases the deleted object": {func(r *tt.Script, uid string) {
			release(r, uid, ctl)
		}, "update"},
		"an update after a get, whose answer the listener does not read": {func(r *tt.Script, uid string) {
			r.Add(tt.Call(ctl, "get", gadgetA, 200, ""))
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
		}, "update"},
		"an update before an answer whose version the observer missed": {func(r *tt.Script, uid string) {
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
			r.Add(write(gadgetModel, "patch", gadgetA, "m1", protect))
		}, "update"},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := heldGadget()
			c.drop(r, uid)
			requireFinding(t, finalizerFindings(t, r), "Finalizers", 0, removed, "kube/toy/gadgets",
				"gadgets.toy ns/a: "+protect+", added by model:gadgets.toy, removed by "+c.verb)
			ix := r.Index()
			spec, err := check.SpecFor(nil, ix)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(check.Judge(ix, spec).Findings, func(f check.Finding) bool { return f.Check == "Finalizers" }) {
				t.Error("Judge gives no Finalizers finding.")
			}
		})
	}
}

func TestFinalizersPassesAControllerThatRemovesItsOwnFinalizer(t *testing.T) {
	for name, remove := range map[string]func(r *tt.Script, uid string){
		"with an update": func(r *tt.Script, uid string) {
			r.Add(write(ctl, "update", gadgetA, "c2"), tt.Version(gadgetA, uid, "c2", gadgetBody(false)))
		},
		"with an update that releases the deleted object": func(r *tt.Script, uid string) {
			r.Add(write(trace.Harness, "delete", gadgetA, "d1", own), tt.Version(gadgetA, uid, "d1", gadgetBody(true, own)))
			r.Add(write(ctl, "update", gadgetA, "d1"), tt.Deleted(gadgetA, uid, "d2", gadgetBody(true, own)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(1)
			r.Begin()
			uid := r.Create(ctl, gadgetA, gadgetBody(false))
			r.Add(write(ctl, "update", gadgetA, "c1", own), tt.Version(gadgetA, uid, "c1", gadgetBody(false, own)))
			// The model's patch keeps the controller's finalizer, so the model is not its adder.
			r.Add(write(gadgetModel, "patch", gadgetA, "m1", own), tt.Version(gadgetA, uid, "m1", gadgetBody(false, own)))
			remove(r, uid)
			requireNone(t, finalizerFindings(t, r))
		})
	}
}

// Listeners record answers as they end, in no fixed order. Here the model's
// no-op, sent after the controller's write, is recorded first.
func TestFinalizersPassesTheControllersFinalizerWhoseVersionAnotherNoOpAnsweredFirst(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, gadgetBody(false))
	r.Tick(5 * tt.Tick)
	r.Add(sent(write(gadgetModel, "patch", gadgetA, "c1", own), tt.Tick))
	r.Add(sent(write(ctl, "update", gadgetA, "c1", own), 3*tt.Tick), tt.Version(gadgetA, uid, "c1", gadgetBody(false, own)))
	r.Add(write(ctl, "update", gadgetA, "c2"), tt.Version(gadgetA, uid, "c2", gadgetBody(false)))
	requireNone(t, finalizerFindings(t, r))
}

// A sample, a fixture or a plan's update may carry the controller's own
// finalizer.
func TestFinalizersPassesAFinalizerTheHarnessWrote(t *testing.T) {
	for name, add := range map[string]func(r *tt.Script) string{
		"in a create": func(r *tt.Script) string { return r.Create(trace.Harness, gadgetA, gadgetBody(false, own)) },
		"in a patch": func(r *tt.Script) string {
			uid := r.Create(trace.Harness, gadgetA, gadgetBody(false))
			r.Add(write(trace.Harness, "patch", gadgetA, "h1", own), tt.Version(gadgetA, uid, "h1", gadgetBody(false, own)))
			return uid
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(1)
			r.Begin()
			uid := add(r)
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
			requireNone(t, finalizerFindings(t, r))
		})
	}
}

// The API server adds foregroundDeletion or orphan for whoever deletes, but
// only the garbage collector may remove them.
func TestFinalizersFailsAControllerWriteThatDropsTheGarbageCollectorsFinalizer(t *testing.T) {
	for name, c := range map[string]struct {
		deleter   trace.Caller
		finalizer string
	}{
		"after the controller's Foreground delete": {ctl, "foregroundDeletion"},
		"after the harness's Foreground delete":    {trace.Harness, "foregroundDeletion"},
		"after the controller's Orphan delete":     {ctl, "orphan"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(1)
			r.Begin()
			uid := r.Create(ctl, gadgetA, gadgetBody(false))
			r.Add(write(ctl, "update", gadgetA, "c1", own), tt.Version(gadgetA, uid, "c1", gadgetBody(false, own)))
			deleting := gadgetBody(true, own, c.finalizer)
			r.Add(write(c.deleter, "delete", gadgetA, "d1", own, c.finalizer), tt.Version(gadgetA, uid, "d1", deleting))
			r.Add(write(ctl, "update", gadgetA, "d1"), tt.Deleted(gadgetA, uid, "d2", deleting))
			findings := finalizerFindings(t, r)
			requireFinding(t, findings, "Finalizers", 0, removed, "kube/toy/gadgets",
				"gadgets.toy ns/a: "+c.finalizer+", added by gc, removed by update")
			if details := findings[0].Details; len(details) != 1 {
				t.Errorf("details = %q, want only the gc's finalizer", details)
			}
		})
	}
}

// The observer misses versions when its watch relists. The trace then lacks
// the changes they made.
func TestFinalizersSkipsAWriteAfterAnAnswerWhoseVersionTheObserverMissed(t *testing.T) {
	for name, race := range map[string]func(r *tt.Script, uid string){
		"after another caller's removal": func(r *tt.Script, uid string) {
			r.Add(write(gadgetModel, "patch", gadgetA, "m1"))
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
		},
		"between two answers": func(r *tt.Script, uid string) {
			r.Add(write(gadgetModel, "patch", gadgetA, "m1"))
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
			r.Add(write(gadgetModel, "patch", gadgetA, "m2", protect))
		},
		"after the controller's add": func(r *tt.Script, uid string) {
			r.Add(write(ctl, "update", gadgetA, "c1", protect, own))
			r.Add(write(gadgetModel, "patch", gadgetA, "m1", protect, own), tt.Version(gadgetA, uid, "m1", gadgetBody(false, protect, own)))
			r.Add(write(ctl, "update", gadgetA, "c2", protect), tt.Version(gadgetA, uid, "c2", gadgetBody(false, protect)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := heldGadget()
			race(r, uid)
			requireNone(t, finalizerFindings(t, r))
		})
	}
}

// A delay fault can hold a write past the end of the step that sent it.
func TestFinalizersGivesAWriteTheStepThatSentIt(t *testing.T) {
	r, uid := heldGadget()
	r.EndFaults().Settle()
	sentAt := r.Now
	r.Tick(tt.Tick).Begin()
	r.Add(sent(write(ctl, "update", gadgetA, "c1"), r.Now-sentAt), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
	requireFinding(t, finalizerFindings(t, r), "Finalizers", 0, removed, "kube/toy/gadgets", protect)
}

// The API server deletes an object once its last finalizer is removed.
func TestFinalizersPassesAModelThatReleasesTheDeletedObject(t *testing.T) {
	r, uid := heldGadget()
	release(r, uid, gadgetModel)
	requireNone(t, finalizerFindings(t, r))
}

// A no-op write answers with the version another caller made. A delay fault
// can hold the write while that caller writes.
func TestFinalizersPassesANoOpWriteAnsweredWithAnotherCallersRemoval(t *testing.T) {
	removal := func(latency time.Duration) trace.Event {
		return sent(write(gadgetModel, "patch", gadgetA, "m1"), latency)
	}
	noOp := func(latency time.Duration) trace.Event { return sent(write(ctl, "patch", gadgetA, "m1"), latency) }
	for name, race := range map[string]func(r *tt.Script, m1 trace.Event){
		"after a recorded removal": func(r *tt.Script, m1 trace.Event) {
			r.Add(removal(0), m1)
			r.Add(noOp(0))
		},
		"after an unrecorded removal": func(r *tt.Script, m1 trace.Event) {
			r.Add(m1)
			r.Add(noOp(0))
		},
		"during a recorded removal": func(r *tt.Script, m1 trace.Event) {
			r.Add(removal(0), m1)
			r.Add(noOp(2 * tt.Tick))
		},
		"recorded before the removal's answer": func(r *tt.Script, m1 trace.Event) {
			r.Add(noOp(2 * tt.Tick))
			r.Add(removal(2*tt.Tick), m1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := heldGadget()
			race(r, tt.Version(gadgetA, uid, "m1", gadgetBody(false)))
			requireNone(t, finalizerFindings(t, r))
		})
	}
}

// A no-op sent after the removal's answer made neither the version it
// carries nor a second removal.
func TestFinalizersReportsARemovalOnceThoughALaterNoOpCarriesItsVersion(t *testing.T) {
	for _, noOp := range []trace.Caller{gadgetModel, ctl} {
		t.Run(string(noOp), func(t *testing.T) {
			r, uid := heldGadget()
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
			r.Add(write(noOp, "patch", gadgetA, "c1"))
			findings := finalizerFindings(t, r)
			requireFinding(t, findings, "Finalizers", 0, removed, "kube/toy/gadgets", protect+", added by model:gadgets.toy, removed by update")
			if details := findings[0].Details; len(details) != 1 {
				t.Errorf("details = %q, want only the update's removal", details)
			}
		})
	}
}

// A built-in kind's status write can change its finalizers.
func TestFinalizersFailsAStatusWriteThatDropsAFinalizer(t *testing.T) {
	service := tt.Ref("services", "ns", "s")
	r := newRun(1)
	r.Begin()
	uid := r.Create(trace.Harness, service, objectBody("s", false))
	r.Add(write(trace.ModelCaller("", "services"), "patch", service, "m1", protect), tt.Version(service, uid, "m1", objectBody("s", false, protect)))
	status := write(ctl, "update", service, "c1")
	status.Call.Subresource = "status"
	r.Add(status, tt.Version(service, uid, "c1", objectBody("s", false)))
	requireFinding(t, finalizerFindings(t, r), "Finalizers", 0, removed, "kube/services",
		"services ns/s: "+protect+", added by model:services, removed by status update")
}

// The API server keeps a CRD's metadata on a status write, so the write
// removes no finalizer. Here the trace lacks the removal.
func TestFinalizersSkipsAStatusWriteOfAnInstalledCRD(t *testing.T) {
	r, uid := heldGadget()
	r.Spec.StatusKinds = []trace.Kind{{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}}
	status := write(ctl, "update", gadgetA, "s1")
	status.Call.Subresource = "status"
	r.Add(status, tt.Version(gadgetA, uid, "s1", gadgetBody(false)))
	requireNone(t, finalizerFindings(t, r))
}

// A scale write answers with a Scale, whose metadata holds no finalizers.
func TestFinalizersSkipsAScaleWrite(t *testing.T) {
	r, uid := heldGadget()
	scale := write(ctl, "update", gadgetA, "c1")
	scale.Call.Subresource = "scale"
	r.Add(scale, tt.Version(gadgetA, uid, "c1", gadgetBody(false, protect)))
	requireNone(t, finalizerFindings(t, r))
}

// A delete's propagation policy decides the orphan finalizer.
func TestFinalizersPassesADeleteThatDropsAFinalizer(t *testing.T) {
	r, uid := heldGadget()
	r.Add(write(trace.Harness, "delete", gadgetA, "d1", protect, "orphan"), tt.Version(gadgetA, uid, "d1", gadgetBody(true, protect, "orphan")))
	r.Add(write(ctl, "delete", gadgetA, "d2", protect), tt.Version(gadgetA, uid, "d2", gadgetBody(true, protect)))
	requireNone(t, finalizerFindings(t, r))
}

// Older traces record no finalizers in calls.
func TestFinalizersSkipsAWriteWithoutRecordedFinalizers(t *testing.T) {
	r, uid := heldGadget()
	r.Add(tt.Call(ctl, "update", gadgetA, 200, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
	requireNone(t, finalizerFindings(t, r))
}

func TestFinalizersSkipsAFinalizerOfAnUnknownWriter(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, gadgetBody(false))
	r.Add(tt.Version(gadgetA, uid, "x1", gadgetBody(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
	requireNone(t, finalizerFindings(t, r))
}

func TestFinalizersSkipsIgnoredKinds(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(trace.Harness, lease, objectBody("l", false))
	r.Add(write(trace.ModelCaller("coordination.k8s.io", "leases"), "patch", lease, "m1", protect), tt.Version(lease, uid, "m1", objectBody("l", false, protect)))
	r.Add(write(ctl, "update", lease, "c1"), tt.Version(lease, uid, "c1", objectBody("l", false)))
	requireNone(t, finalizerFindings(t, r))
}

func TestFinalizersNamesTheCallerThatLastAddedTheFinalizer(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, gadgetBody(false))
	r.Add(write(ctl, "update", gadgetA, "c1", protect), tt.Version(gadgetA, uid, "c1", gadgetBody(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c2"), tt.Version(gadgetA, uid, "c2", gadgetBody(false)))
	r.Add(write(gadgetModel, "patch", gadgetA, "m1", protect), tt.Version(gadgetA, uid, "m1", gadgetBody(false, protect)))
	r.Add(write(trace.Harness, "patch", gadgetA, "h1", protect), tt.Version(gadgetA, uid, "h1", gadgetBody(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c3"), tt.Version(gadgetA, uid, "c3", gadgetBody(false)))
	requireFinding(t, finalizerFindings(t, r), "Finalizers", 0, removed, "kube/toy/gadgets", "added by model:gadgets.toy")
}

// Here the gc's no-op overlaps the model's patch that adds protect.
func TestFinalizersNamesTheFirstRecordedOfTwoPossibleAdders(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, gadgetBody(false))
	r.Tick(5 * tt.Tick)
	r.Add(write(gadgetModel, "patch", gadgetA, "m1", protect), tt.Version(gadgetA, uid, "m1", gadgetBody(false, protect)))
	r.Add(sent(write(trace.GC, "patch", gadgetA, "m1", protect), 2*tt.Tick))
	r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", gadgetBody(false)))
	requireFinding(t, finalizerFindings(t, r), "Finalizers", 0, removed, "kube/toy/gadgets", "added by model:gadgets.toy")
}

func TestFinalizersPassesAControllerThatKeepsAnotherCallersFinalizer(t *testing.T) {
	r, uid := heldGadget()
	r.Add(write(ctl, "update", gadgetA, "c1", protect, own), tt.Version(gadgetA, uid, "c1", gadgetBody(false, protect, own)))
	r.Add(write(ctl, "update", gadgetA, "c2", protect), tt.Version(gadgetA, uid, "c2", gadgetBody(false, protect)))
	requireNone(t, finalizerFindings(t, r))
}

func TestFinalizersGivesOneFindingPerStepAndKindInOrderOfFirstRemoval(t *testing.T) {
	gadgetB := tt.Ref("toy/gadgets", "ns", "b")
	r := newRun(2)
	r.Begin()
	a := r.Create(ctl, gadgetA, gadgetBody(false))
	b := r.Create(ctl, gadgetB, objectBody("b", false))
	c := r.Create(trace.Harness, cm, objectBody("c", false))
	r.Add(write(gadgetModel, "patch", gadgetA, "a1", protect), tt.Version(gadgetA, a, "a1", gadgetBody(false, protect)))
	r.Add(write(gadgetModel, "patch", gadgetB, "b1", protect), tt.Version(gadgetB, b, "b1", objectBody("b", false, protect)))
	r.Add(write(trace.ModelCaller("", "configmaps"), "patch", cm, "c1", protect), tt.Version(cm, c, "c1", objectBody("c", false, protect)))
	r.Add(write(ctl, "update", gadgetA, "a2"), tt.Version(gadgetA, a, "a2", gadgetBody(false)))
	r.Add(write(ctl, "update", gadgetB, "b2"), tt.Version(gadgetB, b, "b2", objectBody("b", false)))
	r.Add(write(ctl, "update", cm, "c2"), tt.Version(cm, c, "c2", objectBody("c", false)))
	r.EndFaults().Settle().Begin()
	var firstRemoval time.Duration
	for n, rvs := range [][2]string{{"a3", "a4"}, {"a5", "a6"}} {
		r.Add(write(gadgetModel, "patch", gadgetA, rvs[0], protect), tt.Version(gadgetA, a, rvs[0], gadgetBody(false, protect)))
		if n == 0 {
			firstRemoval = r.Now
		}
		r.Add(write(ctl, "update", gadgetA, rvs[1]), tt.Version(gadgetA, a, rvs[1], gadgetBody(false)))
	}
	findings := finalizerFindings(t, r)
	if len(findings) != 3 {
		t.Fatalf("findings = %+v, want three", findings)
	}
	if findings[2].At != firstRemoval {
		t.Errorf("Step 1's finding is at %v, want its first removal's %v.", findings[2].At, firstRemoval)
	}
	for n, want := range []struct {
		step    int
		kind    string
		details int
	}{{0, "kube/toy/gadgets", 2}, {0, "kube/configmaps", 1}, {1, "kube/toy/gadgets", 1}} {
		if f := findings[n]; f.Step != want.step || f.Kind != want.kind || len(f.Details) != want.details {
			t.Errorf("finding %d = %+v, want step %d of %s with %d details", n, f, want.step, want.kind, want.details)
		}
	}
}
