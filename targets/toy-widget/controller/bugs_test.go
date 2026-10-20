package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

// fixture wires a Reconciler running one seeded bug to a fake API server.
func fixture(t *testing.T, bug Bug, funcs interceptor.Funcs, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&toyv1.Widget{}).
		WithObjects(objects...).
		WithInterceptorFuncs(funcs).
		Build()
	return &Reconciler{Client: c, APIReader: c, Scheme: scheme, Bug: bug}
}

func reconcile(t *testing.T, r *Reconciler, widget *toyv1.Widget) error {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(widget)})
	return err
}

func mustReconcile(t *testing.T, r *Reconciler, widget *toyv1.Widget) {
	t.Helper()
	if err := reconcile(t, r, widget); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
}

// childNames returns the sorted names of the ConfigMaps in the Widget's namespace.
func childNames(t *testing.T, reader client.Reader, widget *toyv1.Widget) []string {
	t.Helper()
	configMaps := &corev1.ConfigMapList{}
	if err := reader.List(t.Context(), configMaps, client.InNamespace(widget.Namespace)); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(configMaps.Items))
	for _, configMap := range configMaps.Items {
		names = append(names, configMap.Name)
	}
	slices.Sort(names)
	return names
}

func child(t *testing.T, reader client.Reader, widget *toyv1.Widget, name string) *corev1.ConfigMap {
	t.Helper()
	configMap := &corev1.ConfigMap{}
	if err := reader.Get(t.Context(), client.ObjectKey{Namespace: widget.Namespace, Name: name}, configMap); err != nil {
		t.Fatal(err)
	}
	return configMap
}

func readWidget(t *testing.T, reader client.Reader, widget *toyv1.Widget) *toyv1.Widget {
	t.Helper()
	observed := &toyv1.Widget{}
	if err := reader.Get(t.Context(), client.ObjectKeyFromObject(widget), observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func setCount(t *testing.T, r *Reconciler, widget *toyv1.Widget, count int32) {
	t.Helper()
	observed := readWidget(t, r, widget)
	observed.Spec.Count = count
	if err := r.Update(t.Context(), observed); err != nil {
		t.Fatal(err)
	}
}

func TestB1ReportsTheChildrenReadyBeforeItCreatesThem(t *testing.T) {
	widget := newWidget(3)
	readyAtFirstChild := int32(-1)
	watchCreates := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isChild := obj.(*corev1.ConfigMap); isChild && readyAtFirstChild < 0 {
				readyAtFirstChild = readWidget(t, c, widget).Status.Ready
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	r := fixture(t, B1, watchCreates, widget)
	r.B1Hold = 50 * time.Millisecond

	start := time.Now()
	mustReconcile(t, r, widget)

	if readyAtFirstChild != widget.Spec.Count {
		t.Errorf("status.ready was %d when the first child was created, want the whole count %d.",
			readyAtFirstChild, widget.Spec.Count)
	}
	if held := time.Since(start); held < r.B1Hold {
		t.Errorf("Reconcile took %v, want it to hold the premature status for %v.", held, r.B1Hold)
	}
}

func TestB2CreatesAnotherSetOfChildrenOnEveryReconcile(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B2, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)
	mustReconcile(t, r, widget)

	if names := childNames(t, r, widget); len(names) != 4 {
		t.Errorf("Two reconciles left the ConfigMaps %v, want 4 of them for a count of 2.", names)
	}
}

func TestB3OrphansTheFirstChildAndStillReportsItReady(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B3, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)

	if owner := metav1.GetControllerOf(child(t, r, widget, "w-0")); owner != nil {
		t.Errorf("w-0 is controlled by %v, want no ownerReference at all.", owner)
	}
	if owner := metav1.GetControllerOf(child(t, r, widget, "w-1")); owner == nil {
		t.Error("w-1 carries no ownerReference, want the Widget as its controller.")
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != widget.Spec.Count {
		t.Errorf("status.ready is %d, want the count %d: the orphan counts by name.", ready, widget.Spec.Count)
	}
}

func TestB3LeavesTheOrphanBehindWhenTheWidgetGoes(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B3, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	if err := r.Delete(t.Context(), readWidget(t, r, widget)); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, r, widget) // Deletes the children it controls.
	mustReconcile(t, r, widget) // Releases the Widget once none remain.

	if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{}); !apierrors.IsNotFound(err) {
		t.Errorf("Getting the Widget returned %v, want it gone.", err)
	}
	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("Deleting the Widget left the ConfigMaps %v, want the orphan %v.", names, want)
	}
}

func TestB4TakesTheCountFromTheStatus(t *testing.T) {
	t.Run("a fresh Widget never gets a child", func(t *testing.T) {
		widget := newWidget(3)
		r := fixture(t, B4, interceptor.Funcs{}, widget)

		mustReconcile(t, r, widget)

		if names := childNames(t, r, widget); len(names) != 0 {
			t.Errorf("Reconcile left the ConfigMaps %v, want none while status.ready is 0.", names)
		}
	})

	t.Run("a status.ready of 1 gets one child", func(t *testing.T) {
		widget := newWidget(3)
		widget.Status.Ready = 1
		r := fixture(t, B4, interceptor.Funcs{}, widget)

		mustReconcile(t, r, widget)

		if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
			t.Errorf("Reconcile left the ConfigMaps %v, want %v from status.ready.", names, want)
		}
	})
}

func TestB5TreatsAMissingChildAsAnErrorForever(t *testing.T) {
	widget := newWidget(1)
	r := fixture(t, B5, interceptor.Funcs{}, widget)

	for attempt := 1; attempt <= 2; attempt++ {
		if err := reconcile(t, r, widget); !apierrors.IsNotFound(err) {
			t.Errorf("Reconcile %d returned %v, want the NotFound of the missing child.", attempt, err)
		}
	}

	if names := childNames(t, r, widget); len(names) != 0 {
		t.Errorf("Reconcile left the ConfigMaps %v, want none: the Get never gives way to a create.", names)
	}
}

func TestB6WritesTheStatusOnEveryReconcile(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		bug        Bug
		wantWrites int
		wantStamp  bool
	}{
		{"the correct controller writes the status once", 0, 1, false},
		{"B6 writes it again for a fresh lastSyncTime", B6, 2, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			writes := 0
			countStatusWrites := interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object,
					patch client.Patch, opts ...client.SubResourcePatchOption) error {
					writes++
					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			}
			widget := newWidget(1)
			r := fixture(t, testCase.bug, countStatusWrites, widget)

			mustReconcile(t, r, widget)
			mustReconcile(t, r, widget)

			if writes != testCase.wantWrites {
				t.Errorf("Two reconciles wrote the status %d times, want %d.", writes, testCase.wantWrites)
			}
			if stamped := readWidget(t, r, widget).Status.LastSyncTime != nil; stamped != testCase.wantStamp {
				t.Errorf("status.lastSyncTime is set: %t, want %t.", stamped, testCase.wantStamp)
			}
		})
	}
}

func TestB7KeepsTheSurplusChildrenOnScaleDown(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B7, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)

	setCount(t, r, widget, 1)
	mustReconcile(t, r, widget)

	if names, want := childNames(t, r, widget), []string{"w-0", "w-1"}; !slices.Equal(names, want) {
		t.Errorf("Scaling down to 1 left the ConfigMaps %v, want %v.", names, want)
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != 2 {
		t.Errorf("status.ready is %d, want the 2 ConfigMaps that are still there.", ready)
	}
}

func TestB9ReleasesTheWidgetBeforeDeletingItsChildren(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	widget := deletingWidget(1)
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
	if err := controllerutil.SetControllerReference(widget, configMap, scheme); err != nil {
		t.Fatal(err)
	}
	r := fixture(t, B9, interceptor.Funcs{}, widget, configMap)

	mustReconcile(t, r, widget)

	if err := r.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{}); !apierrors.IsNotFound(err) {
		t.Errorf("Getting the Widget returned %v, want it released on the first deletion reconcile.", err)
	}
	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("The deletion reconcile left the ConfigMaps %v, want the abandoned %v.", names, want)
	}
}

func TestB9OrphansEveryChild(t *testing.T) {
	widget := newWidget(2)
	r := fixture(t, B9, interceptor.Funcs{}, widget)

	mustReconcile(t, r, widget)

	for _, name := range []string{"w-0", "w-1"} {
		if owner := metav1.GetControllerOf(child(t, r, widget, name)); owner != nil {
			t.Errorf("%s is controlled by %v, want no ownerReference at all.", name, owner)
		}
	}
}

func TestB10LosesTheStatusOfAWidgetItDidNotCreateChildrenFor(t *testing.T) {
	widget := newWidget(3)
	r := fixture(t, B10, interceptor.Funcs{}, widget)
	mustReconcile(t, r, widget)
	if ready := readWidget(t, r, widget).Status.Ready; ready != 3 {
		t.Fatalf("status.ready is %d after the reconcile that created the children, want 3.", ready)
	}

	restarted := &Reconciler{Client: r.Client, APIReader: r.APIReader, Scheme: r.Scheme, Bug: B10}
	setCount(t, r, widget, 1)
	mustReconcile(t, restarted, widget)

	if names, want := childNames(t, r, widget), []string{"w-0"}; !slices.Equal(names, want) {
		t.Errorf("The restarted reconciler left the ConfigMaps %v, want the converged %v.", names, want)
	}
	if ready := readWidget(t, r, widget).Status.Ready; ready != 3 {
		t.Errorf("status.ready is %d, want it left stale at 3.", ready)
	}
}

func TestB11NeverRetriesAChildCreateTheAPIServerRefused(t *testing.T) {
	for _, testCase := range []struct {
		name string
		bug  Bug
		want []string
	}{
		{"the correct controller creates the child on its next reconcile", 0, []string{"w-0", "w-1"}},
		{"B11 believes in the child the refused create never made", B11, []string{"w-1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			widget := newWidget(2)
			r := fixture(t, testCase.bug, refuseTheFirstChildCreate(), widget)

			if err := reconcile(t, r, widget); err == nil {
				t.Fatal("The reconcile whose create was refused returned no error.")
			}
			mustReconcile(t, r, widget)
			mustReconcile(t, r, widget)

			if names := childNames(t, r, widget); !slices.Equal(names, testCase.want) {
				t.Errorf("Three reconciles left the ConfigMaps %v, want %v.", names, testCase.want)
			}
			if ready, want := readWidget(t, r, widget).Status.Ready, int32(len(testCase.want)); ready != want {
				t.Errorf("status.ready is %d, want the %d the API server holds.", ready, want)
			}
		})
	}
}

// B12 converges a count of 0 and only then divides by it, so every reconcile
// of that spec panics, the first and every one after a restart.
func TestB12DividesByACountOfZeroOnceItHasConverged(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		bug    Bug
		panics bool
	}{
		{"the correct controller", 0, false},
		{"B12", B12, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			widget := newWidget(2)
			r := fixture(t, testCase.bug, interceptor.Funcs{}, widget)
			mustReconcile(t, r, widget)
			setCount(t, r, widget, 0)

			for _, which := range []string{"the reconcile at count 0", "the reconcile after it"} {
				func() {
					defer func() {
						recovered := recover()
						if panicked := strings.Contains(fmt.Sprint(recovered), "integer divide by zero"); panicked != testCase.panics {
							t.Errorf("%s panicked with %v; want it to divide by zero: %t.", which, recovered, testCase.panics)
						}
					}()
					mustReconcile(t, r, widget)
				}()
			}

			if names := childNames(t, r, widget); len(names) != 0 {
				t.Errorf("The reconcile at count 0 left the ConfigMaps %v.", names)
			}
			observed := readWidget(t, r, widget)
			if observed.Status.Ready != 0 || observed.Status.ObservedGeneration != observed.Generation {
				t.Errorf("The reconcile at count 0 left the status %+v of generation %d.", observed.Status, observed.Generation)
			}
		})
	}
}

// B12 lets a panic end the process, where controller-runtime would recover it.
func TestOnlyB12RunsWithoutPanicRecovery(t *testing.T) {
	for _, bug := range []Bug{0, B11, B12} {
		options := (&Reconciler{Bug: bug}).controllerOptions()
		if recovers := options.RecoverPanic == nil || *options.RecoverPanic; recovers != (bug != B12) {
			t.Errorf("Bug %d recovers a panic: %t.", bug, recovers)
		}
	}
}

func TestB13NeverCleansUpADeletedWidget(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		bug         Bug
		wantWidget  bool
		wantConfigs []string
	}{
		{"the correct controller deletes the child and releases the Widget", 0, false, []string{}},
		{"B13 keeps both", B13, true, []string{"w-0"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheme, err := NewScheme()
			if err != nil {
				t.Fatal(err)
			}
			widget := deletingWidget(1)
			configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: widget.Namespace, Name: "w-0"}}
			if err := controllerutil.SetControllerReference(widget, configMap, scheme); err != nil {
				t.Fatal(err)
			}
			r := fixture(t, testCase.bug, interceptor.Funcs{}, widget, configMap)

			mustReconcile(t, r, widget)
			mustReconcile(t, r, widget)

			err = r.Get(t.Context(), client.ObjectKeyFromObject(widget), &toyv1.Widget{})
			if present := err == nil; present != testCase.wantWidget || (err != nil && !apierrors.IsNotFound(err)) {
				t.Errorf("Getting the Widget returned %v, want it present: %t.", err, testCase.wantWidget)
			}
			if names := childNames(t, r, widget); !slices.Equal(names, testCase.wantConfigs) {
				t.Errorf("Two deletion reconciles left the ConfigMaps %v, want %v.", names, testCase.wantConfigs)
			}
		})
	}
}

func TestB15NamesTheChildrenAfterTheKindSoTwoWidgetsFightOverThem(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		bug         Bug
		want        []string
		secondFails bool
	}{
		{"the correct controller names them after each Widget", 0, []string{"w-0", "w2-0"}, false},
		{"B15 names them after the kind", B15, []string{"widget-0"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			first, second := newWidget(1), newWidget(1)
			second.Name, second.UID = "w2", "second-uid"
			r := fixture(t, testCase.bug, interceptor.Funcs{}, first, second)

			mustReconcile(t, r, first)
			err := reconcile(t, r, second)

			if failed := err != nil; failed != testCase.secondFails {
				t.Errorf("Reconciling the second Widget returned %v, want an error: %t.", err, testCase.secondFails)
			}
			if names := childNames(t, r, first); !slices.Equal(names, testCase.want) {
				t.Errorf("The reconciles left the ConfigMaps %v, want %v.", names, testCase.want)
			}
		})
	}
}

// B16 uses Status().Update() instead of Status().Patch(). The fake client
// exercises both paths; a real run under RBAC sees the difference as a 403 on
// widgets/status because the role grants patch, not update.
func TestB16UsesStatusUpdateInsteadOfPatch(t *testing.T) {
	var patched, updated bool
	funcs := interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			patched = true
			return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			updated = true
			return c.SubResource(subResourceName).Update(ctx, obj, opts...)
		},
	}
	widget := newWidget(1)
	r := fixture(t, B16, funcs, widget)
	mustReconcile(t, r, widget)

	if patched {
		t.Error("B16 patched the status, want it to use Update.")
	}
	if !updated {
		t.Error("B16 did not update the status.")
	}

	// The correct controller patches.
	patched, updated = false, false
	correct := fixture(t, 0, funcs, newWidget(1))
	mustReconcile(t, correct, newWidget(1))
	if !patched {
		t.Error("The correct controller did not patch the status.")
	}
	if updated {
		t.Error("The correct controller updated the status, want Patch.")
	}
}

// refuseTheFirstChildCreate is the fault plans/b11-fault.json injects,
// as the fake API server: one refused ConfigMap create and no more.
func refuseTheFirstChildCreate() interceptor.Funcs {
	refused := false
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, isChild := obj.(*corev1.ConfigMap); isChild && !refused {
				refused = true
				return apierrors.NewInternalError(errors.New("reconciler-fuzzer fault"))
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}
