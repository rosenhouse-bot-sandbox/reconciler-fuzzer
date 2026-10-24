package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var gadgetGVR = schema.GroupVersionResource{Group: "toy.reconciler-fuzzer", Version: "v1", Resource: "gadgets"}

type write struct {
	ref         trace.Ref
	subresource string
	patch       map[string]any
	rv          string
}

// fakeStageWriter answers each write with the next resourceVersion from 100,
// or with the queued errors first.
type fakeStageWriter struct {
	mu     sync.Mutex
	writes []write
	errs   []error
	next   int
	// during runs as each write is sent.
	during func()
}

func (f *fakeStageWriter) patch(_ context.Context, ref trace.Ref, subresource string, patch []byte) (string, error) {
	if f.during != nil {
		f.during()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w := write{ref: ref, subresource: subresource}
	json.Unmarshal(patch, &w.patch)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		f.writes = append(f.writes, w)
		return "", err
	}
	f.next++
	w.rv = strconv.Itoa(99 + f.next)
	f.writes = append(f.writes, w)
	return w.rv, nil
}

func (f *fakeStageWriter) all() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

// await waits until there are n writes.
func (f *fakeStageWriter) await(t *testing.T, n int) []write {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(f.all()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("Got writes %+v, want %d.", f.all(), n)
		}
		time.Sleep(time.Millisecond)
	}
	return f.all()
}

// still asserts that the writes stay at n for d.
func (f *fakeStageWriter) still(t *testing.T, n int, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if got := f.all(); len(got) != n {
		t.Fatalf("Got writes %+v, want %d.", got, n)
	}
}

type stageWorks struct {
	mu   sync.Mutex
	list []bool
}

func (w *stageWorks) record(e trace.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.Work != nil {
		w.list = append(w.list, e.Work.Pending)
	}
}

func (w *stageWorks) get() []bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}

const after = 60 * time.Millisecond

func gadgetModel() config.Model {
	return config.Model{Kind: gadgetGVR.GroupVersion().WithKind("Gadget"), Stage: config.ReadyCondition, After: after}
}

func testStage(t *testing.T) (*Stage, *fakeStageWriter, *stageWorks) {
	return testStageWith(t, map[string]any{"phase": "Running"}, nil)
}

func testStageWith(t *testing.T, status, stuckStatus map[string]any) (*Stage, *fakeStageWriter, *stageWorks) {
	m := gadgetModel()
	m.Status, m.StuckStatus = status, stuckStatus
	return testStageOf(t, m)
}

const cleanup = "example.com/cleanup"

// testStageHolding runs a stage that keeps cleanup on each live Gadget, and
// removes it hold past the Gadget's deletion.
func testStageHolding(t *testing.T, hold time.Duration) (*Stage, *fakeStageWriter, *stageWorks) {
	m := gadgetModel()
	m.Finalizer = &config.Finalizer{Name: cleanup, After: hold}
	return testStageOf(t, m)
}

func testStageOf(t *testing.T, m config.Model) (*Stage, *fakeStageWriter, *stageWorks) {
	t.Helper()
	w := &fakeStageWriter{}
	works := &stageWorks{}
	s := newStage(gadgetGVR, m, w, works.record)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go s.run(ctx)
	t.Cleanup(func() { s.Stop(context.Background()) })
	return s, w, works
}

type gadget struct {
	name, uid, rv string
	generation    int64
	deleting      bool
	finalizers    []string
	// ready, when set, is the Ready condition's status, reason and
	// observedGeneration.
	ready *condition
}

type condition struct {
	status, reason string
	generation     int64
}

func (g gadget) version() trace.Version {
	metadata := map[string]any{"name": g.name, "namespace": ns, "uid": g.uid, "resourceVersion": g.rv, "generation": g.generation}
	if g.deleting {
		metadata["deletionTimestamp"] = "2026-01-01T00:00:00Z"
	}
	if g.finalizers != nil {
		metadata["finalizers"] = g.finalizers
	}
	body := map[string]any{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Gadget", "metadata": metadata}
	if g.ready != nil {
		body["status"] = map[string]any{"conditions": []any{map[string]any{
			"type": "Ready", "status": g.ready.status, "reason": g.ready.reason, "observedGeneration": g.ready.generation,
		}}}
	}
	raw, _ := json.Marshal(body)
	return trace.Version{Ref: g.ref(), UID: g.uid, RV: g.rv, Body: raw}
}

func (g gadget) ref() trace.Ref {
	return trace.Ref{System: "kube", Group: gadgetGVR.Group, Resource: gadgetGVR.Resource, Namespace: ns, Name: g.name}
}

func readyFor(generation int64) *condition { return &condition{"True", "Modelled", generation} }

// readyCondition returns the patch's only condition.
func readyCondition(t *testing.T, w write) map[string]any {
	t.Helper()
	status, _ := w.patch["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	if w.subresource != "status" || len(conditions) != 1 {
		t.Fatalf("Got write %+v, want a status write with one condition.", w)
	}
	return conditions[0].(map[string]any)
}

func precondition(w write) any {
	return w.patch["metadata"].(map[string]any)["resourceVersion"]
}

func writtenFinalizers(w write) any { return w.patch["metadata"].(map[string]any)["finalizers"] }

func TestStageIsNamedAfterItsKind(t *testing.T) {
	s := newStage(gadgetGVR, config.Model{}, nil, nil)
	if got := s.Name(); got != "model:gadgets.toy.reconciler-fuzzer" {
		t.Errorf("Name = %q.", got)
	}
}

func TestStageWritesReadyAfterTheDelay(t *testing.T) {
	s, w, _ := testStage(t)
	heard := time.Now()
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())

	got := w.await(t, 1)[0]
	if elapsed := time.Since(heard); elapsed < after {
		t.Errorf("Wrote after %v, want at least %v.", elapsed, after)
	}
	c := readyCondition(t, got)
	if c["type"] != "Ready" || c["status"] != "True" || c["observedGeneration"] != 1.0 || c["reason"] != "Modelled" || c["message"] == "" {
		t.Errorf("Wrote condition %v.", c)
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(c["lastTransitionTime"])); err != nil {
		t.Errorf("lastTransitionTime: %v", err)
	}
	if got.ref.Name != "g" || precondition(got) != "1" || got.patch["status"].(map[string]any)["phase"] != "Running" {
		t.Errorf("Wrote %+v, want g at resourceVersion 1 with the config's status fields.", got)
	}
}

func TestStageWritesOnlyTheConditionWithoutStatusFields(t *testing.T) {
	s, w, _ := testStageWith(t, nil, nil)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	if got := w.await(t, 1)[0].patch["status"].(map[string]any); len(got) != 1 {
		t.Errorf("Wrote status %v, want only conditions.", got)
	}
}

func TestStageWritesEachNewGenerationAfterTheDelay(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	first := w.await(t, 1)[0]
	s.hear(gadget{name: "g", uid: "u", rv: first.rv, generation: 1, ready: readyFor(1)}.version())
	w.still(t, 1, 2*after)

	heard := time.Now()
	s.hear(gadget{name: "g", uid: "u", rv: "200", generation: 2, ready: readyFor(1)}.version())
	second := w.await(t, 2)[1]
	if time.Since(heard) < after || readyCondition(t, second)["observedGeneration"] != 2.0 || precondition(second) != "200" {
		t.Errorf("Wrote %+v after %v, want generation 2 after %v.", second, time.Since(heard), after)
	}
}

func TestStageSkipsReadyAndDeletingObjects(t *testing.T) {
	s, w, works := testStage(t)
	s.hear(gadget{name: "ready", uid: "r", rv: "1", generation: 3, ready: readyFor(3)}.version())
	s.hear(gadget{name: "going", uid: "g", rv: "2", generation: 1, deleting: true, finalizers: []string{"x"}}.version())
	w.still(t, 0, 2*after)
	if got := works.get(); len(got) > 0 {
		t.Errorf("Recorded work %v, want none.", got)
	}
}

func TestStageWaitsForTheNextVersionAfterAConflict(t *testing.T) {
	s, w, _ := testStage(t)
	w.errs = []error{apierrors.NewConflict(gadgetGVR.GroupResource(), "g", nil)}
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	w.await(t, 1)
	w.still(t, 1, 2*after)

	s.hear(gadget{name: "g", uid: "u", rv: "2", generation: 1}.version())
	if got := w.await(t, 2)[1]; precondition(got) != "2" {
		t.Errorf("Wrote %+v, want resourceVersion 2.", got)
	}
}

func TestStageHasWorkUntilItsWriteIsObserved(t *testing.T) {
	s, w, works := testStage(t)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	if got := works.get(); !reflect.DeepEqual(got, []bool{true}) || !s.Pending() {
		t.Fatalf("Work %v while the write is scheduled, want [true].", got)
	}
	written := w.await(t, 1)[0]
	time.Sleep(after)
	if got := works.get(); !reflect.DeepEqual(got, []bool{true}) {
		t.Fatalf("Work %v before the write is observed, want [true].", got)
	}
	s.hear(gadget{name: "g", uid: "u", rv: written.rv, generation: 1, ready: readyFor(1)}.version())
	if got := works.get(); !reflect.DeepEqual(got, []bool{true, false}) || s.Pending() {
		t.Errorf("Work %v after the write is observed, want [true false].", got)
	}
}

func TestStageHasWorkWhileItSendsAFaultsWrite(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1, ready: readyFor(1)}.version())
	var pending bool
	w.during = func() { pending = s.Pending() }
	if _, err := s.Fault(stuck("")); err != nil {
		t.Fatal(err)
	}
	if len(w.all()) != 1 || !pending {
		t.Errorf("Pending was %v while sending %+v, want true.", pending, w.all())
	}
}

func TestStageRetriesAFailedWrite(t *testing.T) {
	s, w, _ := testStage(t)
	w.errs = []error{apierrors.NewInternalError(fmt.Errorf("down"))}
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	got := w.await(t, 2)
	if precondition(got[1]) != "1" {
		t.Errorf("Retried with %+v.", got[1])
	}
}

func stuck(name string) plan.Fault {
	return plan.Fault{Match: plan.Match{Group: gadgetGVR.Group, Resource: gadgetGVR.Resource, Name: name}, Stuck: true}
}

func slowDelete() plan.Fault {
	return plan.Fault{Match: plan.Match{Group: gadgetGVR.Group, Resource: gadgetGVR.Resource}, SlowDelete: true}
}

func TestStageTakesModelFaultsOnItsKind(t *testing.T) {
	s := newStage(gadgetGVR, config.Model{}, nil, nil)
	other := stuck("")
	other.Match.Resource = "widgets"
	request := stuck("")
	request.Stuck, request.Drop = false, true
	for _, c := range []struct {
		f    plan.Fault
		want bool
	}{{stuck(""), true}, {slowDelete(), true}, {other, false}, {request, false}} {
		if got := s.Takes(c.f); got != c.want {
			t.Errorf("Takes(%s) = %v, want %v.", c.f, got, c.want)
		}
	}
	if _, err := s.Fault(other); err == nil {
		t.Error("Fault took a fault on another kind.")
	}
}

func TestStuckKeepsMatchingObjectsNotReadyUntilItEnds(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g1", uid: "u1", rv: "1", generation: 1, ready: readyFor(1)}.version())
	s.hear(gadget{name: "g2", uid: "u2", rv: "2", generation: 1, ready: readyFor(1)}.version())

	end, err := s.Fault(stuck("g1"))
	if err != nil {
		t.Fatal(err)
	}
	writes := w.all()
	if len(writes) != 1 || writes[0].ref.Name != "g1" {
		t.Fatalf("Fault returned with writes %+v, want one to g1.", writes)
	}
	if c := readyCondition(t, writes[0]); c["status"] != "False" || c["reason"] != "Stuck" || c["observedGeneration"] != 1.0 {
		t.Errorf("Wrote %v, want Ready False, Stuck, for generation 1.", c)
	}
	s.hear(gadget{name: "g1", uid: "u1", rv: writes[0].rv, generation: 1, ready: &condition{"False", "Stuck", 1}}.version())
	w.still(t, 1, 2*after)

	s.hear(gadget{name: "g1", uid: "u1", rv: "300", generation: 2, ready: &condition{"False", "Stuck", 1}}.version())
	writes = w.await(t, 2)
	if c := readyCondition(t, writes[1]); c["status"] != "False" || c["observedGeneration"] != 2.0 {
		t.Errorf("Wrote %v for generation 2, want Ready False.", c)
	}
	s.hear(gadget{name: "g1", uid: "u1", rv: writes[1].rv, generation: 2, ready: &condition{"False", "Stuck", 2}}.version())
	w.still(t, 2, 2*after)

	end()
	writes = w.all()
	if len(writes) != 3 {
		t.Fatalf("end returned with writes %+v, want a third.", writes)
	}
	if c := readyCondition(t, writes[2]); writes[2].ref.Name != "g1" || c["status"] != "True" || c["message"] != "fault 1 ended" || c["observedGeneration"] != 2.0 {
		t.Errorf("end wrote %+v, want g1 Ready True with message 'fault 1 ended'.", writes[2])
	}
}

func TestStuckWritesEachObjectsOwnGeneration(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g1", uid: "u1", rv: "1", generation: 1, ready: readyFor(1)}.version())
	s.hear(gadget{name: "g2", uid: "u2", rv: "2", generation: 2, ready: readyFor(2)}.version())
	if _, err := s.Fault(stuck("")); err != nil {
		t.Fatal(err)
	}
	for _, write := range w.await(t, 2) {
		if got, want := readyCondition(t, write)["observedGeneration"], map[string]any{"g1": 1.0, "g2": 2.0}[write.ref.Name]; got != want {
			t.Errorf("The stuck write to %s set observedGeneration %v, want %v.", write.ref.Name, got, want)
		}
	}
}

func TestStuckReplacesTheStatusFieldsUntilItEnds(t *testing.T) {
	for name, c := range map[string]struct {
		status, stuckStatus, readyFields, stuckFields string
	}{
		"a Pod":                       {`{"phase":"Running"}`, `null`, `{"phase":"Running"}`, `{"phase":"Running"}`},
		"a Pod with only stuckStatus": {`null`, `{"phase":"Pending"}`, `{"phase":null}`, `{"phase":"Pending"}`},
		"a PVC":                       {`{"phase":"Bound"}`, `{"phase":"Pending"}`, `{"phase":"Bound"}`, `{"phase":"Pending"}`},
		"a database":                  {`{"endpoint":"10.0.0.1:5432"}`, `{"error":"no capacity"}`, `{"endpoint":"10.0.0.1:5432","error":null}`, `{"endpoint":null,"error":"no capacity"}`},
		"a Service":                   {`{"loadBalancer":{"ingress":[{"ip":"10.0.0.1"}]}}`, `{"loadBalancer":{}}`, `{"loadBalancer":{"ingress":[{"ip":"10.0.0.1"}]}}`, `{"loadBalancer":{"ingress":null}}`},
		"a certificate":               {`{"notAfter":"2027-01-01T00:00:00Z"}`, `{}`, `{"notAfter":"2027-01-01T00:00:00Z"}`, `{"notAfter":null}`},
	} {
		t.Run(name, func(t *testing.T) {
			s, w, _ := testStageWith(t, jsonObject(t, c.status), jsonObject(t, c.stuckStatus))
			g := gadget{name: "g", uid: "u", rv: "1", generation: 1}
			s.hear(g.version())
			readyWrite := w.await(t, 1)[0]
			g.rv, g.ready = readyWrite.rv, readyFor(1)
			s.hear(g.version())
			end, err := s.Fault(stuck(""))
			if err != nil {
				t.Fatal(err)
			}
			stuckWrite := w.await(t, 2)[1]
			g.rv, g.ready = stuckWrite.rv, &condition{"False", "Stuck", 1}
			s.hear(g.version())
			end()
			endWrite := w.await(t, 3)[2]
			for _, step := range []struct {
				name, wantReady, wantFields string
				write                       write
			}{
				{"first", "True", c.readyFields, readyWrite},
				{"stuck", "False", c.stuckFields, stuckWrite},
				{"end's", "True", c.readyFields, endWrite},
			} {
				if ready, fields := readyCondition(t, step.write)["status"], literalFields(step.write); ready != step.wantReady || fields != step.wantFields {
					t.Errorf("The %s write set Ready %v and %s, want %s and %s.", step.name, ready, fields, step.wantReady, step.wantFields)
				}
			}
		})
	}
}

func jsonObject(t *testing.T, s string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(s), &object); err != nil {
		t.Fatal(err)
	}
	return object
}

// literalFields returns, as JSON, the status fields a write sets other than
// conditions.
func literalFields(w write) string {
	status, _ := w.patch["status"].(map[string]any)
	fields := maps.Clone(status)
	delete(fields, "conditions")
	raw, _ := json.Marshal(fields)
	return string(raw)
}

func TestAFaultsEndRewritesAReadyObjectWithItsCount(t *testing.T) {
	s, w, _ := testStage(t)
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, ready: readyFor(1)}
	s.hear(g.version())
	for n := 1; n <= 2; n++ {
		end, err := s.Fault(slowDelete())
		if err != nil {
			t.Fatal(err)
		}
		added := w.all()[len(w.all())-1]
		g.rv, g.finalizers = added.rv, []string{SlowDeleteFinalizer}
		s.hear(g.version())
		end()
		writes := w.all()
		removed, fresh := writes[len(writes)-2], writes[len(writes)-1]
		if removed.subresource != "" || fresh.subresource != "status" {
			t.Fatalf("end wrote %+v, want the finalizer's removal then a status.", writes)
		}
		if c := readyCondition(t, fresh); c["message"] != fmt.Sprintf("fault %d ended", n) || precondition(fresh) != removed.rv {
			t.Errorf("end wrote %+v after %+v, want message 'fault %d ended' at the removal's resourceVersion.", fresh, removed, n)
		}
		g.rv, g.finalizers = fresh.rv, nil
		s.hear(g.version())
	}
}

func TestSlowDeleteHoldsEveryMatchingObjectUntilItEnds(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g1", uid: "u1", rv: "1", generation: 1, finalizers: []string{"other"}, ready: readyFor(1)}.version())

	end, err := s.Fault(slowDelete())
	if err != nil {
		t.Fatal(err)
	}
	writes := w.all()
	if len(writes) != 1 || writes[0].subresource != "" || !reflect.DeepEqual(writtenFinalizers(writes[0]), []any{"other", SlowDeleteFinalizer}) {
		t.Fatalf("Fault returned with writes %+v, want the finalizer added to g1.", writes)
	}
	s.hear(gadget{name: "g1", uid: "u1", rv: writes[0].rv, generation: 1, finalizers: []string{"other", SlowDeleteFinalizer}, ready: readyFor(1)}.version())
	s.hear(gadget{name: "g2", uid: "u2", rv: "5", generation: 1, ready: readyFor(1)}.version())
	writes = w.await(t, 2)
	if writes[1].ref.Name != "g2" || !reflect.DeepEqual(writtenFinalizers(writes[1]), []any{SlowDeleteFinalizer}) {
		t.Fatalf("Wrote %+v, want the finalizer added to g2.", writes[1])
	}
	s.hear(gadget{name: "g2", uid: "u2", rv: writes[1].rv, generation: 1, deleting: true, finalizers: []string{SlowDeleteFinalizer}, ready: readyFor(1)}.version())
	w.still(t, 2, 2*after)

	end()
	writes = w.all()[2:]
	var got []string
	for _, w := range writes {
		got = append(got, fmt.Sprintf("%s %s %v", w.ref.Name, w.subresource, writtenFinalizers(w)))
	}
	want := []string{"g1  [other]", "g2  []", "g1 status <nil>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("end wrote %q, want %q.", got, want)
	}
}

func TestStopEndsTheStagesTimers(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1}.version())
	s.Stop(context.Background())
	w.still(t, 0, 2*after)
}

func TestObservedAcceptsTheAwaitedVersionOrALaterOne(t *testing.T) {
	for _, c := range []struct {
		heard, awaited string
		want           bool
	}{{"5", "5", true}, {"6", "5", true}, {"4", "5", false}, {"x", "y", false}, {"x", "x", true}} {
		if got := observed(c.heard, c.awaited); got != c.want {
			t.Errorf("observed(%q, %q) = %v, want %v.", c.heard, c.awaited, got, c.want)
		}
	}
}

func TestStageCountsAfterFromAGenerationsFirstVersion(t *testing.T) {
	s, _, _ := testStage(t)
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1}
	s.hear(g.version())
	first := time.Now().Add(-time.Hour)
	s.mu.Lock()
	s.objects[g.ref()].heardAt = first
	s.mu.Unlock()
	g.rv = "2"
	s.hear(g.version())
	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.objects[g.ref()].heardAt; !got.Equal(first) {
		t.Errorf("heardAt = %v after a version of the same generation, want %v.", got, first)
	}
}

func TestSlowDeleteAddsNoFinalizerToAnObjectBeingDeleted(t *testing.T) {
	s, w, _ := testStage(t)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1, deleting: true, finalizers: []string{"x"}}.version())
	if _, err := s.Fault(slowDelete()); err != nil {
		t.Fatal(err)
	}
	if got := w.all(); len(got) != 0 {
		t.Errorf("Wrote %+v, want nothing.", got)
	}
}

func TestAFaultsEndKeepsItsStatusDueWhenTheStatusWriteFails(t *testing.T) {
	s, w, _ := testStage(t)
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, ready: readyFor(1)}
	s.hear(g.version())
	end, err := s.Fault(slowDelete())
	if err != nil {
		t.Fatal(err)
	}
	g.rv, g.finalizers = w.all()[0].rv, []string{SlowDeleteFinalizer}
	s.hear(g.version())
	sent := 0
	w.during = func() {
		if sent++; sent == 2 {
			w.errs = []error{apierrors.NewInternalError(fmt.Errorf("down"))}
		}
	}
	end()
	g.rv, g.finalizers = w.all()[1].rv, nil
	s.hear(g.version())
	fresh := w.await(t, 4)[3]
	if c := readyCondition(t, fresh); c["message"] != "fault 1 ended" {
		t.Errorf("Wrote %v after the failed status write, want message 'fault 1 ended'.", c)
	}
}

func TestStageTakesAModelFaultWithNoGroup(t *testing.T) {
	s := newStage(gadgetGVR, config.Model{}, nil, nil)
	f := stuck("")
	f.Match.Group = ""
	if !s.Takes(f) {
		t.Errorf("Takes(%s) = false, want true.", f)
	}
}

func TestAModelFinalizerHoldsEachDeletionForItsAfter(t *testing.T) {
	const hold = 500 * time.Millisecond
	s, w, _ := testStageHolding(t, hold)
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, finalizers: []string{"other"}, ready: readyFor(1)}
	s.hear(g.version())
	added := w.await(t, 1)[0]
	if added.subresource != "" || precondition(added) != "1" || !reflect.DeepEqual(writtenFinalizers(added), []any{"other", cleanup}) {
		t.Fatalf("Wrote %+v on first hearing g, want its finalizer added at resourceVersion 1.", added)
	}
	g.rv, g.finalizers = added.rv, []string{"other", cleanup}
	s.hear(g.version())
	w.still(t, 1, 2*after)

	deleted := time.Now()
	g.rv, g.generation, g.deleting = "200", 2, true
	s.hear(g.version())
	if !s.Pending() {
		t.Error("Pending is false while the finalizer holds the deletion.")
	}
	removed := w.await(t, 2)[1]
	if elapsed := time.Since(deleted); elapsed < hold {
		t.Errorf("Removed the finalizer %v after the deletion, want at least %v.", elapsed, hold)
	}
	if precondition(removed) != "200" || !reflect.DeepEqual(writtenFinalizers(removed), []any{"other"}) {
		t.Errorf("Wrote %+v, want only the stage's finalizer removed at resourceVersion 200.", removed)
	}
	w.still(t, 2, 2*after)
	g.rv, g.finalizers = removed.rv, []string{"other"}
	s.hear(g.version())
	if s.Pending() {
		t.Error("Pending is true after the stage removed its finalizer.")
	}
}

func TestAModelFinalizerReturnsToALiveObjectThatLostIt(t *testing.T) {
	s, w, _ := testStageHolding(t, time.Second)
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, finalizers: []string{cleanup}, ready: readyFor(1)}
	s.hear(g.version())
	w.still(t, 0, 2*after)
	g.rv, g.finalizers = "2", nil
	s.hear(g.version())
	if got := w.await(t, 1)[0]; precondition(got) != "2" || !reflect.DeepEqual(writtenFinalizers(got), []any{cleanup}) {
		t.Errorf("Wrote %+v, want the finalizer added back at resourceVersion 2.", got)
	}
}

func TestAModelFinalizerIsNotAddedToAnObjectBeingDeleted(t *testing.T) {
	s, w, works := testStageHolding(t, time.Second)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1, deleting: true, finalizers: []string{"x"}}.version())
	w.still(t, 0, 2*after)
	if got := works.get(); len(got) > 0 {
		t.Errorf("Recorded work %v, want none.", got)
	}
}

func TestAModelFinalizerCountsAfterFromTheFirstDeletingVersion(t *testing.T) {
	s, _, _ := testStageHolding(t, time.Hour)
	deletingAt := func(g gadget) time.Time {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.objects[g.ref()].deletingAt
	}
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, finalizers: []string{cleanup, "other"}}
	s.hear(g.version())
	heard := time.Now()
	g.rv, g.generation, g.deleting = "2", 2, true
	s.hear(g.version())
	first := deletingAt(g)
	if first.Before(heard) || first.After(time.Now()) {
		t.Fatalf("deletingAt = %v, want the time the stage heard the deletion, after %v.", first, heard)
	}
	time.Sleep(time.Millisecond)
	g.rv, g.finalizers = "3", []string{cleanup}
	s.hear(g.version())
	if got := deletingAt(g); !got.Equal(first) {
		t.Errorf("deletingAt = %v after a later deleting version, want %v.", got, first)
	}
}

func TestSlowDeleteAndAModelFinalizerKeepEachOther(t *testing.T) {
	s, w, _ := testStageHolding(t, after)
	end, err := s.Fault(slowDelete())
	if err != nil {
		t.Fatal(err)
	}
	g := gadget{name: "g", uid: "u", rv: "1", generation: 1, ready: readyFor(1)}
	s.hear(g.version())
	added := w.await(t, 1)[0]
	g.rv, g.finalizers = added.rv, []string{SlowDeleteFinalizer, cleanup}
	s.hear(g.version())
	g.rv, g.generation, g.deleting = "200", 2, true
	s.hear(g.version())
	released := w.await(t, 2)[1]
	g.rv, g.finalizers = released.rv, []string{SlowDeleteFinalizer}
	s.hear(g.version())
	w.still(t, 2, 2*after)
	end()

	var got []string
	for _, w := range w.all() {
		got = append(got, fmt.Sprint(writtenFinalizers(w)))
	}
	want := []string{fmt.Sprint([]any{SlowDeleteFinalizer, cleanup}), fmt.Sprint([]any{SlowDeleteFinalizer}), "[]"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Wrote finalizers %q, want %q.", got, want)
	}
}

// g kept slowDelete's finalizer past the fault's end, but lost the stage's.
func TestAModelFinalizerReplacesALeftoverSlowDeleteFinalizer(t *testing.T) {
	s, w, _ := testStageHolding(t, time.Second)
	s.hear(gadget{name: "g", uid: "u", rv: "1", generation: 1, finalizers: []string{SlowDeleteFinalizer}, ready: readyFor(1)}.version())
	if got := w.await(t, 1)[0]; !reflect.DeepEqual(writtenFinalizers(got), []any{cleanup}) {
		t.Errorf("Wrote %+v, want slowDelete's finalizer replaced by the stage's.", got)
	}
}
