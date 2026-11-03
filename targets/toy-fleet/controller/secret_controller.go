package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
)

// SecretReconciler runs under --protect-secrets. It holds a Secret that a
// Database names with SecretInUseFinalizer, as kubernetes.io/pvc-protection
// holds a claim that a Pod uses, and releases it once no Database names it.
type SecretReconciler struct{ reconciler }

func (r *SecretReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		For(&corev1.Secret{}).
		Watches(&fleetv1.Database{}, handler.EnqueueRequestsFromMapFunc(secretOf)).
		Complete(r)
}

// secretOf maps a Database to the Secret it names. An update maps the old
// Database too, so a Secret it stops naming is released.
func secretOf(_ context.Context, db client.Object) []reconcile.Request {
	var requests []reconcile.Request
	for _, name := range passwordSecretRef(db) {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: db.GetNamespace(), Name: name}})
	}
	return requests
}

func (r *SecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, req.NamespacedName, secret); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	named, err := r.named(ctx, r.Client, secret)
	if err == nil && !named && controllerutil.ContainsFinalizer(secret, fleetv1.SecretInUseFinalizer) {
		named, err = r.named(ctx, r.APIReader, secret) // The cache can miss a new Database.
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	changed := false
	switch {
	case !named:
		changed = controllerutil.RemoveFinalizer(secret, fleetv1.SecretInUseFinalizer)
	case secret.DeletionTimestamp.IsZero() && r.Bug != B126:
		changed = controllerutil.AddFinalizer(secret, fleetv1.SecretInUseFinalizer)
	}
	if !changed {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, secret))
}

// named says whether a Database that reader lists names the Secret. A
// deleting Database still names it.
func (r *SecretReconciler) named(ctx context.Context, reader client.Reader, secret *corev1.Secret) (bool, error) {
	databases := &fleetv1.DatabaseList{}
	if err := reader.List(ctx, databases, client.InNamespace(secret.Namespace)); err != nil && !r.unserved(err) {
		return false, fmt.Errorf("listing Databases: %w", err)
	}
	return slices.ContainsFunc(databases.Items, func(db fleetv1.Database) bool {
		return slices.Contains(passwordSecretRef(&db), secret.Name)
	}), nil
}
