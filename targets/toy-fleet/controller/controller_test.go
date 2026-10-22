package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

const ns = "ns"

type fleet struct {
	t                         *testing.T
	c                         client.Client
	clusters, databases, user interface {
		Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
	}
}

func newFleet(t *testing.T, objects ...client.Object) *fleet {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.Cluster{}, &fleetv1.Database{}, &fleetv1.User{}, &storagev1.Volume{}).
		WithIndex(&fleetv1.Database{}, clusterRefField, func(o client.Object) []string { return []string{o.(*fleetv1.Database).Spec.ClusterRef} }).
		WithIndex(&fleetv1.User{}, databaseRefField, func(o client.Object) []string { return []string{o.(*fleetv1.User).Spec.DatabaseRef} }).
		WithObjects(objects...).Build()
	f := &fleet{t: t, c: c}
	return f.as(1, 0)
}

// as switches the fleet to the controller of version, with bug.
func (f *fleet) as(version int, bug Bug) *fleet {
	base := reconciler{Client: f.c, APIReader: f.c, Scheme: f.c.Scheme(), Bug: bug, Version: version}
	f.clusters, f.databases, f.user = &ClusterReconciler{base}, &DatabaseReconciler{base}, &UserReconciler{base}
	return f
}

// volumes lists the Volumes' names.
func (f *fleet) volumes() []string {
	f.t.Helper()
	list := &storagev1.VolumeList{}
	if err := f.c.List(f.t.Context(), list); err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, v := range list.Items {
		names = append(names, v.Name)
	}
	return names
}

// settle reconciles name with r until it stops changing anything.
func (f *fleet) settle(r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string) {
	f.t.Helper()
	for range 10 {
		if _, err := r.Reconcile(f.t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fleet) get(name string, obj client.Object) error {
	return f.c.Get(f.t.Context(), types.NamespacedName{Namespace: ns, Name: name}, obj)
}

func (f *fleet) cluster() *fleetv1.Cluster {
	f.t.Helper()
	c := &fleetv1.Cluster{}
	if err := f.get("cluster", c); err != nil {
		f.t.Fatal(err)
	}
	return c
}

// readyVolumes plays the third party: it marks every Volume ready.
func (f *fleet) readyVolumes() {
	f.t.Helper()
	list := &storagev1.VolumeList{}
	if err := f.c.List(f.t.Context(), list); err != nil {
		f.t.Fatal(err)
	}
	for _, v := range list.Items {
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: v.Generation})
		if err := f.c.Status().Update(f.t.Context(), &v); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fleet) delete(obj client.Object) {
	f.t.Helper()
	if err := f.c.Delete(f.t.Context(), obj); err != nil {
		f.t.Fatal(err)
	}
}

func clusterObj() *fleetv1.Cluster {
	return &fleetv1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cluster", Generation: 1}, Spec: fleetv1.ClusterSpec{Volumes: 2, VolumeSize: "small"}}
}

func databaseObj() *fleetv1.Database {
	return &fleetv1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "db", Generation: 1}, Spec: fleetv1.DatabaseSpec{ClusterRef: "cluster", Engine: "postgres"}}
}

func userObj() *fleetv1.User {
	return &fleetv1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice", Generation: 1, UID: "u1"}, Spec: fleetv1.UserSpec{DatabaseRef: "db"}}
}

func TestClusterIsReadyOnlyOnceItsVolumesAre(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Provisioning || c.Status.ObservedGeneration != 1 || c.Finalizers[0] != fleetv1.ClusterFinalizer {
		t.Fatalf("Before the Volumes are ready, the Cluster is %+v %v.", c.Status, c.Finalizers)
	}
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready || c.Status.ReadyVolumes != 2 {
		t.Fatalf("After the Volumes are ready, the Cluster is %+v.", c.Status)
	}
}

func TestClusterDeletionWaitsForDatabasesThenVolumes(t *testing.T) {
	f := newFleet(t, clusterObj(), databaseObj())
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")

	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Deleting || c.Status.Message != "waiting for 1 Databases to go" {
		t.Fatalf("With a Database, the deleted Cluster is %+v.", c.Status)
	}
	volumes := &storagev1.VolumeList{}
	if err := f.c.List(t.Context(), volumes); err != nil || len(volumes.Items) != 2 {
		t.Fatalf("With a Database, the Cluster has Volumes %d, %v; want both.", len(volumes.Items), err)
	}

	db := &fleetv1.Database{}
	if err := f.get("db", db); err != nil {
		t.Fatal(err)
	}
	f.delete(db)
	f.settle(f.databases, "db")
	// A Volume's finalizer holds it after the Cluster deletes it.
	for _, v := range volumes.Items {
		v.Finalizers = []string{"storage.thirdparty/protect"}
		if err := f.c.Update(t.Context(), &v); err != nil {
			t.Fatal(err)
		}
	}
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Message != "waiting for 2 Volumes to go" {
		t.Fatalf("With held Volumes, the deleted Cluster is %+v.", c.Status)
	}
	if err := f.c.List(t.Context(), volumes); err != nil {
		t.Fatal(err)
	}
	for _, v := range volumes.Items {
		if v.DeletionTimestamp.IsZero() {
			t.Fatalf("Volume %s was not deleted.", v.Name)
		}
		v.Finalizers = nil
		if err := f.c.Update(t.Context(), &v); err != nil {
			t.Fatal(err)
		}
	}
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Once its Volumes are gone, the Cluster gives %v, want not found.", err)
	}
}

func TestDatabaseWaitsForItsClusterAndHoldsDeletionForUsers(t *testing.T) {
	f := newFleet(t, databaseObj(), userObj())
	f.settle(f.databases, "db")
	f.settle(f.user, "alice")
	db := &fleetv1.Database{}
	if err := f.get("db", db); err != nil || db.Status.Phase != fleetv1.Pending || db.Status.Message != "waiting for Cluster cluster" {
		t.Fatalf("Without a Cluster, the Database is %+v, %v.", db.Status, err)
	}
	user := &fleetv1.User{}
	if err := f.get("alice", user); err != nil || user.Status.Phase != fleetv1.Pending {
		t.Fatalf("On a Pending Database, the User is %+v, %v.", user.Status, err)
	}

	if err := f.c.Create(t.Context(), clusterObj()); err != nil {
		t.Fatal(err)
	}
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	f.settle(f.user, "alice")
	cm := &corev1.ConfigMap{}
	if err := f.get("db-db", cm); err != nil || cm.Data["engine"] != "postgres" {
		t.Fatalf("The Database's ConfigMap is %v, %v.", cm.Data, err)
	}
	secret := &corev1.Secret{}
	if err := f.get("alice-credentials", secret); err != nil || string(secret.Data["password"]) != Password(userObj()) {
		t.Fatalf("The User's Secret is %v, %v.", secret.Data, err)
	}

	if err := f.get("db", db); err != nil {
		t.Fatal(err)
	}
	f.delete(db)
	f.settle(f.databases, "db")
	if err := f.get("db", db); err != nil || db.Status.Message != "waiting for 1 Users to go" {
		t.Fatalf("With a User, the deleted Database is %+v, %v.", db.Status, err)
	}
	f.delete(user)
	f.settle(f.databases, "db")
	if err := f.get("db", db); !apierrors.IsNotFound(err) {
		t.Fatalf("Once its User is gone, the Database gives %v, want not found.", err)
	}
}

// readyV1Cluster leaves a Ready Cluster made by version 1.
func readyV1Cluster(t *testing.T) *fleet {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	return f
}

func TestV2NamesNewVolumesData(t *testing.T) {
	f := newFleet(t, clusterObj()).as(2, 0)
	f.settle(f.clusters, "cluster")
	if got := f.volumes(); !slices.Equal(got, []string{"cluster-data-0", "cluster-data-1"}) {
		t.Errorf("Volumes %v", got)
	}
	if c := f.cluster(); c.Status.Phase != fleetv1.Creating || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizerV2}) {
		t.Errorf("Cluster %+v %v", c.Status, c.Finalizers)
	}
}

func TestV2TakesOverAV1ClusterAndDeletesIt(t *testing.T) {
	f := readyV1Cluster(t).as(2, 0)
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizerV2}) {
		t.Errorf("Upgraded Cluster %+v %v", c.Status, c.Finalizers)
	}
	if got := f.volumes(); !slices.Equal(got, []string{"cluster-vol-0", "cluster-vol-1"}) {
		t.Errorf("Upgraded Volumes %v", got)
	}
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Errorf("Deleted Cluster gives %v, want not found.", err)
	}
}

func TestV2FinishesAV1ClusterStillProvisioning(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	f.as(2, 0).settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready {
		t.Errorf("Cluster %+v", c.Status)
	}
}

func TestB40KeepsTheV1Finalizer(t *testing.T) {
	f := readyV1Cluster(t).as(2, B40)
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizer}) {
		t.Errorf("Deleted Cluster has finalizers %v", c.Finalizers)
	}
}

func TestB41LeaksV1Volumes(t *testing.T) {
	f := readyV1Cluster(t).as(2, B41)
	f.settle(f.clusters, "cluster")
	if got := f.volumes(); len(got) != 4 {
		t.Errorf("Volumes %v, want 2 of each version", got)
	}
}

func TestB42StrandsAV1ClusterStillProvisioning(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	f.as(2, B42).settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Provisioning {
		t.Errorf("Cluster %+v", c.Status)
	}
}

func TestParseBugAcceptsOnlyKnownBugs(t *testing.T) {
	if b, err := ParseBug(0); b != 0 || err != nil {
		t.Errorf("ParseBug(0) = %v, %v", b, err)
	}
	if _, err := ParseBug(39); err == nil {
		t.Error("ParseBug(39) succeeded, want an error.")
	}
	if b, err := ParseBug(42); b != B42 || err != nil {
		t.Errorf("ParseBug(42) = %v, %v", b, err)
	}
}
