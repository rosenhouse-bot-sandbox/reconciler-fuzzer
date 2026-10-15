package observe_test

import (
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

const namespace = "reconciler-fuzzer-run-1"

var (
	configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	widgetGVK    = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	epoch        = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// at returns the timestamp n seconds into a run.
func at(n int) time.Time { return epoch.Add(time.Duration(n) * time.Second) }

func key(gvk schema.GroupVersionKind, name string) observe.Key {
	return observe.Key{GVK: gvk, Namespace: namespace, Name: name}
}

// managing returns Options for a target whose primary CR is a Widget.
func managing(kinds ...schema.GroupVersionKind) observe.Options {
	return observe.Options{Namespace: namespace, Manages: kinds}
}

func object(gvk schema.GroupVersionKind, name, resourceVersion string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(namespace)
	u.SetName(name)
	u.SetResourceVersion(resourceVersion)
	u.SetUID(types.UID("uid-" + name))
	return u
}

func withUID(u *unstructured.Unstructured, uid string) *unstructured.Unstructured {
	u.SetUID(types.UID(uid))
	return u
}

func withData(u *unstructured.Unstructured, value string) *unstructured.Unstructured {
	u.Object["data"] = map[string]any{"value": value}
	return u
}

func withStatus(u *unstructured.Unstructured, status map[string]any) *unstructured.Unstructured {
	u.Object["status"] = status
	return u
}

func withOwner(u *unstructured.Unstructured, uid string) *unstructured.Unstructured {
	u.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: widgetGVK.GroupVersion().String(),
		Kind:       widgetGVK.Kind,
		Name:       "widget",
		UID:        types.UID(uid),
	}})
	return u
}

func names(versions []observe.Version) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.Name
	}
	return out
}

func resourceVersions(versions []observe.Version) []string {
	out := make([]string, len(versions))
	for i, v := range versions {
		out[i] = v.ResourceVersion
	}
	return out
}

func snapshotNames(snapshots []observe.Snapshot) []string {
	out := make([]string, len(snapshots))
	for i, s := range snapshots {
		out[i] = s.Name
	}
	return out
}

func dataValue(t *testing.T, obj *unstructured.Unstructured) string {
	t.Helper()
	value, found, err := unstructured.NestedString(obj.Object, "data", "value")
	if err != nil || !found {
		t.Fatalf("The object carries no data.value: %v", err)
	}
	return value
}

func TestHistoryRecordsEveryVersionInOrder(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))

	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "child", "11"), at(1))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "child", "12"), at(2))

	history := s.History(key(configMapGVK, "child"))
	if got := resourceVersions(history); !slices.Equal(got, []string{"10", "11", "12"}) {
		t.Fatalf("History holds resourceVersions %v, want 10, 11 and 12 in order.", got)
	}
	if !history[0].Time.Equal(at(0)) {
		t.Errorf("The first version is timestamped %v, want %v.", history[0].Time, at(0))
	}
	if history[0].Deleted || history[1].Deleted {
		t.Error("A version recorded before the deletion is marked deleted.")
	}
	if !history[2].Deleted {
		t.Error("The deletion is not marked deleted.")
	}
}

func TestHistoryOfAnObjectNeverSeenIsEmpty(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))

	if got := s.History(key(configMapGVK, "absent")); len(got) != 0 {
		t.Errorf("History returned %d versions for an object never recorded.", len(got))
	}
}

func TestRecordCapturesTheMetadataTheInvariantsRead(t *testing.T) {
	deletionTimestamp := metav1.NewTime(at(9))
	obj := withStatus(withOwner(withData(object(configMapGVK, "child", "10"), "content"), "uid-widget"), map[string]any{
		"observedGeneration": int64(4),
	})
	obj.SetGeneration(5)
	obj.SetFinalizers([]string{"widget.reconciler-fuzzer/cleanup"})
	obj.SetLabels(map[string]string{"app": "toy"})
	obj.SetDeletionTimestamp(&deletionTimestamp)

	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, obj, at(0))

	v := s.History(key(configMapGVK, "child"))[0]
	if v.UID != types.UID("uid-child") {
		t.Errorf("The version carries UID %q, want uid-child.", v.UID)
	}
	if v.Generation != 5 {
		t.Errorf("The version carries generation %d, want 5.", v.Generation)
	}
	if v.ObservedGeneration == nil || *v.ObservedGeneration != 4 {
		t.Errorf("The version carries observedGeneration %v, want 4.", v.ObservedGeneration)
	}
	if !slices.Equal(v.Finalizers, []string{"widget.reconciler-fuzzer/cleanup"}) {
		t.Errorf("The version carries finalizers %v.", v.Finalizers)
	}
	if len(v.OwnerReferences) != 1 || v.OwnerReferences[0].UID != types.UID("uid-widget") {
		t.Errorf("The version carries ownerReferences %v, want one naming uid-widget.", v.OwnerReferences)
	}
	if v.DeletionTimestamp == nil || !v.DeletionTimestamp.Equal(&deletionTimestamp) {
		t.Errorf("The version carries deletionTimestamp %v, want %v.", v.DeletionTimestamp, deletionTimestamp)
	}
	if v.Labels["app"] != "toy" {
		t.Errorf("The version carries labels %v, want app=toy.", v.Labels)
	}
	if v.Object == nil || dataValue(t, v.Object) != "content" {
		t.Error("The version does not carry the whole object, which G5 compares.")
	}
}

func TestRecordCopiesTheObject(t *testing.T) {
	// An informer hands out the object in its cache, which it reuses.
	obj := object(configMapGVK, "child", "10")
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, obj, at(0))

	obj.SetResourceVersion("11")

	if got := s.History(key(configMapGVK, "child"))[0].Object.GetResourceVersion(); got != "10" {
		t.Errorf("A mutation of the caller's object reached the history: resourceVersion is now %q.", got)
	}
}

func TestRecordIgnoresAResourceVersionItAlreadyHolds(t *testing.T) {
	// A relist re-delivers objects the store already saw.
	s := observe.NewStore(managing(configMapGVK))

	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(1))

	if got := len(s.History(key(configMapGVK, "child"))); got != 1 {
		t.Errorf("History holds %d versions, want the repeated resourceVersion recorded once.", got)
	}
}

func TestCurrentReturnsTheLiveObjectsOfOneKind(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "kept", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "gone", "11"), at(1))
	s.Record(widgetGVK, object(widgetGVK, "widget", "12"), at(2))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "gone", "13"), at(3))

	if got := names(s.Current(configMapGVK)); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("Current returned %v, want only the live ConfigMap.", got)
	}
}

func TestCurrentIncludesAnObjectRecreatedUnderTheSameName(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, withUID(object(configMapGVK, "child", "10"), "first"), at(0))
	s.RecordDeletion(configMapGVK, withUID(object(configMapGVK, "child", "11"), "first"), at(1))
	s.Record(configMapGVK, withUID(object(configMapGVK, "child", "12"), "second"), at(2))

	current := s.Current(configMapGVK)
	if len(current) != 1 || current[0].UID != types.UID("second") {
		t.Errorf("Current returned %v, want the recreated object with UID second.", current)
	}
}

func TestSnapshotAtReturnsTheLatestVersionAtOrBeforeTheMoment(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, withData(object(configMapGVK, "child", "10"), "before"), at(1))
	s.Record(configMapGVK, withData(object(configMapGVK, "child", "11"), "after"), at(3))

	for _, tc := range []struct {
		moment time.Time
		want   string
	}{
		{at(1), "before"}, // the version recorded at the moment itself counts
		{at(2), "before"},
		{at(3), "after"},
	} {
		snapshot := s.SnapshotAt(tc.moment)
		if len(snapshot) != 1 {
			t.Fatalf("The snapshot at %v holds %d objects, want 1.", tc.moment, len(snapshot))
		}
		if got := dataValue(t, snapshot[0].Object); got != tc.want {
			t.Errorf("The snapshot at %v holds data %q, want %q.", tc.moment, got, tc.want)
		}
		if snapshot[0].GVK != configMapGVK || snapshot[0].Name != "child" {
			t.Errorf("The snapshot is keyed by %v/%q, want the kind and name.", snapshot[0].GVK, snapshot[0].Name)
		}
	}
}

// KindName orders the kinds, so toy.reconciler-fuzzer/v1/Widget comes before
// v1/ConfigMap.
func TestSnapshotsAndLiveObjectsAreOrderedByKindThenName(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK, widgetGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.Record(widgetGVK, object(widgetGVK, "widget", "11"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "another", "12"), at(0))
	want := []string{"widget", "another", "child"}

	if got := snapshotNames(s.SnapshotAt(at(0))); !slices.Equal(got, want) {
		t.Errorf("SnapshotAt returned %v, want %v.", got, want)
	}
	if got := names(s.Managed()); !slices.Equal(got, want) {
		t.Errorf("Managed returned %v, want %v.", got, want)
	}
}

func TestSnapshotAtIsEmptyBeforeTheFirstVersion(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(1))

	if got := s.SnapshotAt(at(0)); len(got) != 0 {
		t.Errorf("The snapshot before the first version holds %v.", snapshotNames(got))
	}
}

func TestSnapshotAtExcludesAnObjectAlreadyDeleted(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "child", "11"), at(1))

	if got := s.SnapshotAt(at(2)); len(got) != 0 {
		t.Errorf("The snapshot after the deletion holds %v.", snapshotNames(got))
	}
}

func TestWindowReturnsTheVersionsRecordedWithinIt(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	for i, rv := range []string{"10", "11", "12", "13"} {
		s.Record(configMapGVK, object(configMapGVK, "child", rv), at(i))
	}

	got := resourceVersions(s.Window(at(1), at(2)))
	if !slices.Equal(got, []string{"11", "12"}) {
		t.Errorf("Window returned %v, want the versions at both bounds and nothing outside them.", got)
	}
}

func TestWindowSpansEveryKind(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(widgetGVK, object(widgetGVK, "widget", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "child", "11"), at(1))

	if got := names(s.Window(at(0), at(1))); !slices.Equal(got, []string{"widget", "child"}) {
		t.Errorf("Window returned %v, want both kinds in the order recorded.", got)
	}
}

func TestVersionsOfReturnsOneKindUpToAnInstant(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(widgetGVK, object(widgetGVK, "widget", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "child", "11"), at(1))
	s.Record(widgetGVK, object(widgetGVK, "widget", "12"), at(2))
	s.Record(widgetGVK, object(widgetGVK, "widget", "13"), at(3))

	if got := resourceVersions(s.VersionsOf(widgetGVK, at(2))); !slices.Equal(got, []string{"10", "12"}) {
		t.Errorf("VersionsOf returned %v, want the Widget's versions up to the instant.", got)
	}
}

func TestManagedExcludesFixturesAndTheKindsTheTargetDoesNotManage(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Exclude(configMapGVK, "fixture")

	s.Record(configMapGVK, object(configMapGVK, "fixture", "10"), at(0))
	s.Record(configMapGVK, object(configMapGVK, "child", "11"), at(1))
	s.Record(widgetGVK, object(widgetGVK, "widget", "12"), at(2))

	if got := names(s.Managed()); !slices.Equal(got, []string{"child"}) {
		t.Errorf("Managed returned %v, want only the object the target created.", got)
	}
}

func TestAnExcludedObjectStaysExcludedWhenItIsRecreated(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Exclude(configMapGVK, "kube-root-ca.crt")

	s.Record(configMapGVK, object(configMapGVK, "kube-root-ca.crt", "10"), at(0))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "kube-root-ca.crt", "11"), at(1))
	s.Record(configMapGVK, withUID(object(configMapGVK, "kube-root-ca.crt", "12"), "uid-recreated"), at(2))

	if got := names(s.Managed()); len(got) != 0 {
		t.Errorf("Managed returned %v, want the recreated object excluded.", got)
	}
	if s.IsManaged(key(configMapGVK, "kube-root-ca.crt")) {
		t.Error("IsManaged accepted the recreated object.")
	}
}

func TestManagedExcludesADeletedObject(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "child", "11"), at(1))

	if got := names(s.Managed()); len(got) != 0 {
		t.Errorf("Managed returned %v after the object was deleted.", got)
	}
}

func TestIsManagedHoldsAfterTheObjectIsDeleted(t *testing.T) {
	// G3 asks whether an object that is now gone was the target's.
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))
	s.RecordDeletion(configMapGVK, object(configMapGVK, "child", "11"), at(1))

	if !s.IsManaged(key(configMapGVK, "child")) {
		t.Error("IsManaged rejected a deleted object the target created.")
	}
	if s.IsManaged(key(configMapGVK, "never-seen")) {
		t.Error("IsManaged accepted an object the store never recorded.")
	}
}

func TestManagedAppliesTheSelector(t *testing.T) {
	opts := managing(configMapGVK)
	opts.Selector = labels.SelectorFromSet(labels.Set{"app": "toy"})
	s := observe.NewStore(opts)

	labelled := object(configMapGVK, "labelled", "10")
	labelled.SetLabels(map[string]string{"app": "toy"})
	s.Record(configMapGVK, labelled, at(0))
	s.Record(configMapGVK, object(configMapGVK, "unlabelled", "11"), at(1))

	if got := names(s.Managed()); !slices.Equal(got, []string{"labelled"}) {
		t.Errorf("Managed returned %v, want only the object the selector matches.", got)
	}
}

func TestManagedByReturnsTheObjectsOneOwnerOwns(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, withOwner(object(configMapGVK, "owned", "10"), "uid-widget"), at(0))
	s.Record(configMapGVK, withOwner(object(configMapGVK, "other", "11"), "uid-other"), at(1))
	s.Record(configMapGVK, object(configMapGVK, "orphan", "12"), at(2))

	if got := names(s.ManagedBy("uid-widget")); !slices.Equal(got, []string{"owned"}) {
		t.Errorf("ManagedBy returned %v, want only the object owned by uid-widget.", got)
	}
}

func TestManagedByExcludesObjectsReconcilerFuzzerCreated(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Exclude(configMapGVK, "fixture")
	s.Record(configMapGVK, withOwner(object(configMapGVK, "fixture", "10"), "uid-widget"), at(0))

	if got := names(s.ManagedBy("uid-widget")); len(got) != 0 {
		t.Errorf("ManagedBy returned %v, want the fixture excluded however it is owned.", got)
	}
}

func TestObservedGenerationComesFromTheTopLevelStatus(t *testing.T) {
	s := observe.NewStore(managing(widgetGVK))
	s.Record(widgetGVK, withStatus(object(widgetGVK, "widget", "10"), map[string]any{
		"observedGeneration": int64(3),
		"conditions":         []any{map[string]any{"type": "Ready", "observedGeneration": int64(1)}},
	}), at(0))

	got := s.History(key(widgetGVK, "widget"))[0].ObservedGeneration
	if got == nil || *got != 3 {
		t.Errorf("ObservedGeneration is %v, want the top-level 3.", got)
	}
}

func TestObservedGenerationFallsBackToTheLeastTheConditionsReport(t *testing.T) {
	s := observe.NewStore(managing(widgetGVK))
	s.Record(widgetGVK, withStatus(object(widgetGVK, "widget", "10"), map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "observedGeneration": int64(7)},
			map[string]any{"type": "Issuing", "observedGeneration": int64(6)},
			map[string]any{"type": "Stale"},
		},
	}), at(0))

	got := s.History(key(widgetGVK, "widget"))[0].ObservedGeneration
	if got == nil || *got != 6 {
		t.Errorf("ObservedGeneration is %v, want the least a condition reports, 6.", got)
	}
}

func TestObservedGenerationIsAbsentWhenNothingReportsIt(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(configMapGVK, object(configMapGVK, "child", "10"), at(0))

	if got := s.History(key(configMapGVK, "child"))[0].ObservedGeneration; got != nil {
		t.Errorf("ObservedGeneration is %v for an object that reports none.", *got)
	}
}

func TestTheStoreServesReadersWhileItRecords(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 50 {
				s.Record(configMapGVK, object(configMapGVK, "child", strconv.Itoa(worker*1000+i)), at(i))
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				s.Managed()
				s.Current(configMapGVK)
				s.SnapshotAt(at(25))
				s.Window(at(0), at(50))
				s.History(key(configMapGVK, "child"))
			}
		}()
	}
	wg.Wait()
}

// The Observer watches one namespace, so a kind and a name identify an object
// (DESIGN.md §5.3).
func TestHistoryOfFillsInTheWatchedNamespace(t *testing.T) {
	s := observe.NewStore(managing(configMapGVK))
	s.Record(widgetGVK, object(widgetGVK, "widget", "10"), at(0))
	s.Record(widgetGVK, object(widgetGVK, "widget", "11"), at(1))
	s.Record(configMapGVK, object(configMapGVK, "widget", "12"), at(2))

	if got := resourceVersions(s.HistoryOf(widgetGVK, "widget")); !slices.Equal(got, []string{"10", "11"}) {
		t.Errorf("HistoryOf holds resourceVersions %v, want the widget's 10 and 11 in order.", got)
	}
}

func TestKindNameLeavesOutTheCoreGroup(t *testing.T) {
	for gvk, want := range map[schema.GroupVersionKind]string{
		configMapGVK: "v1/ConfigMap",
		widgetGVK:    "toy.reconciler-fuzzer/v1/Widget",
	} {
		if got := observe.KindName(gvk); got != want {
			t.Errorf("KindName(%#v) is %q, want %q, as target.yaml writes it.", gvk, got, want)
		}
	}
}

func TestResourceIsWhereTheMapperServesTheKind(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widgetGVK, meta.RESTScopeNamespace)
	s := observe.NewStore(observe.Options{Namespace: namespace, Mapper: mapper})

	if got, want := s.Resource(widgetGVK), (schema.GroupResource{Group: "toy.reconciler-fuzzer", Resource: "widgets"}); got != want {
		t.Errorf("Resource(%v) is %v, want %v.", widgetGVK, got, want)
	}
}

func TestResourceIsEmptyForAKindTheStoreCannotResolve(t *testing.T) {
	for name, mapper := range map[string]meta.RESTMapper{
		"no mapper":                 nil,
		"a mapper without the kind": knowsNothing(),
	} {
		t.Run(name, func(t *testing.T) {
			s := observe.NewStore(observe.Options{Namespace: namespace, Mapper: mapper})

			if got := s.Resource(widgetGVK); !got.Empty() {
				t.Errorf("Resource(%v) is %v, want none.", widgetGVK, got)
			}
		})
	}
}
