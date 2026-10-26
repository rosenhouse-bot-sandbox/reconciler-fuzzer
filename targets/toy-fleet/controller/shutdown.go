package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
)

// VolumeFinalizer holds a Volume under B151.
const VolumeFinalizer = "fleet.toy/volume"

// Shutdown runs after the manager stops. Under B150 it strips the fleet's
// finalizers from deleting Clusters and Databases.
func Shutdown(ctx context.Context, c client.Client, namespace string, bug Bug) error {
	if bug != B150 {
		return nil
	}
	clusters, databases := &fleetv1.ClusterList{}, &fleetv1.DatabaseList{}
	for _, list := range []client.ObjectList{clusters, databases} {
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("listing for shutdown: %w", err)
		}
	}
	var deleting []client.Object
	for i := range clusters.Items {
		deleting = append(deleting, &clusters.Items[i])
	}
	for i := range databases.Items {
		deleting = append(deleting, &databases.Items[i])
	}
	for _, o := range deleting {
		if o.GetDeletionTimestamp().IsZero() {
			continue
		}
		stripped := false
		for _, f := range []string{fleetv1.ClusterFinalizer, fleetv1.ClusterFinalizerV2, fleetv1.DatabaseFinalizer} {
			stripped = controllerutil.RemoveFinalizer(o, f) || stripped
		}
		if stripped {
			if err := c.Update(ctx, o); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("releasing %s: %w", o.GetName(), err)
			}
		}
	}
	return nil
}
