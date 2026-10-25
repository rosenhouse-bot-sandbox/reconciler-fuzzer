package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

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

// UserReconciler readies a User on its Database and keeps its credentials.
type UserReconciler struct{ reconciler }

func (r *UserReconciler) SetupWithManager(m ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(m).
		For(&fleetv1.User{}).
		Owns(&corev1.Secret{}).
		Watches(&fleetv1.Database{}, handler.EnqueueRequestsFromMapFunc(r.usersOf)).
		Complete(r)
}

func (r *UserReconciler) usersOf(ctx context.Context, db client.Object) []reconcile.Request {
	list := &fleetv1.UserList{}
	if err := r.List(ctx, list, client.InNamespace(db.GetNamespace()), client.MatchingFields{databaseRefField: db.GetName()}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, u := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&u)})
	}
	return requests
}

func (r *UserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	user := &fleetv1.User{}
	if err := r.Get(ctx, req.NamespacedName, user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !user.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // The garbage collector deletes its Secret.
	}
	old := user.Status
	user.Status = fleetv1.CommonStatus{Phase: fleetv1.Pending, ObservedGeneration: user.Generation}
	db := &fleetv1.Database{}
	err := r.Get(ctx, types.NamespacedName{Namespace: user.Namespace, Name: user.Spec.DatabaseRef}, db)
	switch {
	case apierrors.IsNotFound(err):
		user.Status.Message = fmt.Sprintf("waiting for Database %s", user.Spec.DatabaseRef)
	case err != nil:
		return ctrl.Result{}, err
	case !reportsReady(db, db.Status):
		user.Status.Message = fmt.Sprintf("waiting for Database %s to be Ready", db.Name)
	default:
		if err := r.syncSecret(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
		user.Status.Phase = fleetv1.Ready
	}
	return ctrl.Result{}, r.writeStatus(ctx, user, old, user.Status)
}

func (r *UserReconciler) syncSecret(ctx context.Context, user *fleetv1.User) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: user.Namespace, Name: user.Name + "-credentials"}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Data = map[string][]byte{
			"database": []byte(user.Spec.DatabaseRef),
			"username": []byte(user.Name),
			"password": []byte(Password(user)),
		}
		return controllerutil.SetControllerReference(user, secret, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("ensuring Secret %s: %w", secret.Name, err)
	}
	return nil
}

// Password derives the User's password from its UID, so that a recreated
// Secret holds the same one.
func Password(user *fleetv1.User) string {
	sum := sha256.Sum256([]byte(user.UID))
	return hex.EncodeToString(sum[:8])
}
