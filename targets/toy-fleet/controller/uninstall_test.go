package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

// unserved answers lists of its kind as a server whose CRD is gone does.
type unserved struct {
	client.Reader
	kind client.ObjectList
}

func (u unserved) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if fmt.Sprintf("%T", list) == fmt.Sprintf("%T", u.kind) {
		return apierrors.NewNotFound(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: "databases"}, "")
	}
	return u.Reader.List(ctx, list, opts...)
}

func clusterRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}}
}

// deletedReadyCluster returns a fleet whose Cluster is Ready and deleted.
func deletedReadyCluster(t *testing.T, bug Bug) *fleet {
	f := newBuggyFleet(t, bug, clusterObj())
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	return f
}

func TestClusterDeletionEndsWhenDatabasesAreNotServed(t *testing.T) {
	for bug, wantGone := range map[Bug]bool{0: true, B52: false} {
		f := deletedReadyCluster(t, bug)
		r := &ClusterReconciler{reconciler{Client: f.c, APIReader: unserved{f.c, &fleetv1.DatabaseList{}}, Scheme: f.c.Scheme(), Bug: bug}}
		for range 10 {
			r.Reconcile(t.Context(), clusterRequest())
		}
		if err := f.get("cluster", &fleetv1.Cluster{}); apierrors.IsNotFound(err) != wantGone {
			t.Errorf("B%d: without a Database CRD, the deleted Cluster gives %v; want gone %v.", bug, err, wantGone)
		}
	}
}

func TestShutdownStripsDeletingCRsFinalizersOnlyUnderB50(t *testing.T) {
	for bug, wantStripped := range map[Bug]bool{0: false, B50: true} {
		f := newBuggyFleet(t, bug, clusterObj(), databaseObj())
		f.settle(f.clusters, "cluster")
		f.delete(f.cluster())
		if err := Shutdown(t.Context(), f.c, ns, bug); err != nil {
			t.Fatal(err)
		}
		err := f.get("cluster", &fleetv1.Cluster{})
		if apierrors.IsNotFound(err) != wantStripped {
			t.Errorf("B%d: after shutdown, the deleted Cluster blocked by a Database gives %v; want gone %v.", bug, err, wantStripped)
		}
	}
}

func TestB51ReleasesTheClusterBeforeItsVolumes(t *testing.T) {
	f := deletedReadyCluster(t, B51)
	volumes := &storagev1.VolumeList{}
	if err := f.c.List(t.Context(), volumes); err != nil || len(volumes.Items) != 2 || !slices.Contains(volumes.Items[0].Finalizers, VolumeFinalizer) {
		t.Fatalf("Under B51, the Volumes are %d, %v; want 2 with %s.", len(volumes.Items), err, VolumeFinalizer)
	}
	if _, err := f.clusters.Reconcile(t.Context(), clusterRequest()); err != nil {
		t.Fatal(err)
	}
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Under B51, the deleted Cluster gives %v; want gone before its Volumes.", err)
	}
	if err := f.c.List(t.Context(), volumes); err != nil || len(volumes.Items) != 0 {
		t.Errorf("Under B51, a later reconcile leaves Volumes %+v, %v; want none.", volumes.Items, err)
	}
}
