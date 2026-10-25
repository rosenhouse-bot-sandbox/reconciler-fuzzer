package check_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// unmodelled is the unmodelled-kind detail lines of the one finding of a
// step in which slot a never becomes ready, while the controller makes the
// calls.
func unmodelled(t *testing.T, sp check.Spec, calls ...trace.Event) []string {
	t.Helper()
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"), tt.Start(1), tt.Phase(0, trace.FaultPeriod)).
		At(100*ms, tt.Version(widgetA, "ua", "1", ready(false))).
		At(1*s, tt.EndFaults(0, 10*s, false)).
		At(2*s, calls...).
		At(10*s, tt.Phase(0, trace.TimedOut))
	findings, _ := check.Converge(tl.Index(), sp)
	f := only(t, findings)
	if f.Summary != "did not settle (clause 1)" || f.Kind != "kube/toy/widgets" {
		t.Fatalf("finding = %+v", f)
	}
	var lines []string
	for _, d := range f.Details {
		if strings.Contains(d, "models:") {
			lines = append(lines, d)
		}
	}
	return lines
}

// bare is a version of ref with no status.
func bare(ref trace.Ref) trace.Event {
	return tt.Version(ref, "u-"+ref.Name, "1", map[string]any{"metadata": map[string]any{"name": ref.Name}})
}

func TestConvergeNamesAnUnmodelledKind(t *testing.T) {
	sp := spec
	sp.StatusKinds = []trace.Kind{{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}}
	pod, pvc, job := tt.Ref("pods", "ns", "p"), tt.Ref("persistentvolumeclaims", "ns", "p"), tt.Ref("batch/jobs", "ns", "j")
	for name, c := range map[string]struct {
		events []trace.Event
		want   string
	}{
		"a status kind": {[]trace.Event{bare(gadgetA), tt.Call(ctl, "get", gadgetA, 200, "")}, "{kind: toy/v1/Gadget, stages: readyCondition, after: 200ms}"},
		"a watch":       {[]trace.Event{bare(gadgetA), watchOf(tt.Ref("toy/gadgets", "", ""))}, "toy/v1/Gadget"},
		"a spec write":  {[]trace.Event{bare(gadgetA), watchOf(gadgetA), tt.Call(ctl, "patch", gadgetA, 200, "")}, "toy/v1/Gadget"},
		"a pod":         {[]trace.Event{bare(pod), tt.Call(ctl, "get", pod, 200, "")}, "{kind: v1/Pod, stages: readyCondition, after: 200ms, status: {phase: Running}}, and set its status fields to the ones the controller reads"},
		"a pvc":         {[]trace.Event{bare(pvc), tt.Call(ctl, "get", pvc, 200, "")}, "{kind: v1/PersistentVolumeClaim, stages: readyCondition, after: 200ms, status: {phase: Bound}, stuckStatus: {phase: Pending}}"},
		"a job":         {[]trace.Event{bare(job), tt.Call(ctl, "get", job, 200, "")}, "{kind: batch/v1/Job, stages: readyCondition, after: 200ms, status: {succeeded: 1}, stuckStatus: {active: 1}}"},
	} {
		lines := unmodelled(t, sp, c.events...)
		if len(lines) != 1 || !strings.Contains(lines[0], c.want) || !strings.Contains(lines[0], "reads") {
			t.Errorf("%s: lines = %q, want one with %q", name, lines, c.want)
		}
	}
}

func watchOf(ref trace.Ref) trace.Event {
	e := tt.Call(ctl, "watch", ref, 200, "")
	e.Call.Watch = true
	return e
}

func TestConvergeNamesNoKind(t *testing.T) {
	sp := spec
	sp.StatusKinds = []trace.Kind{{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}, {Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgets"}}
	statusWrite := tt.Call(ctl, "patch", gadgetA, 200, "")
	statusWrite.Call.Subresource = "status"
	modelled := sp
	modelled.Models = []check.Model{{Kind: sp.StatusKinds[0]}}
	get := tt.Call(ctl, "get", gadgetA, 200, "")
	withStatus := tt.Version(gadgetA, "u-a", "1", map[string]any{"status": map[string]any{"phase": "Up"}})
	deployment := tt.Ref("apps/deployments", "ns", "d")
	listed := sp
	listed.Primaries = append(slices.Clone(sp.Primaries), check.Primary{Kind: sp.StatusKinds[0], Ready: sp.Primaries[0].Ready})
	for name, c := range map[string]struct {
		s      check.Spec
		events []trace.Event
	}{
		"modelled":                  {modelled, []trace.Event{bare(gadgetA), get}},
		"its status written":        {sp, []trace.Event{bare(gadgetA), get, statusWrite}},
		"no status kind":            {sp, []trace.Event{bare(cm), tt.Call(ctl, "get", cm, 200, "")}},
		"the primary":               {sp, []trace.Event{tt.Call(ctl, "get", widgetA, 200, "")}},
		"a CR of another kind":      {listed, []trace.Event{bare(gadgetA), get}},
		"not read":                  {sp, []trace.Event{bare(gadgetA), tt.Call(trace.Harness, "get", gadgetA, 200, "")}},
		"no instance":               {sp, []trace.Event{get}},
		"an instance with a status": {sp, []trace.Event{withStatus, get}},
		"a deployment":              {sp, []trace.Event{bare(deployment), tt.Call(ctl, "get", deployment, 200, "")}},
	} {
		if lines := unmodelled(t, c.s, c.events...); len(lines) != 0 {
			t.Errorf("%s: lines = %q", name, lines)
		}
	}
}

func TestConvergeNamesOnlyAKindReadOnKubeByTheFinding(t *testing.T) {
	sp := spec
	sp.StatusKinds = []trace.Kind{{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}}
	other := gadgetA
	other.System = "other"
	if lines := unmodelled(t, sp, bare(gadgetA), tt.Call(ctl, "get", other, 200, "")); len(lines) != 0 {
		t.Errorf("a read on another system: lines = %q", lines)
	}
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"), tt.Start(1), tt.Phase(0, trace.FaultPeriod)).
		At(100*ms, tt.Version(widgetA, "ua", "1", ready(false)), bare(gadgetA)).
		At(1*s, tt.EndFaults(0, 10*s, false)).
		At(10*s, tt.Phase(0, trace.TimedOut)).
		At(11*s, tt.Call(ctl, "get", gadgetA, 200, ""))
	findings, _ := check.Converge(tl.Index(), sp)
	if f := only(t, findings); slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, "models:") }) {
		t.Errorf("a read after the finding: details = %q", f.Details)
	}
}

// terminating is the body of a ready CR named name that a finalizer holds
// after its delete.
func terminating(name string) map[string]any {
	body := tt.CR(name, true)
	body["metadata"].(map[string]any)["deletionTimestamp"] = "2026-01-01T00:00:00Z"
	return body
}

func convergeOf(t *testing.T, r *tt.Script) ([]check.Finding, []string) {
	t.Helper()
	ix := r.Index()
	sp, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	return check.Converge(ix, sp)
}

// The plan stops the controller before it deletes slot a's CR, whose
// finalizer then stays, as it should.
func TestConvergeNotesACRDeletedAfterTheStop(t *testing.T) {
	r := newRun(3)
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().Stop().EndFaults().Settle()
	r.Begin().Update(trace.Harness, widgetA, terminating("a")).Act("delete", "a", &widgetA).EndFaults().Settle()
	findings, notes := convergeOf(t, r)
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
	want := "widgets.toy ns/a is not judged: the stop did not wait for it, and it is still being deleted when the trace ends."
	if !slices.Equal(notes, []string{want}) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}

func TestConvergeFailsAStopStepWhoseDeletedCRStays(t *testing.T) {
	r := newRun(2)
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().Update(trace.Harness, widgetA, terminating("a")).Act("delete", "a", &widgetA).Stop().EndFaults().TimeOut()
	findings, notes := convergeOf(t, r)
	requireFinding(t, findings, "Converge", 1, "did not settle (clause 2)", "kube/toy/widgets")
	if !slices.Equal(findings[0].Details, []string{"widgets.toy ns/a is being deleted"}) {
		t.Errorf("details = %q, want only slot a's CR being deleted", findings[0].Details)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %q, want none", notes)
	}
}

func TestConvergeNotesADeletionTheStopDidNotWaitFor(t *testing.T) {
	r := newRun(2)
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().Update(trace.Harness, widgetA, terminating("a")).Stop().EndFaults().Settle()
	findings, notes := convergeOf(t, r)
	if len(findings) != 0 {
		t.Errorf("findings = %+v, want none", findings)
	}
	want := "widgets.toy ns/a is not judged: the stop did not wait for it, and it is still being deleted when the trace ends."
	if !slices.Equal(notes, []string{want}) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}
