package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// fakeHarness records what the Runner did to the run, in order, and answers
// without an API server.
type fakeHarness struct {
	calls []string

	converged bool
	clean     bool
	// added counts the faults the proxy was given, which is the ID of the
	// next, and removed is when the Runner took each off.
	added   int
	removed map[proxy.FaultID]time.Time
	// faulting makes the harness answer as a proxy that applies every fault
	// the moment it is given it, and that only a removal retires. It ignores
	// that the first fault to match a request wins. faultingInWait has it
	// first apply them once a wait begins, as the target's requests do.
	// faultingAsRemoved has it apply a fault just as it is removed, as a
	// request that raced the removal would.
	faulting          bool
	faultingInWait    bool
	faultingAsRemoved bool
	applied           map[proxy.FaultID]time.Time
	// store is the version history the checks and the report read.
	store *observe.Store
	// logged are the requests the harness answers with before the run's own,
	// so that a test can drive the log past what a violation quotes.
	logged []proxy.Request
	// applying is what the harness says the proxy did with each fault, by
	// ID. Its zero value is a fault the proxy applied to no request.
	applying []proxy.FaultWindow

	managed map[schema.GroupVersionKind][]string
	count   int
	forced  []string
	fail    map[string]error
	// unresolved is what the collector reports once the harness has stopped.
	unresolved []cluster.Unresolved
	// gone names the managed objects the target deleted before reconciler-fuzzer
	// could.
	gone []string

	// deleteCRDelay holds the CR delete open, and deletedCRAt is when it began.
	// Together they show whether a stamp was taken before or after the delete.
	deleteCRDelay time.Duration
	deletedCRAt   time.Time
	// finalizerStays has the Observer record a deleted CR still under deletion.
	finalizerStays bool
	// crStays has a recreate's wait for the CR to go end with it still there,
	// and awaitedUntil is when each such wait was told to give up. inAwait
	// runs as each such wait begins.
	crStays      bool
	awaitedUntil []time.Time
	inAwait      func()

	// targetGone makes the harness report a target that has stopped, and
	// stopsAfter is the call it exits at. exitsInWait is the settle wait it
	// exits during, counting from 1. Once supervised, an exit is recorded
	// with the next of says as its last words and the target runs on,
	// restarting restartsIn later, unless restartFails.
	targetGone   bool
	stopsAfter   string
	exitsInWait  int
	targetExit   error
	says         []string
	supervised   bool
	restartsIn   time.Duration
	restartFails bool
	exited       []Exit
	// settles are the outcomes of the settle waits in turn. Later waits
	// converge as converged says.
	settles []bool
	waits   int

	// owed is what each settle wait was told the target owed, and
	// applyingInWait replaces applying once a wait begins.
	owed           []time.Time
	applyingInWait []proxy.FaultWindow
	// floors is the floor each settle wait was given.
	floors []time.Time
	// retiresInWait has the proxy retire the first fault this long into a
	// wait, which then runs in real time until the target owes nothing.
	retiresInWait time.Duration
	// inWait runs as each wait begins.
	inWait func()
	// cancel ends the run's context at the call cancelsAfter names.
	cancel       context.CancelFunc
	cancelsAfter string
	// restoredAt is when reconciler-fuzzer last created a fixture.
	restoredAt time.Time
	// supervising is the context the run supervised the target under, and
	// stoppedAtOnce is whether the teardown stopped the target under a
	// context that had ended, which kills it at once. cancelled names the
	// writes made under such a context, which the API server never gets.
	supervising   context.Context
	stoppedAtOnce bool
	cancelled     []string
	// clock is the run's clock. Nil is the wall clock.
	clock func() time.Time
	// holds answers for the proxy what it held. Nil holds nothing.
	holds func(before time.Time) (bool, time.Time)
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{
		converged: true,
		clean:     true,
		store:     observe.NewStore(observe.Options{Namespace: fakeNamespace, Manages: []schema.GroupVersionKind{configMapKind}}),
		managed:   map[schema.GroupVersionKind][]string{configMapKind: {"widget-0", "widget-1"}},
		fail:      map[string]error{},
		applied:   map[proxy.FaultID]time.Time{},
		removed:   map[proxy.FaultID]time.Time{},
	}
}

func (f *fakeHarness) record(call string) error {
	f.calls = append(f.calls, call)
	if call == f.stopsAfter {
		f.exit()
	}
	if call == f.cancelsAfter {
		f.cancel()
	}
	return f.fail[call]
}

const fakeNamespace = "reconciler-fuzzer-run-test"

func (f *fakeHarness) namespace() string { return fakeNamespace }

func (f *fakeHarness) now() time.Time {
	if f.clock == nil {
		return time.Now()
	}
	return f.clock()
}

func (f *fakeHarness) settle(ctx context.Context, owed func() time.Time, floor time.Time) (bool, error) {
	f.floors = append(f.floors, floor)
	if f.applyingInWait != nil {
		f.applying = f.applyingInWait
	}
	if f.inWait != nil {
		f.inWait()
	}
	if f.retiresInWait > 0 {
		time.Sleep(f.retiresInWait)
		f.applying[0].Retired = time.Now()
		time.Sleep(time.Until(owed()))
	}
	f.owed = append(f.owed, owed())
	if f.faultingInWait {
		f.apply()
	}
	if f.waits++; f.waits == f.exitsInWait {
		f.exit()
	}
	if err := f.record("settle"); err != nil {
		return false, err
	}
	// The live wait sees a stopped target before it sleeps on ctx.
	if f.targetGone {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if f.waits <= len(f.settles) {
		return f.settles[f.waits-1], nil
	}
	return f.converged, nil
}

// exit stops the target as it would stop on its own.
func (f *fakeHarness) exit() {
	if f.supervised && !f.restartFails {
		said := ""
		if n := len(f.exited); n < len(f.says) {
			said = f.says[n]
		}
		now := f.now()
		f.exited = append(f.exited, Exit{At: now, Err: f.targetExit, Said: said, Restart: now.Add(f.restartsIn)})
		return
	}
	f.targetGone = true
}

func (f *fakeHarness) supervise(ctx context.Context) {
	f.supervised, f.supervising = true, ctx
	_ = f.record("supervise")
}

func (f *fakeHarness) exits() []Exit { return slices.Clone(f.exited) }

// write records a write to the API server.
func (f *fakeHarness) write(ctx context.Context, call string) error {
	if ctx.Err() != nil {
		f.cancelled = append(f.cancelled, call)
	}
	return f.record(call)
}

// sleep ends with the context, as the live harness's does, and a sleep under a
// context that has ended waits for nothing.
func (f *fakeHarness) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(f.record("sleep "+d.String()), ctx.Err())
}

func (f *fakeHarness) restart(context.Context) error { return f.record("restart") }

func (f *fakeHarness) addFault(proxy.FaultSpec) proxy.FaultID {
	id := proxy.FaultID(f.added)
	f.added++
	if f.faulting {
		f.apply()
	}
	_ = f.record(fmt.Sprintf("addFault %d", id))
	return id
}

func (f *fakeHarness) removeFault(id proxy.FaultID) {
	f.remove(id)
	_ = f.record(fmt.Sprintf("removeFault %d", id))
}

func (f *fakeHarness) clearFaults() {
	for id := range proxy.FaultID(f.added) {
		f.remove(id)
	}
	_ = f.record("clearFaults")
}

// remove retires the fault, as the proxy does, unless it is retired already.
func (f *fakeHarness) remove(id proxy.FaultID) {
	if !f.faultWindow(id).Retired.IsZero() {
		return
	}
	if _, applied := f.applied[id]; f.faultingAsRemoved && !applied {
		f.applied[id] = f.now()
	}
	f.removed[id] = f.now()
}

// apply has the proxy apply each fault it holds, from now if it has not yet.
func (f *fakeHarness) apply() {
	for id := range proxy.FaultID(f.added) {
		_, removed := f.removed[id]
		if _, applied := f.applied[id]; !applied && !removed {
			f.applied[id] = f.now()
		}
	}
}

// fakeServed is what discovery answers every fake harness.
var fakeServed = []metav1.APIResource{
	{Name: "configmaps", SingularName: "configmap", Kind: "ConfigMap"},
	{Name: "secrets", SingularName: "secret", Kind: "Secret"},
	{Name: "widgets", SingularName: "widget", Kind: "Widget"},
}

func (f *fakeHarness) servedResources() ([]metav1.APIResource, error) {
	return fakeServed, f.fail["servedResources"]
}

func (f *fakeHarness) held(before time.Time) (bool, time.Time) {
	if f.holds == nil {
		return false, time.Time{}
	}
	return f.holds(before)
}

// faultWindow answers as the proxy does. A test either says what the proxy
// did with each fault, or has it apply every fault as it is given.
func (f *fakeHarness) faultWindow(id proxy.FaultID) proxy.FaultWindow {
	var window proxy.FaultWindow
	if int(id) < len(f.applying) {
		window = f.applying[id]
	}
	if first, applied := f.applied[id]; applied && window.First.IsZero() {
		window.First = first
	}
	if removed, ok := f.removed[id]; ok && (window.Retired.IsZero() || removed.Before(window.Retired)) {
		window.Retired = removed
	}
	return window
}

func (f *fakeHarness) createCR(_ context.Context, obj *unstructured.Unstructured) error {
	return f.record("createCR " + obj.GetName())
}

func (f *fakeHarness) patchCR(_ context.Context, name string, patch map[string]any) error {
	return f.record(fmt.Sprintf("patchCR %s %v", name, patch))
}

// targetStatus answers as a live target unless a test says otherwise.
func (f *fakeHarness) targetStatus() launch.Status {
	if f.targetGone {
		return launch.Status{Exit: f.targetExit}
	}
	return launch.Status{Running: true}
}

func (f *fakeHarness) deleteCR(ctx context.Context, name string) error {
	f.deletedCRAt = time.Now()
	if f.finalizerStays {
		f.recordDeletingCR(name, "deleted", f.deletedCRAt)
	}
	time.Sleep(f.deleteCRDelay)
	return f.write(ctx, "deleteCR "+name)
}

func (f *fakeHarness) awaitCRGone(_ context.Context, name string, until func() time.Time) (bool, error) {
	if f.inAwait != nil {
		f.inAwait()
	}
	f.awaitedUntil = append(f.awaitedUntil, until())
	return !f.crStays, f.record("awaitCRGone " + name)
}

func (f *fakeHarness) managedObjects(gvk schema.GroupVersionKind) []string {
	_ = f.record("managedObjects " + observe.KindName(gvk))
	return f.managed[gvk]
}

func (f *fakeHarness) deleteManaged(_ context.Context, gvk schema.GroupVersionKind, name string) (bool, error) {
	err := f.record("deleteManaged " + observe.KindName(gvk) + " " + name)
	return err == nil && !slices.Contains(f.gone, name), err
}

func (f *fakeHarness) patchFixture(_ context.Context, gvk schema.GroupVersionKind, name string, patch map[string]any) error {
	return f.record(fmt.Sprintf("patchFixture %s %s %v", observe.KindName(gvk), name, patch))
}

func (f *fakeHarness) deleteFixture(_ context.Context, gvk schema.GroupVersionKind, name string) error {
	return f.record("deleteFixture " + observe.KindName(gvk) + " " + name)
}

func (f *fakeHarness) createFixture(_ context.Context, fixture *unstructured.Unstructured) error {
	f.restoredAt = time.Now()
	return f.record(fmt.Sprintf("createFixture %s %s %v", observe.KindName(fixture.GroupVersionKind()), fixture.GetName(), fixture.Object["data"]))
}

func (f *fakeHarness) managedCount() int { return f.count }

// awaitClean reads the namespace before it waits, as the live harness's does.
func (f *fakeHarness) awaitClean(ctx context.Context, within time.Duration) (bool, error) {
	err := f.record("awaitClean " + within.String())
	if f.clean || err != nil {
		return f.clean, err
	}
	return false, ctx.Err()
}

func (f *fakeHarness) forceFinalizers(ctx context.Context) ([]string, error) {
	return f.forced, f.write(ctx, "forceFinalizers")
}

func (f *fakeHarness) empty(ctx context.Context) error { return f.write(ctx, "empty") }
func (f *fakeHarness) objects() *observe.Store         { return f.store }
func (f *fakeHarness) stop(ctx context.Context) error {
	f.stoppedAtOnce = ctx.Err() != nil
	return f.record("stop")
}

func (f *fakeHarness) unresolvedOwners() []cluster.Unresolved {
	if !slices.Contains(f.calls, "stop") {
		return nil
	}
	return f.unresolved
}

// requests grows with the run, so that a log sampled late is longer than one
// sampled at a checkpoint. Each entry the run adds names the call it followed.
func (f *fakeHarness) requests() []proxy.Request {
	log := slices.Clone(f.logged)
	for _, call := range f.calls {
		log = append(log, proxy.Request{Path: call})
	}
	return log
}

// recordCR records a version of the CR, as the Observer would.
func (f *fakeHarness) recordCR(name, resourceVersion string) {
	object := widget(name)
	object.SetNamespace(fakeNamespace)
	object.SetResourceVersion(resourceVersion)
	f.store.Record(widgetKind, object, time.Now())
}

// recordDeletingCR records the CR under deletion at the instant given, still
// held by a finalizer.
func (f *fakeHarness) recordDeletingCR(name, resourceVersion string, at time.Time) {
	object := widget(name)
	object.SetNamespace(fakeNamespace)
	object.SetResourceVersion(resourceVersion)
	object.SetFinalizers([]string{"example.com/stuck"})
	object.SetDeletionTimestamp(&metav1.Time{Time: at})
	f.store.Record(widgetKind, object, at)
}

// recordGoneCR records the CR's deletion at the instant given.
func (f *fakeHarness) recordGoneCR(name, resourceVersion string, at time.Time) {
	object := widget(name)
	object.SetNamespace(fakeNamespace)
	object.SetResourceVersion(resourceVersion)
	f.store.RecordDeletion(widgetKind, object, at)
}

// recordChild records a managed object of the CR, as the Observer would.
func (f *fakeHarness) recordChild(name, resourceVersion string) {
	object := &unstructured.Unstructured{Object: map[string]any{}}
	object.SetGroupVersionKind(configMapKind)
	object.SetNamespace(fakeNamespace)
	object.SetName(name)
	object.SetResourceVersion(resourceVersion)
	f.store.Record(configMapKind, object, time.Now())
}

func (f *fakeHarness) opCalls() []string       { return f.calls[:f.teardownStart()] }
func (f *fakeHarness) teardownCalls() []string { return f.calls[f.teardownStart():] }

// teardownStart is where the teardown begins: it clears the faults.
func (f *fakeHarness) teardownStart() int {
	if i := slices.Index(f.calls, "clearFaults"); i >= 0 {
		return i
	}
	return len(f.calls)
}

// fakeChecker answers each checkpoint from violations and notes, and records
// what it was given.
type fakeChecker struct {
	violations [][]Violation
	notes      [][]string
	err        error
	inputs     []Input
}

func (c *fakeChecker) Check(in Input) (Findings, error) {
	c.inputs = append(c.inputs, in)
	if c.err != nil {
		return Findings{}, c.err
	}
	var found Findings
	n := len(c.inputs) - 1
	if n < len(c.violations) {
		found.Violations = c.violations[n]
	}
	if n < len(c.notes) {
		found.Notes = c.notes[n]
	}
	return found, nil
}

var toyTarget = &target.Target{
	Name:      "toy-widget",
	Primary:   widgetKind,
	Sample:    widget("widget"),
	Manages:   []schema.GroupVersionKind{configMapKind},
	Timeouts:  testTimeouts,
	Ready:     readyCountsChildren,
	ReadyExpr: "has(status.ready) && status.ready == spec.count",
}

func readyCountsChildren(cr *unstructured.Unstructured) (bool, error) {
	ready, found, err := unstructured.NestedFloat64(cr.Object, "status", "ready")
	count, _, _ := unstructured.NestedFloat64(cr.Object, "spec", "count")
	return found && ready == count, err
}

func widget(name string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"count": float64(3)}}}
	object.SetGroupVersionKind(widgetKind)
	object.SetName(name)
	return object
}

// generated is a Widget the API server would name from the prefix.
func generated(prefix string) *unstructured.Unstructured {
	object := widget("")
	object.SetGenerateName(prefix)
	return object
}

// sequenceOf numbers the ops, as the format requires.
func sequenceOf(ops ...Op) Sequence {
	for i := range ops {
		ops[i].Index = i
	}
	sequence := Sequence{Seed: 1, Target: toyTarget.Name, Ops: ops}
	return sequence
}

func nth(i int) *int { return &i }

// runFake executes the sequence against a fake harness.
func runFake(t *testing.T, h *fakeHarness, check Checker, sequence Sequence) (Result, error) {
	t.Helper()
	if check == nil {
		check = &fakeChecker{}
	}
	return runSequence(t.Context(), toyTarget, sequence, Options{Check: check}, h)
}

func TestRunAppliesTheOpsInOrder(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
		Op{Type: OpSettle},
		Op{Type: OpRestart},
		Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(1)},
		Op{Type: OpRecreate, Obj: widget("widget")},
		Op{Type: OpDelete},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v, want no violation.", result.Violation)
	}
	// The first wait that converged shows the target runs, so reconciler-fuzzer
	// supervises it from there on, once.
	want := []string{
		"createCR widget", "settle", "supervise",
		"patchCR widget map[spec:map[count:5]]", "settle",
		"settle",
		"restart",
		"managedObjects v1/ConfigMap", "deleteManaged v1/ConfigMap widget-1", "settle",
		"deleteCR widget", "awaitCRGone widget", "createCR widget", "settle",
		"deleteCR widget", "settle",
	}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
	}
}

// withSecret is the toy with a Secret fixture.
func withSecret() *target.Target {
	withFixture := *toyTarget
	secret := &unstructured.Unstructured{Object: map[string]any{"data": map[string]any{"token": "czNjcjN0"}}}
	secret.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Secret"})
	secret.SetName("token")
	withFixture.Fixtures = []*unstructured.Unstructured{secret}
	return &withFixture
}

func TestRunChangesDeletesAndRestoresAFixture(t *testing.T) {
	h := newFakeHarness()
	declared := withSecret()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdateFixture, Kind: "v1/Secret", Name: "token", Patch: map[string]any{"data": map[string]any{"token": "abcd"}}},
		Op{Type: OpDeleteFixture, Kind: "v1/Secret", Name: "token", Until: &Until{Op: 4}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}, NoSettle: true},
		Op{Type: OpRestart},
		Op{Type: OpSettle},
	)

	result, err := runSequence(t.Context(), declared, sequence, Options{Check: &fakeChecker{}}, h)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"createCR widget", "settle", "supervise",
		"patchFixture v1/Secret token map[data:map[token:abcd]]", "settle",
		"deleteFixture v1/Secret token",
		"patchCR widget map[spec:map[count:5]]",
		"createFixture v1/Secret token map[token:abcd]", "restart",
		"settle",
	}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
	}
	var restored []int
	for _, applied := range result.Timeline.Ops {
		if applied.Restored {
			restored = append(restored, applied.Op.Index)
		}
		if applied.Deleted != "" {
			t.Errorf("Op %d names %s as deleted, and G7 asks back only a managed object.", applied.Op.Index, applied.Deleted)
		}
	}
	if !slices.Equal(restored, []int{4}) {
		t.Errorf("The ops %v restored a fixture, want op 4.", restored)
	}
	// Anything the target did in reply came after op 4 began.
	if h.restoredAt.Before(result.Timeline.Ops[4].At) {
		t.Errorf("reconciler-fuzzer restored the fixture at %v, before op 4 began at %v.", h.restoredAt, result.Timeline.Ops[4].At)
	}
	if token := declared.Fixtures[0].Object["data"].(map[string]any)["token"]; token != "czNjcjN0" {
		t.Errorf("The target's fixture holds the token %v, want the one it declares: the next run starts from it.", token)
	}
}

func TestAnOpRestoredAFixtureOnlyWhereACreateSucceeded(t *testing.T) {
	declared := withSecret()
	other := declared.Fixtures[0].DeepCopy()
	other.SetName("other")
	declared.Fixtures = append(declared.Fixtures, other)
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpDeleteFixture, Kind: "v1/Secret", Name: "token", Until: &Until{Op: 3}},
		Op{Type: OpDeleteFixture, Kind: "v1/Secret", Name: "other", Until: &Until{Op: 3}},
		Op{Type: OpSettle},
	)
	for _, test := range []struct {
		failing  string
		restored bool
	}{
		{failing: "token", restored: false},
		{failing: "other", restored: true},
	} {
		t.Run("where the restore of "+test.failing+" fails", func(t *testing.T) {
			h := newFakeHarness()
			refused := errors.New("the API server refused the create")
			h.fail["createFixture v1/Secret "+test.failing+" map[token:czNjcjN0]"] = refused

			result, err := runSequence(t.Context(), declared, sequence, Options{Check: &fakeChecker{}}, h)

			if !errors.Is(err, refused) {
				t.Fatalf("The run returned %v, want the refused create.", err)
			}
			if restored := result.Timeline.Ops[3].Restored; restored != test.restored {
				t.Errorf("Op 3 restored a fixture: %t, want %t.", restored, test.restored)
			}
		})
	}
}

func TestRunRefusesAnOpOnAFixtureTheTargetDoesNotDeclare(t *testing.T) {
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpDeleteFixture, Kind: "v1/Secret", Name: "other", Until: &Until{Op: 2}},
		Op{Type: OpSettle},
	)

	_, err := runSequence(t.Context(), withSecret(), sequence, Options{Check: &fakeChecker{}}, newFakeHarness())

	if want := "op 1 (deleteFixture): the target declares no fixture v1/Secret other"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("The run returned %v, want an error saying %q.", err, want)
	}
}

func TestRunRefusesAnOpOnAClusterScopedFixture(t *testing.T) {
	clusterRoleKind := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	withClusterRole := *toyTarget
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(clusterRoleKind)
	fixture.SetName("shared")
	withClusterRole.Fixtures = []*unstructured.Unstructured{fixture}
	withClusterRole.ClusterFixtures = []schema.GroupVersionKind{clusterRoleKind}
	for _, opType := range []OpType{OpUpdateFixture, OpDeleteFixture} {
		t.Run(string(opType), func(t *testing.T) {
			op := Op{Type: opType, Kind: "rbac.authorization.k8s.io/v1/ClusterRole", Name: "shared"}
			if opType == OpUpdateFixture {
				op.Patch = map[string]any{"rules": []any{}}
			}
			if opType == OpDeleteFixture {
				op.Until = &Until{Op: 2}
			}
			sequence := sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				op,
				Op{Type: OpSettle},
			)

			_, err := runSequence(t.Context(), &withClusterRole, sequence, Options{Check: &fakeChecker{}}, newFakeHarness())

			want := "cluster-scoped"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("The run returned %v, want an error saying %q.", err, want)
			}
		})
	}
}

func TestRunNamesTheCREachCROpWrote(t *testing.T) {
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("a")},
		Op{Type: OpUpdate, CR: "a", Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
		Op{Type: OpSettle},
		Op{Type: OpRestart},
		Op{Type: OpRecreate, CR: "a", Obj: widget("a")},
		Op{Type: OpDelete, CR: "a"},
	)

	result, err := runFake(t, newFakeHarness(), nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	var got []string
	for _, op := range result.Timeline.Ops {
		got = append(got, op.CR)
	}
	if want := []string{"a", "a", "", "", "a", "a"}; !slices.Equal(got, want) {
		t.Errorf("The ops name the CRs %q, want %q.", got, want)
	}
}

func TestRunActsOnTheCREachOpNames(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpUpdate, CR: "widget-2", Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
		Op{Type: OpRecreate, CR: "widget-2", Obj: widget("widget-2")},
		Op{Type: OpDelete, CR: "widget-2"},
		Op{Type: OpDelete},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"createCR widget", "settle", "supervise",
		"createCR widget-2", "settle",
		"patchCR widget-2 map[spec:map[count:5]]", "settle",
		"deleteCR widget-2", "awaitCRGone widget-2", "createCR widget-2", "settle",
		"deleteCR widget-2", "settle",
		"deleteCR widget", "settle",
	}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
	}
	var crs []string
	for _, op := range result.Timeline.Ops {
		crs = append(crs, op.CR)
	}
	if want := []string{"widget", "widget-2", "widget-2", "widget-2", "widget-2", "widget"}; !slices.Equal(crs, want) {
		t.Errorf("The ops name the CRs %q, want %q.", crs, want)
	}
}

func TestTheTeardownDeletesEveryCRTheRunCreated(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpRecreate, Obj: widget("widget")},
	)

	if _, err := runFake(t, h, nil, sequence); err != nil {
		t.Fatalf("The run failed: %v", err)
	}

	var deleted []string
	for _, call := range h.teardownCalls() {
		if strings.HasPrefix(call, "deleteCR ") {
			deleted = append(deleted, call)
		}
	}
	if want := []string{"deleteCR widget", "deleteCR widget-2"}; !slices.Equal(deleted, want) {
		t.Errorf("The teardown did %v, want each CR the run created deleted once: %v.", deleted, want)
	}
}

func TestRunSkipsTheSettleAfterANoSettleOp(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true},
		Op{Type: OpDelete, NoSettle: true},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); slices.Contains(got, "settle") {
		t.Errorf("The run did %v, want no settle wait after an op that says noSettle.", got)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{Teardown}) {
		t.Errorf("The run checkpointed at the ops %v, want the teardown's alone: no settle ends, no checkpoint.", got)
	}
}

// The checkpoints of DESIGN.md §4: one where a settle wait ends, converged or
// not, and one after the teardown deletion window.
func TestRunCheckpointsWhereTheDesignSaysSo(t *testing.T) {
	h := newFakeHarness()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}, NoSettle: true},
		Op{Type: OpRestart},
		Op{Type: OpSettle},
		Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []int{0, 3, 4, Teardown}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, want) {
		t.Errorf("The run checkpointed at the ops %v, want %v.", got, want)
	}
}

func TestRunCheckpointsWhenTheSettleExpires(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faulting = true
	sequence := sequenceOf(Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}, Op{Type: OpSettle})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, Recovery}) {
		t.Errorf("The run checkpointed at the ops %v, want one where the expired settle ended.", got)
	}
	if wait := result.Timeline.Ops[1].Settled; wait == nil || wait.Converged {
		t.Errorf("The settle op recorded %+v, want a wait that did not converge.", wait)
	}
}

func TestRunEndsAtTheFirstViolation(t *testing.T) {
	h := newFakeHarness()
	first := Violation{ID: "G2", Statement: "the target churns"}
	check := &fakeChecker{violations: [][]Violation{
		nil,
		{first, {ID: "G1"}},
		{{ID: "G6"}},
	}}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
		Op{Type: OpSettle},
		Op{Type: OpDelete},
	)

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != first.ID {
		t.Errorf("The run reported %+v, want the first violation %+v.", result.Violation, first)
	}
	if got := h.opCalls(); slices.Contains(got, "deleteCR widget") {
		t.Errorf("The run did %v, want it to stop at the first violation.", got)
	}
	if len(check.inputs) != 2 {
		t.Errorf("The checks ran %d times, want them to stop at the first violation.", len(check.inputs))
	}
}

func TestRunRecordsG4WhenASettleExpiresWithNoFaultActive(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpDelete})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %+v, want a G4 violation.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at the ops %v, want one where the expired wait ended.", got)
	}
	if got := h.opCalls(); slices.Contains(got, "deleteCR widget") {
		t.Errorf("The run did %v, want it to end at the G4 violation.", got)
	}
}

// A G4 the expired wait raised carries the evidence a check's G4 would: the
// CR's version history and the request log as the expiry left it
// (DESIGN.md §5.7).
func TestTheG4OfAnExpiredWaitCarriesTheEvidence(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	for i := range 25 {
		h.recordCR("widget", strconv.Itoa(10+i))
		h.logged = append(h.logged, proxy.Request{Path: "before-" + strconv.Itoa(i)})
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %+v, want a G4 violation.", result.Violation)
	}
	if wait := result.Timeline.Ops[0].Settled; wait == nil || !result.Violation.At.Equal(wait.Window.End) {
		t.Errorf("The violation is stamped %v, want the instant the settle wait expired.", result.Violation.At)
	}
	versions := result.Violation.Versions
	if len(versions) != 20 {
		t.Fatalf("The violation carries %d versions of the CR, want the 20 a report quotes.", len(versions))
	}
	if last := versions[19].ResourceVersion; last != "34" {
		t.Errorf("The versions end at resourceVersion %s, want the CR's latest, 34.", last)
	}
	requests := result.Violation.Requests
	if len(requests) != 20 {
		t.Fatalf("The violation carries %d requests, want the 20 a report quotes.", len(requests))
	}
	if last := requests[19].Path; last != "settle" {
		t.Errorf("The requests end at %q, want the settle the wait expired in.", last)
	}
	if first := requests[0].Path; first != "before-7" {
		t.Errorf("The requests open at %q, want the 20 nearest the expiry, from before-7.", first)
	}
	// A report cannot say what the bound left out unless the violation says how
	// much it chose from (#22).
	if want := 25; result.Violation.VersionsTotal != want {
		t.Errorf("The violation says it chose from %d versions, want the %d the run recorded.", result.Violation.VersionsTotal, want)
	}
	if total := result.Violation.RequestsTotal; total <= len(requests) {
		t.Errorf("The violation says it chose from %d requests and quotes %d of them.", total, len(requests))
	}
}

// A message that prints a violation wants the finding. The evidence behind one
// runs to thousands of characters, and the recordings hold it.
func TestAViolationPrintsAsOneLine(t *testing.T) {
	carrying := Violation{
		ID: "G4", Statement: "the target converges", Evidence: "the settle wait expired",
		Versions: []observe.Version{{ResourceVersion: "12"}},
	}
	bare := Violation{ID: "G1", Statement: "the target falls quiet"}

	if got, want := fmt.Sprintf("%+v", carrying), "G4 the target converges; the settle wait expired"; got != want {
		t.Errorf("A violation prints as %q, want %q.", got, want)
	}
	if got, want := fmt.Sprintf("%v", bare), "G1 the target falls quiet"; got != want {
		t.Errorf("A violation carrying no evidence prints as %q, want %q.", got, want)
	}
}

// The G4 the settle wait found comes before what the checks find at the same
// checkpoint, because a run ends at its first violation.
func TestRunKeepsTheViolationTheExpiredSettleFoundFirst(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	check := &fakeChecker{violations: [][]Violation{{{ID: "P1"}}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %+v, want the G4 the expired wait found first.", result.Violation)
	}
}

func TestRunExcusesEveryWaitAFaultWasActiveThrough(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(3)}}},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	// The fault excused every op. The target never recovered once the
	// teardown cleared it.
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %+v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, 2, Recovery}) {
		t.Errorf("The run checkpointed at the ops %v.", got)
	}
}

// A fault excuses the target over the window the proxy applied it in, so a
// fault whose own trigger has run out excuses nothing after it
// (DESIGN.md §5.2, D36).
func TestRunStopsExcusingTheTargetWhereTheProxyRetiredTheFault(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	applied, retired := time.Now().Add(-10*time.Second), time.Now().Add(-9*time.Second)
	h.applying = []proxy.FaultWindow{{First: applied, Retired: retired}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %v, want a G4: the proxy had stopped applying the fault.", result.Violation)
	}
	if got := result.Timeline.Faults; len(got) != 1 || !got[0].Start.Equal(applied) || !got[0].End.Equal(retired) {
		t.Errorf("The fault's window is %+v, want the %v to %v the proxy applied it in.", got, applied, retired)
	}
}

// Dropping a fault that ran out leaves the next fault its own window, as when
// two fault ops inject equal specs.
func TestRunKeepsTheWindowOfTheFaultAfterOneThatRanOut(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	first, retired, second := time.Now().Add(-10*time.Second), time.Now().Add(-9*time.Second), time.Now().Add(-8*time.Second)
	h.applying = []proxy.FaultWindow{{First: first}}
	h.applyingInWait = []proxy.FaultWindow{{First: first, Retired: retired}, {First: second}}
	fault := Fault{Match: Match{Verb: "create", Resource: "configmaps"}, Action: Action{Error: 500}, Until: Trigger{Count: 12}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &fault},
		Op{Type: OpFault, Fault: &fault},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got, settled := result.Timeline.Faults[1], result.Timeline.Ops[3].Settled; !got.Start.Equal(second) || !got.End.After(settled.Window.End) {
		t.Errorf("The second fault's window is %+v, want its own from %v, open past the last settle wait.", got, second)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// A request can meet a fault just as the Runner takes it off, whether its op
// trigger ran out or the teardown cleared it. The target then owes recovery.
func TestRunKeepsARequestFaultedAsTheFaultWasRemoved(t *testing.T) {
	h := newFakeHarness()
	h.faultingAsRemoved = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(2)}}},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	for i, window := range result.Timeline.Faults {
		if window.Start.IsZero() || window.End.Before(window.Start) {
			t.Errorf("Fault %d ran %+v, want the request it faulted as it was removed.", i, window)
		}
	}
	if result.Timeline.Recovery == nil {
		t.Error("The teardown gave the target no recovery from the fault it cleared.")
	}
}

// A fault that stopped as the wait ended leaves the target time it has not
// had yet, so the Runner leaves the wait to the teardown's recovery.
func TestRunExcusesAWaitThatEndedWhileTheTargetWasOwedRecovery(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.applying = []proxy.FaultWindow{{First: time.Now().Add(-time.Second), Retired: time.Now()}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// The proxy first applies a fault when the target makes a request it matches,
// which may be after the wait began.
func TestRunExcusesAWaitAFaultFirstReachedPartWayThrough(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faultingInWait = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if applied, wait := result.Timeline.Faults[0].Start, result.Timeline.Ops[1].Settled.Window; !applied.After(wait.Start) {
		t.Fatalf("The proxy first applied the fault at %v, want it after the wait began at %v.", applied, wait.Start)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want only the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// A fault active as a wait began excuses the wait only while the target is
// still owed time to recover from it when the wait ends.
func TestRunRecordsG4WhenAWaitOutlastsTheRecoveryTheFaultOwed(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.applying = []proxy.FaultWindow{{First: time.Now()}}
	h.retiresInWait = time.Millisecond
	short := *toyTarget
	short.Timeouts.Settle = 10 * time.Millisecond
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runSequence(t.Context(), &short, sequence, Options{Check: &fakeChecker{}}, h)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if fault, wait := result.Timeline.Faults[0], result.Timeline.Ops[1].Settled.Window; fault.Start.After(wait.Start) || !fault.End.After(wait.Start) {
		t.Fatalf("The fault was active from %v to %v, want it active as the wait began at %v.", fault.Start, fault.End, wait.Start)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "after op 1 (create)") {
		t.Errorf("The run reported %v, want the G4 of the create's wait.", result.Violation)
	}
}

// A fault op whose fault matches no request changes nothing, so it leaves the
// run as judged as one with no fault op at all (DESIGN.md §6, D36).
func TestRunJudgesARunWhoseFaultMatchedNothing(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: "secrets"}, Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %v, want a G4: the proxy never applied the fault.", result.Violation)
	}
	if got := result.Timeline.Faults; len(got) != 1 || !got[0].Start.IsZero() {
		t.Errorf("The fault's window is %+v, want no window: the proxy applied nothing.", got)
	}
	if !slices.Contains(result.Notes, "the proxy applied the fault of op 0 to no request") {
		t.Errorf("The run noted %q, want it to say the proxy applied the fault to nothing.", result.Notes)
	}
}

func TestOnlyAFaultTheProxyNeverAppliedLeavesANote(t *testing.T) {
	h := newFakeHarness()
	h.applying = []proxy.FaultWindow{{First: time.Now()}, {}}
	fault := &Fault{Match: Match{Resource: "configmaps"}, Action: Action{Error: 500}, Until: Trigger{Count: 1}}

	result, err := runFake(t, h, nil, sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: fault},
		Op{Type: OpFault, Fault: fault},
		Op{Type: OpSettle},
	))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	var noted []string
	for _, note := range result.Notes {
		if strings.Contains(note, "to no request") {
			noted = append(noted, note)
		}
	}
	if want := []string{"the proxy applied the fault of op 2 to no request"}; !slices.Equal(noted, want) {
		t.Errorf("The run noted %q, want %q: the proxy applied the fault of op 1.", noted, want)
	}
}

// No op follows the fault op here, so no op reads the fault's window after
// the proxy first applied it.
func TestAFaultTheProxyFirstAppliedAfterTheLastOpLeavesNoNote(t *testing.T) {
	h := newFakeHarness()
	h.faulting = true

	result, err := runFake(t, h, nil, sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
	))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "to no request") {
			t.Errorf("The run noted %q about a fault the proxy applied.", note)
		}
	}
}

// The proxy records a request's resource as the plural the API server serves,
// so a fault on any other name would match nothing.
func TestAFaultOnAResourceTheAPIServerDoesNotServeEndsTheRun(t *testing.T) {
	for _, test := range []struct {
		resource string
		want     []string
	}{
		{"configmap", []string{"op 1 (fault)", `match.resource "configmap"`, "did you mean configmaps?"}},
		{"ConfigMaps", []string{`match.resource "ConfigMaps"`, "did you mean configmaps?"}},
		{"Secret", []string{"did you mean secrets?"}},
		{"gizmos", []string{`match.resource "gizmos"`, "plural"}},
	} {
		t.Run(test.resource, func(t *testing.T) {
			h := newFakeHarness()

			_, err := runFake(t, h, nil, sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: test.resource}, Action: Action{Error: 500}}},
				Op{Type: OpSettle},
			))

			if err == nil {
				t.Fatal("The run accepted a fault on a resource the API server does not serve.")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("The run returned %q, want %q.", err, want)
				}
			}
			if h.added != 0 {
				t.Errorf("The proxy was given %d faults, want none.", h.added)
			}
		})
	}
}

func TestAFaultThatDiscoveryCannotResolveEndsTheRun(t *testing.T) {
	h := newFakeHarness()
	h.fail["servedResources"] = errors.New("discovery is down")

	_, err := runFake(t, h, nil, sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: "configmaps"}, Action: Action{Error: 500}}},
		Op{Type: OpSettle},
	))

	if err == nil || !strings.Contains(err.Error(), "discovery is down") {
		t.Errorf("The run returned %v, want the discovery error.", err)
	}
}

func TestRunRecordsG4OnceTheFaultHasExpired(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(1)}}},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %+v, want a G4 violation: the fault ended at op 1.", result.Violation)
	}
	if got := h.opCalls(); !slices.Contains(got, "removeFault 0") {
		t.Errorf("The run did %v, want the expired fault removed.", got)
	}
}

// A settle wait runs until the target has had as long as the faults lasted,
// and T_settle more, to recover from them. The fault here stops during the
// wait.
func TestRunTellsTheSettleWaitWhatRecoveryTheFaultsAreOwed(t *testing.T) {
	h := newFakeHarness()
	applied, retired := time.Now().Add(-3*time.Second), time.Now().Add(-time.Second)
	h.applying = []proxy.FaultWindow{{First: applied}}
	h.applyingInWait = []proxy.FaultWindow{{First: applied, Retired: retired}}
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	_, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := retired.Add(retired.Sub(applied) + testTimeouts.Settle)
	if len(h.owed) == 0 || !h.owed[0].Equal(want) {
		t.Errorf("The settle wait was told the target owed %v, want %v.", h.owed, want)
	}
}

// Nothing shows what a deleteManaged changed, and a target can converge while
// an informer of its still backs off from a fault. So the wait after the op
// does not converge before each fault that stopped has had as long as it
// lasted, and T_settle more.
func TestRunHoldsTheWaitAfterADeleteManagedForEachFaultThatStopped(t *testing.T) {
	applied, retired := time.Now().Add(-6*time.Second), time.Now().Add(-2*time.Second)
	owed := retired.Add(retired.Sub(applied) + testTimeouts.Settle)
	fault := Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}}
	create := Op{Type: OpCreate, Obj: widget("widget")}
	for _, test := range []struct {
		name     string
		ops      []Op
		applying []proxy.FaultWindow
		// floors are the floors of the waits in turn.
		floors []time.Time
	}{
		{name: "after a fault the run converged since",
			ops:      []Op{create, fault, {Type: OpSettle}, {Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}},
			applying: []proxy.FaultWindow{{First: applied, Retired: retired}}, floors: []time.Time{{}, {}, owed}},
		{name: "with no fault",
			ops: []Op{create, {Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}}, floors: []time.Time{{}, {}}},
		{name: "that deleted nothing",
			ops:      []Op{create, fault, {Type: OpSettle}, {Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(7)}},
			applying: []proxy.FaultWindow{{First: applied, Retired: retired}}, floors: []time.Time{{}, {}, {}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.applying = test.applying

			_, err := runFake(t, h, nil, sequenceOf(test.ops...))

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if !slices.EqualFunc(h.floors, test.floors, time.Time.Equal) {
				t.Errorf("The settle waits had the floors %v, want %v.", h.floors, test.floors)
			}
		})
	}
}

func TestRunGivesADeletionUntilItsDeadline(t *testing.T) {
	h := newFakeHarness()
	h.finalizerStays = true

	_, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpDelete}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	deleted := h.store.HistoryOf(widgetKind, "widget")[0].Time
	want := []time.Time{{}, deleted.Add(testTimeouts.Delete)}
	if !slices.EqualFunc(h.owed, want, time.Time.Equal) {
		t.Errorf("The settle waits were told the run owed %v, want %v.", h.owed, want)
	}
}

// A CR past its deletion deadline is G3's to judge, whichever wait ends on it.
func TestRunLeavesToG3AWaitThatEndedOnACRPastItsDeletionDeadline(t *testing.T) {
	for _, test := range []struct {
		name     string
		faulting bool
		sequence Sequence
		want     []int
	}{
		{name: "an op's wait", sequence: sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpDelete}),
			want: []int{0, 1, Teardown}},
		{name: "the recovery", faulting: true,
			sequence: sequenceOf(Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}, Op{Type: OpCreate, Obj: widget("widget")}),
			want:     []int{1, Recovery, Teardown}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.converged = false
			h.faulting = test.faulting
			h.recordDeletingCR("widget", "11", time.Now().Add(-testTimeouts.Delete-time.Second))

			result, err := runFake(t, h, nil, test.sequence)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if result.Violation != nil {
				t.Errorf("The run reported %v, want no G4: the CR had outlived its deletion deadline.", result.Violation)
			}
			if got := checkpointsAt(result.Timeline); !slices.Equal(got, test.want) {
				t.Errorf("The run checkpointed at %v, want %v.", got, test.want)
			}
		})
	}
}

// A request the proxy held where a wait ended was about to change what the
// checks read there.
func TestRunMarksEachCheckpointWhereTheProxyHeldARequest(t *testing.T) {
	for _, test := range []struct {
		name  string
		holds func(before time.Time) (bool, time.Time)
		want  bool
	}{
		{"none", nil, false},
		{"one it still holds", func(time.Time) (bool, time.Time) { return true, time.Time{} }, true},
		{"one it released after", func(before time.Time) (bool, time.Time) { return false, before.Add(time.Millisecond) }, true},
		{"one it released within T_stable", func(before time.Time) (bool, time.Time) {
			return false, before.Add(time.Millisecond - testTimeouts.Stable)
		}, true},
		{"one it released T_stable before", func(before time.Time) (bool, time.Time) { return false, before.Add(-testTimeouts.Stable) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.holds = test.holds
			check := &heldChecker{}

			result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if got, want := checkpointsAt(result.Timeline), []int{0, Teardown}; !slices.Equal(got, want) {
				t.Fatalf("The run checkpointed at %v, want %v.", got, want)
			}
			if want := []bool{test.want, test.want}; !slices.Equal(check.read, want) {
				t.Errorf("The checks read the checkpoints as held %v, want %v.", check.read, want)
			}
		})
	}
}

// heldChecker records whether the checks read each checkpoint as held.
type heldChecker struct{ read []bool }

func (c *heldChecker) Check(in Input) (Findings, error) {
	c.read = append(c.read, in.Timeline.Checkpoints[len(in.Timeline.Checkpoints)-1].Held)
	return Findings{}, nil
}

func TestRunGivesTheCRARecreateDeletesUntilItsDeadline(t *testing.T) {
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpRecreate, Obj: widget("widget")})

	t.Run("once the Observer has seen the deletion", func(t *testing.T) {
		h := newFakeHarness()
		h.finalizerStays = true

		if _, err := runFake(t, h, nil, sequence); err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		deleted := h.store.HistoryOf(widgetKind, "widget")[0].Time
		if want := []time.Time{deleted.Add(testTimeouts.Delete)}; !slices.EqualFunc(h.awaitedUntil, want, time.Time.Equal) {
			t.Errorf("The recreate waited for the CR until %v, want %v.", h.awaitedUntil, want)
		}
	})

	t.Run("before the Observer has seen it", func(t *testing.T) {
		h := newFakeHarness()
		began := time.Now()

		_, err := runFake(t, h, nil, sequence)

		ended := time.Now()
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if len(h.awaitedUntil) != 1 || h.awaitedUntil[0].Before(began.Add(testTimeouts.Delete)) || h.awaitedUntil[0].After(ended.Add(testTimeouts.Delete)) {
			t.Errorf("The recreate waited for the CR until %v, want %v after the delete.", h.awaitedUntil, testTimeouts.Delete)
		}
	})

	t.Run("past a request the proxy held by then", func(t *testing.T) {
		h := newFakeHarness()
		const releasedAfter = time.Minute
		h.holds = func(before time.Time) (bool, time.Time) { return false, before.Add(releasedAfter) }
		began := time.Now()

		_, err := runFake(t, h, nil, sequence)

		ended := time.Now()
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		past := testTimeouts.Delete + releasedAfter + testTimeouts.Settle
		if len(h.awaitedUntil) != 1 || h.awaitedUntil[0].Before(began.Add(past)) || h.awaitedUntil[0].After(ended.Add(past)) {
			t.Errorf("The recreate waited for the CR until %v, want T_settle past the release %v after its deadline.", h.awaitedUntil, releasedAfter)
		}
	})

	t.Run("past a request the proxy still holds well after its deadline", func(t *testing.T) {
		h := newFakeHarness()
		at := time.Now()
		h.clock = func() time.Time { return at }
		h.inAwait = func() { at = at.Add(testTimeouts.Delete + time.Minute) }
		h.holds = func(time.Time) (bool, time.Time) { return true, time.Time{} }

		if _, err := runFake(t, h, nil, sequence); err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if want := []time.Time{at.Add(testTimeouts.Settle)}; !slices.EqualFunc(h.awaitedUntil, want, time.Time.Equal) {
			t.Errorf("The recreate waited for the CR until %v, want %v: T_settle from now, since the release is yet to come.", h.awaitedUntil, want)
		}
	})
}

// A recreate cannot create a CR while the old one stays, so the wait for it is
// judged where it ends.
func TestRunJudgesARecreateWhoseCRStayed(t *testing.T) {
	for _, test := range []struct {
		name string
		// fault is the fault op's window, placed from the deletion.
		fault func(deleted time.Time) proxy.FaultWindow
		found []Violation
		want  string
		says  string
	}{
		{name: "a check reports it", found: []Violation{{ID: "G3"}}, want: "G3"},
		{name: "a fault reached into its deletion", fault: func(deleted time.Time) proxy.FaultWindow {
			return proxy.FaultWindow{First: deleted.Add(time.Second), Retired: deleted.Add(2 * time.Second)}
		}, want: "G4", says: fmt.Sprintf("(timeouts.delete is %s)", testTimeouts.Delete)},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.crStays = true
			h.deleteCRDelay = 20 * time.Millisecond
			deleted := time.Now().Add(-testTimeouts.Delete - time.Second)
			h.recordDeletingCR("widget", "11", deleted)
			if test.fault != nil {
				h.applying = []proxy.FaultWindow{test.fault(deleted)}
			}
			check := &fakeChecker{violations: [][]Violation{nil, test.found}}
			sequence := sequenceOf(
				Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRecreate, Obj: widget("widget")},
			)

			result, err := runFake(t, h, check, sequence)

			ended := time.Now()
			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if got := result.Violation; got == nil || got.ID != test.want || !strings.Contains(got.Statement, test.says) {
				t.Errorf("The run reported %v, want %s saying %q.", got, test.want, test.says)
			}
			if got, want := checkpointsAt(result.Timeline), []int{1, 2}; !slices.Equal(got, want) {
				t.Fatalf("The run checkpointed at %v, want %v.", got, want)
			}
			if settled, stayed := result.Timeline.Checkpoints[0], result.Timeline.Checkpoints[1]; settled.Stayed || !stayed.Stayed {
				t.Errorf("The checkpoints are %+v and %+v, want the second alone marked as a recreate's whose CR stayed.", settled, stayed)
			}
			deleteReturned := result.Timeline.Ops[2].At.Add(h.deleteCRDelay)
			if stayed := result.Timeline.Checkpoints[1]; stayed.Began.Before(deleteReturned) || stayed.At.Before(stayed.Began) || stayed.At.After(ended) {
				t.Errorf("The recreate's checkpoint spans %v to %v, want the wait for its CR, which began after %v.", stayed.Began, stayed.At, deleteReturned)
			}
			if ops := check.inputs[len(check.inputs)-1].Timeline.Ops; len(ops) != 3 || ops[2].CR != "widget" {
				t.Errorf("The checks at the recreate's checkpoint read the ops %+v, want the recreate of widget last.", ops)
			}
			want := []string{"addFault 0", "createCR widget", "settle", "supervise", "deleteCR widget", "awaitCRGone widget"}
			if got := h.opCalls(); !slices.Equal(got, want) {
				t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
			}
		})
	}
}

// The op cannot create its CR while a fault keeps the old one, so the run
// goes on to the teardown, which judges the target once the fault is cleared.
func TestARecreateWhoseOldCRAFaultKeptStopsTheRun(t *testing.T) {
	h := newFakeHarness()
	h.crStays = true
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpRecreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil || result.Violation != nil {
		t.Fatalf("The run returned %v and reported %v, want neither: a fault kept the CR.", err, result.Violation)
	}
	const stopped = "op 2 (recreate) stopped the run: the CR widget was still there"
	if !slices.ContainsFunc(result.Notes, func(note string) bool { return strings.HasPrefix(note, stopped) }) {
		t.Errorf("The run noted %q, want one beginning %q.", result.Notes, stopped)
	}
	want := []string{"addFault 0", "createCR widget", "settle", "supervise", "deleteCR widget", "awaitCRGone widget"}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did\n\t%v\nwant\n\t%v", got, want)
	}
	if result.Timeline.Recovery == nil {
		t.Error("The teardown gave the target no recovery from the fault that kept the CR.")
	}
	if got, want := checkpointsAt(result.Timeline), []int{1, 2, Recovery, Teardown}; !slices.Equal(got, want) {
		t.Errorf("The run checkpointed at %v, want %v.", got, want)
	}
}

// The checks excuse a CR past its G3 deadline only for G3 to report it, so
// one that no check reports means they disagree.
func TestARecreateWhoseCRStaysUnexcusedIsAHarnessError(t *testing.T) {
	h := newFakeHarness()
	h.crStays = true
	h.recordDeletingCR("widget", "11", time.Now().Add(-testTimeouts.Delete-time.Second))
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpRecreate, Obj: widget("widget")},
	)

	_, err := runFake(t, h, nil, sequence)

	if want := "op 1 (recreate): the CR widget was still there"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("The run returned %v, want %q.", err, want)
	}
}

func TestARecreateWhoseCRStayedSaysHowLongItWaited(t *testing.T) {
	began := time.Now()
	stayed := &crStayed{cr: "widget", wait: Wait{Window: Window{Start: began, End: began.Add(10*time.Second + 300*time.Millisecond)}}}

	if got, want := stayed.Error(), "the CR widget was still there 10s after its delete"; got != want {
		t.Errorf("The error says %q, want %q.", got, want)
	}
}

func TestRunReportsAWaitWhoseCRWentBeforeItsDeletionDeadline(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.inWait = func() {
		gone := time.Now().Add(time.Millisecond)
		deadline := gone.Add(time.Millisecond)
		h.recordDeletingCR("widget", "11", deadline.Add(-testTimeouts.Delete))
		h.recordGoneCR("widget", "12", gone)
		time.Sleep(time.Until(deadline))
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Errorf("The run reported %v, want G4: the CR went before its deletion deadline.", result.Violation)
	}
}

// A fault still active when the ops end is one no settle wait gave the target
// time to recover from, so the teardown gives it one before its quiet window.
func TestTheTeardownWaitsForTheTargetToRecoverFromAFaultItCleared(t *testing.T) {
	h := newFakeHarness()
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"clearFaults",
		"settle",
		"sleep " + testTimeouts.Stable.String(),
		"deleteCR widget",
		"awaitClean " + (testTimeouts.Delete + deletionMargin).String(),
		"empty",
		"forceFinalizers",
		"stop",
	}
	if got := h.teardownCalls(); !slices.Equal(got, want) {
		t.Errorf("The teardown did\n\t%v\nwant\n\t%v", got, want)
	}
	recovery := result.Timeline.Recovery
	if recovery == nil || !recovery.Converged {
		t.Fatalf("The run recorded the recovery %+v, want a wait that converged.", recovery)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0, 2, Recovery, Teardown}) {
		t.Errorf("The run checkpointed at %v, want one where the recovery ended.", got)
	}
	if quiet := result.Timeline.Quiet.Start; quiet.Before(recovery.Window.End) {
		t.Errorf("The teardown's quiet window opens at %v, before the recovery ended at %v.", quiet, recovery.Window.End)
	}
}

func TestTheTeardownWaitsForNoRecoveryTheTargetIsNotOwed(t *testing.T) {
	for _, test := range []struct {
		name     string
		applying []proxy.FaultWindow
	}{
		{name: "a fault the proxy applied to no request"},
		{name: "a fault the target converged after",
			applying: []proxy.FaultWindow{{First: time.Now().Add(-3 * time.Second), Retired: time.Now().Add(-2 * time.Second)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.applying = test.applying
			sequence := sequenceOf(
				Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 1}}},
				Op{Type: OpCreate, Obj: widget("widget")},
			)

			result, err := runFake(t, h, nil, sequence)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if got := h.teardownCalls(); slices.Contains(got, "settle") || result.Timeline.Recovery != nil {
				t.Errorf("The teardown did %v, want no wait: the target owed nothing.", got)
			}
		})
	}
}

// A run that ended early is judged no further, so the teardown gives it no
// time to recover. The recorded run still shows where the fault stopped.
func TestTheTeardownWaitsForNoRecoveryAfterTheRunEnded(t *testing.T) {
	for _, test := range []struct {
		name  string
		check *fakeChecker
		fail  map[string]error
	}{
		{name: "at a violation", check: &fakeChecker{violations: [][]Violation{{{ID: "G2"}}}}},
		{name: "at a harness error", check: &fakeChecker{}, fail: map[string]error{"restart": errors.New("the target will not die")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.faulting = true
			if test.fail != nil {
				h.fail = test.fail
			}
			sequence := sequenceOf(
				Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRestart},
			)

			result, _ := runFake(t, h, test.check, sequence)

			if got := h.teardownCalls(); slices.Contains(got, "settle") || result.Timeline.Recovery != nil {
				t.Errorf("The teardown did %v, want no wait: the run had ended.", got)
			}
			if fault := result.Timeline.Faults[0]; fault.End.IsZero() {
				t.Errorf("The fault ran %+v, want its window closed where the teardown cleared it.", fault)
			}
		})
	}
}

func TestRunRecordsG4WhenTheRecoveryExpires(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}},
		Op{Type: OpCreate, Obj: widget("widget")},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	violation, recovery := result.Violation, result.Timeline.Recovery
	if violation == nil || violation.ID != "G4" || !strings.Contains(violation.Statement, "after the last fault stopped") {
		t.Fatalf("The run reported %v, want the G4 of the wait after the last fault stopped.", violation)
	}
	if recovery == nil || recovery.Converged || !violation.At.Equal(recovery.Window.End) {
		t.Errorf("The violation is stamped %v, want the end of a recovery that expired: %+v.", violation.At, recovery)
	}
	if want := fmt.Sprintf("with no fault active: in %v (timeouts.settle is %v),",
		recovery.Window.End.Sub(recovery.Window.Start).Round(time.Millisecond), toyTarget.Timeouts.Settle); !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say how long the wait ran: %q.", violation.Statement, want)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{1, Recovery}) {
		t.Errorf("The run checkpointed at %v, want the recovery's last and no deletion judged.", got)
	}
}

// An interrupt or the deadline ends the run's context, and the run is
// abandoned wherever it is. Its teardown waits for nothing more and judges
// nothing, and it still takes back what the run made, every CR included.
func TestAnAbandonedRunWaitsForNothing(t *testing.T) {
	quiet := "sleep " + testTimeouts.Stable.String()
	deletion := "awaitClean " + (testTimeouts.Delete + deletionMargin).String()
	cleanup := []string{"deleteCR widget", "deleteCR widget-2", "empty", "forceFinalizers", "stop"}
	for _, test := range []struct {
		name  string
		at    string
		fault bool
		want  []string
	}{
		{name: "in an op's settle wait", at: "settle", want: slices.Concat([]string{"clearFaults"}, cleanup)},
		{name: "in the recovery", at: "clearFaults", fault: true, want: slices.Concat([]string{"clearFaults", "settle"}, cleanup)},
		{name: "in the quiet window", at: quiet, want: slices.Concat([]string{"clearFaults", quiet}, cleanup)},
		{name: "in the deletion window", at: deletion,
			want: []string{"clearFaults", quiet, "deleteCR widget", "deleteCR widget-2", deletion, "empty", "forceFinalizers", "stop"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.clean, h.forced = false, []string{"toy.reconciler-fuzzer/v1/Widget widget"}
			ctx, cancel := context.WithCancel(t.Context())
			h.cancel, h.cancelsAfter = cancel, test.at
			ops := []Op{{Type: OpCreate, Obj: widget("widget"), NoSettle: true}, {Type: OpCreate, Obj: widget("widget-2")}}
			if test.fault {
				h.faulting = true
				ops = append(ops, Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}, Op{Type: OpSettle})
			}

			result, err := runSequence(ctx, toyTarget, sequenceOf(ops...), Options{Check: &fakeChecker{}}, h)

			if !errors.Is(err, context.Canceled) {
				t.Fatalf("The run returned %v, want the caller's context ended.", err)
			}
			if got := h.teardownCalls(); !slices.Equal(got, test.want) {
				t.Errorf("The teardown did\n\t%v\nwant\n\t%v", got, test.want)
			}
			if !h.stoppedAtOnce {
				t.Error("The teardown gave the target its grace period, want it killed at once.")
			}
			if len(h.cancelled) > 0 {
				t.Errorf("The teardown made the writes %v under the context that had ended.", h.cancelled)
			}
			if got := checkpointsAt(result.Timeline); slices.Contains(got, Teardown) {
				t.Errorf("The run checkpointed at %v, want no deletion judged.", got)
			}
			// The namespace had no time to empty on its own.
			if notes := strings.Join(result.Notes, "\n"); strings.Contains(notes, "force-removed") {
				t.Errorf("The run notes %q, and nothing waited for the finalizers.", notes)
			}
		})
	}
}

// A violation found before the context ended stands, however far the teardown
// got.
func TestAViolationStandsWhenItsTeardownIsAbandoned(t *testing.T) {
	h := newFakeHarness()
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel, h.cancelsAfter = cancel, "sleep "+testTimeouts.Stable.String()
	check := &fakeChecker{violations: [][]Violation{{{ID: "G2"}}}}

	result, err := runSequence(ctx, toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}), Options{Check: check}, h)

	if err != nil {
		t.Errorf("The run returned %v, want the violation alone.", err)
	}
	if result.Violation == nil || result.Violation.ID != "G2" {
		t.Errorf("The run reported %v, want the G2 it found before the teardown.", result.Violation)
	}
}

// An interrupt ends supervision too, so the target is not restarted after it.
func TestTheRunSupervisesTheTargetUnderItsContext(t *testing.T) {
	h := newFakeHarness()
	ctx, cancel := context.WithCancel(t.Context())

	_, err := runSequence(ctx, toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}), Options{Check: &fakeChecker{}}, h)
	cancel()

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if h.supervising == nil || h.supervising.Err() == nil {
		t.Error("The run supervised the target under a context its own did not end.")
	}
}

// A terminal's Ctrl-C stops the target as it ends the run's context. A target
// found stopped after that may be the context's doing, and one found stopped
// before is not.
func TestATargetFoundStoppedOnceTheContextEndedCarriesTheContextsError(t *testing.T) {
	quiet := "sleep " + testTimeouts.Stable.String()
	deletion := "awaitClean " + (testTimeouts.Delete + deletionMargin).String()
	create := Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true}
	fault := Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}
	settle := Op{Type: OpSettle}
	for _, test := range []struct {
		name string
		ops  []Op
		// The target stops at the call stopsAfter names, and the context ends
		// with ending at the call endsAfter names.
		stopsAfter, endsAfter string
		ending                error
		wraps                 bool
	}{
		{"at an op", []Op{create, settle}, "createCR widget", "createCR widget", context.Canceled, true},
		{"at an op, past the deadline", []Op{create, settle}, "createCR widget", "createCR widget", context.DeadlineExceeded, true},
		{"at the end of a settle wait", []Op{settle}, "settle", "settle", context.Canceled, true},
		{"in the recovery", []Op{settle, fault, settle}, "clearFaults", "clearFaults", context.Canceled, true},
		{"in the teardown", []Op{settle}, deletion, deletion, context.Canceled, true},
		{"before the context ended", []Op{create, settle}, "createCR widget", quiet, context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.faulting, h.restartFails, h.stopsAfter = true, true, test.stopsAfter
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			end := cancel
			if test.ending == context.DeadlineExceeded {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, time.Millisecond)
				defer stop()
				end = func() { <-ctx.Done() }
			}
			h.cancel, h.cancelsAfter = end, test.endsAfter

			_, err := runSequence(ctx, toyTarget, sequenceOf(test.ops...), Options{Check: &fakeChecker{}, Dir: t.TempDir()}, h)

			if !errors.Is(err, ErrTargetStopped) {
				t.Fatalf("The run returned %v, want the target stopped.", err)
			}
			if wraps := errors.Is(err, test.ending); wraps != test.wraps {
				t.Errorf("The run returned %q, which wraps %v: %t, want %t.", err, test.ending, wraps, test.wraps)
			}
		})
	}
}

func TestRunResolvesDeleteManagedAtExecutionTime(t *testing.T) {
	h := newFakeHarness()
	h.managed[configMapKind] = []string{"widget-0", "widget-1", "widget-2"}
	sequence := sequenceOf(Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(2)})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); !slices.Contains(got, "deleteManaged v1/ConfigMap widget-2") {
		t.Errorf("The run did %v, want the third managed ConfigMap deleted.", got)
	}
	if got := result.Timeline.Ops[0].Deleted; got != "widget-2" {
		t.Errorf("The op recorded %q as the object it deleted, want widget-2.", got)
	}
}

// A generated deleteManaged can outrun what the target manages, and the run is
// worth finishing. The skip is a note, never silence (DESIGN.md §6).
func TestRunSkipsADeleteManagedThatResolvesToNothing(t *testing.T) {
	h := newFakeHarness()
	op := Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(7)}

	result, err := runFake(t, h, nil, sequenceOf(op))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := h.opCalls(); slices.ContainsFunc(got, func(call string) bool {
		return strings.HasPrefix(call, "deleteManaged")
	}) {
		t.Errorf("The run did %v, want nothing deleted: the index resolves to nothing.", got)
	}
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], "op 0 (deleteManaged)") {
		t.Errorf("The run reported the notes %v, want the skipped op named.", result.Notes)
	}
}

// The Observer lags the API server, so the object an index resolves to can be
// gone by the delete. The op then deleted nothing, so it names no object for
// G3 or G7 (DESIGN.md §7).
func TestRunNotesADeleteManagedWhoseObjectWasGone(t *testing.T) {
	h := newFakeHarness()
	h.gone = []string{"widget-1"}
	op := Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(1)}

	result, err := runFake(t, h, nil, sequenceOf(op))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got := result.Timeline.Ops[0].Deleted; got != "" {
		t.Errorf("The op recorded %q as the object it deleted, and it deleted nothing.", got)
	}
	want := "op 0 (deleteManaged) deleted nothing: index 1 resolved to the v1/ConfigMap widget-1, which was gone before reconciler-fuzzer could delete it"
	if !slices.Equal(result.Notes, []string{want}) {
		t.Errorf("The run reported the notes %q, want %q.", result.Notes, want)
	}
}

func TestRunRecordsNoObjectForADeleteManagedThatFailed(t *testing.T) {
	h := newFakeHarness()
	h.fail["deleteManaged v1/ConfigMap widget-0"] = apierrors.NewInternalError(errors.New("etcd is down"))

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}))

	if err == nil {
		t.Fatalf("The run succeeded, want the failed delete's error.")
	}
	if got := result.Timeline.Ops[0].Deleted; got != "" {
		t.Errorf("The op recorded %q as the object it deleted, and the delete failed.", got)
	}
}

func TestRunReportsADeleteManagedOfAKindTheTargetDoesNotManage(t *testing.T) {
	h := newFakeHarness()
	op := Op{Type: OpDeleteManaged, Kind: "v1/Secret", Nth: nth(0)}

	_, err := runFake(t, h, nil, sequenceOf(op))

	if err == nil {
		t.Fatalf("The run succeeded, want a harness error.")
	}
	if !strings.Contains(err.Error(), op.Kind) {
		t.Errorf("The run returned %q, want the kind named.", err)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestRunTearsDownInTheOrderTheDesignGives(t *testing.T) {
	h := newFakeHarness()
	h.forced = []string{"v1/ConfigMap widget-0", "toy.reconciler-fuzzer/v1/Widget widget"}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"clearFaults",
		"sleep " + testTimeouts.Stable.String(),
		"deleteCR widget",
		"awaitClean " + (testTimeouts.Delete + deletionMargin).String(),
		"empty",
		"forceFinalizers",
		"stop",
	}
	if got := h.teardownCalls(); !slices.Equal(got, want) {
		t.Errorf("The teardown did\n\t%v\nwant\n\t%v", got, want)
	}
	if !slices.Equal(result.Timeline.Forced, h.forced) {
		t.Errorf("The run recorded the forced finalizers %v, want %v.", result.Timeline.Forced, h.forced)
	}
	// A namespace reconciler-fuzzer emptied by hand is not one the target cleaned,
	// and no check judges what the teardown did (DESIGN.md §5.5, D37).
	note := strings.Join(result.Notes, "\n")
	for _, forced := range h.forced {
		if !strings.Contains(note, forced) {
			t.Errorf("The run notes are %q, want them to name the finalizer forced off %s.", note, forced)
		}
	}
	if !strings.Contains(note, "did not empty on its own") {
		t.Errorf("The run notes are %q, want them to say the namespace did not empty on its own.", note)
	}
}

// The teardown's checkpoint lands after the deletion window and before any
// finalizer is forced off, which is the window G3 judges.
func TestRunCheckpointsAfterTheDeletionWindow(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	window := result.Timeline.Deletion
	if window.Start.IsZero() || window.End.Before(window.Start) {
		t.Errorf("The deletion window is %+v, want one the teardown opened and closed.", window)
	}
	last := result.Timeline.Checkpoints[len(result.Timeline.Checkpoints)-1]
	if last.Op != Teardown || last.At.Before(window.End) {
		t.Errorf("The last checkpoint is %+v, want one after the deletion window closed at %v.", last, window.End)
	}
	if forced := slices.Index(h.calls, "forceFinalizers"); forced < slices.Index(h.calls, "awaitClean "+(testTimeouts.Delete+deletionMargin).String()) {
		t.Errorf("The teardown forced finalizers before the deletion window: %v", h.calls)
	}
	if got := len(check.inputs); got != 2 {
		t.Errorf("The checks ran %d times, want one per settle wait and one after the deletion window.", got)
	}
}

func TestRunEndsOverTheHarnessObjectLimit(t *testing.T) {
	h := newFakeHarness()
	h.count = defaultMaxManaged + 1
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	_, err := runFake(t, h, nil, sequence)

	if err == nil || !strings.Contains(err.Error(), fmt.Sprint(defaultMaxManaged)) {
		t.Fatalf("The run returned %v, want a harness limit naming %d.", err, defaultMaxManaged)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestRunGivesEachCheckTheRunSoFar(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpSettle})

	result, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if len(check.inputs) != 3 {
		t.Fatalf("The checks ran %d times, want 3.", len(check.inputs))
	}
	if check.inputs[0].Target != toyTarget {
		t.Errorf("The check was given the target %v, want the run's.", check.inputs[0].Target)
	}
	if got := len(check.inputs[0].Timeline.Ops); got != 1 {
		t.Errorf("The first check was given %d ops, want the one applied so far.", got)
	}
	if got := len(check.inputs[0].Timeline.Checkpoints); got != 1 {
		t.Errorf("The first check was given %d checkpoints, want the one it is running at.", got)
	}
	if got := len(check.inputs[2].Timeline.Ops); got != 2 {
		t.Errorf("The last check was given %d ops, want both.", got)
	}
	if got := len(result.Timeline.Checkpoints); got != 3 {
		t.Errorf("The run recorded %d checkpoints, want 3.", got)
	}
}

func TestRunNeedsACRBeforeAnOpThatActsOnOne(t *testing.T) {
	for _, opType := range []OpType{OpUpdate, OpDelete, OpRecreate} {
		t.Run(string(opType), func(t *testing.T) {
			op := Op{Type: opType, Index: 0}
			switch opType {
			case OpUpdate:
				op.Patch = map[string]any{"spec": map[string]any{}}
			case OpRecreate:
				op.Obj = widget("widget")
			}

			err := validateRun(toyTarget, Sequence{Seed: 1, Target: toyTarget.Name, Ops: []Op{op}}, Options{Dir: "out", Check: &fakeChecker{}})

			if err == nil || !strings.Contains(err.Error(), "which no op before it creates") {
				t.Errorf("validateRun returned %v, want an error: the sequence creates no CR.", err)
			}
		})
	}
}

func TestRunReportsAFailedOp(t *testing.T) {
	h := newFakeHarness()
	h.fail["restart"] = errors.New("the target will not die")

	_, err := runFake(t, h, nil, sequenceOf(Op{Type: OpRestart}))

	if err == nil || !strings.Contains(err.Error(), "the target will not die") {
		t.Errorf("The run returned %v, want the failure of op 0.", err)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

func TestAWriteTheAPIServerRefusesIsARefusalOfItsOp(t *testing.T) {
	widgets := schema.GroupResource{Group: "toy.reconciler-fuzzer", Resource: "widgets"}
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "toy.reconciler-fuzzer", Kind: "Widget"}, "widget",
		field.ErrorList{field.Invalid(field.NewPath("spec"), "object", "maxUnavailable must not exceed count")})
	for _, test := range []struct {
		name, call string
		err        error
		refusedOp  int
	}{
		{"a create its CRD refuses", "createCR widget", invalid, 0},
		{"an update its CRD refuses", "patchCR widget map[spec:map[count:5]]", invalid, 2},
		{"a create a webhook denies with its default code", "createCR widget",
			apierrors.NewBadRequest(`admission webhook "validate.toy.reconciler-fuzzer" denied the request: no`), 0},
		{"a create a webhook forbids", "createCR widget",
			apierrors.NewForbidden(widgets, "widget", errors.New(`admission webhook "validate.toy.reconciler-fuzzer" denied the request`)), 0},
		{"a server that fails", "createCR widget", apierrors.NewInternalError(errors.New("etcd is down")), -1},
		{"a managed object reconciler-fuzzer may not delete", "deleteManaged v1/ConfigMap widget-0",
			apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "widget-0", errors.New("no")), -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newFakeHarness()
			h.fail[test.call] = test.err

			_, err := runFake(t, h, nil, sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)},
				Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": int64(5)}}},
			))

			var refused *Refused
			switch isRefused := errors.As(err, &refused); {
			case test.refusedOp < 0 && isRefused:
				t.Errorf("The run returned %v, a refusal of what op %d wrote.", err, refused.Op.Index)
			case test.refusedOp >= 0 && !isRefused:
				t.Errorf("The run returned %v, want a refusal of op %d.", err, test.refusedOp)
			case isRefused && refused.Op.Index != test.refusedOp:
				t.Errorf("The run blamed op %d, want op %d.", refused.Op.Index, test.refusedOp)
			case isRefused && !strings.HasPrefix(err.Error(), refused.Error()):
				t.Errorf("The run returned %q, which does not open with the refusal.", err)
			}
			if err == nil || !strings.Contains(err.Error(), test.err.Error()) {
				t.Errorf("The run returned %v, want the API server's %v.", err, test.err)
			}
		})
	}
}

func TestADeleteAWebhookForbidsIsNoRefusedWrite(t *testing.T) {
	protected := apierrors.NewForbidden(schema.GroupResource{Group: "toy.reconciler-fuzzer", Resource: "widgets"}, "widget",
		errors.New(`admission webhook "protect.toy.reconciler-fuzzer" denied the request: deletion is protected`))
	for _, deleting := range []Op{{Type: OpDelete}, {Type: OpRecreate, Obj: widget("widget")}} {
		t.Run(string(deleting.Type), func(t *testing.T) {
			h := newFakeHarness()
			h.fail["deleteCR widget"] = protected

			_, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, deleting))

			var refused *Refused
			if errors.As(err, &refused) {
				t.Errorf("The run returned %v, a refusal of what op %d wrote.", err, refused.Op.Index)
			}
			if err == nil || !strings.Contains(err.Error(), "deletion is protected") {
				t.Errorf("The run returned %v, want the API server's %v.", err, protected)
			}
		})
	}
}

func TestRunValidatesTheSequenceAgainstTheTarget(t *testing.T) {
	for _, test := range []struct {
		name     string
		sequence Sequence
		check    Checker
		want     string
	}{
		{
			name:     "a sequence for another target",
			sequence: Sequence{Target: "cert-manager"},
			check:    &fakeChecker{},
			want:     "cert-manager",
		},
		{
			name:     "a malformed op",
			sequence: Sequence{Target: toyTarget.Name, Ops: []Op{{Index: 3, Type: OpSettle}}},
			check:    &fakeChecker{},
			want:     "position",
		},
		{
			name:     "no checker",
			sequence: Sequence{Target: toyTarget.Name},
			want:     "check",
		},
		{
			name: "an update of a CR no op creates",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpUpdate, CR: "widget-2", Patch: map[string]any{"spec": map[string]any{"count": float64(1)}}},
			),
			check: &fakeChecker{},
			want:  "op 1 (update) acts on the CR widget-2, which no op before it creates",
		},
		{
			name: "a delete of the sample's CR where only another was created",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget-2")},
				Op{Type: OpDelete},
			),
			check: &fakeChecker{},
			want:  "op 1 (delete) names no cr, so it acts on the sample's widget, which no op before it creates",
		},
		{
			name: "a recreate that creates another CR",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRecreate, Obj: widget("widget-2")},
			),
			check: &fakeChecker{},
			want:  "op 1 (recreate) acts on the CR widget and creates widget-2",
		},
		{
			name: "a create of a CR an earlier op created",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(1)}}},
				Op{Type: OpCreate, Obj: widget("widget")},
			),
			check: &fakeChecker{},
			want:  "op 2 (create) creates the CR widget, which op 0 created",
		},
		{
			name: "a recreate of a CR only a later op creates",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRecreate, CR: "widget-2", Obj: widget("widget-2")},
				Op{Type: OpCreate, Obj: widget("widget-2")},
			),
			check: &fakeChecker{},
			want:  "op 1 (recreate) acts on the CR widget-2, which no op before it creates",
		},
		{
			name: "a create of a CR a recreate made again",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpDelete},
				Op{Type: OpRecreate, Obj: widget("widget")},
				Op{Type: OpCreate, Obj: widget("widget")},
			),
			check: &fakeChecker{},
			want:  "op 3 (create) creates the CR widget, which op 2 created and no op since deleted",
		},
		{
			name: "a create of a CR a noSettle delete may still hold",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpCreate, Obj: widget("widget-2")},
				Op{Type: OpDelete, NoSettle: true},
				Op{Type: OpUpdate, CR: "widget-2", Patch: map[string]any{"spec": map[string]any{"count": float64(1)}}, NoSettle: true},
				Op{Type: OpRestart},
				Op{Type: OpCreate, Obj: widget("widget")},
			),
			check: &fakeChecker{},
			want:  "op 5 (create) creates the CR widget, which may still be there: op 2 deleted it with noSettle and no op since settles",
		},
		{
			name: "an update of a deleted CR",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpCreate, Obj: widget("widget-2")},
				Op{Type: OpDelete},
				Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(1)}}},
			),
			check: &fakeChecker{},
			want:  "op 3 (update) acts on the CR widget, which op 2 deleted",
		},
		{
			name: "a delete of a deleted CR",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpDelete},
				Op{Type: OpDelete},
			),
			check: &fakeChecker{},
			want:  "op 2 (delete) acts on the CR widget, which op 1 deleted",
		},
		{
			name:     "a create of a CR with no name",
			sequence: sequenceOf(Op{Type: OpCreate, Obj: generated("w-")}),
			check:    &fakeChecker{},
			want:     "op 0 (create) writes a CR with no metadata.name",
		},
		{
			name: "a recreate of a CR with no name",
			sequence: sequenceOf(
				Op{Type: OpCreate, Obj: widget("widget")},
				Op{Type: OpRecreate, Obj: generated("widget-")},
			),
			check: &fakeChecker{},
			want:  "op 1 (recreate) writes a CR with no metadata.name",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateRun(toyTarget, test.sequence, Options{Dir: "out", Check: test.check})

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("validateRun returned %v, want an error naming %q.", err, test.want)
			}
		})
	}
}

func TestRunAcceptsOpsOnTheCRsEarlierOpsCreate(t *testing.T) {
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpUpdate, CR: "widget-2", Patch: map[string]any{"spec": map[string]any{"count": float64(1)}}},
		Op{Type: OpRecreate, CR: "widget-2", Obj: widget("widget-2")},
		Op{Type: OpDelete, CR: "widget-2"},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpDelete, CR: "widget-2", NoSettle: true},
		Op{Type: OpSettle},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpDelete, CR: "widget-2", NoSettle: true},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(2)}}},
		Op{Type: OpCreate, Obj: widget("widget-2")},
		Op{Type: OpDelete},
		Op{Type: OpSettle},
	)

	if err := validateRun(toyTarget, sequence, Options{Dir: "out", Check: &fakeChecker{}}); err != nil {
		t.Errorf("validateRun returned %v, want every op to act on a CR an earlier op creates.", err)
	}
}

func TestRunWritesTheSequenceToTheRunDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run-1")
	sequence := readGolden(t)

	if err := WriteRunSequence(dir, sequence); err != nil {
		t.Fatalf("Writing the run's sequence failed: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, sequenceFile))
	if err != nil {
		t.Fatalf("Reading back the sequence failed: %v", err)
	}
	want, err := sequence.Marshal()
	if err != nil {
		t.Fatalf("Encoding the sequence failed: %v", err)
	}
	if string(written) != string(want) {
		t.Errorf("The run directory holds\n%s\nwant\n%s", written, want)
	}
}

func checkpointsAt(timeline Timeline) []int {
	var ops []int
	for _, checkpoint := range timeline.Checkpoints {
		ops = append(ops, checkpoint.Op)
	}
	return ops
}

// The Runner knows when each fault op injected its fault and when the fault
// was removed, which is the window the checks ignore what happened in.
func TestRunRecordsTheWindowEachFaultWasActiveIn(t *testing.T) {
	h := newFakeHarness()
	h.faulting = true
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: "configmaps"}, Action: Action{Error: 500}, Until: Trigger{Op: nth(2)}}},
		Op{Type: OpFault, Fault: &Fault{Match: Match{Resource: "secrets"}, Action: Action{Drop: true}}},
		Op{Type: OpSettle},
		Op{Type: OpSettle},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	faults := result.Timeline.Faults
	if len(faults) != 2 {
		t.Fatalf("The run recorded %d fault windows, want one per fault op.", len(faults))
	}
	ops := result.Timeline.Ops
	if !between(faults[0].Start, ops[0].At, ops[1].At) || !between(faults[0].End, ops[1].At, ops[2].At) {
		t.Errorf("The first fault was active %+v, want from op 0 until op 2 removed it.", faults[0])
	}
	if !between(faults[1].Start, ops[1].At, ops[2].At) || !faults[1].End.After(ops[3].At) {
		t.Errorf("The second fault was active %+v, want from op 1 until the teardown cleared it.", faults[1])
	}
	want := []string{"addFault 0", "addFault 1", "removeFault 0", "settle", "supervise", "settle"}
	if got := h.opCalls(); !slices.Equal(got, want) {
		t.Errorf("The run did %v, want %v.", got, want)
	}
}

// between reports whether the run reached when in [from, to].
func between(when, from, to time.Time) bool { return !when.Before(from) && !when.After(to) }

// The recorded run is what the teardown left: the bug matrix and a report read
// the whole of it, not the part the last checkpoint saw.
func TestRunRecordsTheWholeRunTheTeardownLeftBehind(t *testing.T) {
	h := newFakeHarness()

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if got, want := len(result.Recorded.Requests), len(h.calls); got != want {
		t.Errorf("The run recorded %d requests, want the %d of the whole run: it was sampled after the teardown.",
			got, want)
	}
	if last := result.Recorded.Timeline.Checkpoints; len(last) == 0 || last[len(last)-1].Op != Teardown {
		t.Errorf("The recorded run holds the checkpoints %v, want the teardown's among them.", checkpointsAt(result.Recorded.Timeline))
	}
	if result.Recorded.Target != toyTarget {
		t.Errorf("The recorded run names the target %v, want the run's.", result.Recorded.Target)
	}
}

// §5.5 step 4 waits T_stable before it deletes anything, which §6 judges as
// the run's last quiet window.
func TestRunStampsTheQuietWindowTheTeardownWaited(t *testing.T) {
	h := newFakeHarness()

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	quiet, settled := result.Timeline.Quiet, result.Timeline.Ops[0].Settled
	if !quiet.Start.After(settled.Window.End) {
		t.Errorf("The teardown's window opens at %v, want it after the settle wait ended at %v.",
			quiet.Start, settled.Window.End)
	}
	if quiet.End != result.Timeline.Deletion.Start {
		t.Errorf("The teardown's window closes at %v, want it where the deletion opens, at %v.",
			quiet.End, result.Timeline.Deletion.Start)
	}
}

// G3 reads whether the teardown saw the namespace empty (DESIGN.md §6, D34). A
// run that already found a violation records no teardown checkpoint, so the
// clean cannot ride on one.
func TestTheTeardownRecordsACleanNamespaceEvenAfterAViolation(t *testing.T) {
	// The delay separates the window's ends, so that a stamp taken from the
	// wrong one fails by a visible margin rather than by nanoseconds.
	h := newFakeHarness()
	h.deleteCRDelay = 20 * time.Millisecond
	check := &fakeChecker{violations: [][]Violation{{{ID: "G2"}}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil {
		t.Fatal("The run reported no violation, so it does not exercise the case.")
	}
	if result.Timeline.Cleaned.IsZero() {
		t.Errorf("The teardown saw the namespace clean and the timeline does not say so, so G3 cannot judge it.")
	}
	if got, want := result.Timeline.Cleaned, result.Timeline.Deletion.End; !got.Equal(want) {
		t.Errorf("The timeline reports it clean at %v, want the %v the deletion window closed at.", got, want)
	}
}

// A namespace the teardown never saw empty leaves no instant to report.
func TestTheTeardownReportsNoCleanWhenTheNamespaceStaysDirty(t *testing.T) {
	h := newFakeHarness()
	h.clean = false

	result, err := runFake(t, h, &fakeChecker{}, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if !result.Timeline.Cleaned.IsZero() {
		t.Errorf("The timeline reports the namespace clean at %v, and the teardown never saw it empty.",
			result.Timeline.Cleaned)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v, and a namespace that stays dirty is for G3 to judge.", result.Violation)
	}
}

// The checks name what they could not judge, and the run carries it out, so
// that a reader can tell a skipped check from a passing one (DESIGN.md §6).
func TestRunCarriesTheNotesTheLastCheckpointLeft(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{notes: [][]string{{"G5 is not evaluated for op 0"}, {"G3 is not evaluated for the deletion of widget"}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if want := check.notes[1]; !slices.Equal(result.Notes, want) {
		t.Errorf("The run carried the notes %v, want %v: each checkpoint reads the whole run so far.",
			result.Notes, want)
	}
}

// A child the collector keeps would fail G3 with no reason given.
func TestRunNotesEachOwnerTheCollectorCouldNotResolve(t *testing.T) {
	h := newFakeHarness()
	h.unresolved = []cluster.Unresolved{
		{
			DependentKind: configMapKind, DependentName: "widget-cfg",
			OwnerKind: schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1alpha9", Kind: "Widget"}, OwnerName: "widget",
			Unserved: true,
		},
		{
			DependentKind: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, DependentName: "widget-tls",
			OwnerKind: schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, OwnerName: "issuer",
		},
	}
	check := &fakeChecker{notes: [][]string{nil, {"G3 is not evaluated for the deletion of widget"}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := []string{
		"reconciler-fuzzer's garbage collector never deletes v1/ConfigMap widget-cfg, because the API server does not serve toy.reconciler-fuzzer/v1alpha9/Widget, the kind of its owner widget",
		"reconciler-fuzzer's garbage collector never deletes v1/Secret widget-tls, because it does not watch apps/v1/Deployment, the kind of its owner issuer",
		"G3 is not evaluated for the deletion of widget",
	}
	if !slices.Equal(result.Notes, want) {
		t.Errorf("The run carried the notes\n\t%q\nwant\n\t%q", result.Notes, want)
	}
}

func TestRunNotesWhatTheAPIServerForbadeTheTarget(t *testing.T) {
	h := newFakeHarness()
	h.logged = []proxy.Request{
		{Verb: "patch", Resource: "widgets", Subresource: "status", Status: 403},
		{Verb: "update", Resource: "widgets", Subresource: "status", Status: 403},
		{Verb: "patch", Resource: "widgets", Subresource: "status", Status: 403}, // duplicate
		{Verb: "get", Resource: "configmaps", Status: 200},                       // not forbidden
		{Verb: "list", Resource: "widgets", Status: 403, Fault: "error(403)"},    // injected fault
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := "the API server forbade the target: patch widgets/status, update widgets/status"
	if !slices.Contains(result.Notes, want) {
		t.Errorf("The run noted %q, want %q.", result.Notes, want)
	}
}

func TestRunDoesNotNoteForbiddenWhenNoneOccurred(t *testing.T) {
	h := newFakeHarness()
	h.logged = []proxy.Request{
		{Verb: "get", Resource: "configmaps", Status: 200},
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	for _, note := range result.Notes {
		if strings.Contains(note, "forbade") {
			t.Errorf("The run noted %q, want no forbidden note.", note)
		}
	}
}

// A check that cannot be evaluated is a configuration error, so the run ends
// as a harness error rather than as a finding (DESIGN.md §11).
func TestRunEndsWhenACheckCannotBeEvaluated(t *testing.T) {
	h := newFakeHarness()
	check := &fakeChecker{err: errors.New("no such field: spec.nonesuch")}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err == nil || !strings.Contains(err.Error(), "spec.nonesuch") {
		t.Fatalf("The run returned %v, want the evaluation error.", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v, want no finding: the check did not evaluate.", result.Violation)
	}
	if !slices.Contains(h.calls, "stop") {
		t.Errorf("The run did %v, want it torn down anyway.", h.calls)
	}
}

// G3's window opens when the Observer records the deletion, a moment after the
// teardown asked for it, so the teardown holds the namespace open past
// T_delete.
func TestRunHoldsTheDeletionWindowOpenPastTDelete(t *testing.T) {
	h := newFakeHarness()
	h.clean = false

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := "awaitClean " + (testTimeouts.Delete + deletionMargin).String()
	if got := h.teardownCalls(); !slices.Contains(got, want) {
		t.Errorf("The teardown did %v, want %q.", got, want)
	}
	if window := result.Timeline.Deletion; !window.End.After(window.Start) {
		t.Errorf("The deletion window is %+v, want one the teardown held open.", window)
	}
}

// TestTheTeardownIsStampedBeforeItChangesAnything pins Timeline.Deletion.Start
// to the instant before the CR delete. The invariants take it as the moment
// reconciler-fuzzer became the one changing the namespace (DESIGN.md §6), so a
// stamp taken after the delete returns leaves a window in which the CR
// reconciler-fuzzer is deleting is judged against the target.
func TestTheTeardownIsStampedBeforeItChangesAnything(t *testing.T) {
	h := &fakeHarness{converged: true, clean: true, deleteCRDelay: 50 * time.Millisecond}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}}, h)
	if err != nil {
		t.Fatalf("the run returned an error: %v", err)
	}
	if h.deletedCRAt.IsZero() {
		t.Fatal("the teardown never deleted the CR.")
	}
	if start := result.Timeline.Deletion.Start; start.After(h.deletedCRAt) {
		t.Errorf("The teardown is stamped %v after the CR delete began; it must be stamped before it.",
			start.Sub(h.deletedCRAt))
	}
}

// A target that stopped takes the run with it, so the ops behind it never
// reach the cluster: they would run against nothing.
func TestTheRunnerAppliesNoOpToATargetThatStopped(t *testing.T) {
	h := &fakeHarness{converged: true, clean: true, stopsAfter: "createCR widget", targetExit: errors.New("exit status 1")}
	dir := t.TempDir()
	written := "toy-widget: listen tcp 127.0.0.1:9440: bind: address already in use"
	if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte(written+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget"), NoSettle: true},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}, Dir: dir}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"op 1 (update)", "no longer running", "exit status 1", written} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if !errors.Is(err, ErrTargetStopped) {
		t.Errorf("The error is %q, which does not wrap ErrTargetStopped.", err)
	}
	if got, want := h.opCalls(), []string{"createCR widget"}; !slices.Equal(got, want) {
		t.Errorf("The run did %v, want %v: an op is never applied to a target that stopped.", got, want)
	}
	if len(result.Timeline.Ops) != 1 {
		t.Errorf("The run recorded %d ops, want the one it applied while the target ran.", len(result.Timeline.Ops))
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v against the target; a target that stopped is the harness's failure.", result.Violation)
	}
}

// The teardown judges the deletion it asked for. A target that could not start
// again cleaned nothing up, so the window is not its to answer for.
func TestTheTeardownJudgesNoDeletionWithATargetThatStopped(t *testing.T) {
	h := &fakeHarness{converged: true, stopsAfter: "deleteCR widget", targetExit: errors.New("exit status 1"), restartFails: true}
	g3 := Violation{ID: "G3", Statement: "the CR widget still carried its finalizers"}
	check := &fakeChecker{violations: [][]Violation{nil, {g3}}}

	result, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
		Options{Check: check, Dir: t.TempDir()}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"teardown", "no longer running", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v; a target that stopped answers for no deletion.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at %v, want the ops it judged while the target ran.", got)
	}
}

// A run a harness error ended did not run all its ops, so the deletion is not
// the target's to answer for.
func TestTheTeardownJudgesNoDeletionAfterAHarnessError(t *testing.T) {
	h := newFakeHarness()
	h.fail["restart"] = errors.New("the target will not die")
	g3 := Violation{ID: "G3", Statement: "the CR widget still carried its finalizers"}
	check := &fakeChecker{violations: [][]Violation{nil, {g3}}}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}, Op{Type: OpRestart}))

	if err == nil || !strings.Contains(err.Error(), "op 1 (restart)") {
		t.Errorf("The run returned %v, want the failure of op 1.", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v; a run that failed answers for no deletion.", result.Violation)
	}
	if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0}) {
		t.Errorf("The run checkpointed at %v, want the ops it judged before the failure.", got)
	}
	if len(check.inputs) != 1 {
		t.Errorf("The checks ran %d times, want once: the teardown judges nothing after a failure.", len(check.inputs))
	}
}

// TestASettleExpiryWithADeadTargetIsAHarnessError pins the difference between
// the target failing and the harness failing. A target that is gone cannot
// converge, so reporting G4 would accuse a controller of a fault that is ours:
// a port collision reads exactly this way (DESIGN.md §5.1).
func TestASettleExpiryWithADeadTargetIsAHarnessError(t *testing.T) {
	h := &fakeHarness{clean: true, stopsAfter: "settle", targetExit: errors.New("exit status 1")}
	sequence := sequenceOf(Op{Type: OpCreate, Obj: widget("widget")})
	// A target that refuses its own flags says so on the way out, and that
	// line is what the reader acts on.
	dir := t.TempDir()
	written := "toy-widget: --bug=12: want a bug ID from 0 to 11"
	if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte("starting\n"+written+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := runSequence(t.Context(), toyTarget, sequence, Options{Check: &fakeChecker{}, Dir: dir}, h)
	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	for _, want := range []string{"no longer running", "exit status 1", "target.log", written} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("The error is %q, which does not mention %q.", err, want)
		}
	}
	if result.Violation != nil {
		t.Errorf("The run reported %+v against the target; a dead target is the harness's failure.", result.Violation)
	}
}

// A target that stopped once reconciler-fuzzer had created the CR, and before
// it ran supervised, may have crashed on it if it had read a resource. The run
// directory holds the sequence that replays it. A supervised target stops only
// where a restart failed.
func TestAStoppedTargetsErrorSaysWhetherTheCRMayHaveCrashedIt(t *testing.T) {
	watched := []proxy.Request{{Verb: "watch", Resource: "widgets", Watch: true}}
	const crashed = "panic: runtime error\n"
	for _, test := range []struct {
		name    string
		h       *fakeHarness
		log     string
		created bool
	}{
		{"before the CR", &fakeHarness{clean: true, targetGone: true, logged: watched}, crashed, false},
		{"after the CR", &fakeHarness{clean: true, stopsAfter: "createCR widget", logged: watched}, crashed, true},
		{"after the CR, having read nothing", &fakeHarness{clean: true, stopsAfter: "createCR widget"}, crashed, false},
		{"after the CR, over a taken port", &fakeHarness{clean: true, stopsAfter: "createCR widget", logged: watched},
			"listen tcp :8081: bind: address already in use\n", false},
		{"once a restart failed", &fakeHarness{clean: true, converged: true, restartFails: true, stopsAfter: "deleteCR widget", logged: watched},
			crashed, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.h.targetExit = errors.New("exit status 2")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte(test.log), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
				Options{Check: &fakeChecker{}, Dir: dir}, test.h)

			if err == nil {
				t.Fatal("The run reported no error although the target had stopped.")
			}
			pointer := "reconciler-fuzzer had created the CR, so the CR may have crashed the target, and " +
				filepath.Join(dir, "sequence.json") + " replays the run"
			if says := strings.Contains(err.Error(), pointer); says != test.created {
				t.Errorf("The error is %q; want it to say %q: %t.", err, pointer, test.created)
			}
			if !errors.Is(err, ErrTargetStopped) {
				t.Errorf("The error is %q, which does not wrap ErrTargetStopped.", err)
			}
		})
	}
}

// A launcher holding no process knows no exit status, and the error says what
// it knows.
func TestTheStoppedTargetsErrorWithoutAnExitStatus(t *testing.T) {
	h := &fakeHarness{clean: true, targetGone: true}

	_, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
		Options{Check: &fakeChecker{}, Dir: t.TempDir()}, h)

	if err == nil {
		t.Fatal("The run reported no error although the target had stopped.")
	}
	if !errors.Is(err, ErrTargetStopped) || strings.Contains(err.Error(), "<nil>") {
		t.Errorf("The error is %q, want it to say what the launcher knows.", err)
	}
}

// The harness quotes what the target said on the way out, so what it quotes
// has to be a whole line: klog puts the level and the message at the front,
// and a Go panic puts the stack after it.
func TestWhatTheTargetSaidOnTheWayOut(t *testing.T) {
	// The sizes are literal, so that a wider maxTail fails this rather than
	// scaling the log with it.
	const pastTheTail = 1_100_000
	for _, log := range []struct {
		name  string
		wrote string
		want  string
	}{
		{"one line and no newline", "toy-widget: --bug=12: want a bug ID from 0 to 11",
			"toy-widget: --bug=12: want a bug ID from 0 to 11"},
		{"a trailing blank line", "the message\n   \n", "the message"},
		{"a line in spaces", "  fatal: bad flag \r\n", "fatal: bad flag"},
		{"a panic before its stack",
			"starting\npanic: runtime error: index out of range\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:57 +0x1d5\n",
			"panic: runtime error: index out of range"},
		// The log holds every process a restart started.
		{"a panic after an earlier one",
			"panic: the first\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:10 +0x1d\nstarting\npanic: the second\n\ngoroutine 1 [running]:\nmain.main()\n\t/src/main.go:57 +0x1d5\n",
			"panic: the second"},
		{"a last line that ends in a port", "starting\nE0923 16:05:39.116650    7054 main.go:30] cannot reach 10.96.0.1:443\n",
			"E0923 16:05:39.116650    7054 main.go:30] cannot reach 10.96.0.1:443"},
		{"several lines and no panic", "starting\nlistening on :8080\nE0921 fatal: reconcile failed\n",
			"E0921 fatal: reconcile failed"},
		{"an error above cobra's usage text", "Error: unknown flag: --x\nUsage:\n  app [flags]\n\nFlags:\n  -h, --help   help for app\n",
			"Error: unknown flag: --x"},
		{"a usage text with nothing above it", "Usage of app:\n  -x\tdoes x\n", "-x\tdoes x"},
		{"a blank line above a usage text", "E0921 bad flag\n\nUsage of app:\n  -x\tdoes x\n", "E0921 bad flag"},
		{"two usage texts", "first\nUsage of app:\n  -x\tdoes x\nsecond\nUsage of app:\n  -x\tdoes x\n", "second"},
		{"a tail that begins mid-line", strings.Repeat("y", pastTheTail) + "\nE0921 fatal: reconcile failed\n",
			"E0921 fatal: reconcile failed"},
		{"a last line longer than the tail", "E0921 fatal: " + strings.Repeat("x", pastTheTail) + "\n", ""},
		{"nothing at all", "", ""},
		{"only newlines", "\n\n\n", ""},
	} {
		t.Run(log.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), targetLogFile)
			if err := os.WriteFile(path, []byte(log.wrote), 0o644); err != nil {
				t.Fatal(err)
			}

			if got, _ := whyItStopped(path, 0); got != log.want {
				t.Errorf("A log of %d bytes reads as %q, want %q.", len(log.wrote), got, log.want)
			}
		})
	}
	if got, _ := whyItStopped(filepath.Join(t.TempDir(), "no-such-log"), 0); got != "" {
		t.Errorf("A log reconciler-fuzzer never wrote reads as %q.", got)
	}
}

// Real programs wrote these logs as they stopped. A stack trace follows the
// line that says why, so the last line is a frame.
func TestWhatTheTargetSaidAboveItsStackTrace(t *testing.T) {
	for _, log := range []struct{ file, want string }{
		{"zap-bind.log", "2026-09-23T16:06:01Z\tERROR\tsetup\tunable to start manager\t" +
			`{"error": "error listening on :45147: listen tcp :45147: bind: address already in use"}`},
		{"zap-reconcile.log", "2026-09-23T16:03:53Z\tERROR\tReconciler error\t" +
			`{"controller": "configmap", "controllerGroup": "", "controllerKind": "ConfigMap", "ConfigMap": {"name":"widget-0","namespace":"default"}, "namespace": "default", "name": "widget-0", "reconcileID": "8a0fad41-f98f-47ea-87d1-990173d1d560", "error": "spec.count 11 is out of range"}`},
		{"panic.log", "panic: runtime error: index out of range [150] with length 0"},
		{"klog-fatal.log", "F0923 16:05:39.116650    7054 main.go:30] reconciling widget: the cache never synced"},
		{"zap-json.log", `{"level":"error","ts":"2026-09-23T16:04:03Z","logger":"setup","msg":"unable to create controller","controller":"Widget","error":"no matches for kind \"Widget\" in version \"toy.reconciler-fuzzer/v1\"","stacktrace":"main.main\n\tgithub.com/rosenhouse-bot-sandbox/reconciler-fuzzer/zzprobe/main.go:39\nruntime.main\n\truntime/proc.go:290"}`},
		// Go's flag package prints the error above its usage text.
		{"flag-usage.log", "flag provided but not defined: -no-such-flag"},
		{"cobra-usage.log", "Error: unknown flag: --no-such-flag"},
	} {
		t.Run(log.file, func(t *testing.T) {
			if got, _ := whyItStopped(filepath.Join("testdata", "stopped", log.file), 0); got != log.want {
				t.Errorf("%s reads as %q, want %q.", log.file, got, log.want)
			}
		})
	}
}

func TestATargetThatCouldNotBindItsPortIsToldWhere(t *testing.T) {
	for _, log := range []struct {
		file string
		hint bool
	}{
		{"zap-bind.log", true},
		{"klog-fatal.log", false},
	} {
		t.Run(log.file, func(t *testing.T) {
			wrote, err := os.ReadFile(filepath.Join("testdata", "stopped", log.file))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, targetLogFile), wrote, 0o644); err != nil {
				t.Fatal(err)
			}
			h := &fakeHarness{clean: true, targetGone: true, targetExit: errors.New("exit status 1")}

			_, err = runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
				Options{Check: &fakeChecker{}, Dir: dir}, h)

			if err == nil {
				t.Fatal("The run reported no error although the target had stopped.")
			}
			if hinted := strings.Contains(err.Error(), "launch.args"); hinted != log.hint {
				t.Errorf("The error is %q; want it to name launch.args only where a port was taken.", err)
			}
		})
	}
}

// A target that wrote nothing leaves the reader its log, and one that wrote
// control bytes leaves them quoted: the line goes into an error CI prints.
func TestTheStoppedTargetsErrorWithoutALineAndWithControlBytes(t *testing.T) {
	for _, log := range []struct {
		name  string
		wrote string
		want  string
		says  bool
	}{
		{name: "an empty log", wrote: "", want: "its output is in"},
		{name: "an escape sequence", wrote: "fatal: \x1b[31mbad flag\x1b[0m\n", want: `\x1b[31mbad flag`, says: true},
	} {
		t.Run(log.name, func(t *testing.T) {
			h := &fakeHarness{clean: true, targetGone: true, targetExit: errors.New("exit status 1")}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, targetLogFile), []byte(log.wrote), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := runSequence(t.Context(), toyTarget, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
				Options{Check: &fakeChecker{}, Dir: dir}, h)

			if err == nil {
				t.Fatal("The run reported no error although the target had stopped.")
			}
			if !strings.Contains(err.Error(), log.want) {
				t.Errorf("The error is %q, which does not carry %q.", err, log.want)
			}
			if said := strings.Contains(err.Error(), "it wrote"); said != log.says {
				t.Errorf("The error is %q, and the log held %q.", err, log.wrote)
			}
		})
	}
}

// The G4 an expired wait raised counts what the target managed, as a check's
// G4 does (#13).
func TestTheG4OfAnExpiredWaitCountsTheManagedObjects(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	for i := range 3 {
		h.recordChild(fmt.Sprintf("widget-%d", i), "12")
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %v, want a G4 violation.", result.Violation)
	}
	if want := "the target managed 3 objects of the kinds it declares"; !strings.Contains(result.Violation.Evidence, want) {
		t.Errorf("The G4's evidence is %q, want it to say %q.", result.Violation.Evidence, want)
	}
	if result.Violation.ManagedTotal == nil || *result.Violation.ManagedTotal != 3 {
		t.Errorf("The G4 carried out no count of 3, and its evidence quotes one.")
	}
}

// The G4 an expired wait raised quotes the children as a table of their own,
// bounded and newest first, since the object a readiness finding is about is
// usually one of them (#24).
func TestTheG4OfAnExpiredWaitQuotesTheManagedObjects(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.recordCR("widget", "11")
	for i := range 25 {
		h.recordChild(fmt.Sprintf("widget-%d", i), strconv.Itoa(12+i))
	}

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" {
		t.Fatalf("The run reported %v, want a G4 violation.", result.Violation)
	}
	managed := result.Violation.Managed
	if len(managed) != invariant.MaxEvidence {
		t.Fatalf("The violation quotes %d managed objects, want the bound of %d.", len(managed), invariant.MaxEvidence)
	}
	if managed[0].Name != "widget-24" {
		t.Errorf("The state opens at %s, want widget-24, the child recorded last.", managed[0].Name)
	}
	if result.Violation.ManagedTotal == nil || *result.Violation.ManagedTotal != 25 {
		t.Errorf("The violation counts %v managed objects, want 25.", result.Violation.ManagedTotal)
	}
	if want := observe.KindName(widgetKind) + " widget"; result.Violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", result.Violation.VersionsOf, want)
	}
	versions := result.Violation.Versions
	if !slices.ContainsFunc(versions, func(v observe.Version) bool { return v.GVK == widgetKind }) {
		t.Errorf("The timeline holds %d versions and not the CR the statement names.", len(versions))
	}
	if slices.ContainsFunc(versions, func(v observe.Version) bool { return v.GVK == configMapKind }) {
		t.Errorf("The timeline holds %v, want the CR's history alone.", versions)
	}
}

// engineChecker is the engine, remembering what it found at each checkpoint.
type engineChecker struct{ found []Findings }

func (c *engineChecker) Check(in Input) (Findings, error) {
	found, err := Engine{}.Check(in)
	c.found = append(c.found, found)
	return found, err
}

// The Runner raises the G4 of an expired wait itself, and the engine raises it
// again at that checkpoint. They must say the same.
func TestTheRunnersG4OfAnExpiredWaitIsTheEngines(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.recordCR("widget", "11")
	h.recordChild("widget-0", "12")
	check := &engineChecker{}

	result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || len(check.found) == 0 {
		t.Fatalf("The run reported %v and the engine %v, want a G4 from each.", result.Violation, check.found)
	}
	var engine *Violation
	for _, v := range check.found[0].Violations {
		if strings.HasPrefix(v.Statement, "the settle wait") {
			engine = &v
		}
	}
	if engine == nil {
		t.Fatalf("The engine found %v, want the G4 of the expired wait.", check.found[0].Violations)
	}
	if !reflect.DeepEqual(*result.Violation, *engine) {
		t.Errorf("The Runner raised\n\t%+v\nand the engine\n\t%+v", *result.Violation, *engine)
	}
	if !strings.Contains(result.Violation.Statement, "ready never held: it evaluated to false") {
		t.Errorf("The Runner says %q, want it to say why the wait expired.", result.Violation.Statement)
	}
	if result.Violation.Ready == nil || result.Violation.Ready.Expr != toyTarget.ReadyExpr {
		t.Errorf("The Runner quotes the ready predicate %+v, want the target's.", result.Violation.Ready)
	}
}

func TestRunEndsAtAReadyThatYieldsNoBoolInTheHistory(t *testing.T) {
	h := newFakeHarness()
	h.converged = false
	h.recordCR("widget", "11")
	yieldsAnInt := *toyTarget
	yieldsAnInt.Ready = func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded float64", target.ErrNotBool)}
	}

	result, err := runSequence(t.Context(), &yieldsAnInt, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}),
		Options{Check: &fakeChecker{}}, h)

	if !errors.Is(err, target.ErrNotBool) {
		t.Errorf("The run returned the error %v, want the ready that yields no bool.", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v against the target, and the declaration is at fault.", result.Violation)
	}
}

// A checkpoint is where its settle wait ended, which is where a quiet window
// opens, and it says where the wait began.
func TestACheckpointIsWhereItsWaitEnded(t *testing.T) {
	h := newFakeHarness()

	result, err := runFake(t, h, nil, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	wait := result.Timeline.Ops[0].Settled
	checkpoint := result.Timeline.Checkpoints[0]
	if wait == nil || !checkpoint.At.Equal(wait.Window.End) || !checkpoint.Began.Equal(wait.Window.Start) {
		t.Errorf("The checkpoint is %+v, want it over the wait %+v.", checkpoint, wait)
	}
}

// Until a settle wait converges, reconciler-fuzzer has not seen the target
// work, and an exit reads like a bad flag or a taken port: a harness error.
func TestAnExitBeforeAnyWaitConvergedEndsTheRun(t *testing.T) {
	h := newFakeHarness()
	h.converged, h.faulting = false, true
	h.exitsInWait, h.targetExit = 2, errors.New("exit status 1")
	sequence := sequenceOf(
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(2)}}},
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpSettle},
	)

	_, err := runFake(t, h, nil, sequence)

	if err == nil || !strings.Contains(err.Error(), "op 2 (settle): the target is no longer running") {
		t.Errorf("The run returned %v, want the harness error of the exit in op 2.", err)
	}
	if slices.Contains(h.calls, "supervise") {
		t.Errorf("The run did %v, and no wait converged to show the target worked.", h.calls)
	}
}

// crashLoop is a target that converges on op 0 and exits in the wait of the
// op after.
func crashLoop() *fakeHarness {
	h := newFakeHarness()
	h.exitsInWait, h.targetExit = 2, errors.New("exit status 2")
	h.says = []string{"panic: runtime error: integer divide by zero"}
	return h
}

const crashNote = `the target exited during op 1 (update) with exit status 2 after writing ` +
	`"panic: runtime error: integer divide by zero"`

// A target that exits once it has worked is restarted, as a kubelet would.
// One that keeps exiting never converges, and G4 quotes the last exit since it
// last converged.
func TestACrashLoopIsAG4ThatQuotesTheLastExit(t *testing.T) {
	h := crashLoop()
	h.settles = []bool{true, false}
	h.stopsAfter = "patchCR widget map[spec:map[count:0]]"
	h.says = []string{"E0923 lost the lease", "panic: runtime error: integer divide by zero"}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(0)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" || !strings.Contains(result.Violation.Statement, "op 1 (update)") {
		t.Fatalf("The run reported %v, want the G4 of the wait after op 1.", result.Violation)
	}
	want := `the target exited 2 times since it last converged, last with exit status 2 after writing ` +
		`"panic: runtime error: integer divide by zero"`
	if !strings.Contains(result.Violation.Statement, want) {
		t.Errorf("The G4 says %q, want it to say %q.", result.Violation.Statement, want)
	}
	if !slices.Contains(result.Notes, crashNote) {
		t.Errorf("The run noted %q, want %q.", result.Notes, crashNote)
	}
	if len(result.Timeline.Exits) != 2 {
		t.Errorf("The run recorded the exits %+v, want the two in op 1.", result.Timeline.Exits)
	}
}

// A G4 quotes a single exit too.
func TestAG4QuotesTheOneExitSinceTheTargetConverged(t *testing.T) {
	h := crashLoop()
	h.settles = []bool{true, false}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(0)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	want := `the target exited 1 time since it last converged, last with exit status 2 after writing ` +
		`"panic: runtime error: integer divide by zero"`
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, want) {
		t.Errorf("The run reported %+v, want a G4 that says %q.", result.Violation, want)
	}
}

// An Exit reads as what follows "with" in a note or a G4, whatever it holds.
func TestAnExitSaysWhyTheTargetStopped(t *testing.T) {
	for _, exit := range []struct {
		err  error
		said string
		want string
	}{
		{errors.New("exit status 2"), "panic: boom", `exit status 2 after writing "panic: boom"`},
		{errors.New("exit status 2"), "", "exit status 2"},
		{nil, "panic: boom", `no error after writing "panic: boom"`},
		{nil, "", "no error"},
	} {
		if got := (Exit{Err: exit.err, Said: exit.said}).String(); got != exit.want {
			t.Errorf("Exit{Err: %v, Said: %q} reads %q, want %q.", exit.err, exit.said, got, exit.want)
		}
	}
}

// A target that converges after it exited passes, and the exit is noted.
func TestATargetThatConvergesAfterItExitedPasses(t *testing.T) {
	h := crashLoop()
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(0)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v against a target that converged after it exited.", result.Violation)
	}
	if !slices.Contains(result.Notes, crashNote) {
		t.Errorf("The run noted %q, want %q.", result.Notes, crashNote)
	}
}

// A G4 quotes only the exits since the target last converged.
func TestAG4QuotesNoExitBeforeTheTargetLastConverged(t *testing.T) {
	h := crashLoop()
	h.settles = []bool{true, true, false}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(0)}}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(7)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || !strings.Contains(result.Violation.Statement, "op 2 (update)") {
		t.Fatalf("The run reported %v, want the G4 of the wait after op 2.", result.Violation)
	}
	if strings.Contains(result.Violation.Statement, "exited") {
		t.Errorf("The G4 says %q, and the target converged after it exited.", result.Violation.Statement)
	}
	if !slices.Contains(result.Notes, crashNote) {
		t.Errorf("The run noted %q, want %q.", result.Notes, crashNote)
	}
}

// A target that exits on a fault's error waits out the restart's backoff,
// which is not its to answer for. The wait and the checks give it T_settle
// past the restart.
func TestAnExitAFaultExcusedIsOwedTSettlePastItsRestart(t *testing.T) {
	h := crashLoop()
	h.faulting, h.exitsInWait, h.restartsIn = true, 0, time.Hour
	h.stopsAfter = "patchCR widget map[spec:map[count:5]]"
	check := &fakeChecker{}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: nth(2)}}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	_, err := runFake(t, h, check, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if len(h.exited) != 1 {
		t.Fatalf("The target exited %d times, want once.", len(h.exited))
	}
	if want := h.exited[0].Restart.Add(toyTarget.Timeouts.Settle); len(h.owed) < 2 || !h.owed[1].Equal(want) {
		t.Errorf("The waits were told the target owed recovery until %v, want the wait after op 2 told %v.", h.owed, want)
	}
	if exits := check.inputs[1].Timeline.Exits; len(exits) != 1 {
		t.Errorf("The checks after op 2 read the exits %+v, want the one in its wait.", exits)
	}
}

// reconciler-fuzzer chose to restart the target, so the wait after the restart
// gives it T_settle past its return.
func TestTheWaitAfterARestartIsOwedTSettlePastTheTargetsReturn(t *testing.T) {
	h := newFakeHarness()
	var back time.Time
	h.inWait = func() {
		if h.waits == 1 {
			back = time.Now()
			h.logged = append(h.logged, proxy.Request{Start: back, Verb: "list", Version: "v1", Resource: "configmaps"})
		}
	}
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpRestart},
		Op{Type: OpSettle},
	)

	_, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if want := back.Add(testTimeouts.Settle); len(h.owed) < 2 || !h.owed[1].Equal(want) {
		t.Errorf("The waits were told the target owed %v, want the wait after op 1 told %v.", h.owed, want)
	}
}

// An op lands no sooner than a restart a fault excused. While the fault is
// active, the wait after op 2 is owed only the first exit.
func TestAnOpWaitsForARestartAFaultExcused(t *testing.T) {
	for _, test := range []struct {
		name      string
		fault, op Op
	}{
		{name: "a fault that stops at the op", fault: faultUntil(3), op: Op{Type: OpDelete}},
		{name: "a fault still active", fault: faultOp, op: Op{Type: OpDelete}},
		{name: "a restart op", fault: faultOp, op: Op{Type: OpRestart}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(testTimeouts)
			h.settles, h.exitsLate, h.restartsIn = []bool{true, false}, []int{2, 2}, time.Minute

			result, err := runSequence(t.Context(), toyTarget, sequenceOf(createOp, test.fault, updateOp, test.op),
				Options{Check: &fakeChecker{}}, h)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if landed, restart := result.Timeline.Ops[3].At, h.exited[1].Restart; !landed.Equal(restart) {
				t.Errorf("Op 3 landed %v after the restart, want at it.", landed.Sub(restart))
			}
		})
	}
}

// Under a fault still active, the target exits twice in the wait after op 2,
// and then once during op 3. Op 3's wait is owed T_settle past the restart
// after that exit, as op 2's is after its first.
func TestAWaitUnderAFaultOwesTheFirstExitDuringItsOp(t *testing.T) {
	for _, test := range []struct {
		name  string
		op    Op
		exits func(*waitingHarness)
		ended func(*waitingHarness, Result) time.Time
	}{
		{name: "a settle", op: settleOp,
			exits: func(h *waitingHarness) { h.exitsLate = append(h.exitsLate, 3) },
			ended: func(_ *waitingHarness, result Result) time.Time { return result.Timeline.Ops[3].Settled.Window.End }},
		{name: "a recreate's wait for its CR to go", op: Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true},
			exits: func(h *waitingHarness) { h.stopsAfter = "deleteCR widget" },
			ended: func(h *waitingHarness, _ Result) time.Time { return h.awaitedUntil[0] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(testTimeouts)
			h.settles, h.exitsLate, h.restartsIn = []bool{true, false, false}, []int{2, 2}, time.Minute
			test.exits(h)

			result, err := runSequence(t.Context(), toyTarget, sequenceOf(createOp, faultOp, updateOp, test.op),
				Options{Check: &fakeChecker{}}, h)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if len(h.exited) < 3 {
				t.Fatalf("The target exited %d times, want 3 by op 3.", len(h.exited))
			}
			restart := h.exited[2].Restart
			if ended := test.ended(h, result); !ended.Equal(restart.Add(testTimeouts.Settle)) {
				t.Errorf("The wait ended %v after the restart, want T_settle after it.", ended.Sub(restart))
			}
		})
	}
}

// Under a fault still active, the target exits twice in the wait after op 2,
// and op 3 lands at or just after the restart from the second exit. Op 3's
// wait is owed T_settle past the target's return from that restart.
func TestAnOpThatLandsSoonAfterARestartOwesTheTargetsReturn(t *testing.T) {
	for _, test := range []struct {
		name       string
		restartsIn time.Duration
	}{
		{name: "an op that waited for the restart", restartsIn: time.Minute},
		{name: "an op after the restart", restartsIn: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(testTimeouts)
			h.settles, h.exitsLate, h.restartsIn, h.returnsLate = []bool{true, false, false}, []int{2, 2}, test.restartsIn, true

			result, err := runSequence(t.Context(), toyTarget, sequenceOf(createOp, faultOp, updateOp, updateOp),
				Options{Check: &fakeChecker{}}, h)

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if len(h.exited) != 2 {
				t.Fatalf("The target exited %d times, want 2.", len(h.exited))
			}
			// A target that returns late returns just before T_settle past
			// the restart.
			back := h.exited[1].Restart.Add(testTimeouts.Settle - time.Millisecond)
			if ended := result.Timeline.Ops[3].Settled.Window.End; !ended.Equal(back.Add(testTimeouts.Settle)) {
				t.Errorf("Op 3's wait ended %v after the target's return, want T_settle after it.", ended.Sub(back))
			}
		})
	}
}

// sleepEnds runs ends as a sleep begins.
type sleepEnds struct {
	*waitingHarness
	ends func()
}

func (s sleepEnds) sleep(ctx context.Context, d time.Duration) error {
	s.ends()
	return s.waitingHarness.sleep(ctx, d)
}

// A run whose wait for a restart ends, at a deadline or where the restart
// fails, ends before the op.
func TestAnOpDoesNotLandOnceItsWaitForARestartEnds(t *testing.T) {
	for _, test := range []struct {
		name string
		ends func(*waitingHarness, context.CancelFunc)
		want error
	}{
		{name: "a deadline", ends: func(_ *waitingHarness, cancel context.CancelFunc) { cancel() }, want: context.Canceled},
		{name: "a failed restart", want: ErrTargetStopped, ends: func(h *waitingHarness, _ context.CancelFunc) {
			h.targetGone, h.targetExit = true, errors.New("exit status 1, and restarting it failed")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(testTimeouts)
			h.settles, h.exitsLate, h.restartsIn = []bool{true, false}, []int{2, 2}, time.Minute
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			result, err := runSequence(ctx, toyTarget, sequenceOf(createOp, faultOp, updateOp, updateOp),
				Options{Check: &fakeChecker{}}, sleepEnds{waitingHarness: h, ends: func() { test.ends(h, cancel) }})

			if !errors.Is(err, test.want) {
				t.Errorf("The run ended with %v, want %v.", err, test.want)
			}
			if landed := len(result.Timeline.Ops); landed != 3 {
				t.Errorf("%d ops landed, want the 3 before the wait for the restart.", landed)
			}
		})
	}
}

// The wait for recovery from a fault is the teardown's too.
func TestAnExitWhileTheTeardownAwaitsRecoveryIsTheTeardowns(t *testing.T) {
	h := crashLoop()
	h.faulting, h.exitsInWait, h.says = true, 3, nil
	sequence := sequenceOf(
		Op{Type: OpCreate, Obj: widget("widget")},
		Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 30}}},
		Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}},
	)

	result, err := runFake(t, h, nil, sequence)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Timeline.Recovery == nil {
		t.Fatal("The teardown waited for no recovery.")
	}
	if want := "the target exited during the teardown with exit status 2"; !slices.Contains(result.Notes, want) {
		t.Errorf("The run noted %q, want %q.", result.Notes, want)
	}
}

// The teardown judges the deletion of a target that exited during it, and the
// note says where the exit came. The teardown begins as it clears the faults.
func TestAnExitDuringTheTeardownIsNotedAsSuch(t *testing.T) {
	for _, exit := range []struct {
		call string
		// judged is whether the teardown's checks read the exit.
		judged bool
	}{
		{"clearFaults", true},
		{"deleteCR widget", true},
		{"forceFinalizers", false},
	} {
		t.Run(exit.call, func(t *testing.T) {
			h := crashLoop()
			h.exitsInWait, h.stopsAfter, h.says = 0, exit.call, nil
			check := &fakeChecker{}

			result, err := runFake(t, h, check, sequenceOf(Op{Type: OpCreate, Obj: widget("widget")}))

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if want := "the target exited during the teardown with exit status 2"; !slices.Contains(result.Notes, want) {
				t.Errorf("The run noted %q, want %q.", result.Notes, want)
			}
			if got := checkpointsAt(result.Timeline); !slices.Equal(got, []int{0, Teardown}) {
				t.Errorf("The run checkpointed at %v, want the deletion judged too.", got)
			}
			if read := len(check.inputs[1].Timeline.Exits) == 1; read != exit.judged {
				t.Errorf("The teardown's checks read the exits %+v; want the exit read: %t.", check.inputs[1].Timeline.Exits, exit.judged)
			}
		})
	}
}

func TestTheProxyGetsTheFaultAsWritten(t *testing.T) {
	for _, test := range []struct {
		spec string
		want proxy.FaultSpec
	}{
		{
			spec: `{"match": {"verb": "patch", "resource": "widgets", "name": "widget-*", "fraction": 0.25},
				"action": {"delay": "500ms"}, "until": {"count": 2, "for": "3s"}}`,
			want: proxy.FaultSpec{
				Match:  proxy.RequestMatcher{Verb: "patch", Resource: "widgets", Name: "widget-*", Fraction: 0.25},
				Action: proxy.Delay{For: 500 * time.Millisecond},
				Until:  proxy.Trigger{Count: 2, For: 3 * time.Second},
			},
		},
		{
			spec: `{"action": {"error": 503}}`,
			want: proxy.FaultSpec{Action: proxy.Error{Code: 503}},
		},
	} {
		var fault Fault
		if err := json.Unmarshal([]byte(test.spec), &fault); err != nil {
			t.Fatal(err)
		}

		if got := fault.spec(); !reflect.DeepEqual(got, test.want) {
			t.Errorf("The fault %s reached the proxy as %+v, want %+v.", test.spec, got, test.want)
		}
	}
}
