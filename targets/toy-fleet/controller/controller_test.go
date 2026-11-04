package controller

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sstoragev1 "k8s.io/api/storage/v1"
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
	secrets   *SecretReconciler
}

func newClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme, err := NewScheme(0)
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fleetv1.Cluster{}, &fleetv1.Database{}, &fleetv1.User{}, &storagev1.Volume{}).
		WithIndex(&fleetv1.Database{}, clusterRefField, clusterRef).
		WithIndex(&fleetv1.Database{}, passwordSecretRefField, passwordSecretRef).
		WithIndex(&fleetv1.User{}, databaseRefField, databaseRef).
		WithObjects(objects...).Build()
}

func newFleet(t *testing.T, objects ...client.Object) *fleet { return newBuggyFleet(t, 0, objects...) }

func newBuggyFleet(t *testing.T, bug Bug, objects ...client.Object) *fleet {
	c := newClient(t, objects...)
	base := reconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Bug: bug, Version: 1}
	return &fleet{t, c, &ClusterReconciler{reconciler: base}, &DatabaseReconciler{base}, &UserReconciler{base}, &SecretReconciler{base}}
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

func (f *fleet) database() *fleetv1.Database {
	f.t.Helper()
	db := &fleetv1.Database{}
	if err := f.get("db", db); err != nil {
		f.t.Fatal(err)
	}
	return db
}

func (f *fleet) secret() *corev1.Secret {
	f.t.Helper()
	s := &corev1.Secret{}
	if err := f.get("admin-password", s); err != nil {
		f.t.Fatal(err)
	}
	return s
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

// databaseNaming is databaseObj naming the Secret secret.
func databaseNaming(secret string) *fleetv1.Database {
	db := databaseObj()
	db.Spec.PasswordSecretRef = &corev1.LocalObjectReference{Name: secret}
	return db
}

func passwordSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "admin-password"}, Data: data}
}

// readyCluster is clusterObj reporting Ready for its generation.
func readyCluster() *fleetv1.Cluster {
	c := clusterObj()
	c.Status = fleetv1.ClusterStatus{CommonStatus: readyStatus(), ReadyVolumes: c.Spec.Volumes}
	return c
}

func userObj() *fleetv1.User {
	return &fleetv1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "alice", Generation: 1, UID: "u1"}, Spec: fleetv1.UserSpec{DatabaseRef: "db"}}
}

// pendingStatus is the Pending status of a generation 1 CR with message.
func pendingStatus(message string) fleetv1.CommonStatus {
	return fleetv1.CommonStatus{Phase: fleetv1.Pending, ObservedGeneration: 1, Message: message}
}

// readyStatus is the Ready status of a generation 1 CR.
func readyStatus() fleetv1.CommonStatus {
	return fleetv1.CommonStatus{Phase: fleetv1.Ready, ObservedGeneration: 1}
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

// B0 counts a Volume ready while it is live with Ready True for its
// generation. B131 counts every Volume, and B172 also Ready True of an older
// generation.
func TestCountsReadyNeedsReadyTrueForItsGenerationWhileLive(t *testing.T) {
	volume := func(status metav1.ConditionStatus, observed int64) *storagev1.Volume {
		v := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, Reason: "Test", ObservedGeneration: observed})
		return v
	}
	unready := map[string]*storagev1.Volume{
		"no condition":        {},
		"Ready False":         volume(metav1.ConditionFalse, 2),
		"an older generation": volume(metav1.ConditionTrue, 1),
		"deleting":            deleting(volume(metav1.ConditionTrue, 2), "storage.thirdparty/protect"),
	}
	for bug, want := range map[Bug][]string{
		0:    nil,
		B131: {"Ready False", "an older generation", "deleting", "no condition"},
		B172: {"an older generation"},
	} {
		r := &ClusterReconciler{reconciler: reconciler{Bug: bug}}
		if !r.countsReady(volume(metav1.ConditionTrue, 2)) {
			t.Errorf("B%d does not count ready a Volume with Ready True for its generation.", bug)
		}
		var counted []string
		for name, v := range unready {
			if r.countsReady(v) {
				counted = append(counted, name)
			}
		}
		slices.Sort(counted)
		if !slices.Equal(counted, want) {
			t.Errorf("B%d counts ready the Volumes with %q, want %q.", bug, counted, want)
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

func TestB116StripsTheFinalizersOfVolumesOnlyWhileTheClusterIsDeleted(t *testing.T) {
	for bug, wantStripped := range map[Bug]bool{0: false, B116: true} {
		f := newBuggyFleet(t, bug, clusterObj())
		f.settle(f.clusters, "cluster")
		f.holdVolumes()
		c := f.cluster()
		c.Spec.Volumes = 1
		if err := f.c.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		f.settle(f.clusters, "cluster")
		if n := len(f.listVolumes()); n != 2 {
			t.Errorf("B%d: after a scale-down, %d Volumes remain; want 2, one of them held.", bug, n)
		}
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if n := len(f.listVolumes()); (n == 0) != wantStripped {
			t.Errorf("B%d: the deleted Cluster leaves %d held Volumes; want them gone %v.", bug, n, wantStripped)
		}
	}
}

func TestB115ReleasesTheClusterWhileItsVolumesAreHeld(t *testing.T) {
	for bug, wantGone := range map[Bug]bool{0: false, B115: true} {
		f := newBuggyFleet(t, bug, clusterObj())
		f.settle(f.clusters, "cluster")
		f.holdVolumes()
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if gone := apierrors.IsNotFound(f.get("cluster", &fleetv1.Cluster{})); gone != wantGone {
			t.Errorf("B%d: with its Volumes held, the deleted Cluster is gone %v, want %v.", bug, gone, wantGone)
		}
		for _, v := range f.listVolumes() {
			if v.DeletionTimestamp.IsZero() {
				t.Errorf("B%d: Volume %s was not deleted.", bug, v.Name)
			}
		}
	}
}

// failVolumes plays the third party: it marks every Volume not ready.
func (f *fleet) failVolumes() {
	f.t.Helper()
	for _, v := range f.listVolumes() {
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Stuck", ObservedGeneration: v.Generation})
		if err := f.c.Status().Update(f.t.Context(), &v); err != nil {
			f.t.Fatal(err)
		}
	}
}

// B117 gives up for good: the Cluster stays after its Volumes recover.
func TestB117StopsFinalizingAClusterWhoseVolumeFailed(t *testing.T) {
	for bug, wantGone := range map[Bug]bool{0: true, B117: false} {
		f := newBuggyFleet(t, bug, clusterObj())
		f.settle(f.clusters, "cluster")
		f.failVolumes()
		f.delete(f.cluster())
		f.reconcileOnce(f.clusters, "cluster")
		f.readyVolumes()
		f.settle(f.clusters, "cluster")
		if gone := apierrors.IsNotFound(f.get("cluster", &fleetv1.Cluster{})); gone != wantGone {
			t.Fatalf("B%d: once a Volume failed, the deleted Cluster is gone %v, want %v.", bug, gone, wantGone)
		}
		if bug == B117 {
			if c := f.cluster(); c.Status.Phase != fleetv1.Deleting || c.Status.Message != "a Volume failed; clean up by hand" {
				t.Errorf("B117: the abandoned Cluster's status is %+v.", c.Status)
			}
			if n := len(f.listVolumes()); n != 2 {
				t.Errorf("B117 deleted %d of 2 Volumes, want none.", 2-n)
			}
		}
	}
}

func TestB118DeletesVolumesWithOrphanPropagation(t *testing.T) {
	for bug, want := range map[Bug]metav1.DeletionPropagation{0: "", B118: metav1.DeletePropagationOrphan} {
		f := newBuggyFleet(t, bug, clusterObj())
		policies := map[string]metav1.DeletionPropagation{}
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				o := &client.DeleteOptions{}
				o.ApplyOptions(opts)
				policies[obj.GetName()] = ptr.Deref(o.PropagationPolicy, "")
				return c.Delete(ctx, obj, opts...)
			},
		})
		f.settle(f.clusters, "cluster")
		c := f.cluster()
		c.Spec.Volumes = 1
		if err := f.c.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		f.settle(f.clusters, "cluster")
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if wantAll := map[string]metav1.DeletionPropagation{"cluster-vol-0": want, "cluster-vol-1": want}; !maps.Equal(policies, wantAll) {
			t.Errorf("B%d: the Volume deletes have propagation %v, want %v.", bug, policies, wantAll)
		}
	}
}

// storageClass is the StorageClass of the Cluster named cluster in ns.
const storageClass = "fleet-ns-cluster"

func TestAClusterWithStorageClassesDeletesItsStorageClassBeforeItsRelease(t *testing.T) {
	for _, row := range []struct {
		bug  Bug
		want []string
	}{
		{0, []string{"delete cluster-vol-0", "delete cluster-vol-1", "delete " + storageClass, "update cluster"}},
		{B119, []string{"delete cluster-vol-0", "delete cluster-vol-1", "update cluster", "delete " + storageClass}},
	} {
		f := newBuggyFleet(t, row.bug, clusterObj())
		f.clusters.storageClasses = true
		f.settle(f.clusters, "cluster")
		sc := &k8sstoragev1.StorageClass{}
		if err := f.c.Get(t.Context(), types.NamespacedName{Name: storageClass}, sc); err != nil || sc.Provisioner != Provisioner {
			t.Fatalf("B%d: the Cluster's StorageClass is %+v, %v.", row.bug, sc, err)
		}
		var writes []string
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes = append(writes, "update "+obj.GetName())
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes = append(writes, "delete "+obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		})
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if !slices.Equal(writes, row.want) {
			t.Errorf("B%d: the deleted Cluster's controller writes %q, want %q.", row.bug, writes, row.want)
		}
		if err := f.c.Get(t.Context(), types.NamespacedName{Name: storageClass}, sc); !apierrors.IsNotFound(err) {
			t.Errorf("B%d: getting the deleted Cluster's StorageClass gives %v, want NotFound.", row.bug, err)
		}
	}
}

func TestAClusterWithoutStorageClassesHasNone(t *testing.T) {
	f := newFleet(t, clusterObj())
	f.settle(f.clusters, "cluster")
	list := &k8sstoragev1.StorageClassList{}
	if err := f.c.List(t.Context(), list); err != nil || len(list.Items) != 0 {
		t.Errorf("Without storage classes, the StorageClasses are %+v, %v.", list.Items, err)
	}
}

func TestB131ReportsAClusterReadyBeforeItsVolumes(t *testing.T) {
	f := newBuggyFleet(t, B131, clusterObj())
	f.settle(f.clusters, "cluster")
	if c := f.cluster(); c.Status.Phase != fleetv1.Ready {
		t.Errorf("Under B131, before its Volumes are ready, the Cluster is %+v.", c.Status)
	}
}

// Each Volume gets the Cluster's label and controller reference in its
// create, not only in a later update.
func TestEachVolumeHasTheClusterLabelAndOwnerFromItsCreate(t *testing.T) {
	for _, bug := range []Bug{0, B132} {
		f := newBuggyFleet(t, bug, clusterObj())
		var created []storagev1.Volume
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if v, ok := obj.(*storagev1.Volume); ok {
					created = append(created, *v.DeepCopy())
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		f.settle(f.clusters, "cluster")
		cluster := f.cluster()
		if len(created) != 2 || slices.ContainsFunc(created, func(v storagev1.Volume) bool {
			return v.Labels[ClusterLabel] != cluster.Name || !metav1.IsControlledBy(&v, cluster)
		}) {
			t.Errorf("B%d creates Volumes %+v, want 2 with the Cluster's label and controller reference.", bug, created)
		}
	}
}

// B132 forgets the name of each Volume it deletes, so a later Volume at that
// index gets a new generated name.
func TestB132NamesEachNewVolumeAfresh(t *testing.T) {
	f := newBuggyFleet(t, B132, clusterObj())
	f.settle(f.clusters, "cluster")
	first := f.volumes()
	for _, volumes := range []int32{1, 2} {
		c := f.cluster()
		c.Spec.Volumes = volumes
		if err := f.c.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		f.settle(f.clusters, "cluster")
	}
	got := f.volumes()
	if len(first) != 2 || len(got) != 2 || got[0] != first[0] || got[1] == first[1] || !strings.HasPrefix(got[1], "cluster-vol-1-") {
		t.Errorf("Under B132, the Volumes were %q. After a change to 1 Volume and back to 2, they are %q, want the first and a new cluster-vol-1.", first, got)
	}
}

// A restarted B132 has lost the generated names of its Volumes, so it makes
// a second set and deletes only its own surplus ones.
func TestB132MakesASecondSetOfVolumesAfterARestart(t *testing.T) {
	f := newBuggyFleet(t, B132, clusterObj())
	f.settle(f.clusters, "cluster")
	first := f.volumes()
	restarted := &ClusterReconciler{reconciler: f.clusters.reconciler}
	f.settle(restarted, "cluster")
	c := f.cluster()
	c.Spec.Volumes = 1
	if err := f.c.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	f.settle(restarted, "cluster")
	got := f.volumes()
	added := slices.DeleteFunc(slices.Clone(got), func(v string) bool { return slices.Contains(first, v) })
	if len(first) != 2 || len(got) != 3 || len(added) != 1 || !strings.HasPrefix(added[0], "cluster-vol-0-") {
		t.Errorf("Under B132, the first process made Volumes %q. After a restart and a change to 1 Volume, the Volumes are %q, want those and one more cluster-vol-0.", first, got)
	}
}

// A scale from 3 Volumes to 1 can come while the cache misses the third. After
// a scale back to 2, the Cluster has its first Volume and a new second one,
// under B132 too.
func TestAScaleDownDeletesAVolumeTheCacheMissed(t *testing.T) {
	for _, bug := range []Bug{0, B132} {
		f := newBuggyFleet(t, bug, clusterObj())
		scale := func(volumes int32) {
			c := f.cluster()
			c.Spec.Volumes = volumes
			if err := f.c.Update(t.Context(), c); err != nil {
				t.Fatal(err)
			}
		}
		f.settle(f.clusters, "cluster")
		var cached []string
		for _, v := range f.listVolumes() {
			cached = append(cached, v.Name)
		}
		scale(3)
		f.reconcileOnce(f.clusters, "cluster")
		scale(1)
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				err := c.List(ctx, list, opts...)
				if volumes, ok := list.(*storagev1.VolumeList); ok {
					volumes.Items = slices.DeleteFunc(volumes.Items, func(v storagev1.Volume) bool { return !slices.Contains(cached, v.Name) })
				}
				return err
			},
		})
		f.reconcileOnce(f.clusters, "cluster")
		f.clusters.Client = f.c
		scale(2)
		f.settle(f.clusters, "cluster")
		if got := f.volumes(); len(cached) != 2 || len(got) != 2 || got[0] != cached[0]+" small" || !strings.HasPrefix(got[1], "cluster-vol-1") {
			t.Errorf("B%d: the cache held Volumes %q. After a change to 3 Volumes, to 1 while the cache missed the third, and to 2, the Volumes are %q, want the first and a cluster-vol-1.", bug, cached, got)
		}
	}
}

// After one failed Volume create, B133 gives up on the Cluster for good. B0
// returns the error and tries again.
func TestB133GivesUpForGoodAfterAFailedVolumeCreate(t *testing.T) {
	for bug, want := range map[Bug]struct{ errors, volumes int }{0: {1, 2}, B133: {0, 0}} {
		f := newBuggyFleet(t, bug, clusterObj())
		failed := false
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*storagev1.Volume); ok && !failed {
					failed = true
					return apierrors.NewInternalError(errors.New("injected"))
				}
				return c.Create(ctx, obj, opts...)
			},
		})
		errs := 0
		for range 10 {
			if _, err := f.clusters.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}}); err != nil {
				errs++
			}
		}
		c, n := f.cluster(), len(f.listVolumes())
		if errs != want.errors || n != want.volumes || c.Status.Phase != fleetv1.Provisioning || strings.HasPrefix(c.Status.Message, gaveUp) != (bug == B133) {
			t.Errorf("B%d: after a failed Volume create, 10 reconciles give %d errors, %d Volumes and status %+v; want %d errors, %d Volumes and phase Provisioning.", bug, errs, n, c.Status, want.errors, want.volumes)
		}
	}
}

// A restarted B135 makes a second set of Volumes and deletes only its own
// surplus ones. A restarted B0 keeps the first set.
func TestB135MakesASecondSetOfVolumesAfterARestart(t *testing.T) {
	for bug, want := range map[Bug][]string{
		0:    {"cluster-vol-0 small"},
		B135: {"cluster-vol-0-one small", "cluster-vol-0-two small", "cluster-vol-1-one small"},
	} {
		f := newBuggyFleet(t, bug, clusterObj())
		f.clusters.suffix = "one"
		f.settle(f.clusters, "cluster")
		restarted := &ClusterReconciler{reconciler: f.clusters.reconciler, suffix: "two"}
		f.settle(restarted, "cluster")
		c := f.cluster()
		c.Spec.Volumes = 1
		if err := f.c.Update(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		f.settle(restarted, "cluster")
		if got := f.volumes(); !slices.Equal(got, want) {
			t.Errorf("B%d: after a restart and a change to 1 Volume, the Volumes are %q, want %q.", bug, got, want)
		}
	}
}

// A kill between B160's release of the Cluster and its Volume deletes leaks
// the Volumes, since no ownerReference lets gc collect them.
func TestB160ReleasesTheClusterBeforeDeletingItsUnownedVolumes(t *testing.T) {
	for _, row := range []struct {
		bug     Bug
		volumes int32
		want    []string
	}{
		{0, 2, []string{"delete cluster-vol-0", "delete cluster-vol-1", "update cluster"}},
		{B160, 2, []string{"update cluster", "delete cluster-vol-0", "delete cluster-vol-1"}},
		{B160, 0, []string{"update cluster"}},
	} {
		bug, cluster := row.bug, clusterObj()
		cluster.Spec.Volumes = row.volumes
		f := newBuggyFleet(t, bug, cluster)
		f.settle(f.clusters, "cluster")
		for _, v := range f.listVolumes() {
			if owned := metav1.GetControllerOf(&v) != nil; owned != (bug == 0) {
				t.Errorf("B%d: Volume %s has a controller: %v.", bug, v.Name, owned)
			}
		}
		var writes []string
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				writes = append(writes, "update "+obj.GetName())
				return c.Update(ctx, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				writes = append(writes, "delete "+obj.GetName())
				return c.Delete(ctx, obj, opts...)
			},
		})
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		if !slices.Equal(writes, row.want) {
			t.Errorf("B%d, %d Volumes: the deleted Cluster's controller writes %q, want %q.", bug, row.volumes, writes, row.want)
		}
	}
}

func TestB160MapsAVolumeToTheClusterItsLabelNames(t *testing.T) {
	labeled := &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cluster-vol-0", Labels: map[string]string{ClusterLabel: "cluster"}}}
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}}}
	if got := clusterOfVolume(t.Context(), labeled); !slices.Equal(got, want) {
		t.Errorf("A labeled Volume maps to %v, want %v.", got, want)
	}
	if got := clusterOfVolume(t.Context(), &storagev1.Volume{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other"}}); got != nil {
		t.Errorf("An unlabeled Volume maps to %v, want none.", got)
	}
}

// B161 keeps in memory the finalizations that its process began.
func TestB161ForgetsAPendingDeletionAfterARestart(t *testing.T) {
	for _, c := range []struct {
		bug     Bug
		restart bool
		// another says that the new process first finalizes another Cluster.
		another bool
		gone    bool
	}{
		{bug: 0, restart: true, gone: true},
		{bug: B161, gone: true},
		{bug: B161, restart: true},
		{bug: B161, restart: true, another: true},
	} {
		f := newBuggyFleet(t, c.bug, clusterObj())
		f.settle(f.clusters, "cluster")
		f.holdVolumes()
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		r := f.clusters
		if c.restart {
			r = &ClusterReconciler{reconciler: r.reconciler}
		}
		if c.another {
			other := clusterObj()
			other.Name = "other"
			if err := f.c.Create(t.Context(), other); err != nil {
				t.Fatal(err)
			}
			f.settle(r, "other")
			f.delete(other)
			f.settle(r, "other")
			if !apierrors.IsNotFound(f.get("other", &fleetv1.Cluster{})) {
				t.Fatalf("B%d: the new process leaves another deleted Cluster.", c.bug)
			}
		}
		for _, v := range f.listVolumes() {
			v.Finalizers = nil
			if err := f.c.Update(t.Context(), &v); err != nil {
				t.Fatal(err)
			}
		}
		f.settle(r, "cluster")
		if gone := apierrors.IsNotFound(f.get("cluster", &fleetv1.Cluster{})); gone != c.gone {
			t.Errorf("B%d, restart %v, another %v: once its Volumes are gone, the deleted Cluster is gone %v.", c.bug, c.restart, c.another, gone)
		}
	}
}

func TestB162DeletesHeldVolumesOnEachReconcileAndRequeuesAfter1ms(t *testing.T) {
	for bug, want := range map[Bug]struct {
		deletes int
		requeue time.Duration
	}{0: {1, 0}, B162: {11, time.Millisecond}} {
		f := newBuggyFleet(t, bug, clusterObj())
		deletes := map[string]int{}
		f.clusters.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes[obj.GetName()]++
				return c.Delete(ctx, obj, opts...)
			},
		})
		f.settle(f.clusters, "cluster")
		f.holdVolumes()
		f.delete(f.cluster())
		f.settle(f.clusters, "cluster")
		result, err := f.clusters.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}})
		if err != nil || result.RequeueAfter != want.requeue || deletes["cluster-vol-0"] != want.deletes || deletes["cluster-vol-1"] != want.deletes {
			t.Errorf("B%d: 11 reconciles of a deleted Cluster with held Volumes give %+v, %v, and Volume deletes %v; want a requeue after %v and %d deletes of each Volume.",
				bug, result, err, deletes, want.requeue, want.deletes)
		}
	}
}

func TestB162DoesNotRequeueAClusterThatADatabaseNames(t *testing.T) {
	f := newBuggyFleet(t, B162, clusterObj(), databaseObj())
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	result, err := f.clusters.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "cluster"}})
	if err != nil || result.RequeueAfter != 0 {
		t.Errorf("B162 reconciles a deleted Cluster that a Database names to %+v, %v; want no requeue.", result, err)
	}
}

func TestB162ReleasesTheClusterOnceItsVolumesAreGone(t *testing.T) {
	f := newBuggyFleet(t, B162, clusterObj())
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Errorf("Getting the deleted Cluster gives %v, want NotFound.", err)
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

func TestDatabaseWaitsForItsClusterToReportItsGenerationReady(t *testing.T) {
	cluster := readyCluster()
	cluster.Generation = 2
	db := databaseObj()
	db.Status = readyStatus()
	f := newFleet(t, cluster, db)
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Cluster cluster to be Ready") {
		t.Errorf("On a Cluster that reports an older generation Ready, the Ready Database is %+v.", got)
	}
}

// Deletion raises a CR's generation by one, and the Cluster then writes only
// its phase and message. A Cluster that waits in deletion readies no new
// Database, and keeps a Ready one Ready through a change to its spec.
func TestADeletingClusterKeepsOnlyItsReadyDatabasesReady(t *testing.T) {
	cluster := deleting(readyCluster(), fleetv1.ClusterFinalizer)
	cluster.Generation, cluster.Status.Phase = 2, fleetv1.Deleting
	ready := databaseObj()
	ready.Generation, ready.Spec.Engine, ready.Status = 2, "mysql", readyStatus()
	pending := databaseObj()
	pending.Name = "pending"
	f := newFleet(t, cluster, ready, pending)
	f.settle(f.databases, "db")
	f.settle(f.databases, "pending")
	if got, want := f.database().Status, (fleetv1.CommonStatus{Phase: fleetv1.Ready, ObservedGeneration: 2}); got != want {
		t.Errorf("On a deleting Cluster, the Ready Database of generation 2 is %+v, want %+v.", got, want)
	}
	cm := &corev1.ConfigMap{}
	if err := f.get("db-db", cm); err != nil || cm.Data["engine"] != "mysql" {
		t.Errorf("On a deleting Cluster, the Ready Database's ConfigMap is %v, %v; want engine mysql.", cm.Data, err)
	}
	if err := f.get("pending", pending); err != nil || pending.Status != pendingStatus("waiting for Cluster cluster to be Ready") {
		t.Errorf("On a deleting Cluster, the new Database is %+v, %v.", pending.Status, err)
	}
}

// A Cluster that was not Ready when its deletion started keeps no Database
// Ready through a change to its spec.
func TestADeletingClusterThatWasNotReadyKeepsNoDatabaseReady(t *testing.T) {
	for when, status := range map[string]fleetv1.ClusterStatus{
		"before it saw its spec":       {CommonStatus: fleetv1.CommonStatus{Phase: fleetv1.Deleting, ObservedGeneration: 1}, ReadyVolumes: 2},
		"while a Volume was not ready": {CommonStatus: fleetv1.CommonStatus{Phase: fleetv1.Deleting, ObservedGeneration: 2}, ReadyVolumes: 1},
	} {
		cluster := deleting(readyCluster(), fleetv1.ClusterFinalizer)
		cluster.Generation, cluster.Status = 3, status
		db := databaseObj()
		db.Generation, db.Status = 2, readyStatus()
		f := newFleet(t, cluster, db)
		f.settle(f.databases, "db")
		want := fleetv1.CommonStatus{Phase: fleetv1.Pending, ObservedGeneration: 2, Message: "waiting for Cluster cluster to be Ready"}
		if got := f.database().Status; got != want {
			t.Errorf("On a Cluster deleted %s, the Ready Database of generation 2 is %+v, want %+v.", when, got, want)
		}
	}
}

// A deleting Cluster keeps a Ready Database, but not after its Secret is deleted.
func TestADeletingClusterKeepsNoDatabaseWithoutItsSecret(t *testing.T) {
	db := databaseNaming("admin-password")
	db.Status = readyStatus()
	f := newFleet(t, deleting(readyCluster(), fleetv1.ClusterFinalizer), db)
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Secret admin-password") {
		t.Errorf("On a deleting Cluster, without its Secret, the Database is %+v.", got)
	}
}

func TestDatabaseWaitsForASecretWithAPassword(t *testing.T) {
	f := newFleet(t, readyCluster(), databaseNaming("admin-password"))
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Secret admin-password") {
		t.Fatalf("Without its Secret, the Database is %+v.", got)
	}
	secret := passwordSecret(map[string][]byte{"username": []byte("admin")})
	if err := f.c.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Secret admin-password to hold a password") {
		t.Fatalf("On a Secret without a password, the Database is %+v.", got)
	}
	secret.Data["password"] = []byte("p")
	if err := f.c.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	f.settle(f.databases, "db")
	if got := f.database().Status; got != readyStatus() {
		t.Fatalf("On a Secret with a password, the Database is %+v.", got)
	}
	f.delete(secret)
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Secret admin-password") {
		t.Errorf("After its Secret went, the Database is %+v.", got)
	}
}

func TestAFailedReadLeavesTheDatabaseAsItWas(t *testing.T) {
	for _, failed := range []client.Object{&fleetv1.Cluster{}, &corev1.Secret{}} {
		f := newFleet(t, readyCluster(), databaseNaming("admin-password"), passwordSecret(map[string][]byte{"password": []byte("p")}))
		f.reconcileOnce(f.databases, "db") // It adds its finalizer.
		f.databases.Client = interceptor.NewClient(f.c, interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if reflect.TypeOf(obj) == reflect.TypeOf(failed) {
					return apierrors.NewInternalError(errors.New("injected"))
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		_, err := f.databases.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "db"}})
		if db := f.database(); err == nil || db.Status != (fleetv1.CommonStatus{}) {
			t.Errorf("When reading its %T fails, the Database reconcile returns %v and writes %+v.", failed, err, db.Status)
		}
	}
}

func TestADatabaseNamingAnEmptySecretNameNeedsNoSecret(t *testing.T) {
	f := newFleet(t, readyCluster(), databaseNaming(""))
	f.settle(f.databases, "db")
	if got := f.database().Status; got != readyStatus() {
		t.Errorf("Naming no Secret, the Database is %+v, want Ready.", got)
	}
}

// heldSecret is the admin-password Secret, held by the in-use finalizer.
func heldSecret() *corev1.Secret {
	s := passwordSecret(nil)
	s.Finalizers = []string{fleetv1.SecretInUseFinalizer}
	return s
}

func TestASecretIsHeldWhileADatabaseNamesIt(t *testing.T) {
	f := newFleet(t, passwordSecret(nil), databaseNaming("admin-password"))
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; !slices.Equal(got, []string{fleetv1.SecretInUseFinalizer}) {
		t.Fatalf("While a Database names it, the Secret has finalizers %q.", got)
	}
	f.delete(f.secret())
	f.settle(f.secrets, "admin-password")
	if s := f.secret(); s.DeletionTimestamp.IsZero() || !slices.Equal(s.Finalizers, []string{fleetv1.SecretInUseFinalizer}) {
		t.Fatalf("While a Database names it, the deleted Secret has finalizers %q.", s.Finalizers)
	}
	f.delete(f.database())
	f.settle(f.secrets, "admin-password")
	if err := f.get("admin-password", &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("Once no Database names it, the deleted Secret gives %v, want not found.", err)
	}
}

func TestADeletingDatabaseStillHoldsItsSecret(t *testing.T) {
	f := newFleet(t, passwordSecret(nil), deleting(databaseNaming("admin-password"), fleetv1.DatabaseFinalizer))
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; !slices.Equal(got, []string{fleetv1.SecretInUseFinalizer}) {
		t.Errorf("While a deleting Database names it, the Secret has finalizers %q.", got)
	}
}

// A Secret takes no new finalizer once its deletion starts.
func TestADeletingSecretIsNotHeld(t *testing.T) {
	f := newFleet(t, deleting(passwordSecret(nil), "other.example/hold"), databaseNaming("admin-password"))
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; !slices.Equal(got, []string{"other.example/hold"}) {
		t.Errorf("The deleting Secret has finalizers %q, want only its own.", got)
	}
}

func TestASecretNoDatabaseNamesIsReleased(t *testing.T) {
	inOther := databaseNaming("admin-password")
	inOther.Namespace = "other"
	f := newFleet(t, heldSecret(), databaseNaming("another"), inOther)
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; len(got) != 0 {
		t.Errorf("With no Database in its namespace naming it, the Secret has finalizers %q.", got)
	}
}

// The cache can miss a new Database, so the release lists Databases past it.
func TestSecretReleaseSeesDatabasesTheCacheMisses(t *testing.T) {
	f := newFleet(t, heldSecret())
	f.secrets.APIReader = newClient(t, databaseNaming("admin-password"))
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; !slices.Equal(got, []string{fleetv1.SecretInUseFinalizer}) {
		t.Errorf("With a Database the cache misses, the Secret has finalizers %q.", got)
	}
}

// A stale cache can show the finalizer on a Secret that an earlier
// reconcile released.
func TestASecretGoneBeforeItsReleaseIsDone(t *testing.T) {
	f := newFleet(t, deleting(passwordSecret(nil), fleetv1.SecretInUseFinalizer))
	f.secrets.Client = interceptor.NewClient(f.c, interceptor.Funcs{
		Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
			return apierrors.NewNotFound(corev1.Resource("secrets"), obj.GetName())
		},
	})
	f.reconcileOnce(f.secrets, "admin-password")
}

func TestOnlyAReleaseListsDatabasesPastTheCache(t *testing.T) {
	for name, objects := range map[string][]client.Object{
		"an unheld Secret that no Database names": {passwordSecret(nil), databaseNaming("another")},
		"a held Secret that a Database names":     {heldSecret(), databaseNaming("admin-password")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFleet(t, objects...)
			f.secrets.APIReader = unserved{f.c, &fleetv1.DatabaseList{}, apierrors.NewInternalError(errors.New("injected"))}
			f.reconcileOnce(f.secrets, "admin-password")
		})
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

func TestUserWaitsForItsDatabaseToReportItsGenerationReady(t *testing.T) {
	db := databaseObj()
	db.Generation, db.Status = 2, readyStatus()
	f := newFleet(t, db, userObj())
	f.settle(f.users, "alice")
	user := &fleetv1.User{}
	if err := f.get("alice", user); err != nil || user.Status != pendingStatus("waiting for Database db to be Ready") {
		t.Errorf("On a Database that reports an older generation Ready, the User is %+v, %v.", user.Status, err)
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

// A change to a Cluster or a Secret requeues the Databases that name it, and
// a change to a Database its Users and its Secret.
func TestAChangeRequeuesWhatNamesIt(t *testing.T) {
	inOther := func(o client.Object) client.Object {
		o.SetNamespace("other")
		return o
	}
	elsewhere := &fleetv1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "elsewhere"}, Spec: fleetv1.DatabaseSpec{ClusterRef: "other"}}
	secured := databaseNaming("admin-password")
	secured.Name, secured.Spec.ClusterRef = "secured", "other"
	bob := &fleetv1.User{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bob"}, Spec: fleetv1.UserSpec{DatabaseRef: "other"}}
	f := newFleet(t, databaseObj(), userObj(), elsewhere, secured, bob, inOther(databaseNaming("admin-password")), inOther(userObj()))
	only := func(name string) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
	}
	if got := f.databases.databasesNaming(clusterRefField)(t.Context(), clusterObj()); !slices.Equal(got, only("db")) {
		t.Errorf("A Cluster's change requeues %v, want its Database.", got)
	}
	if got := f.databases.databasesNaming(passwordSecretRefField)(t.Context(), passwordSecret(nil)); !slices.Equal(got, only("secured")) {
		t.Errorf("A Secret's change requeues %v, want the Database that names it.", got)
	}
	if got := f.users.usersOf(t.Context(), databaseObj()); !slices.Equal(got, only("alice")) {
		t.Errorf("A Database's change requeues %v, want its User.", got)
	}
	if got := secretOf(t.Context(), databaseNaming("admin-password")); !slices.Equal(got, only("admin-password")) {
		t.Errorf("A Database's change requeues %v, want the Secret it names.", got)
	}
	if got := secretOf(t.Context(), databaseObj()); len(got) != 0 {
		t.Errorf("A change to a Database that names no Secret requeues %v.", got)
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
	for _, r := range []reconcile.Reconciler{f.clusters, f.databases, f.users, f.secrets} {
		f.reconcileOnce(r, "missing")
	}
}

func TestParseBugAcceptsOnlyKnownBugs(t *testing.T) {
	for _, id := range []int{0, 115, 116, 117, 118, 119, 120, 121, 122, 123, 124, 125, 126, 131, 132, 133, 135, 140, 141, 142, 150, 151, 152, 160, 161, 162, 170, 171, 172} {
		if b, err := ParseBug(id); int(b) != id || err != nil {
			t.Errorf("ParseBug(%d) = %v, %v", id, b, err)
		}
	}
	if _, err := ParseBug(100); err == nil {
		t.Error("ParseBug(100) succeeded, want an error.")
	}
}

func TestB120ReleasesAClusterWhileADatabaseNamesIt(t *testing.T) {
	f := newBuggyFleet(t, B120, clusterObj(), databaseObj())
	f.settle(f.clusters, "cluster")
	f.readyVolumes()
	f.settle(f.clusters, "cluster")
	f.delete(f.cluster())
	f.settle(f.clusters, "cluster")
	if err := f.get("cluster", &fleetv1.Cluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Under B120, with a Database, the Cluster gives %v, want not found.", err)
	}
}

func TestB121ReleasesADatabaseWhileAUserNamesIt(t *testing.T) {
	f := newBuggyFleet(t, B121, databaseObj(), userObj())
	f.settle(f.databases, "db")
	f.delete(f.database())
	f.settle(f.databases, "db")
	if err := f.get("db", &fleetv1.Database{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Under B121, with a User, the Database gives %v, want not found.", err)
	}
}

func TestB122RequeuesNoUserWhenItsDatabaseChanges(t *testing.T) {
	f := newBuggyFleet(t, B122, databaseObj(), userObj())
	if got := f.users.usersOf(t.Context(), databaseObj()); len(got) != 0 {
		t.Errorf("Under B122, a Database's change requeues %v, want no User.", got)
	}
}

func TestB123ReadiesADatabaseOnAPendingCluster(t *testing.T) {
	f := newFleet(t, clusterObj(), databaseObj())
	f.databases.Bug = B123
	f.settle(f.clusters, "cluster")
	f.settle(f.databases, "db")
	if db, c := f.database(), f.cluster(); db.Status != readyStatus() || c.Status.Phase == fleetv1.Ready {
		t.Errorf("Under B123, on Cluster %+v, the Database is %+v.", c.Status, db.Status)
	}
}

func TestB124ReadiesADatabaseWithoutItsSecret(t *testing.T) {
	f := newFleet(t, readyCluster(), databaseNaming("admin-password"))
	f.databases.Bug = B124
	f.settle(f.databases, "db")
	if got := f.database().Status; got != readyStatus() {
		t.Errorf("Under B124, without its Secret, the Database is %+v, want Ready.", got)
	}
}

func TestB125KeepsADatabaseReadyAfterItsSecretGoes(t *testing.T) {
	f := newFleet(t, readyCluster(), databaseNaming("admin-password"))
	f.databases.Bug = B125
	f.settle(f.databases, "db")
	if got := f.database().Status; got != pendingStatus("waiting for Secret admin-password") {
		t.Fatalf("Under B125, before its Secret, the Database is %+v.", got)
	}
	secret := passwordSecret(map[string][]byte{"password": []byte("p")})
	if err := f.c.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	f.settle(f.databases, "db")
	f.delete(secret)
	f.settle(f.databases, "db")
	if got := f.database().Status; got != readyStatus() {
		t.Errorf("Under B125, after its Secret went, the Database is %+v, want Ready.", got)
	}
}

func TestB126NeverHoldsASecret(t *testing.T) {
	f := newBuggyFleet(t, B126, passwordSecret(nil), databaseNaming("admin-password"))
	f.settle(f.secrets, "admin-password")
	if got := f.secret().Finalizers; len(got) != 0 {
		t.Errorf("Under B126, while a Database names it, the Secret has finalizers %q.", got)
	}
}
