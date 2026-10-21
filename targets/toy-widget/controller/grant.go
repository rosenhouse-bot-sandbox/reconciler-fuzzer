package controller

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

const (
	// GrantFinalizer names the cleanup path a deleted Grant runs.
	GrantFinalizer = "grant.reconciler-fuzzer/cleanup"
	// GrantUIDLabel holds a Grant's UID on its children.
	GrantUIDLabel = "toy.reconciler-fuzzer/grant-uid"
	// GrantConfig names the ConfigMap in the mirror namespace whose
	// data.label each Grant's ConfigMap copies.
	GrantConfig = "grant-config"
)

// GrantReconciler gives each Grant a ClusterRole and a ConfigMap in
// MirrorNamespace, and deletes them through a finalizer.
type GrantReconciler struct {
	client.Client
	// APIReader reads past the cache, so the finalizer never goes on a stale
	// read of the children.
	APIReader       client.Reader
	Scheme          *runtime.Scheme
	Bug             Bug
	MirrorNamespace string
}

func (r *GrantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(r.grantsFor)
	return ctrl.NewControllerManagedBy(mgr).
		For(&toyv1.Grant{}).
		Watches(&rbacv1.ClusterRole{}, enqueue).
		Watches(&corev1.ConfigMap{}, enqueue).
		Complete(r)
}

// grantsFor names the Grant whose UID object's label holds, or every Grant
// for grant-config.
func (r *GrantReconciler) grantsFor(ctx context.Context, object client.Object) []ctrl.Request {
	uid := object.GetLabels()[GrantUIDLabel]
	isConfig := object.GetNamespace() == r.MirrorNamespace && object.GetName() == GrantConfig
	grants := &toyv1.GrantList{}
	if err := r.List(ctx, grants); err != nil {
		return nil
	}
	var requests []ctrl.Request
	for i := range grants.Items {
		if isConfig || string(grants.Items[i].UID) == uid {
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&grants.Items[i])})
		}
	}
	return requests
}

func (r *GrantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	grant := &toyv1.Grant{}
	if err := r.Get(ctx, req.NamespacedName, grant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !grant.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanUp(ctx, grant)
	}
	if !controllerutil.ContainsFinalizer(grant, GrantFinalizer) {
		base := grant.DeepCopy()
		controllerutil.AddFinalizer(grant, GrantFinalizer)
		if err := r.Patch(ctx, grant, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding the finalizer: %w", err)
		}
	}

	labels := map[string]string{GrantUIDLabel: string(grant.UID)}
	clusterRole := r.clusterRoleOf(grant)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, clusterRole, func() error {
		clusterRole.Labels = labels
		clusterRole.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{toyv1.GroupVersion.Group}, Resources: []string{"grants"},
			ResourceNames: []string{grant.Name}, Verbs: []string{"get"},
		}}
		if r.Bug == B28 {
			// B28: a namespaced owner, which the garbage collector ignores
			// for a cluster-scoped object.
			clusterRole.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(grant, toyv1.GroupVersion.WithKind("Grant"))}
		}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ClusterRole %s: %w", clusterRole.Name, err)
	}

	config := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.MirrorNamespace, Name: GrantConfig}, config); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("reading ConfigMap %s: %w", GrantConfig, err)
	}
	configMap := r.configMapOf(grant)
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		configMap.Labels = labels
		configMap.Data = map[string]string{labelKey: config.Data[labelKey], "level": strconv.Itoa(int(grant.Spec.Level))}
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ConfigMap %s: %w", configMap.Name, err)
	}

	status := toyv1.ReadyStatus{ObservedGeneration: grant.Generation, Ready: true}
	return ctrl.Result{}, patchReadyStatus(ctx, r.Client, grant, &grant.Status, status)
}

func (r *GrantReconciler) clusterRoleOf(grant *toyv1.Grant) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("grant-%s-%s", grant.Namespace, grant.Name)}}
}

func (r *GrantReconciler) configMapOf(grant *toyv1.Grant) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: r.MirrorNamespace, Name: grant.Namespace + "." + grant.Name}}
}

// cleanUp deletes a deleted Grant's children and releases it once an
// uncached read finds them gone.
func (r *GrantReconciler) cleanUp(ctx context.Context, grant *toyv1.Grant) error {
	var children []client.Object
	if r.Bug != B28 { // B28 leaves the ClusterRole to the garbage collector.
		children = append(children, r.clusterRoleOf(grant))
	}
	if r.Bug != B27 { // B27 forgets the ConfigMap in the other namespace.
		children = append(children, r.configMapOf(grant))
	}
	for _, child := range children {
		if err := r.Delete(ctx, child); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting %T %s: %w", child, child.GetName(), err)
		}
	}
	for _, child := range children {
		err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(child), child)
		if err == nil {
			return nil // Its deletion reconciles the Grant again.
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("reading %T %s: %w", child, child.GetName(), err)
		}
	}
	if !controllerutil.ContainsFinalizer(grant, GrantFinalizer) {
		return nil
	}
	base := grant.DeepCopy()
	controllerutil.RemoveFinalizer(grant, GrantFinalizer)
	if err := r.Patch(ctx, grant, client.MergeFrom(base)); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("removing the finalizer: %w", err)
	}
	return nil
}
