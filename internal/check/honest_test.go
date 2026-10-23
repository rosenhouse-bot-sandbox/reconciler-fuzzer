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
	gadgetA     = tt.Ref("toy/gadgets", "ns", "a")
	gadgetModel = trace.ModelCaller("toy", "gadgets")
)

const (
	beforeSeeing = "reported ready before seeing its dependency ready"
	noDependency = "reported ready with no dependency"
	stayedReady  = "stayed ready after seeing its dependency not ready"
)

// gadgetRun starts step 0, in which the harness creates slot a and the
// controller creates its Gadget, which the model then makes ready.
func gadgetRun(dep trace.Dependency) *tt.Script {
	r := newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &dep}}
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Create(ctl, gadgetA, gadget(r, false))
	r.Update(gadgetModel, gadgetA, gadget(r, true))
	return r
}

func gadget(r *tt.Script, ready bool) map[string]any {
	return tt.OwnedBy(tt.ReadyCondition("a", 1, ready), r.UID(widgetA), true)
}

func honest(t *testing.T, r *tt.Script) []check.Finding {
	t.Helper()
	return judgeWith(t, r.EndFaults().Settle(), check.Honest)
}

func TestHonestPassesACRReadyAfterSeeingItsDependencyReady(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA).Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestHonestFailsACRReadyBeforeSeeingItsDependencyReady(t *testing.T) {
	for name, see := range map[string]func(r *tt.Script){
		"not seen": func(r *tt.Script) {},
		"seen not ready": func(r *tt.Script) {
			r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
		},
		"seen before the controller's write": func(r *tt.Script) {
			r.See(ctl, gadgetA).Update(ctl, gadgetA, gadget(r, true))
		},
		"seen a version the trace lacks": func(r *tt.Script) {
			r.Add(tt.Seen(ctl, gadgetA, r.UID(gadgetA), "99"))
		},
		"seen after the CR's write started": func(r *tt.Script) {
			r.UpdateInFlight(ctl, widgetA, tt.CR("a", true), 50*ms).See(ctl, gadgetA)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := gadgetRun(trace.Dependency{Required: true})
			see(r)
			if name != "seen after the CR's write started" {
				r.Ready(widgetA)
			}
			requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "slot a", "gadgets.toy ns/a")
		})
	}
}

func TestHonestCountsAFailedOrNoOpWriteAsNoWrite(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA).Call(ctl, "update", gadgetA, 409).NoOp(ctl, gadgetA).Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestHonestCountsAWriteOfUnknownOutcome(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	dropped := tt.Call(ctl, "update", gadgetA, 0, "")
	dropped.Call.Injected = trace.DropResponse
	r.See(ctl, gadgetA).Add(dropped).Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets")
}

func TestHonestJudgesOnlyTheControllersReadyWrites(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.Update(trace.Harness, widgetA, tt.CR("a", true))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesAnObservedGenerationRaise(t *testing.T) {
	statuses := map[string]func(og int) map[string]any{
		"status.observedGeneration": func(og int) map[string]any {
			return map[string]any{"ready": true, "observedGeneration": og}
		},
		"conditions": func(og int) map[string]any {
			return map[string]any{"ready": true, "conditions": []any{map[string]any{"type": "B", "observedGeneration": og}, map[string]any{"type": "A", "observedGeneration": 1}}}
		},
	}
	for name, status := range statuses {
		for _, og := range []int{1, 2} {
			r := gadgetRun(trace.Dependency{})
			r.See(ctl, gadgetA).Update(ctl, widgetA, map[string]any{"metadata": map[string]any{"name": "a"}, "status": status(1)})
			r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
			r.Update(ctl, widgetA, map[string]any{"metadata": map[string]any{"name": "a", "labels": "x"}, "status": status(og)})
			findings := honest(t, r)
			if og == 1 {
				if len(findings) != 0 {
					t.Errorf("%s: findings = %+v, want none without a raise", name, findings)
				}
				continue
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/gadgets")
		}
	}
}

func TestHonestRequiresADependency(t *testing.T) {
	r := newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Required: true}}}
	r.Begin().CreateCR("a", widgetA)
	r.Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/widgets", "slot a")

	r = newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{}}}
	r.Begin().CreateCR("a", widgetA)
	r.Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestHonestIgnoresARule4Dependency(t *testing.T) {
	r := newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Track: 500 * ms}}}
	r.Begin().CreateCR("a", widgetA)
	r.Create(ctl, gadgetA, tt.ReadyCondition("a", 1, false))
	r.Ready(widgetA).See(ctl, gadgetA)
	requireNone(t, honest(t, r))
}

// degrade has the model make slot a's Gadget not ready, and the controller
// see it, after the controller made slot a ready honestly.
func degrade(track bool) *tt.Script {
	dep := trace.Dependency{}
	if track {
		dep.Track = 500 * ms
	}
	r := gadgetRun(dep)
	r.See(ctl, gadgetA).Ready(widgetA)
	return r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
}

func TestHonestWithTrackFailsACRThatStaysReady(t *testing.T) {
	r := degrade(true)
	seen := r.Now - tt.Tick
	findings := honest(t, r)
	requireFinding(t, findings, "Honest", 0, stayedReady, "kube/toy/gadgets", "slot a", "gadgets.toy ns/a")
	if findings[0].At != seen+500*ms {
		t.Errorf("at = %v, want %v", findings[0].At, seen+500*ms)
	}
	requireNone(t, honest(t, degrade(false)))
}

func TestHonestWithTrackPasses(t *testing.T) {
	for name, then := range map[string]func(r *tt.Script){
		"the CR goes not ready in time": func(r *tt.Script) {
			r.Tick(400*ms).Update(ctl, widgetA, tt.CR("a", false))
		},
		"the controller sees it ready in time": func(r *tt.Script) {
			r.Tick(400*ms).Update(gadgetModel, gadgetA, gadget(r, true)).See(ctl, gadgetA)
		},
		"the controller sees it deleted": func(r *tt.Script) {
			r.Tick(400*ms).SeeDeleted(ctl, gadgetA)
		},
		"it is gone in time": func(r *tt.Script) {
			r.Tick(400*ms).Delete(trace.GC, gadgetA)
		},
		"the controller deletes it in time": func(r *tt.Script) {
			r.Tick(400*ms).Call(ctl, "delete", gadgetA, 200)
		},
		"the trace ends first": func(r *tt.Script) {},
	} {
		t.Run(name, func(t *testing.T) {
			r := degrade(true)
			then(r)
			if name == "the trace ends first" {
				requireNone(t, judgeWith(t, r, check.Honest))
				return
			}
			requireNone(t, honest(t, r))
		})
	}
}

func TestHonestGivesOneFindingPerStepSummaryAndKind(t *testing.T) {
	r := gadgetRun(trace.Dependency{})
	r.Ready(widgetA).Update(ctl, widgetA, tt.CR("a", false)).Ready(widgetA)
	r.EndFaults().Settle().Begin()
	r.Update(ctl, widgetA, tt.CR("a", false)).Ready(widgetA)
	findings := judgeWith(t, r.EndFaults().Settle(), check.Honest)
	if len(findings) != 2 || findings[0].Step != 0 || findings[1].Step != 1 || len(findings[0].Details) != 1 {
		t.Fatalf("findings = %+v, want one in each step", findings)
	}
}

// replace has the controller delete slot a's Gadget, which a finalizer
// holds, and make a Gadget b that it sees ready.
func replace(dep trace.Dependency) *tt.Script {
	gadgetB := tt.Ref("toy/gadgets", "ns", "b")
	r := gadgetRun(dep)
	r.See(ctl, gadgetA).Ready(widgetA)
	terminating := gadget(r, false)
	terminating["metadata"].(map[string]any)["generation"] = 2
	terminating["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-20T00:00:00Z"
	r.Update(ctl, gadgetA, terminating).See(ctl, gadgetA)
	r.Create(ctl, gadgetB, gadget(r, false))
	return r.Update(gadgetModel, gadgetB, gadget(r, true)).See(ctl, gadgetB)
}

func TestHonestSkipsADependencyBeingDeleted(t *testing.T) {
	r := replace(trace.Dependency{Required: true})
	r.Update(ctl, widgetA, tt.CR("a", false)).Ready(widgetA)
	requireNone(t, honest(t, r))
	requireNone(t, honest(t, replace(trace.Dependency{Track: 500 * ms})))
}

func TestHonestPassesAReadyFromAStaleReadOfAGoneDependency(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA).Delete(trace.Harness, gadgetA).Ready(widgetA)
	requireNone(t, honest(t, r))

	r = gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA).Delete(trace.Harness, gadgetA).SeeDeleted(ctl, gadgetA).Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/widgets", "slot a")
}

// The GC deletes slot a's only Gadget, and the controller sees the delete
// before it reports ready. Without required, the CR may be ready with none.
func TestHonestFailsAReadyAfterSeeingItsDependencyDeleted(t *testing.T) {
	for name, order := range map[string]func(r *tt.Script){
		"the observer records the delete first": func(r *tt.Script) {
			r.Delete(trace.GC, gadgetA).SeeDeleted(ctl, gadgetA).Ready(widgetA)
		},
		"the controller sees the delete first": func(r *tt.Script) {
			r.SeeDeleted(ctl, gadgetA).Ready(widgetA).Delete(trace.GC, gadgetA)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := gadgetRun(trace.Dependency{Required: true})
			order(r)
			requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/widgets", "slot a")

			r = gadgetRun(trace.Dependency{})
			order(r)
			requireNone(t, honest(t, r))
		})
	}
}

// TestHonestSkipsADependencyTheControllerDeleted scales slot a down. The
// controller deletes Gadget b, which it saw not ready, and then reports
// ready on Gadget a. It sends the delete before the ready write. Each
// subtest names the delete's events that come before the ready write: its
// response, its watch event and its observer record.
func TestHonestSkipsADependencyTheControllerDeleted(t *testing.T) {
	gadgetB := tt.Ref("toy/gadgets", "ns", "b")
	response := func(latency time.Duration) trace.Event {
		e := tt.Call(ctl, "delete", gadgetB, 200, "90")
		e.Call.Latency = latency
		return e
	}
	watched := func(r *tt.Script) trace.Event {
		e := tt.Seen(ctl, gadgetB, r.UID(gadgetB), "90")
		e.Seen.Deleted = true
		return e
	}
	recorded := func(r *tt.Script) trace.Event { return tt.Deleted(gadgetB, r.UID(gadgetB), "90", gadget(r, false)) }
	for name, order := range map[string]func(r *tt.Script){
		"the response": func(r *tt.Script) {
			r.Add(response(0)).Ready(widgetA).Add(watched(r), recorded(r))
		},
		"the response and watch event": func(r *tt.Script) {
			r.Add(response(0), watched(r)).Ready(widgetA).Add(recorded(r))
		},
		"the response and observer record": func(r *tt.Script) {
			r.Add(response(0), recorded(r)).Ready(widgetA).Add(watched(r))
		},
		"all three": func(r *tt.Script) {
			r.Add(response(0), watched(r), recorded(r)).Ready(widgetA)
		},
		"the observer record": func(r *tt.Script) {
			r.Add(recorded(r)).Ready(widgetA).Add(response(3*tt.Tick), watched(r))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := gadgetRun(trace.Dependency{Required: true})
			r.See(ctl, gadgetA)
			r.Create(ctl, gadgetB, gadget(r, false))
			r.See(ctl, gadgetB)
			order(r)
			requireNone(t, honest(t, r))
		})
	}
}

// A delete's response can be lost after the server deletes the object.
func TestHonestSkipsADependencyTheControllerDeletedWithAnUnknownOutcome(t *testing.T) {
	r := gadgetRun(trace.Dependency{})
	r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
	dropped := tt.Call(ctl, "delete", gadgetA, 200, "90")
	dropped.Call.Injected = trace.DropResponse
	r.Add(dropped).Ready(widgetA)
	requireNone(t, honest(t, r))
}

// The model makes slot a's Gadget not ready, and the controller sees it.
// Then the controller sends a DELETE that does not delete that Gadget before
// it reports ready.
func TestHonestCountsADependencyTheControllerDidNotDelete(t *testing.T) {
	for name, then := range map[string]func(r *tt.Script){
		"the delete failed": func(r *tt.Script) {
			r.Call(ctl, "delete", gadgetA, 500).Ready(widgetA)
		},
		"the delete named a subresource": func(r *tt.Script) {
			tagging := tt.Call(ctl, "delete", gadgetA, 204, "")
			tagging.Call.Subresource = "tagging"
			r.Add(tagging).See(ctl, gadgetA).Ready(widgetA)
		},
		"the delete removed an earlier Gadget of that name": func(r *tt.Script) {
			r.Delete(ctl, gadgetA)
			r.Create(ctl, gadgetA, gadget(r, false))
			r.See(ctl, gadgetA).Ready(widgetA)
		},
		"the delete came after the ready write": func(r *tt.Script) {
			r.Ready(widgetA).Delete(ctl, gadgetA)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := gadgetRun(trace.Dependency{Required: true})
			r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
			then(r)
			requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "slot a", "gadgets.toy ns/a seen not ready")
		})
	}
}

func TestHonestCountsASeenOfTheWritesVersionRecordedBeforeItsResponse(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA)
	sent := r.Now
	r.UpdateInFlight(ctl, gadgetA, gadget(r, true), 50*ms)
	r.At(sent+30*ms, tt.Seen(ctl, gadgetA, r.UID(gadgetA), "6"))
	r.Tick(50 * ms).Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestHonestTimesAReadAgainstAWriteWithNoResultByTheClock(t *testing.T) {
	for _, gap := range []time.Duration{0, tt.Tick} {
		r := gadgetRun(trace.Dependency{Required: true})
		r.Call(ctl, "update", gadgetA, 200)
		r.At(r.Now-tt.Tick+gap, tt.Seen(ctl, gadgetA, r.UID(gadgetA), "5"))
		r.Tick(tt.Tick).Ready(widgetA)
		findings := honest(t, r)
		if (len(findings) == 0) != (gap > 0) {
			t.Errorf("seen %v after the write: findings = %+v", gap, findings)
		}
	}
}

func TestHonestKeepsTheEarliestTimeOfAFinding(t *testing.T) {
	r := newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Required: true}}}
	r.Begin().CreateCR("a", widgetA)
	r.CreateCR("b", widgetB)
	r.Ready(widgetB)
	at := r.Now - tt.Tick
	r.Ready(widgetA)
	findings := honest(t, r)
	if len(findings) != 1 || findings[0].At != at || len(findings[0].Details) != 2 {
		t.Fatalf("findings = %+v, want one at %v", findings, at)
	}
}

func TestHonestWithTrackJudgesUntilTheTraceEnds(t *testing.T) {
	r := degrade(true)
	r.At(r.Now-tt.Tick+500*ms, tt.Call(trace.Harness, "get", cm, 200, ""))
	requireFinding(t, judgeWith(t, r, check.Honest), "Honest", 0, stayedReady, "kube/toy/gadgets")
}

func TestHonestWithTrackStartsAtAReadyAtTheSameTime(t *testing.T) {
	r := gadgetRun(trace.Dependency{Track: 500 * ms})
	r.Update(gadgetModel, gadgetA, gadget(r, false))
	r.At(r.Now, tt.Seen(ctl, gadgetA, r.UID(gadgetA), "6"))
	r.Ready(widgetA)
	findings := honest(t, r)
	if !slices.ContainsFunc(findings, func(f check.Finding) bool { return f.Summary == stayedReady }) {
		t.Errorf("findings = %+v, want one that says %q", findings, stayedReady)
	}
}

func TestHonestWithTrackExcuses(t *testing.T) {
	failed := tt.Call(ctl, "patch", widgetA, 500, "")
	failed.Call.Subresource = "status"
	for name, then := range map[string]func(r *tt.Script){
		"a kill":                   func(r *tt.Script) { r.Add(tt.Exit("signal: killed", true, "")) },
		"a restart":                func(r *tt.Script) { r.Add(tt.Exit("signal: killed", true, ""), tt.Start(2)) },
		"a failed write of the CR": func(r *tt.Script) { r.Add(failed) },
	} {
		t.Run(name, func(t *testing.T) {
			r := degrade(true)
			then(r)
			requireNone(t, honest(t, r))
		})
	}
}

func TestHonestWithTrackPassesASeenReadyAtTheDeadline(t *testing.T) {
	r := degrade(true)
	seen := r.Now - tt.Tick
	r.Update(gadgetModel, gadgetA, gadget(r, true))
	r.At(seen+500*ms, tt.Seen(ctl, gadgetA, r.UID(gadgetA), "8"))
	r.Tick(500 * ms)
	requireNone(t, honest(t, r))
}

func opaque(ref trace.Ref, stream uint64) trace.Event {
	return trace.Event{Seen: &trace.Seen{Caller: ctl, Ref: ref, Opaque: true, Stream: stream}}
}

func TestHonestSkipsADependencyTheControllerLastReadOpaquely(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.Add(opaque(tt.Ref("toy/gadgets", "ns", ""), 0)).Ready(widgetA)
	requireNone(t, honest(t, r))

	r = gadgetRun(trace.Dependency{Required: true})
	r.Add(opaque(tt.Ref("toy/gadgets", "ns", "b"), 0)).Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "not seen")

	r = gadgetRun(trace.Dependency{Required: true})
	r.Add(opaque(gadgetA, 0)).Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA).Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "seen not ready")
}

func TestHonestWithTrackSkipsAnOpaqueWatch(t *testing.T) {
	r := gadgetRun(trace.Dependency{Track: 500 * ms})
	r.Add(opaque(tt.Ref("toy/gadgets", "", ""), 7)).See(ctl, gadgetA).Ready(widgetA)
	r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
	requireNone(t, honest(t, r))

	r = gadgetRun(trace.Dependency{Track: 500 * ms})
	r.Add(opaque(tt.Ref("toy/gadgets", "", ""), 7), trace.Event{WatchEnd: &trace.WatchEnd{Caller: ctl, System: "kube", Stream: 7}})
	r.See(ctl, gadgetA).Ready(widgetA)
	r.Update(gadgetModel, gadgetA, gadget(r, false)).See(ctl, gadgetA)
	requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "kube/toy/gadgets")
}

func TestHonestSkipsAGoneDependencyItsCRReplaced(t *testing.T) {
	r := gadgetRun(trace.Dependency{Required: true})
	r.See(ctl, gadgetA).Delete(trace.Harness, gadgetA)
	r.Create(ctl, gadgetA, gadget(r, false))
	r.Update(gadgetModel, gadgetA, gadget(r, true)).See(ctl, gadgetA).Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestHonestJudgesADependencyTheControllerSawBeforeTheObserver(t *testing.T) {
	r := newRun(1)
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Required: true}}}
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Add(tt.Create(ctl, gadgetA, "ug", "50"), tt.Seen(ctl, gadgetA, "ug", "50"))
	r.UpdateInFlight(ctl, widgetA, tt.CR("a", true), 50*ms)
	r.Add(tt.Version(gadgetA, "ug", "50", gadget(r, false)))
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/toy/gadgets", "slot a", "gadgets.toy ns/a seen not ready")
}
