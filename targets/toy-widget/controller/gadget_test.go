package controller

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

// gadgetFixture is fixture with --gadget.
func gadgetFixture(t *testing.T, bug Bug, funcs interceptor.Funcs, objects ...client.Object) *Reconciler {
	t.Helper()
	r := fixture(t, bug, funcs, objects...)
	r.Gadget = true
	return r
}

func readGadget(t *testing.T, reader client.Reader, widget *toyv1.Widget) *toyv1.Gadget {
	t.Helper()
	gadget := &toyv1.Gadget{}
	if err := reader.Get(t.Context(), client.ObjectKeyFromObject(widget), gadget); err != nil {
		t.Fatal(err)
	}
	return gadget
}

func gadgetGone(t *testing.T, reader client.Reader, widget *toyv1.Widget) bool {
	t.Helper()
	err := reader.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Gadget{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err != nil
}

// setGadgetReady plays the Gadget's controller: it sets the Ready condition
// for the Gadget's current generation.
func setGadgetReady(t *testing.T, c client.Client, widget *toyv1.Widget, status metav1.ConditionStatus) {
	t.Helper()
	gadget := readGadget(t, c, widget)
	meta.SetStatusCondition(&gadget.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: status, Reason: "Test", ObservedGeneration: gadget.Generation,
	})
	if err := c.Status().Update(t.Context(), gadget); err != nil {
		t.Fatal(err)
	}
}

func gadgetReadyOf(t *testing.T, reader client.Reader, widget *toyv1.Widget) *bool {
	t.Helper()
	return readWidget(t, reader, widget).Status.GadgetReady
}

func TestEachWidgetOwnsAGadgetNamedAfterItSizedByItsCount(t *testing.T) {
	widget := newWidget(2)
	r := gadgetFixture(t, 0, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	gadget := readGadget(t, r, widget)
	if gadget.Spec.Size != 2 {
		t.Errorf("The Gadget has size %d, want the count 2.", gadget.Spec.Size)
	}
	if owner := metav1.GetControllerOf(gadget); owner == nil || owner.UID != widget.UID {
		t.Errorf("The Gadget is controlled by %v, want the Widget.", owner)
	}

	setCount(t, r, widget, 3)
	mustReconcile(t, r, widget)
	if size := readGadget(t, r, widget).Spec.Size; size != 3 {
		t.Errorf("The Gadget has size %d after the count changed to 3.", size)
	}
}

func TestGadgetReadyFollowsTheGadgetsReadyCondition(t *testing.T) {
	widget := newWidget(1)
	r := gadgetFixture(t, 0, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)
	if ready := gadgetReadyOf(t, r, widget); ready == nil || *ready {
		t.Errorf("gadgetReady is %v for a Gadget with no condition, want false.", ready)
	}

	setGadgetReady(t, r.Client, widget, metav1.ConditionTrue)
	mustReconcile(t, r, widget)
	if ready := gadgetReadyOf(t, r, widget); ready == nil || !*ready {
		t.Errorf("gadgetReady is %v for a ready Gadget, want true.", ready)
	}

	setGadgetReady(t, r.Client, widget, metav1.ConditionFalse)
	mustReconcile(t, r, widget)
	if ready := gadgetReadyOf(t, r, widget); ready == nil || *ready {
		t.Errorf("gadgetReady is %v for a Gadget that is not ready, want false.", ready)
	}
}

func TestGadgetIsReadyOnlyForTheCurrentGeneration(t *testing.T) {
	gadget := &toyv1.Gadget{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	for _, testCase := range []struct {
		status   metav1.ConditionStatus
		observed int64
		want     bool
	}{
		{metav1.ConditionTrue, 2, true},
		{metav1.ConditionTrue, 1, false},
		{metav1.ConditionFalse, 2, false},
	} {
		gadget.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: testCase.status, ObservedGeneration: testCase.observed}}
		if ready := gadgetIsReady(gadget); ready != testCase.want {
			t.Errorf("A Gadget of generation 2 with Ready %s for generation %d is ready: %t, want %t.",
				testCase.status, testCase.observed, ready, testCase.want)
		}
	}
	gadget.Status.Conditions = nil
	if gadgetIsReady(gadget) {
		t.Error("A Gadget with no condition is ready.")
	}
}

func TestStatusForReportsAChangedGadgetReady(t *testing.T) {
	widget := newWidget(1)
	widget.Status = toyv1.WidgetStatus{Ready: 1, ObservedGeneration: 1, GadgetReady: ptr.To(false)}

	if _, changed := statusFor(widget, 1, ptr.To(false)); changed {
		t.Error("statusFor asked for a write although gadgetReady already matched.")
	}
	if status, changed := statusFor(widget, 1, ptr.To(true)); !changed || !*status.GadgetReady {
		t.Errorf("statusFor returned %+v, changed %t, for a Gadget that became ready.", status, changed)
	}
}

func TestWithoutGadgetTheWidgetHasNoGadget(t *testing.T) {
	widget := newWidget(1)
	r := fixture(t, 0, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	if !gadgetGone(t, r, widget) {
		t.Error("A Gadget exists without --gadget.")
	}
	if ready := gadgetReadyOf(t, r, widget); ready != nil {
		t.Errorf("gadgetReady is %v without --gadget, want it unset.", *ready)
	}
}

// heldGadget is a Gadget of widget that another finalizer holds, as a
// slowDelete fault does.
func heldGadget(t *testing.T, widget *toyv1.Widget) *toyv1.Gadget {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	gadget := &toyv1.Gadget{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: widget.Name, Finalizers: []string{"test/hold"}}}
	if err := controllerutil.SetControllerReference(widget, gadget, scheme); err != nil {
		t.Fatal(err)
	}
	return gadget
}

// deletingGadget is a held Gadget that another client deleted, as the garbage
// collector does in a Foreground deletion of its Widget.
func deletingGadget(t *testing.T, widget *toyv1.Widget) *toyv1.Gadget {
	t.Helper()
	gadget := heldGadget(t, widget)
	deletedAt := metav1.Now()
	gadget.DeletionTimestamp = &deletedAt
	return gadget
}

func release(t *testing.T, c client.Client, widget *toyv1.Widget) {
	t.Helper()
	gadget := readGadget(t, c, widget)
	gadget.Finalizers = nil
	if err := c.Update(t.Context(), gadget); err != nil {
		t.Fatal(err)
	}
}

func widgetGone(t *testing.T, reader client.Reader, widget *toyv1.Widget) bool {
	t.Helper()
	err := reader.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err != nil
}

func TestCleanUpKeepsTheFinalizerUntilTheGadgetIsGone(t *testing.T) {
	widget := deletingWidget(0)
	r := gadgetFixture(t, 0, interceptor.Funcs{}, widget, heldGadget(t, widget))

	mustReconcile(t, r, widget)
	if readGadget(t, r, widget).DeletionTimestamp.IsZero() {
		t.Error("The cleanup did not delete the Gadget.")
	}
	if widgetGone(t, r, widget) {
		t.Fatal("The cleanup released the Widget while its Gadget remained.")
	}

	release(t, r.Client, widget)
	mustReconcile(t, r, widget)
	if !widgetGone(t, r, widget) {
		t.Error("The cleanup kept the Widget after its Gadget was gone.")
	}
}
