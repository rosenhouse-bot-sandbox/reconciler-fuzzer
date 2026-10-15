// Package controller reconciles Widgets.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

// Finalizer names the cleanup path a deleted Widget runs.
const Finalizer = "widget.reconciler-fuzzer/cleanup"

const (
	indexKey = "index"
	labelKey = "label"
)

// NewScheme returns a scheme holding the core Kubernetes types and the Widget API.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, toyv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building the scheme: %w", err)
		}
	}
	return scheme, nil
}

// Reconciler keeps a Widget's ConfigMaps in step with its spec.
type Reconciler struct {
	client.Client
	// APIReader reads past the cache, so that the deletion path never releases a
	// Widget on a stale list of children.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Bug       Bug
	// B1Hold is how long B1 holds its premature status; zero behaves as
	// defaultB1Hold.
	B1Hold time.Duration
	// Resync requeues every Widget this often and writes its status each
	// time, changed or not. Zero turns the timer off.
	Resync time.Duration
	// CleanupDelay is how long a deleted Widget keeps its finalizer before
	// the cleanup begins.
	CleanupDelay time.Duration
	// LabelFrom names a ConfigMap in the Widget's namespace whose data.label
	// each child copies. Empty turns this off.
	LabelFrom string

	// createdFor holds the Widgets this process created a child for. Only B10
	// reads it, and a restart loses it. Reconciles run on one worker (the
	// default MaxConcurrentReconciles), so nothing else touches it.
	createdFor sets.Set[types.NamespacedName]
	// believedPresent holds the children this process has asked the API server
	// for. Only B11 reads it, and a create the API server refused leaves an
	// entry no reconcile revisits.
	believedPresent sets.Set[types.NamespacedName]
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).For(&toyv1.Widget{})
	if r.Bug != B8 {
		builder = builder.Owns(&corev1.ConfigMap{}) // B8: without this watch, a deleted child goes unnoticed.
	}
	if r.Bug != B14 {
		// B14: without this watch, a changed label reaches no child.
		builder = builder.Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.widgetsCopying))
	}
	return builder.WithOptions(r.controllerOptions()).Complete(r)
}

// widgetsCopying names every Widget in the namespace of the ConfigMap
// LabelFrom names.
func (r *Reconciler) widgetsCopying(ctx context.Context, configMap client.Object) []ctrl.Request {
	if configMap.GetName() != r.LabelFrom {
		return nil
	}
	widgets := &toyv1.WidgetList{}
	if err := r.List(ctx, widgets, client.InNamespace(configMap.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, len(widgets.Items))
	for i := range widgets.Items {
		requests[i] = ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&widgets.Items[i])}
	}
	return requests
}

// controllerOptions let a panic end B12's process.
func (r *Reconciler) controllerOptions() controller.Options {
	if r.Bug != B12 {
		return controller.Options{}
	}
	recoverPanic := false
	return controller.Options{RecoverPanic: &recoverPanic}
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	widget := &toyv1.Widget{}
	if err := r.Get(ctx, req.NamespacedName, widget); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !widget.DeletionTimestamp.IsZero() {
		if wait := time.Until(widget.DeletionTimestamp.Add(r.CleanupDelay)); r.CleanupDelay > 0 && wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		return ctrl.Result{}, r.cleanUp(ctx, widget)
	}

	// Patches throughout, so a cached Widget that predates the last write cannot conflict.
	if !controllerutil.ContainsFinalizer(widget, Finalizer) {
		base := widget.DeepCopy()
		controllerutil.AddFinalizer(widget, Finalizer)
		if err := r.Patch(ctx, widget, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	if r.Bug == B1 {
		if err := r.claimChildrenEarly(ctx, widget); err != nil {
			return ctrl.Result{}, err
		}
	}

	ready, err := r.syncChildren(ctx, widget)
	if err != nil {
		return ctrl.Result{}, err
	}

	status, changed := statusFor(widget, ready)
	if r.Bug == B6 {
		// B6: a fresh lastSyncTime on every reconcile.
		syncedAt := metav1.NowMicro()
		status.LastSyncTime = &syncedAt
		return ctrl.Result{}, r.patchStatus(ctx, widget, status)
	}
	if r.Bug == B10 && !r.createdChildFor(widget) {
		return ctrl.Result{}, nil // B10: the status follows a flag a restart lost.
	}
	if changed || r.Resync > 0 {
		if err := r.patchStatus(ctx, widget, status); err != nil {
			return ctrl.Result{}, err
		}
	}
	if r.Bug == B12 {
		// B12: a count of 0 divides by zero.
		log.FromContext(ctx).Info("reconciled", "percentReady", 100*status.Ready/widget.Spec.Count)
	}
	return ctrl.Result{RequeueAfter: r.Resync}, nil
}

// claimChildrenEarly reports the children ready and holds that state, so that a
// checkpoint sees a status no ConfigMap backs (B1).
func (r *Reconciler) claimChildrenEarly(ctx context.Context, widget *toyv1.Widget) error {
	status, changed := statusFor(widget, widget.Spec.Count)
	if !changed {
		return nil
	}
	if err := r.patchStatus(ctx, widget, status); err != nil {
		return err
	}
	select {
	case <-time.After(r.b1Hold()):
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (r *Reconciler) b1Hold() time.Duration {
	if r.B1Hold == 0 {
		return defaultB1Hold
	}
	return r.B1Hold
}

// patchStatus writes the whole status, not the difference from the one the
// Widget carries: a difference omits a field whose value did not change, so a
// Widget created at count 0 would never gain status.ready at all.
func (r *Reconciler) patchStatus(ctx context.Context, widget *toyv1.Widget, status toyv1.WidgetStatus) error {
	widget.Status = status
	// B16: Update instead of Patch requires "update" on widgets/status, which
	// the correct RBAC grants only as "patch".
	if r.Bug == B16 {
		if err := r.Status().Update(ctx, widget); err != nil {
			return fmt.Errorf("writing the status: %w", err)
		}
		return nil
	}
	patch, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return fmt.Errorf("encoding the status: %w", err)
	}
	if err := r.Status().Patch(ctx, widget, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("writing the status: %w", err)
	}
	return nil
}

// desiredChildren returns the ConfigMaps a Widget requires, one per index below
// count, each carrying the label unless it is empty.
func desiredChildren(widget *toyv1.Widget, count int32, label string) []corev1.ConfigMap {
	children := make([]corev1.ConfigMap, count)
	for index := range children {
		children[index] = corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: widget.Namespace,
				Name:      fmt.Sprintf("%s-%d", widget.Name, index),
			},
			Data: map[string]string{indexKey: strconv.Itoa(index)},
		}
		if label != "" {
			children[index].Data[labelKey] = label
		}
	}
	return children
}

// label is the data.label of the ConfigMap LabelFrom names, or empty.
func (r *Reconciler) label(ctx context.Context, namespace string) (string, error) {
	if r.LabelFrom == "" {
		return "", nil
	}
	config := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: r.LabelFrom}, config)
	if err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return config.Data[labelKey], nil
}

// statusFor returns the status a Widget should carry and whether that differs
// from the one it carries now.
func statusFor(widget *toyv1.Widget, ready int32) (toyv1.WidgetStatus, bool) {
	status := toyv1.WidgetStatus{Ready: ready, ObservedGeneration: widget.Generation}
	current := widget.Status
	// Field by field: LastSyncTime is a pointer, so struct equality would
	// compare addresses rather than the times themselves.
	changed := status.Ready != current.Ready ||
		status.ObservedGeneration != current.ObservedGeneration ||
		!status.LastSyncTime.Equal(current.LastSyncTime)
	return status, changed
}

// syncChildren creates the ConfigMaps the spec requires, deletes the ones it
// no longer requires, and returns how many required ones are present. A seeded
// bug may count them differently.
func (r *Reconciler) syncChildren(ctx context.Context, widget *toyv1.Widget) (int32, error) {
	count := widget.Spec.Count
	if r.Bug == B4 {
		count = widget.Status.Ready // B4: the count comes from the status.
	}
	label, err := r.label(ctx, widget.Namespace)
	if err != nil {
		return 0, fmt.Errorf("reading ConfigMap %s: %w", r.LabelFrom, err)
	}
	desired := desiredChildren(widget, count, label)
	if r.Bug == B15 {
		// B15: the children are named after the kind, so two Widgets share them.
		for index := range desired {
			desired[index].Name = fmt.Sprintf("widget-%d", index)
		}
	}
	for index := range desired {
		if err := r.ensureChild(ctx, widget, index, &desired[index]); err != nil {
			return 0, err
		}
	}

	required := sets.New[string]()
	for index := range desired {
		required.Insert(desired[index].Name)
	}

	controlled, err := controlledChildren(ctx, r.Client, widget)
	if err != nil {
		return 0, err
	}
	var ready int32
	for index := range controlled {
		if required.Has(controlled[index].Name) || r.keepsSurplusChildren() {
			ready++
			continue
		}
		if err := r.deleteChild(ctx, &controlled[index]); err != nil {
			return 0, err
		}
	}
	if r.Bug == B3 {
		// B3: the orphan counts by name, so only deletion reveals it.
		return r.presentChildren(ctx, widget, required)
	}
	return ready, nil
}

func (r *Reconciler) presentChildren(ctx context.Context, widget *toyv1.Widget, required sets.Set[string]) (int32, error) {
	configMaps := &corev1.ConfigMapList{}
	if err := r.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return 0, fmt.Errorf("listing ConfigMaps: %w", err)
	}
	var present int32
	for _, configMap := range configMaps.Items {
		if required.Has(configMap.Name) {
			present++
		}
	}
	return present, nil
}

func (r *Reconciler) keepsSurplusChildren() bool {
	// B2 must keep its duplicates, which no name requires; B7 skips the scale-down delete.
	return r.Bug == B2 || r.Bug == B7
}

func (r *Reconciler) ensureChild(ctx context.Context, widget *toyv1.Widget, index int, desired *corev1.ConfigMap) error {
	switch r.Bug {
	case B2:
		return r.createGeneratedChild(ctx, widget, desired)
	case B5:
		// B5: an uncached Get whose NotFound never gives way to a create.
		child := &corev1.ConfigMap{}
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(desired), child); err != nil {
			return fmt.Errorf("reading ConfigMap %s: %w", desired.Name, err)
		}
		return nil
	case B11:
		// B11: the child is believed present from the moment it is
		// asked for, so a create the API server refused is never retried.
		if r.believesPresent(desired) {
			return nil
		}
		r.noteBelievedPresent(desired)
	}

	child := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, Name: desired.Name}}
	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, child, func() error {
		child.Data = desired.Data
		if r.orphans(index) {
			return nil
		}
		return controllerutil.SetControllerReference(widget, child, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring ConfigMap %s: %w", desired.Name, err)
	}
	if r.Bug == B10 && result == controllerutil.OperationResultCreated {
		r.noteCreatedChildFor(widget)
	}
	return nil
}

func (r *Reconciler) orphans(index int) bool {
	// B3 orphans child 0; B9 orphans every child.
	return r.Bug == B9 || (r.Bug == B3 && index == 0)
}

func (r *Reconciler) createGeneratedChild(ctx context.Context, widget *toyv1.Widget, desired *corev1.ConfigMap) error {
	// B2: the child gets a generated name, so no later reconcile recognises it.
	child := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: desired.Namespace, GenerateName: desired.Name + "-"},
		Data:       desired.Data,
	}
	if err := controllerutil.SetControllerReference(widget, child, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, child); err != nil {
		return fmt.Errorf("creating a ConfigMap for %s: %w", desired.Name, err)
	}
	return nil
}

func (r *Reconciler) noteCreatedChildFor(widget *toyv1.Widget) {
	if r.createdFor == nil {
		r.createdFor = sets.New[types.NamespacedName]()
	}
	r.createdFor.Insert(client.ObjectKeyFromObject(widget))
}

func (r *Reconciler) createdChildFor(widget *toyv1.Widget) bool {
	return r.createdFor.Has(client.ObjectKeyFromObject(widget))
}

func (r *Reconciler) noteBelievedPresent(child *corev1.ConfigMap) {
	if r.believedPresent == nil {
		r.believedPresent = sets.New[types.NamespacedName]()
	}
	r.believedPresent.Insert(client.ObjectKeyFromObject(child))
}

func (r *Reconciler) believesPresent(child *corev1.ConfigMap) bool {
	return r.believedPresent.Has(client.ObjectKeyFromObject(child))
}

func controlledChildren(ctx context.Context, reader client.Reader, widget *toyv1.Widget) ([]corev1.ConfigMap, error) {
	configMaps := &corev1.ConfigMapList{}
	if err := reader.List(ctx, configMaps, client.InNamespace(widget.Namespace)); err != nil {
		return nil, fmt.Errorf("listing ConfigMaps: %w", err)
	}
	var controlled []corev1.ConfigMap
	for _, configMap := range configMaps.Items {
		if owner := metav1.GetControllerOf(&configMap); owner != nil && owner.UID == widget.UID {
			controlled = append(controlled, configMap)
		}
	}
	return controlled, nil
}

// cleanUp deletes the children a deleted Widget still controls and releases the
// Widget once none remain.
func (r *Reconciler) cleanUp(ctx context.Context, widget *toyv1.Widget) error {
	switch r.Bug {
	case B9:
		return r.releaseWidget(ctx, widget) // B9: the finalizer goes before the children do.
	case B13:
		return nil // B13: the cleanup never runs.
	}
	controlled, err := controlledChildren(ctx, r.APIReader, widget)
	if err != nil {
		return err
	}
	for index := range controlled {
		if err := r.deleteChild(ctx, &controlled[index]); err != nil {
			return err
		}
	}
	if len(controlled) > 0 {
		return nil // Each deletion reconciles the Widget again.
	}
	return r.releaseWidget(ctx, widget)
}

func (r *Reconciler) releaseWidget(ctx context.Context, widget *toyv1.Widget) error {
	if !controllerutil.ContainsFinalizer(widget, Finalizer) {
		return nil
	}
	base := widget.DeepCopy()
	controllerutil.RemoveFinalizer(widget, Finalizer)
	// NotFound: an earlier reconcile already released the Widget.
	if err := r.Patch(ctx, widget, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("removing the finalizer: %w", err)
	}
	return nil
}

func (r *Reconciler) deleteChild(ctx context.Context, child *corev1.ConfigMap) error {
	if err := r.Delete(ctx, child); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting ConfigMap %s: %w", child.Name, err)
	}
	return nil
}
