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

// heldBody is a body named name with finalizers, and a deletionTimestamp
// when deleting.
func heldBody(name string, deleting bool, finalizers ...string) map[string]any {
	metadata := map[string]any{"name": name, "finalizers": finalizers}
	if deleting {
		metadata["deletionTimestamp"] = "2026-10-20T00:00:00Z"
	}
	return map[string]any{"metadata": metadata}
}

func held(deleting bool, finalizers ...string) map[string]any {
	return heldBody("a", deleting, finalizers...)
}

// write is a write by caller of ref whose answer carries rv and finalizers,
// as a kube listener records it.
func write(caller trace.Caller, verb string, ref trace.Ref, rv string, finalizers ...string) trace.Event {
	e := tt.Call(caller, verb, ref, 200, rv)
	e.Call.ResultFinalizers = &finalizers
	return e
}

// heldGadget starts step 0, in which the controller creates slot a's
// Gadget and its model puts protect on it at resourceVersion "10".
func heldGadget() (*tt.Script, string) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, held(false))
	r.Add(write(gadgetModel, "patch", gadgetA, "10", protect), tt.Version(gadgetA, uid, "10", held(false, protect)))
	return r, uid
}

// release has the controller delete slot a's Gadget, which protect holds,
// and writer write it with no finalizers. The API server deletes it instead:
// its answer carries the last resourceVersion, and its deleted version the
// last body.
func release(r *tt.Script, uid string, writer trace.Caller) {
	r.Add(write(ctl, "delete", gadgetA, "d1", protect), tt.Version(gadgetA, uid, "d1", held(true, protect)))
	r.Add(write(writer, "update", gadgetA, "d1"), tt.Deleted(gadgetA, uid, "d2", held(true, protect)))
}

func TestFinalizersFailsAControllerWriteThatDropsAModelsFinalizer(t *testing.T) {
	for name, drop := range map[string]func(r *tt.Script, uid string){
		"an update": func(r *tt.Script, uid string) {
			r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", held(false)))
		},
		"a patch": func(r *tt.Script, uid string) {
			r.Add(write(ctl, "patch", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", held(false)))
		},
		"an update that releases the deleted object": func(r *tt.Script, uid string) {
			release(r, uid, ctl)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := heldGadget()
			drop(r, uid)
			ix := r.Index()
			requireFinding(t, check.Finalizers(ix), "Finalizers", 0, removed, "kube/toy/gadgets",
				"gadgets.toy ns/a: "+protect+", added by model:gadgets.toy")
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
			r.Add(write(ctl, "update", gadgetA, "c2"), tt.Version(gadgetA, uid, "c2", held(false)))
		},
		"with an update that releases the deleted object": func(r *tt.Script, uid string) {
			r.Add(write(trace.Harness, "delete", gadgetA, "d1", own), tt.Version(gadgetA, uid, "d1", held(true, own)))
			r.Add(write(ctl, "update", gadgetA, "d1"), tt.Deleted(gadgetA, uid, "d2", held(true, own)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRun(1)
			r.Begin()
			uid := r.Create(ctl, gadgetA, held(false))
			r.Add(write(ctl, "update", gadgetA, "c1", own), tt.Version(gadgetA, uid, "c1", held(false, own)))
			// The model's patch keeps the controller's finalizer, so the model is not its adder.
			r.Add(write(gadgetModel, "patch", gadgetA, "m1", own), tt.Version(gadgetA, uid, "m1", held(false, own)))
			remove(r, uid)
			requireNone(t, check.Finalizers(r.Index()))
		})
	}
}

// The API server deletes an object once its last finalizer goes.
func TestFinalizersPassesAModelThatReleasesTheDeletedObject(t *testing.T) {
	r, uid := heldGadget()
	release(r, uid, gadgetModel)
	requireNone(t, check.Finalizers(r.Index()))
}

func TestFinalizersPassesANoOpWriteAfterAnotherCallerRemovedTheFinalizer(t *testing.T) {
	for name, removal := range map[string][]trace.Event{
		"in a recorded call":  {write(gadgetModel, "patch", gadgetA, "m1")},
		"in no recorded call": nil,
	} {
		t.Run(name, func(t *testing.T) {
			r, uid := heldGadget()
			r.Add(append(removal, tt.Version(gadgetA, uid, "m1", held(false)))...)
			r.Add(write(ctl, "update", gadgetA, "m1"))
			requireNone(t, check.Finalizers(r.Index()))
		})
	}
}

// A scale write answers with a Scale, whose metadata holds no finalizers.
func TestFinalizersSkipsASubresourceWrite(t *testing.T) {
	r, uid := heldGadget()
	scale := write(ctl, "update", gadgetA, "c1")
	scale.Call.Subresource = "scale"
	r.Add(scale, tt.Version(gadgetA, uid, "c1", held(false, protect)))
	requireNone(t, check.Finalizers(r.Index()))
}

// A delete's propagation policy decides the orphan finalizer.
func TestFinalizersPassesADeleteThatDropsAFinalizer(t *testing.T) {
	r, uid := heldGadget()
	r.Add(write(trace.Harness, "delete", gadgetA, "d1", protect, "orphan"), tt.Version(gadgetA, uid, "d1", held(true, protect, "orphan")))
	r.Add(write(ctl, "delete", gadgetA, "d2", protect), tt.Version(gadgetA, uid, "d2", held(true, protect)))
	requireNone(t, check.Finalizers(r.Index()))
}

// Older traces record no finalizers in calls.
func TestFinalizersSkipsAWriteWithoutRecordedFinalizers(t *testing.T) {
	r, uid := heldGadget()
	r.Add(tt.Call(ctl, "update", gadgetA, 200, "c1"), tt.Version(gadgetA, uid, "c1", held(false)))
	requireNone(t, check.Finalizers(r.Index()))
}

func TestFinalizersSkipsAFinalizerOfAnUnknownWriter(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, held(false))
	r.Add(tt.Version(gadgetA, uid, "x1", held(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c1"), tt.Version(gadgetA, uid, "c1", held(false)))
	requireNone(t, check.Finalizers(r.Index()))
}

func TestFinalizersSkipsIgnoredKinds(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(trace.Harness, lease, heldBody("l", false, protect))
	r.Add(write(ctl, "update", lease, "c1"), tt.Version(lease, uid, "c1", heldBody("l", false)))
	requireNone(t, check.Finalizers(r.Index()))
}

func TestFinalizersNamesTheCallerThatLastAddedTheFinalizer(t *testing.T) {
	r := newRun(1)
	r.Begin()
	uid := r.Create(ctl, gadgetA, held(false))
	r.Add(write(ctl, "update", gadgetA, "c1", protect), tt.Version(gadgetA, uid, "c1", held(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c2"), tt.Version(gadgetA, uid, "c2", held(false)))
	r.Add(write(gadgetModel, "patch", gadgetA, "m1", protect), tt.Version(gadgetA, uid, "m1", held(false, protect)))
	r.Add(write(trace.Harness, "patch", gadgetA, "h1", protect), tt.Version(gadgetA, uid, "h1", held(false, protect)))
	r.Add(write(ctl, "update", gadgetA, "c3"), tt.Version(gadgetA, uid, "c3", held(false)))
	requireFinding(t, check.Finalizers(r.Index()), "Finalizers", 0, removed, "kube/toy/gadgets", "added by model:gadgets.toy")
}

func TestFinalizersPassesAControllerThatKeepsAnotherCallersFinalizer(t *testing.T) {
	r, uid := heldGadget()
	r.Add(write(ctl, "update", gadgetA, "c1", protect, own), tt.Version(gadgetA, uid, "c1", held(false, protect, own)))
	r.Add(write(ctl, "update", gadgetA, "c2", protect), tt.Version(gadgetA, uid, "c2", held(false, protect)))
	requireNone(t, check.Finalizers(r.Index()))
}

func TestFinalizersGivesOneFindingPerStepAndKindInOrderOfFirstRemoval(t *testing.T) {
	gadgetB := tt.Ref("toy/gadgets", "ns", "b")
	r := newRun(2)
	r.Begin()
	a := r.Create(ctl, gadgetA, held(false))
	b := r.Create(ctl, gadgetB, heldBody("b", false))
	c := r.Create(trace.Harness, cm, heldBody("c", false, protect))
	r.Add(write(gadgetModel, "patch", gadgetA, "a1", protect), tt.Version(gadgetA, a, "a1", held(false, protect)))
	r.Add(write(gadgetModel, "patch", gadgetB, "b1", protect), tt.Version(gadgetB, b, "b1", heldBody("b", false, protect)))
	r.Add(write(ctl, "update", gadgetA, "a2"), tt.Version(gadgetA, a, "a2", held(false)))
	r.Add(write(ctl, "update", gadgetB, "b2"), tt.Version(gadgetB, b, "b2", heldBody("b", false)))
	r.Add(write(ctl, "update", cm, "c1"), tt.Version(cm, c, "c1", heldBody("c", false)))
	r.EndFaults().Settle().Begin()
	var firstRemoval time.Duration
	for n, rvs := range [][2]string{{"a3", "a4"}, {"a5", "a6"}} {
		r.Add(write(gadgetModel, "patch", gadgetA, rvs[0], protect), tt.Version(gadgetA, a, rvs[0], held(false, protect)))
		if n == 0 {
			firstRemoval = r.Now
		}
		r.Add(write(ctl, "update", gadgetA, rvs[1]), tt.Version(gadgetA, a, rvs[1], held(false)))
	}
	findings := check.Finalizers(r.Index())
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
