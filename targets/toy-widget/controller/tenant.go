package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

const (
	// TenantBase names the ClusterRole whose rules each Tenant's ClusterRole copies.
	TenantBase = "tenant-base"
	// QuotaLabel holds a Tenant's spec.quota on its children.
	QuotaLabel = "toy.reconciler-fuzzer/quota"
	// ChildFinalizer holds a Tenant's children until its controller releases them.
	ChildFinalizer = "tenant.reconciler-fuzzer/child"
)

// TenantReconciler gives each Tenant a Namespace and a ClusterRole.
type TenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Bug    Bug
}

func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&toyv1.Tenant{}).
		Owns(&corev1.Namespace{}).
		Owns(&rbacv1.ClusterRole{}).
		Watches(&rbacv1.ClusterRole{}, handler.EnqueueRequestsFromMapFunc(r.tenantsUsing)).
		Complete(r)
}

// tenantsUsing names every Tenant when clusterRole is tenant-base.
func (r *TenantReconciler) tenantsUsing(ctx context.Context, clusterRole client.Object) []ctrl.Request {
	if clusterRole.GetName() != TenantBase {
		return nil
	}
	tenants := &toyv1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, len(tenants.Items))
	for i := range tenants.Items {
		requests[i] = ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&tenants.Items[i])}
	}
	return requests
}

func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tenant := &toyv1.Tenant{}
	err := r.Get(ctx, req.NamespacedName, tenant)
	switch {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, r.release(ctx, childName(req.Name))
	case err != nil:
		return ctrl.Result{}, err
	case !tenant.DeletionTimestamp.IsZero() && r.Bug == B29:
		return ctrl.Result{}, nil // B29: a Tenant in Foreground deletion never releases its children.
	case !tenant.DeletionTimestamp.IsZero():
		return ctrl.Result{}, r.release(ctx, childName(tenant.Name))
	}

	name := childName(tenant.Name)
	labels := map[string]string{QuotaLabel: strconv.Itoa(int(tenant.Spec.Quota))}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, namespace, func() error {
		namespace.Labels = labels
		r.holdChild(namespace)
		return controllerutil.SetControllerReference(tenant, namespace, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring Namespace %s: %w", name, err)
	}

	base := &rbacv1.ClusterRole{}
	if err := r.Get(ctx, client.ObjectKey{Name: TenantBase}, base); client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, fmt.Errorf("reading ClusterRole %s: %w", TenantBase, err)
	}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, clusterRole, func() error {
		clusterRole.Labels = labels
		clusterRole.Rules = base.Rules
		r.holdChild(clusterRole)
		if r.Bug == B26 {
			return nil // B26: the ClusterRole has no owner, so it outlives the Tenant.
		}
		return controllerutil.SetControllerReference(tenant, clusterRole, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ClusterRole %s: %w", name, err)
	}

	status := toyv1.ReadyStatus{ObservedGeneration: tenant.Generation, Ready: namespace.DeletionTimestamp.IsZero()}
	return ctrl.Result{}, patchReadyStatus(ctx, r.Client, tenant, &tenant.Status, status)
}

func childName(tenant string) string { return "tenant-" + tenant }

// holdChild gives a live child ChildFinalizer, and releases one being
// deleted.
func (r *TenantReconciler) holdChild(child client.Object) {
	if child.GetDeletionTimestamp().IsZero() {
		controllerutil.AddFinalizer(child, ChildFinalizer)
	} else {
		controllerutil.RemoveFinalizer(child, ChildFinalizer)
	}
}

// release removes ChildFinalizer from the children named name.
func (r *TenantReconciler) release(ctx context.Context, name string) error {
	for _, child := range []client.Object{&corev1.Namespace{}, &rbacv1.ClusterRole{}} {
		err := r.Get(ctx, client.ObjectKey{Name: name}, child)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading %T %s: %w", child, name, err)
		}
		if !controllerutil.RemoveFinalizer(child, ChildFinalizer) {
			continue
		}
		if err := r.Update(ctx, child); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("releasing %T %s: %w", child, name, err)
		}
	}
	return nil
}

// patchReadyStatus writes status to object, whose status current points at,
// unless it already holds it.
func patchReadyStatus(ctx context.Context, c client.Client, object client.Object, current *toyv1.ReadyStatus, status toyv1.ReadyStatus) error {
	if *current == status {
		return nil
	}
	*current = status
	patch, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return fmt.Errorf("encoding the status: %w", err)
	}
	if err := c.Status().Patch(ctx, object, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("writing the status: %w", err)
	}
	return nil
}
