package invariant_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// The fixtures are the toy target of DESIGN.md §9: a Widget owning one
// ConfigMap per index, with the toy's short windows.
const (
	namespace     = "reconciler-fuzzer-run-1"
	widgetName    = "w"
	widgetUID     = "uid-w"
	settleTimeout = 5 * time.Second
	stableWindow  = 2 * time.Second
	deleteTimeout = 10 * time.Second
	errLoop       = 5
)

var (
	widgetGVK    = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	secretGVK    = schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	epoch        = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// at returns the instant d into the run.
func at(d time.Duration) time.Time { return epoch.Add(d) }

func toyTarget() *target.Target {
	return &target.Target{
		Name:    "toy-widget",
		Primary: widgetGVK,
		Manages: []schema.GroupVersionKind{configMapGVK},
		Ready:   widgetReady,
		Properties: []target.Property{{
			ID:          "P1",
			Description: "status.ready never exceeds the number of ConfigMaps present.",
			Eval:        readyCountsChildren,
			When:        target.Checkpoint,
		}},
		Timeouts:   target.Timeouts{Settle: settleTimeout, Stable: stableWindow, Delete: deleteTimeout},
		Thresholds: target.Thresholds{ErrLoop: errLoop},
	}
}

// widgetReady is the toy's ready predicate (DESIGN.md §9).
func widgetReady(cr *unstructured.Unstructured) (bool, error) {
	observed, found, err := unstructured.NestedInt64(cr.Object, "status", "observedGeneration")
	if !found || err != nil {
		return false, err
	}
	ready, found, err := unstructured.NestedInt64(cr.Object, "status", "ready")
	if !found || err != nil {
		return false, err
	}
	count, _, err := unstructured.NestedInt64(cr.Object, "spec", "count")
	return observed == cr.GetGeneration() && ready == count, err
}

// readyCountsChildren is the toy's P1 (DESIGN.md §9).
func readyCountsChildren(cr *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error) {
	if cr == nil {
		return true, nil
	}
	ready, found, err := unstructured.NestedInt64(cr.Object, "status", "ready")
	return !found || ready <= int64(len(managed)), err
}

// run assembles one run's Input.
type run struct {
	in    invariant.Input
	store *observe.Store
}

// newRun returns a run of the toy target whose primary CR reconciler-fuzzer
// created, so that the CR is never a managed object (DESIGN.md §6).
func newRun() *run { return newRunManaging(configMapGVK) }

// newRunManaging returns a run of a target that manages the kinds named, since
// a real target manages several: cert-manager's example declares two.
func newRunManaging(kinds ...schema.GroupVersionKind) *run {
	t := toyTarget()
	t.Manages = kinds
	store := observe.NewStore(observe.Options{Namespace: namespace, Manages: kinds, Mapper: toyMapper()})
	store.Exclude(widgetGVK, widgetName)
	return &run{in: invariant.Input{Target: t, History: store}, store: store}
}

// toyMapper serves every kind the fixtures record.
func toyMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range []schema.GroupVersionKind{widgetGVK, configMapGVK, secretGVK} {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

// fixture marks an object as reconciler-fuzzer's, so that it is never managed
// (§6).
func (r *run) fixture(obj *unstructured.Unstructured) *run {
	r.store.Exclude(obj.GroupVersionKind(), obj.GetName())
	return r
}

// op is an op of the type given, which writes the CR w if it is a CR op.
func (r *run) op(opType invariant.OpType, when time.Duration) *run {
	return r.opOn(opType, when, widgetName)
}

// opOn is an op of the type given, which writes the CR named if it is a CR op.
func (r *run) opOn(opType invariant.OpType, when time.Duration, cr string) *run {
	op := invariant.Op{Index: len(r.in.Ops), Type: opType, Time: at(when)}
	if slices.Contains([]invariant.OpType{invariant.OpCreate, invariant.OpUpdate, invariant.OpDelete, invariant.OpRecreate}, opType) {
		op.CR = observe.Key{GVK: widgetGVK, Namespace: namespace, Name: cr}
	}
	r.in.Ops = append(r.in.Ops, op)
	return r
}

// withSecondWidget has reconciler-fuzzer create the Widget w2 too, so that it
// is never managed either.
func (r *run) withSecondWidget() *run {
	r.store.Exclude(widgetGVK, secondName)
	return r
}

// deletedManaged is a DeleteManaged op and the object it resolved to, which
// reconciler-fuzzer deleted behind the target's back (DESIGN.md §5.4).
func (r *run) deletedManaged(when time.Duration, name string) *run {
	return r.deletedManagedOf(when, configMapGVK, name)
}

func (r *run) deletedManagedOf(when time.Duration, gvk schema.GroupVersionKind, name string) *run {
	r.in.Ops = append(r.in.Ops, invariant.Op{
		Index:   len(r.in.Ops),
		Type:    invariant.OpDeleteManaged,
		Time:    at(when),
		Deleted: observe.Key{GVK: gvk, Namespace: namespace, Name: name},
	})
	return r
}

// restoring is an op of the type given, before which reconciler-fuzzer restored
// a fixture it had deleted.
func (r *run) restoring(opType invariant.OpType, when time.Duration) *run {
	r.op(opType, when)
	r.in.Ops[len(r.in.Ops)-1].Restored = true
	return r
}

// checkpoint ends the settle wait of the last op, which began where the op
// was applied.
func (r *run) checkpoint(when time.Duration, result invariant.SettleResult) *run {
	var began time.Time
	if n := len(r.in.Ops); n > 0 {
		began = r.in.Ops[n-1].Time
	}
	r.in.Checkpoints = append(r.in.Checkpoints, invariant.Checkpoint{
		Op: len(r.in.Ops) - 1, Began: began, Time: at(when), Settle: result,
	})
	return r
}

// waitBegan moves where the last settle wait began, which the Runner stamps
// once the op has returned.
func (r *run) waitBegan(when time.Duration) *run {
	r.in.Checkpoints[len(r.in.Checkpoints)-1].Began = at(when)
	return r
}

// stayed makes the last wait a recreate's for its old CR to go, which ended
// with the CR still there.
func (r *run) stayed() *run {
	r.in.Checkpoints[len(r.in.Checkpoints)-1].Stayed = true
	return r
}

// held has the proxy hold a request of the target's at the last checkpoint.
func (r *run) held() *run {
	r.in.Checkpoints[len(r.in.Checkpoints)-1].Held = true
	return r
}

// settled ends the last op's settle wait at when, which is where §6's quiet
// window opens. The teardown follows one T_stable later, because §5.5 step 4
// waits that long before it deletes.
func (r *run) settled(when time.Duration, result invariant.SettleResult) *run {
	return r.checkpoint(when, result).teardown(when + stableWindow + 100*time.Millisecond)
}

// teardownCheckpoint follows the teardown's deletion window and no settle wait.
func (r *run) teardownCheckpoint(when time.Duration) *run {
	r.in.Checkpoints = append(r.in.Checkpoints, invariant.Checkpoint{Op: invariant.Teardown, Time: at(when)})
	return r
}

// teardown is when reconciler-fuzzer began emptying the namespace (DESIGN.md
// §5.5).
func (r *run) teardown(when time.Duration) *run {
	r.in.Teardown = at(when)
	return r
}

// quiet is the T_stable the teardown waits before it deletes, which §5.5
// step 4 makes the run's last quiet window.
func (r *run) quiet(from time.Duration) *run {
	r.in.Quiet = at(from)
	return r.teardown(from + stableWindow)
}

// cleaned is when reconciler-fuzzer saw the run namespace empty (DESIGN.md
// §5.5).
func (r *run) cleaned(when time.Duration) *run {
	r.in.Cleaned = at(when)
	return r
}

func (r *run) fault(from, to time.Duration) *run {
	r.in.Faults = append(r.in.Faults, invariant.FaultWindow{Start: at(from), End: at(to)})
	return r
}

// activeFault is a fault that has not stopped by the end of the observation.
func (r *run) activeFault(from time.Duration) *run {
	r.in.Faults = append(r.in.Faults, invariant.FaultWindow{Start: at(from)})
	return r
}

// exit is the target stopping on its own at when, and reconciler-fuzzer
// starting it again at restart.
func (r *run) exit(when, restart time.Duration) *run {
	r.in.Exits = append(r.in.Exits, invariant.Exit{At: at(when), Restart: at(restart), Why: "the exit at " + when.String()})
	return r
}

func (r *run) request(when time.Duration, req proxy.Request) *run {
	req.Start = at(when)
	r.in.Requests = append(r.in.Requests, req)
	return r
}

// running records a request that shows the target runs.
func (r *run) running(when time.Duration) *run { return r.request(when, watch()) }

// requests repeats one request at a fixed interval.
func (r *run) requests(first, every time.Duration, count int, req proxy.Request) *run {
	for i := range count {
		r.request(first+time.Duration(i)*every, req)
	}
	return r
}

func (r *run) record(when time.Duration, objects ...*unstructured.Unstructured) *run {
	for _, obj := range objects {
		r.store.Record(obj.GroupVersionKind(), obj, at(when))
	}
	return r
}

func (r *run) remove(when time.Duration, objects ...*unstructured.Unstructured) *run {
	for _, obj := range objects {
		r.store.RecordDeletion(obj.GroupVersionKind(), obj, at(when))
	}
	return r
}

// through ends the observation at when, which is where the engine evaluates.
func (r *run) through(when time.Duration) invariant.Input {
	r.in.End = at(when)
	return r.in
}

type option func(*unstructured.Unstructured)

func widget(resourceVersion string, opts ...option) *unstructured.Unstructured {
	defaults := []option{uid(widgetUID), generation(1)}
	return object(widgetGVK, widgetName, resourceVersion, append(defaults, opts...)...)
}

// child is a ConfigMap of the Widget, owned by it unless an option says
// otherwise.
func child(name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	return object(configMapGVK, name, resourceVersion, append([]option{ownedByWidget}, opts...)...)
}

// The second Widget of a run that creates two.
const (
	secondName = "w2"
	secondUID  = "uid-w2"
)

func secondWidget(resourceVersion string, opts ...option) *unstructured.Unstructured {
	defaults := []option{uid(secondUID), generation(1)}
	return object(widgetGVK, secondName, resourceVersion, append(defaults, opts...)...)
}

// secondChild is a ConfigMap of the second Widget.
func secondChild(name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	return object(configMapGVK, name, resourceVersion, append([]option{ownedBySecond}, opts...)...)
}

func ownedBySecond(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       secondName,
		UID:        secondUID,
	}})
}

// ownedByBoth names both Widgets as owners.
func ownedByBoth(u *unstructured.Unstructured) {
	ownedByWidget(u)
	first := u.GetOwnerReferences()
	ownedBySecond(u)
	u.SetOwnerReferences(append(first, u.GetOwnerReferences()...))
}

// secret is a Secret of the Widget, a second managed kind.
func secret(name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	return object(secretGVK, name, resourceVersion, append([]option{ownedByWidget}, opts...)...)
}

func object(gvk schema.GroupVersionKind, name, resourceVersion string, opts ...option) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetResourceVersion(resourceVersion)
	u.SetUID(types.UID("uid-" + name))
	for _, opt := range opts {
		opt(u)
	}
	return u
}

func spec(count int64) option {
	return func(u *unstructured.Unstructured) {
		u.Object["spec"] = map[string]any{"count": count}
	}
}

func status(ready, observedGeneration int64) option {
	return func(u *unstructured.Unstructured) {
		u.Object["status"] = map[string]any{"ready": ready, "observedGeneration": observedGeneration}
	}
}

func generation(g int64) option {
	return func(u *unstructured.Unstructured) { u.SetGeneration(g) }
}

func uid(id string) option {
	return func(u *unstructured.Unstructured) { u.SetUID(types.UID(id)) }
}

func finalizers(names ...string) option {
	return func(u *unstructured.Unstructured) { u.SetFinalizers(names) }
}

func deleting(when time.Duration) option {
	return func(u *unstructured.Unstructured) {
		stamp := metav1.NewTime(at(when))
		u.SetDeletionTimestamp(&stamp)
	}
}

func ownedByWidget(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       widgetName,
		UID:        widgetUID,
	}})
}

// ownedByRecreated names the Widget w that a recreate made, under a new UID.
func ownedByRecreated(u *unstructured.Unstructured) {
	ownedByWidget(u)
	refs := u.GetOwnerReferences()
	refs[0].UID = "uid-w-again"
	u.SetOwnerReferences(refs)
}

func orphaned(u *unstructured.Unstructured) { u.SetOwnerReferences(nil) }

// ownedByAPod names an owner of a kind the target does not declare.
func ownedByAPod(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "p", UID: "uid-p"}})
}

// ownedByAWidgetElsewhere names an owner of the primary's kind in another
// group.
func ownedByAWidgetElsewhere(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "other.example/v1", Kind: widgetGVK.Kind, Name: "w", UID: "uid-other-w"}})
}

// ownedByGhost names an owner no snapshot holds, which §6 ignores.
func ownedByGhost(u *unstructured.Unstructured) {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       "gone",
		UID:        "uid-gone",
	}})
}

func nothing(*unstructured.Unstructured) {}

func created(when time.Duration) option {
	return func(u *unstructured.Unstructured) {
		u.SetCreationTimestamp(metav1.NewTime(at(when)))
	}
}

func managedFields(manager string) option {
	return func(u *unstructured.Unstructured) {
		u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: manager, Operation: metav1.ManagedFieldsOperationUpdate}})
	}
}

func condition(status string, lastTransition time.Duration) option {
	return func(u *unstructured.Unstructured) {
		u.Object["status"] = map[string]any{"conditions": []any{map[string]any{
			"type":               "Ready",
			"status":             status,
			"lastTransitionTime": at(lastTransition).Format(time.RFC3339),
		}}}
	}
}

func labelled(value string) option {
	return func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"app": value}) }
}

func annotated(value string) option {
	return func(u *unstructured.Unstructured) { u.SetAnnotations(map[string]string{"note": value}) }
}

func data(value string) option {
	return func(u *unstructured.Unstructured) {
		u.Object["data"] = map[string]any{"index": value}
	}
}

// get is the read a quiet target does not make.
func get(name string) proxy.Request {
	return proxy.Request{Verb: "get", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: name, Status: 200}
}

// createChild is a create, which names its object in the body, not the path.
func createChild() proxy.Request {
	created := get("")
	created.Verb, created.Status = "create", http.StatusCreated
	return created
}

// write is a write of the verb to the ConfigMap named.
func write(verb, name string) proxy.Request {
	written := get(name)
	written.Verb = verb
	return written
}

// widgetPatch changes the Widget's metadata or spec, not its status.
func widgetPatch() proxy.Request {
	patched := statusPatch()
	patched.Subresource = ""
	return patched
}

func failedGet(name string, status int) proxy.Request {
	failed := get(name)
	failed.Status = status
	return failed
}

func failedDelete(name string, status int) proxy.Request {
	failed := get(name)
	failed.Verb, failed.Status = "delete", status
	return failed
}

// conflicted is the request the API server answered 409.
func conflicted(verb, name string) proxy.Request {
	failed := get(name)
	failed.Verb, failed.Status = verb, http.StatusConflict
	return failed
}

func failedWidgetGet(status int) proxy.Request {
	return proxy.Request{
		Verb: "get", Group: widgetGVK.Group, Version: widgetGVK.Version, Resource: "widgets",
		Namespace: namespace, Name: widgetName, Status: status,
	}
}

func failedStatusUpdate(status int) proxy.Request {
	failed := failedWidgetGet(status)
	failed.Verb, failed.Subresource = "update", "status"
	return failed
}

func watch() proxy.Request {
	return proxy.Request{Verb: "watch", Version: "v1", Resource: "configmaps", Namespace: namespace, Watch: true, Status: 200}
}

// failedWatch is the watch envtest's flow control turned away.
func failedWatch(status int) proxy.Request {
	failed := watch()
	failed.Status = status
	return failed
}

// lease is one leader-election request, which §6 excludes from G1 whether it
// reads or writes.
func lease(verb string) proxy.Request {
	return proxy.Request{
		Verb: verb, Group: "coordination.k8s.io", Version: "v1", Resource: "leases",
		Namespace: namespace, Name: "toy-widget", Status: 200,
	}
}

func leaseAnswered(verb string, status int) proxy.Request {
	answered := lease(verb)
	answered.Status = status
	return answered
}

// leaseCandidate is a request that coordinated leader election makes before the
// target leads, as well as after.
func leaseCandidate(verb string) proxy.Request {
	return proxy.Request{
		Verb: verb, Group: "coordination.k8s.io", Version: "v1beta1", Resource: "leasecandidates",
		Namespace: namespace, Name: "toy-widget", Status: 200,
	}
}

// leasesElsewhere reads a resource that shares the name of leader election's
// leases but not their group.
func leasesElsewhere() proxy.Request {
	elsewhere := lease("get")
	elsewhere.Group = "example.com"
	return elsewhere
}

// nonResource is a request to a path that names no resource: a health probe,
// a discovery read or the OpenAPI schema.
func nonResource(path string) proxy.Request {
	return proxy.Request{Verb: "get", Path: path, Status: 200}
}

func statusPatch() proxy.Request {
	return proxy.Request{
		Verb: "patch", Group: widgetGVK.Group, Version: widgetGVK.Version, Resource: "widgets",
		Namespace: namespace, Name: widgetName, Subresource: "status", Status: 200,
	}
}

// fired returns the single violation the check reported.
func fired(t *testing.T, check invariant.Check, in invariant.Input) invariant.Violation {
	t.Helper()
	result := evaluate(t, check, in)
	if len(result.Violations) != 1 {
		t.Fatalf("%s reported %d violations, want exactly one: %v", result.ID, len(result.Violations), statements(result))
	}
	return result.Violations[0]
}

// noted fails the test unless the check reported no violation and one note
// saying what it did not judge.
func noted(t *testing.T, check invariant.Check, in invariant.Input, want string) {
	t.Helper()
	result := silent(t, check, in)
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], want) {
		t.Fatalf("%s noted %v, want one note saying %q.", result.ID, result.Notes, want)
	}
}

// silent fails the test unless the check reported nothing.
func silent(t *testing.T, check invariant.Check, in invariant.Input) invariant.Result {
	t.Helper()
	result := evaluate(t, check, in)
	if len(result.Violations) > 0 {
		t.Fatalf("%s reported %v, want no violation.", result.ID, statements(result))
	}
	return result
}

func evaluate(t *testing.T, check invariant.Check, in invariant.Input) invariant.Result {
	t.Helper()
	result, err := check(in)
	if err != nil {
		t.Fatalf("The check returned an error: %v", err)
	}
	return result
}

func statements(result invariant.Result) []string {
	out := make([]string, len(result.Violations))
	for i, v := range result.Violations {
		out[i] = v.Statement
	}
	return out
}

// managed renders a violation's count of managed objects for a message, and
// "no" where no check asked.
func managed(v invariant.Violation) string {
	if v.ManagedTotal == nil {
		return "no"
	}
	return strconv.Itoa(*v.ManagedTotal)
}

// timelineOf is the object a violation's timeline quotes, as the report
// names it.
func timelineOf(gvk schema.GroupVersionKind, name string) string { return kindOf(gvk) + " " + name }

// state names the managed objects a violation quotes, for a message.
func state(v invariant.Violation) []string { return names(v.Managed) }

// version is one recorded version of an object, for a message or a sample.
func version(gvk schema.GroupVersionKind, name string, when time.Duration) observe.Version {
	return observe.Version{Key: observe.Key{GVK: gvk, Name: name}, Time: at(when)}
}

func names(versions []observe.Version) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Name
	}
	return out
}

// quoted names the versions a violation carries, for a message.
func quoted(v invariant.Violation) []string {
	names := make([]string, len(v.Versions))
	for i, version := range v.Versions {
		names[i] = version.Name + "@" + version.ResourceVersion
	}
	return names
}

func versionsOf(versions []observe.Version, gvk schema.GroupVersionKind) []observe.Version {
	var found []observe.Version
	for _, version := range versions {
		if version.GVK == gvk {
			found = append(found, version)
		}
	}
	return found
}

func kindOf(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return gvk.Version + "/" + gvk.Kind
	}
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}
