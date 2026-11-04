package controller

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// addFinalizer and removeFinalizer patch with an optimistic lock, since a
// merge patch replaces the whole finalizers list.
func addFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string) error {
	base := object.DeepCopyObject().(client.Object)
	controllerutil.AddFinalizer(object, finalizer)
	return c.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func removeFinalizer(ctx context.Context, c client.Client, object client.Object, finalizer string) error {
	base := object.DeepCopyObject().(client.Object)
	controllerutil.RemoveFinalizer(object, finalizer)
	return c.Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}
