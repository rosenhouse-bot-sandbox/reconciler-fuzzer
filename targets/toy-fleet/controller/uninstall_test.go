package controller

import (
	"context"
	"reflect"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
)

// unserved fails each list of kind with err, as a server whose CRD of kind
// is gone does.
type unserved struct {
	client.Reader
	kind client.ObjectList
	err  error
}

func (u unserved) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if reflect.TypeOf(list) == reflect.TypeOf(u.kind) {
		return u.err
	}
	return u.Reader.List(ctx, list, opts...)
}

// unservedErrors are the errors of a list of kind, whose resource is
// resource, once its CRD is gone: the server's 404, or the REST mapper's.
func unservedErrors(resource, kind string) map[string]error {
	return map[string]error{
		"NotFound":    apierrors.NewNotFound(schema.GroupResource{Group: fleetv1.GroupVersion.Group, Resource: resource}, ""),
		"NoKindMatch": &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: fleetv1.GroupVersion.Group, Kind: kind}, SearchedVersions: []string{"v1"}},
	}
}

// reconcileTenTimes reconciles name with r ten times, whatever each returns.
func reconcileTenTimes(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string) {
	for range 10 {
		r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	}
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
	for name, err := range unservedErrors("databases", "Database") {
		for bug, wantGone := range map[Bug]bool{0: true, B152: false} {
			f := deletedReadyCluster(t, bug)
			f.clusters.APIReader = unserved{f.c, &fleetv1.DatabaseList{}, err}
			reconcileTenTimes(t, f.clusters, "cluster")
			if err := f.get("cluster", &fleetv1.Cluster{}); apierrors.IsNotFound(err) != wantGone {
				t.Errorf("B%d, Databases %s: the deleted Cluster gives %v; want gone %v.", bug, name, err, wantGone)
			}
		}
	}
}

func TestDatabaseDeletionEndsWhenUsersAreNotServed(t *testing.T) {
	for name, err := range unservedErrors("users", "User") {
		for bug, wantGone := range map[Bug]bool{0: true, B152: false} {
			f := newBuggyFleet(t, bug, deleting(databaseObj(), fleetv1.DatabaseFinalizer))
			f.databases.APIReader = unserved{f.c, &fleetv1.UserList{}, err}
			reconcileTenTimes(t, f.databases, "db")
			if err := f.get("db", &fleetv1.Database{}); apierrors.IsNotFound(err) != wantGone {
				t.Errorf("B%d, Users %s: the deleted Database gives %v; want gone %v.", bug, name, err, wantGone)
			}
		}
	}
}

func TestShutdownStripsDeletingCRsFinalizersOnlyUnderB150(t *testing.T) {
	for bug, wantStripped := range map[Bug]bool{0: false, B150: true} {
		f := newBuggyFleet(t, bug, clusterObj(), databaseObj(), userObj())
		f.settle(f.clusters, "cluster")
		f.settle(f.databases, "db")
		db := &fleetv1.Database{}
		if err := f.get("db", db); err != nil {
			t.Fatal(err)
		}
		f.delete(f.cluster())
		f.delete(db)
		if err := Shutdown(t.Context(), f.c, ns, bug); err != nil {
			t.Fatal(err)
		}
		for name, obj := range map[string]client.Object{"cluster": &fleetv1.Cluster{}, "db": &fleetv1.Database{}} {
			if err := f.get(name, obj); apierrors.IsNotFound(err) != wantStripped {
				t.Errorf("B%d: after shutdown, the deleted %T, which a dependent holds, gives %v; want gone %v.", bug, obj, err, wantStripped)
			}
		}
	}
}

func TestB151ReleasesTheClusterBeforeItsVolumes(t *testing.T) {
	f := deletedReadyCluster(t, B151)
	if volumes := f.listVolumes(); len(volumes) != 2 || !slices.Contains(volumes[0].Finalizers, VolumeFinalizer) {
		t.Fatalf("Under B151, the Volumes are %+v; want 2 with %s.", volumes, VolumeFinalizer)
	}
	f.reconcileOnce(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Under B151, the deleted Cluster gives %v; want gone before its Volumes.", err)
	}
	if volumes := f.listVolumes(); len(volumes) != 2 || volumes[0].DeletionTimestamp.IsZero() {
		t.Fatalf("Under B151, the released Cluster's Volumes are %+v; want both, deleting.", volumes)
	}
	f.reconcileOnce(f.clusters, "cluster")
	if volumes := f.listVolumes(); len(volumes) != 0 {
		t.Errorf("Under B151, a reconcile of the gone Cluster leaves Volumes %+v; want none.", volumes)
	}
}
