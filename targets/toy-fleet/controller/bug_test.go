package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

var clusterKey = ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}}

// readyCluster returns a Ready Cluster's reconciler under bug.
func readyCluster(t *testing.T, bug Bug) (*fleet, *ClusterReconciler) {
	f := newFleet(t, clusterObj())
	r := f.clusters.(*ClusterReconciler)
	r.Bug = bug
	f.settle(r, "cluster")
	f.readyVolumes()
	f.settle(r, "cluster")
	return f, r
}

func (f *fleet) volumes() []storagev1.Volume {
	f.t.Helper()
	list := &storagev1.VolumeList{}
	if err := f.c.List(f.t.Context(), list); err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

// holdVolumes plays the third party's finalizer on every Volume.
func (f *fleet) holdVolumes(hold bool) {
	f.t.Helper()
	for _, v := range f.volumes() {
		v.Finalizers = nil
		if hold {
			v.Finalizers = []string{"storage.thirdparty/protect"}
		}
		if err := f.c.Update(f.t.Context(), &v); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestB60ReleasesTheClusterBeforeDeletingItsUnownedVolumes(t *testing.T) {
	for _, bug := range []Bug{0, B60} {
		f, r := readyCluster(t, bug)
		for _, v := range f.volumes() {
			if owned := metav1.GetControllerOf(&v) != nil; owned != (bug == 0) {
				t.Errorf("B%d: Volume %s has a controller %v.", bug, v.Name, owned)
			}
		}
		f.delete(f.cluster())
		r.Client = interceptor.NewClient(f.c.(client.WithWatch), interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*storagev1.Volume); ok {
					return errors.New("killed")
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
		_, _ = r.Reconcile(t.Context(), clusterKey)
		_, _ = r.Reconcile(t.Context(), clusterKey)
		gone := apierrors.IsNotFound(f.get("cluster", &fleetv1.Cluster{}))
		if gone != (bug == B60) || len(f.volumes()) != 2 {
			t.Errorf("B%d: the Cluster is gone %v with %d Volumes left.", bug, gone, len(f.volumes()))
		}
	}
}

func TestB61ForgetsAPendingDeletionAfterARestart(t *testing.T) {
	for _, bug := range []Bug{0, B61} {
		f, r := readyCluster(t, bug)
		f.holdVolumes(true)
		f.delete(f.cluster())
		f.settle(r, "cluster")
		restarted := &ClusterReconciler{reconciler: r.reconciler}
		f.holdVolumes(false)
		f.settle(restarted, "cluster")
		if gone := apierrors.IsNotFound(f.get("cluster", &fleetv1.Cluster{})); gone != (bug == 0) {
			t.Errorf("B%d: after a restart, the Cluster is gone %v.", bug, gone)
		}
	}
}

func TestB62RequeuesATerminatingClusterAtOnce(t *testing.T) {
	for _, bug := range []Bug{0, B62} {
		f, r := readyCluster(t, bug)
		f.holdVolumes(true)
		f.delete(f.cluster())
		f.settle(r, "cluster")
		res, err := r.Reconcile(t.Context(), clusterKey)
		if hot := res.RequeueAfter > 0 && res.RequeueAfter < 10*time.Millisecond; err != nil || hot != (bug == B62) {
			t.Errorf("B%d: with held Volumes, Reconcile gives %+v, %v.", bug, res, err)
		}
	}
}
