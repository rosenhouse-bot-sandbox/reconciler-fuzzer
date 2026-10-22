// Package controller reconciles the fleet's Clusters, Databases and Users.
package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

// Field indexes that map a Cluster to its Databases and a Database to its Users.
const (
	clusterRefField  = "spec.clusterRef"
	databaseRefField = "spec.databaseRef"
)

// NewScheme registers the built-in kinds, the fleet's and the Volume.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, fleetv1.AddToScheme, storagev1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("building the scheme: %w", err)
		}
	}
	return scheme, nil
}

// Setup indexes the references and adds the three reconcilers to m.
func Setup(m ctrl.Manager, bug Bug) error {
	ctx := context.Background()
	indexer := m.GetFieldIndexer()
	if err := indexer.IndexField(ctx, &fleetv1.Database{}, clusterRefField, func(o client.Object) []string {
		return []string{o.(*fleetv1.Database).Spec.ClusterRef}
	}); err != nil {
		return fmt.Errorf("indexing Databases: %w", err)
	}
	if err := indexer.IndexField(ctx, &fleetv1.User{}, databaseRefField, func(o client.Object) []string {
		return []string{o.(*fleetv1.User).Spec.DatabaseRef}
	}); err != nil {
		return fmt.Errorf("indexing Users: %w", err)
	}
	base := reconciler{Client: m.GetClient(), APIReader: m.GetAPIReader(), Scheme: m.GetScheme(), Bug: bug}
	for _, r := range []interface{ SetupWithManager(ctrl.Manager) error }{
		&ClusterReconciler{reconciler: base}, &DatabaseReconciler{base}, &UserReconciler{base},
	} {
		if err := r.SetupWithManager(m); err != nil {
			return err
		}
	}
	return nil
}

// reconciler holds what every fleet reconciler needs.
type reconciler struct {
	client.Client
	// APIReader reads past the cache, so that no finalizer comes off on a
	// stale list of dependents.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Bug       Bug
}

// writeStatus writes obj's status when it differs from old.
func (r reconciler) writeStatus(ctx context.Context, obj client.Object, old, status any) error {
	if equality.Semantic.DeepEqual(old, status) {
		return nil
	}
	return r.Status().Update(ctx, obj)
}
