package controller

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

func newWidget(count int32) *toyv1.Widget {
	return &toyv1.Widget{
		ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "ns", Generation: 1, UID: "widget-uid"},
		Spec:       toyv1.WidgetSpec{Count: count},
	}
}

func deletingWidget(count int32) *toyv1.Widget {
	widget := newWidget(count)
	widget.Finalizers = []string{Finalizer}
	deletedAt := metav1.Now()
	widget.DeletionTimestamp = &deletedAt
	return widget
}

func TestDesiredChildrenNamesAndIndexesOnePerCount(t *testing.T) {
	children := desiredChildren(newWidget(3), 3, "")

	want := []corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-0"}, Data: map[string]string{"index": "0"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-1"}, Data: map[string]string{"index": "1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "w-2"}, Data: map[string]string{"index": "2"}},
	}
	if !reflect.DeepEqual(children, want) {
		t.Errorf("desiredChildren returned %v, want %v", children, want)
	}
}

func TestDesiredChildrenFollowsTheCountItIsGiven(t *testing.T) {
	if children := desiredChildren(newWidget(3), 0, ""); len(children) != 0 {
		t.Errorf("desiredChildren returned %v for count 0.", children)
	}
	if children := desiredChildren(newWidget(0), 2, ""); len(children) != 2 {
		t.Errorf("desiredChildren returned %v for count 2.", children)
	}
}

func TestStatusForCarriesTheReadyCountAndTheGeneration(t *testing.T) {
	next, changed := statusFor(newWidget(3), 2)

	if want := (toyv1.WidgetStatus{Ready: 2, ObservedGeneration: 1}); next != want {
		t.Errorf("statusFor returned %+v, want %+v", next, want)
	}
	if !changed {
		t.Error("statusFor reported no change although the status was empty.")
	}
}

func TestStatusForReportsAChangeOnlyWhenOneIsNeeded(t *testing.T) {
	widget := newWidget(3)
	widget.Status = toyv1.WidgetStatus{Ready: 3, ObservedGeneration: 1}

	if _, changed := statusFor(widget, 3); changed {
		t.Error("statusFor asked for a write although the status already matched.")
	}
	if _, changed := statusFor(widget, 2); !changed {
		t.Error("statusFor kept a stale child count.")
	}

	widget.Generation = 2
	if _, changed := statusFor(widget, 3); !changed {
		t.Error("statusFor kept a stale observedGeneration.")
	}
}

func labelConfig(label string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "config"}, Data: map[string]string{"label": label}}
}

func TestEachChildCopiesTheLabelOfTheConfigMapLabelFromNames(t *testing.T) {
	widget := newWidget(1)
	config := labelConfig("red")
	r := fixture(t, 0, interceptor.Funcs{}, widget, config)
	r.LabelFrom = "config"
	mustReconcile(t, r, widget)

	config.Data["label"] = "blue"
	if err := r.Update(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, widget)

	if data, want := child(t, r, widget, "w-0").Data, map[string]string{"index": "0", "label": "blue"}; !maps.Equal(data, want) {
		t.Errorf("w-0 holds %v, want %v.", data, want)
	}
}

func TestAChildCopiesNoLabelWhereTheConfigMapIsMissing(t *testing.T) {
	for _, labelFrom := range []string{"", "config"} {
		widget := newWidget(1)
		r := fixture(t, 0, interceptor.Funcs{}, widget)
		r.LabelFrom = labelFrom

		mustReconcile(t, r, widget)

		if data, want := child(t, r, widget, "w-0").Data, map[string]string{"index": "0"}; !maps.Equal(data, want) {
			t.Errorf("With LabelFrom %q, w-0 holds %v, want %v.", labelFrom, data, want)
		}
	}
}

func TestAFailedReadOfTheLabelFailsTheReconcile(t *testing.T) {
	refused := errors.New("the API server refused the read")
	refuseTheConfig := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if key.Name == "config" {
				return refused
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	widget := newWidget(1)
	r := fixture(t, 0, refuseTheConfig, widget, labelConfig("red"))
	r.LabelFrom = "config"

	if err := reconcile(t, r, widget); !errors.Is(err, refused) {
		t.Errorf("Reconcile returned %v, want the refused read.", err)
	}
}

// The watch on the ConfigMap wakes every Widget that copies its label.
func TestAChangeToTheConfigMapReconcilesEveryWidgetInItsNamespace(t *testing.T) {
	other := newWidget(1)
	other.Name, other.UID = "v", "other-uid"
	elsewhere := newWidget(1)
	elsewhere.Namespace = "elsewhere"
	r := fixture(t, 0, interceptor.Funcs{}, newWidget(1), other, elsewhere)
	r.LabelFrom = "config"

	for _, test := range []struct {
		namespace, name string
		want            []string
	}{
		{"ns", "config", []string{"ns/v", "ns/w"}},
		{"elsewhere", "config", []string{"elsewhere/w"}},
		{"ns", "unrelated", nil},
	} {
		changed := labelConfig("blue")
		changed.Namespace, changed.Name = test.namespace, test.name

		var woken []string
		for _, request := range r.widgetsCopying(t.Context(), changed) {
			woken = append(woken, request.String())
		}

		slices.Sort(woken)
		if !slices.Equal(woken, test.want) {
			t.Errorf("A change to the ConfigMap %s reconciles %v, want %v.", test.name, woken, test.want)
		}
	}
}

func TestAListThatFailsWakesNoWidget(t *testing.T) {
	failsWithAWidget := interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
			list.(*toyv1.WidgetList).Items = []toyv1.Widget{*newWidget(1)}
			return errors.New("the API server cut the list short")
		},
	}
	r := fixture(t, 0, failsWithAWidget)
	r.LabelFrom = "config"

	if woken := r.widgetsCopying(t.Context(), labelConfig("blue")); len(woken) != 0 {
		t.Errorf("A failed list reconciles %v, want none.", woken)
	}
}

// A controller that resyncs on a timer requeues itself and writes its status
// on every tick, changed or not. Without the timer, a converged Widget costs
// nothing.
func TestResyncRequeuesAndWritesTheStatusEachTime(t *testing.T) {
	for _, tc := range []struct {
		resync       time.Duration
		statusWrites int
	}{
		{resync: 0, statusWrites: 0},
		{resync: 15 * time.Second, statusWrites: 1},
	} {
		t.Run(tc.resync.String(), func(t *testing.T) {
			converged := newWidget(0)
			converged.Finalizers = []string{Finalizer}
			converged.Status = toyv1.WidgetStatus{Ready: 0, ObservedGeneration: 1}
			statusWrites := 0
			countStatusWrites := interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					statusWrites++
					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			}
			r := fixture(t, 0, countStatusWrites, converged)
			r.Resync = tc.resync

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(converged)})

			if err != nil {
				t.Fatalf("Reconcile returned an error: %v", err)
			}
			if result.RequeueAfter != tc.resync {
				t.Errorf("Reconcile asked to requeue after %v, want %v.", result.RequeueAfter, tc.resync)
			}
			if statusWrites != tc.statusWrites {
				t.Errorf("Reconcile wrote the status %d times, want %d.", statusWrites, tc.statusWrites)
			}
		})
	}
}

// TestCleanUpFindsChildrenTheCacheHasMissed gives the Reconciler a cache that
// has not seen the child yet, so only a read through APIReader holds the Widget
// back.
func TestCleanUpFindsChildrenTheCacheHasMissed(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(1)
	child := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
	if err := controllerutil.SetControllerReference(widget, child, scheme); err != nil {
		t.Fatal(err)
	}
	cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget.DeepCopy()).Build()
	apiServer := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget.DeepCopy(), child).Build()
	reconciler := &Reconciler{Client: cache, APIReader: apiServer, Scheme: scheme}
	deleting := &toyv1.Widget{}
	if err := cache.Get(t.Context(), client.ObjectKeyFromObject(widget), deleting); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.cleanUp(t.Context(), deleting); err != nil {
		t.Fatalf("cleanUp returned an error: %v", err)
	}

	if !slices.Contains(deleting.Finalizers, Finalizer) {
		t.Errorf("cleanUp left the finalizers %v, want it to hold %s until the child is gone.",
			deleting.Finalizers, Finalizer)
	}
}

func TestCleanUpWaitsOutTheCleanupDelay(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		delay       time.Duration
		deletedAgo  time.Duration
		wantRequeue time.Duration
		wantConfigs []string
	}{
		{"within the delay", time.Hour, time.Minute, 59 * time.Minute, []string{"w-0"}},
		{"past the delay", time.Hour, 2 * time.Hour, 0, []string{}},
		{"no delay, and an API server clock ahead of the controller's", 0, -time.Minute, 0, []string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheme, err := NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			widget := deletingWidget(1)
			widget.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-testCase.deletedAgo)}
			configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
			if err := controllerutil.SetControllerReference(widget, configMap, scheme); err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget, configMap).Build()
			r := &Reconciler{Client: c, APIReader: c, Scheme: scheme, CleanupDelay: testCase.delay}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(widget)})

			if err != nil {
				t.Fatalf("Reconcile returned an error: %v", err)
			}
			if requeue := result.RequeueAfter; requeue > testCase.wantRequeue || requeue < testCase.wantRequeue-time.Minute {
				t.Errorf("Reconcile asked to requeue after %v, want about %v.", requeue, testCase.wantRequeue)
			}
			configMaps := &corev1.ConfigMapList{}
			if err := c.List(t.Context(), configMaps); err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, item := range configMaps.Items {
				names = append(names, item.Name)
			}
			if !slices.Equal(names, testCase.wantConfigs) {
				t.Errorf("Reconcile left the ConfigMaps %v, want %v.", names, testCase.wantConfigs)
			}
		})
	}
}

// TestCleanUpIgnoresAWidgetThatIsAlreadyGone covers the reconcile that follows
// the last child's deletion, holding a Widget the API server has removed.
func TestCleanUpIgnoresAWidgetThatIsAlreadyGone(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(0)
	collected := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &Reconciler{Client: collected, APIReader: collected, Scheme: scheme}

	if err := reconciler.cleanUp(t.Context(), widget); err != nil {
		t.Errorf("cleanUp returned an error for a Widget the collector had taken: %v", err)
	}
}

// A refused status write fails the reconcile, so controller-runtime retries it.
func TestReconcileReturnsARefusedStatusWrite(t *testing.T) {
	refused := errors.New("the API server refused the status write")
	refuseStatus := interceptor.Funcs{
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return refused
		},
	}
	widget := newWidget(1)
	r := fixture(t, 0, refuseStatus, widget)

	if err := reconcile(t, r, widget); !errors.Is(err, refused) {
		t.Errorf("Reconcile returned %v, want the refused status write.", err)
	}
}
