package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
)

// DatabaseReconciler readies a Database on its Cluster and on the Secret it
// may name, and holds its deletion while a User names it.
type DatabaseReconciler struct{ reconciler }

func (r *DatabaseReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		For(&fleetv1.Database{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&fleetv1.Cluster{}, handler.EnqueueRequestsFromMapFunc(r.databasesNaming(clusterRefField))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.databasesNaming(passwordSecretRefField))).
		Watches(&fleetv1.User{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.(*fleetv1.User).Spec.DatabaseRef}}}
		})).
		Complete(r)
}

// databasesNaming maps an object to the Databases in its namespace whose
// indexed field names it.
func (r *DatabaseReconciler) databasesNaming(field string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &fleetv1.DatabaseList{}
		if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{field: o.GetName()}); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(list.Items))
		for _, db := range list.Items {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&db)})
		}
		return requests
	}
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	db := &fleetv1.Database{}
	if err := r.Get(ctx, req.NamespacedName, db); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !db.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, db)
	}
	if controllerutil.AddFinalizer(db, fleetv1.DatabaseFinalizer) {
		return ctrl.Result{}, r.Update(ctx, db)
	}
	old := db.Status
	db.Status = fleetv1.CommonStatus{Phase: fleetv1.Pending, ObservedGeneration: db.Generation}
	cluster := &fleetv1.Cluster{}
	clusterErr := r.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: db.Spec.ClusterRef}, cluster)
	if client.IgnoreNotFound(clusterErr) != nil {
		return ctrl.Result{}, clusterErr
	}
	secretWait, err := r.secretWait(ctx, db)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case clusterErr != nil:
		db.Status.Message = fmt.Sprintf("waiting for Cluster %s", db.Spec.ClusterRef)
	case secretWait != "" && r.Bug != B124 && (r.Bug != B125 || old.Phase != fleetv1.Ready):
		db.Status.Message = secretWait
	case !cluster.DeletionTimestamp.IsZero() && old.Phase == fleetv1.Ready:
		db.Status = old // A deleting Cluster keeps its Ready Databases as they are.
	case !cluster.Status.ReadyAt(cluster.Generation) && r.Bug != B123:
		db.Status.Message = fmt.Sprintf("waiting for Cluster %s to be Ready", cluster.Name)
	default:
		if err := r.syncConfigMap(ctx, db); err != nil {
			return ctrl.Result{}, err
		}
		db.Status.Phase = fleetv1.Ready
	}
	return ctrl.Result{}, r.writeStatus(ctx, db, old, db.Status)
}

// secretWait says what the Database waits for in the Secret it names, or ""
// when it names none or the Secret holds a password.
func (r *DatabaseReconciler) secretWait(ctx context.Context, db *fleetv1.Database) (string, error) {
	ref := db.Spec.PasswordSecretRef
	if ref == nil || ref.Name == "" {
		return "", nil
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: ref.Name}, secret)
	switch _, hasPassword := secret.Data["password"]; {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("waiting for Secret %s", ref.Name), nil
	case err != nil:
		return "", err
	case !hasPassword:
		return fmt.Sprintf("waiting for Secret %s to hold a password", ref.Name), nil
	}
	return "", nil
}

func (r *DatabaseReconciler) syncConfigMap(ctx context.Context, db *fleetv1.Database) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: db.Namespace, Name: db.Name + "-db"}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{"cluster": db.Spec.ClusterRef, "engine": db.Spec.Engine}
		return controllerutil.SetControllerReference(db, cm, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring ConfigMap %s: %w", cm.Name, err)
	}
	return nil
}

// finalize removes the Database's finalizer once no User names it.
func (r *DatabaseReconciler) finalize(ctx context.Context, db *fleetv1.Database) error {
	if !controllerutil.ContainsFinalizer(db, fleetv1.DatabaseFinalizer) {
		return nil
	}
	users := &fleetv1.UserList{}
	if err := r.APIReader.List(ctx, users, client.InNamespace(db.Namespace)); err != nil && !r.unserved(err) {
		return fmt.Errorf("listing Users: %w", err)
	}
	named := slices.ContainsFunc(users.Items, func(u fleetv1.User) bool { return u.Spec.DatabaseRef == db.Name })
	old := db.Status
	db.Status.Phase, db.Status.Message = fleetv1.Deleting, ""
	if named {
		db.Status.Message = "waiting until no User names the Database"
	}
	if err := r.writeStatus(ctx, db, old, db.Status); err != nil || named {
		return err // A User's deletion requeues the Database.
	}
	controllerutil.RemoveFinalizer(db, fleetv1.DatabaseFinalizer)
	return client.IgnoreNotFound(r.Update(ctx, db))
}
