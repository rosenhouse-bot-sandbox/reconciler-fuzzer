package controller

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fleetv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/api/v1"
	storagev1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-fleet/thirdparty/storage/v1"
)

const ns = "ns"

type fleet struct {
	t         *testing.T
	c         client.WithWatch
	clusters  *ClusterReconciler
	databases *DatabaseReconciler
	users     *UserReconciler
}

func newClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.Cluster{}, &fleetv1.Database{}, &fleetv1.User{}, &storagev1.Volume{}).
		WithIndex(&fleetv1.Database{}, clusterRefField, clusterRef).
		WithIndex(&fleetv1.User{}, databaseRefField, databaseRef).
		WithObjects(objects...).Build()
}

func newFleet(t *testing.T, objects ...client.Object) *fleet { return newBuggyFleet(t, 0, objects...) }

func newBuggyFleet(t *testing.T, bug Bug, objects ...client.Object) *fleet {
	c := newClient(t, objects...)
	base := reconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Bug: bug}
	return &fleet{t, c, &ClusterReconciler{base}, &DatabaseReconciler{base}, &UserReconciler{base}}
}

func (f *fleet) reconcileOnce(r reconcile.Reconciler, name string) {
	f.t.Helper()
	if _, err := r.Reconcile(f.t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		f.t.Fatal(err)
	}
}

// settle reconciles name with r ten times, enough for every fleet here.
func (f *fleet) settle(r reconcile.Reconciler, name string) {
	f.t.Helper()
	for range 10 {
		f.reconcileOnce(r, name)
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

func (f *fleet) listVolumes() []storagev1.Volume {
	f.t.Helper()
	list := &storagev1.VolumeList{}
	if err := f.c.List(f.t.Context(), list); err != nil {
		f.t.Fatal(err)
	}
	return list.Items
}

// readyVolumes plays the third party: it marks every Volume ready.
func (f *fleet) readyVolumes() {
	f.t.Helper()
	for _, v := range f.listVolumes() {
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: v.Generation})
		if err := f.c.Status().Update(f.t.Context(), &v); err != nil {
			f.t.Fatal(err)
		}
	}
}

// holdVolumes plays the third party: it puts its finalizer on every Volume.
func (f *fleet) holdVolumes() {
	f.t.Helper()
	for _, v := range f.listVolumes() {
		controllerutil.AddFinalizer(&v, "storage.thirdparty/protect")
		if err := f.c.Update(f.t.Context(), &v); err != nil {
			f.t.Fatal(err)
		}
	}
}

// volumes lists each Volume as its name and size.
func (f *fleet) volumes() []string {
	f.t.Helper()
	var volumes []string
	for _, v := range f.listVolumes() {
		volumes = append(volumes, v.Name+" "+v.Spec.Size)
	}
	return volumes
}

func (f *fleet) delete(obj client.Object) {
	f.t.Helper()
	if err := f.c.Delete(f.t.Context(), obj); err != nil {
		f.t.Fatal(err)
	}
}

// deleting marks o deleted, and held by finalizer.
func deleting[T client.Object](o T, finalizer string) T {
	now := metav1.Now()
	o.SetFinalizers([]string{finalizer})
	o.SetDeletionTimestamp(&now)
	return o
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

// pendingStatus is the Pending status of a generation 1 CR with message.
func pendingStatus(message string) fleetv1.CommonStatus {
	return fleetv1.CommonStatus{Phase: fleetv1.Pending, ObservedGeneration: 1, Message: message}
}

func TestClusterIsReadyOnlyOnceItsVolumesAre(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	provisioning := fleetv1.ClusterStatus{CommonStatus: fleetv1.CommonStatus{Phase: fleetv1.Provisioning, ObservedGeneration: 1, Message: "0 of 2 Volumes ready"}}
	if c := f.cluster(); c.Status != provisioning || !slices.Equal(c.Finalizers, []string{fleetv1.ClusterFinalizer}) {
		t.Fatalf("Before the Volumes are ready, the Cluster is %+v %v.", c.Status, c.Finalizers)
	}
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	ready := fleetv1.ClusterStatus{CommonStatus: fleetv1.CommonStatus{Phase: fleetv1.Ready, ObservedGeneration: 1}, ReadyVolumes: 2}
	if c := f.cluster(); c.Status != ready {
		t.Fatalf("After the Volumes are ready, the Cluster is %+v.", c.Status)
	}
}

func TestClusterIsPendingBeforeItCreatesVolumes(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.reconcileOnce(f.clusters, "cluster") // It adds its finalizer.
	f.reconcileOnce(f.clusters, "cluster")
	if c, volumes := f.cluster(), f.volumes(); c.Status.Phase != fleetv1.Pending || len(volumes) > 0 {
		t.Fatalf("After its finalizer, the Cluster is %+v with Volumes %q.", c.Status, volumes)
	}
}

func TestClusterResizesItsVolumesAndDeletesSurplusOnes(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	c := f.cluster()
	c.Spec.Volumes, c.Spec.VolumeSize = 1, "large"
	if err := f.c.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	f.settle(f.clusters, "cluster")
	if got := f.volumes(); !slices.Equal(got, []string{"cluster-vol-0 large"}) {
		t.Fatalf("After a change to 1 large Volume, the Cluster has %q.", got)
	}
}

func TestClusterLeavesTheVolumesOfAnEarlierClusterOfItsName(t *testing.T) {
	earlier := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cluster-vol-2", Labels: map[string]string{ClusterLabel: "cluster"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: fleetv1.GroupVersion.String(), Kind: "Cluster", Name: "cluster", UID: "earlier", Controller: ptr.To(true)}}},
		Spec: storagev1.VolumeSpec{Size: "small"}}
	f := newFleet(t, clusterObj(), earlier)
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("The deleted Cluster gives %v, want not found.", err)
	}
	if got := f.volumes(); !slices.Equal(got, []string{"cluster-vol-2 small"}) {
		t.Fatalf("After the Cluster's deletion, the Volumes are %q, want the earlier Cluster's.", got)
	}
}

func TestVolumeReadyNeedsReadyTrueForItsGenerationWhileLive(t *testing.T) {
	volume := func(status metav1.ConditionStatus, observed int64) *storagev1.Volume {
		v := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: "Test", ObservedGeneration: observed})
		return v
	}
	if !volumeReady(volume(metav1.ConditionTrue, 2)) {
		t.Error("A Volume with Ready True for its generation is not ready.")
	}
	for name, v := range map[string]*storagev1.Volume{
		"no condition":        {},
		"Ready False":         volume(metav1.ConditionFalse, 2),
		"an older generation": volume(metav1.ConditionTrue, 1),
		"deleting":            deleting(volume(metav1.ConditionTrue, 2), "storage.thirdparty/protect"),
	} {
		if volumeReady(v) {
			t.Errorf("A Volume with %s is ready.", name)
		}
	}
}

func TestClusterDeletionWaitsForDatabasesThenVolumes(t *testing.T) {
	elsewhere := &fleetv1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "elsewhere"}, Spec: fleetv1.DatabaseSpec{ClusterRef: "other", Engine: "mysql"}}
	f := newFleet(t, clusterObj(), databaseObj(), elsewhere)
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")

	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Deleting || c.Status.Message != "waiting until no Database names the Cluster" {
		t.Fatalf("With a Database, the deleted Cluster is %+v.", c.Status)
	}
	if n := len(f.listVolumes()); n != 2 {
		t.Fatalf("With a Database, the Cluster has %d Volumes, want both.", n)
	}

	db := &fleetv1.Database{}
	if err := f.get("db", db); err != nil {
		t.Fatal(err)
	}
	f.delete(db)
	f.settle(f.databases, "db")
	f.holdVolumes()
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Message != "waiting until no Volume of the Cluster remains" {
		t.Fatalf("With held Volumes, the deleted Cluster is %+v.", c.Status)
	}
	for _, v := range f.listVolumes() {
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

// A Volume's finalizer can hold it after the Cluster deletes it.
func TestClusterDeletesEachVolumeOnce(t *testing.T) {
	f := newFleet(t, clusterObj())
	deletes := map[string]int{}
	f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes[obj.GetName()]++
			return c.Delete(ctx, obj, opts...)
		},
	})
	f.settle(f.clusters, "cluster")
	f.holdVolumes()
	c := f.cluster()
	c.Spec.Volumes = 1
	if err := f.c.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	f.settle(f.clusters, "cluster")
	if want := map[string]int{"cluster-vol-1": 1}; !maps.Equal(deletes, want) {
		t.Errorf("After a scale-down, the Cluster sent Volume deletes %v, want %v.", deletes, want)
	}
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if want := map[string]int{"cluster-vol-0": 1, "cluster-vol-1": 1}; !maps.Equal(deletes, want) {
		t.Errorf("After its deletion, the Cluster sent Volume deletes %v, want %v.", deletes, want)
	}
}

func TestDatabaseWaitsForItsClusterAndHoldsDeletionForUsers(t *testing.T) {
	elsewhere := &fleetv1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob"}, Spec: fleetv1.UserSpec{DatabaseRef: "other"}}
	f := newFleet(t, databaseObj(), userObj(), elsewhere)
	f.settle(f.databases, "db")
	f.settle(f.users, "alice")
	db := &fleetv1.Database{}
	if err := f.get("db", db); err != nil || db.Status != pendingStatus("waiting for Cluster cluster") {
		t.Fatalf("Without a Cluster, the Database is %+v, %v.", db.Status, err)
	}
	user := &fleetv1.User{}
	if err := f.get("alice", user); err != nil || user.Status != pendingStatus("waiting for Database db to be Ready") {
		t.Fatalf("On a Pending Database, the User is %+v, %v.", user.Status, err)
	}

	if err := f.c.Create(t.Context(), clusterObj()); err != nil {
		t.Fatal(err)
	}
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	if err := f.get("db", db); err != nil || db.Status != pendingStatus("waiting for Cluster cluster to be Ready") {
		t.Fatalf("On a Provisioning Cluster, the Database is %+v, %v.", db.Status, err)
	}
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	f.settle(f.users, "alice")
	cm := &corev1.ConfigMap{}
	if err := f.get("db-db", cm); err != nil || !maps.Equal(cm.Data, map[string]string{"cluster": "cluster", "engine": "postgres"}) || !metav1.IsControlledBy(cm, db) {
		t.Fatalf("The Database's ConfigMap is %v, owned by %v, %v.", cm.Data, cm.OwnerReferences, err)
	}
	secret := &corev1.Secret{}
	credentials := map[string][]byte{"database": []byte("db"), "username": []byte("alice"), "password": []byte(Password(userObj()))}
	if err := f.get("alice-credentials", secret); err != nil || !maps.EqualFunc(secret.Data, credentials, bytes.Equal) || !metav1.IsControlledBy(secret, user) {
		t.Fatalf("The User's Secret is %v, owned by %v, %v.", secret.Data, secret.OwnerReferences, err)
	}

	if err := f.get("db", db); err != nil {
		t.Fatal(err)
	}
	f.delete(db)
	f.settle(f.databases, "db")
	if err := f.get("db", db); err != nil || db.Status.Phase != fleetv1.Deleting || db.Status.Message != "waiting until no User names the Database" {
		t.Fatalf("With a User, the deleted Database is %+v, %v.", db.Status, err)
	}
	f.delete(user)
	f.settle(f.databases, "db")
	if err := f.get("db", db); !apierrors.IsNotFound(err) {
		t.Fatalf("Once its User is gone, the Database gives %v, want not found.", err)
	}
}

func TestADeletingClusterHostsOnlyReadyDatabases(t *testing.T) {
	cluster := &fleetv1.Cluster{Status: fleetv1.ClusterStatus{CommonStatus: fleetv1.CommonStatus{Phase: fleetv1.Deleting}}}
	if ready, pending := hosts(cluster, fleetv1.Ready), hosts(cluster, fleetv1.Pending); !ready || pending {
		t.Errorf("A Deleting Cluster hosts a Ready Database: %v, and a Pending one: %v; want true and false.", ready, pending)
	}
}

func TestUserWaitsForItsDatabase(t *testing.T) {
	f := newFleet(t, userObj())
	f.settle(f.users, "alice")
	user := &fleetv1.User{}
	if err := f.get("alice", user); err != nil || user.Status != pendingStatus("waiting for Database db") {
		t.Fatalf("Without a Database, the User is %+v, %v.", user.Status, err)
	}
}

// Another finalizer can hold a CR after the controller removes its own. The
// controller then writes the CR no more.
func TestFinalizeLeavesACRItReleased(t *testing.T) {
	cluster, db := deleting(clusterObj(), metav1.FinalizerDeleteDependents), deleting(databaseObj(), metav1.FinalizerDeleteDependents)
	f := newFleet(t, cluster, db)
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	if err := f.get("db", db); err != nil || db.Status.Phase != "" || f.cluster().Status.Phase != "" {
		t.Errorf("The released Cluster is %+v, and the Database %+v, %v; want no status.", f.cluster().Status, db.Status, err)
	}
}

// The cache can miss a new Database or Volume, so finalize lists them past it.
func TestClusterFinalizeSeesDependentsTheCacheMisses(t *testing.T) {
	volume := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cluster-vol-0", Labels: map[string]string{ClusterLabel: "cluster"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: fleetv1.GroupVersion.String(), Kind: "Cluster", Name: "cluster", UID: "c1", Controller: ptr.To(true)}}}}
	for message, dependent := range map[string]client.Object{
		"waiting until no Database names the Cluster":    databaseObj(),
		"waiting until no Volume of the Cluster remains": volume,
	} {
		cluster := deleting(clusterObj(), fleetv1.ClusterFinalizer)
		cluster.UID = "c1"
		f := newFleet(t, cluster)
		f.clusters.APIReader = newClient(t, dependent)
		f.settle(f.clusters, "cluster")
		if c := f.cluster(); c.Status.Message != message {
			t.Errorf("With a %T the cache misses, the deleted Cluster is %+v, want %q.", dependent, c.Status, message)
		}
	}
}

// The cache can miss a new User, so finalize lists Users past it.
func TestDatabaseFinalizeSeesUsersTheCacheMisses(t *testing.T) {
	db := deleting(databaseObj(), fleetv1.DatabaseFinalizer)
	f := newFleet(t, db)
	f.databases.APIReader = newClient(t, userObj())
	f.settle(f.databases, "db")
	if err := f.get("db", db); err != nil || db.Status.Message != "waiting until no User names the Database" {
		t.Fatalf("With a User the cache misses, the deleted Database is %+v, %v.", db.Status, err)
	}
}

// A change to a Cluster requeues the Databases that name it, and a change to
// a Database the Users.
func TestAChangeRequeuesWhatNamesIt(t *testing.T) {
	inOther := func(o client.Object) client.Object {
		o.SetNamespace("other")
		return o
	}
	elsewhere := &fleetv1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "elsewhere"}, Spec: fleetv1.DatabaseSpec{ClusterRef: "other"}}
	bob := &fleetv1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob"}, Spec: fleetv1.UserSpec{DatabaseRef: "other"}}
	f := newFleet(t, databaseObj(), userObj(), elsewhere, bob, inOther(databaseObj()), inOther(userObj()))
	only := func(name string) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
	}
	if got := f.databases.databasesOf(t.Context(), clusterObj()); !slices.Equal(got, only("db")) {
		t.Errorf("A Cluster's change requeues %v, want its Database.", got)
	}
	if got := f.users.usersOf(t.Context(), databaseObj()); !slices.Equal(got, only("alice")) {
		t.Errorf("A Database's change requeues %v, want its User.", got)
	}
}

func TestPasswordFollowsTheUID(t *testing.T) {
	recreated := userObj()
	recreated.UID = "u2"
	if Password(userObj()) != Password(userObj()) || Password(userObj()) == Password(recreated) {
		t.Error("Want one password for each UID.")
	}
}

// The garbage collector deletes a deleting User's Secret, so the controller
// must not write it again.
func TestUserIsLeftAloneWhileDeleting(t *testing.T) {
	user := userObj()
	user.Finalizers = []string{metav1.FinalizerDeleteDependents}
	f := newFleet(t, clusterObj(), databaseObj(), user)
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	f.delete(user)
	f.settle(f.users, "alice")
	if err := f.get("alice-credentials", &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("The deleting User's Secret gives %v, want not found.", err)
	}
}

func TestReconcileWritesNoUnchangedStatus(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	before := f.cluster().ResourceVersion
	f.reconcileOnce(f.clusters, "cluster")
	if after := f.cluster().ResourceVersion; after != before {
		t.Errorf("A settled Cluster went from resourceVersion %s to %s.", before, after)
	}
}

// A queued request can outlive its CR.
func TestReconcileOfAMissingCRSucceeds(t *testing.T) {
	f := newFleet(t)
	for _, r := range []reconcile.Reconciler{f.clusters, f.databases, f.users} {
		f.reconcileOnce(r, "missing")
	}
}

func TestParseBugAcceptsOnlyKnownBugs(t *testing.T) {
	for _, id := range []int{0, 140, 141, 142, 150, 151, 152} {
		if b, err := ParseBug(id); int(b) != id || err != nil {
			t.Errorf("ParseBug(%d) = %v, %v", id, b, err)
		}
	}
	if _, err := ParseBug(100); err == nil {
		t.Error("ParseBug(100) succeeded, want an error.")
	}
}
