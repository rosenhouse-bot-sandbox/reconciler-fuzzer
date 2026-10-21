package controller

import (
	"maps"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

const mirror = "shared"

func newGrant(level int32) *toyv1.Grant {
	return &toyv1.Grant{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", Generation: 1, UID: "grant-uid"},
		Spec:       toyv1.GrantSpec{Level: level},
	}
}

func deletingGrant() *toyv1.Grant {
	grant := newGrant(1)
	grant.Finalizers = []string{GrantFinalizer}
	deletedAt := metav1.Now()
	grant.DeletionTimestamp = &deletedAt
	return grant
}

func grantConfig(label string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: mirror, Name: GrantConfig}, Data: map[string]string{"label": label}}
}

func grantFixture(t *testing.T, bug Bug, objects ...client.Object) *GrantReconciler {
	t.Helper()
	c := fakeClient(t, objects...)
	return &GrantReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Bug: bug, MirrorNamespace: mirror}
}

func mustReconcileGrant(t *testing.T, r *GrantReconciler, grant *toyv1.Grant) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(grant)}); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
}

var (
	grantClusterRoleKey = client.ObjectKey{Name: "grant-ns-g"}
	grantConfigMapKey   = client.ObjectKey{Namespace: mirror, Name: "ns.g"}
)

func grantChildren(t *testing.T, reader client.Reader) (*rbacv1.ClusterRole, *corev1.ConfigMap) {
	t.Helper()
	clusterRole, configMap := &rbacv1.ClusterRole{}, &corev1.ConfigMap{}
	if err := reader.Get(t.Context(), grantClusterRoleKey, clusterRole); err != nil {
		t.Fatal(err)
	}
	if err := reader.Get(t.Context(), grantConfigMapKey, configMap); err != nil {
		t.Fatal(err)
	}
	return clusterRole, configMap
}

func gone(t *testing.T, reader client.Reader, key client.ObjectKey, object client.Object) bool {
	t.Helper()
	err := reader.Get(t.Context(), key, object)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err != nil
}

func readGrant(t *testing.T, reader client.Reader, grant *toyv1.Grant) *toyv1.Grant {
	t.Helper()
	observed := &toyv1.Grant{}
	if err := reader.Get(t.Context(), client.ObjectKeyFromObject(grant), observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func TestAGrantHasALabelledClusterRoleAndMirrorConfigMap(t *testing.T) {
	grant := newGrant(2)
	r := grantFixture(t, 0, grant, grantConfig("red"))
	mustReconcileGrant(t, r, grant)

	clusterRole, configMap := grantChildren(t, r)
	for _, child := range []client.Object{clusterRole, configMap} {
		if uid := child.GetLabels()[GrantUIDLabel]; uid != "grant-uid" {
			t.Errorf("%T %s has grant-uid label %q, want the Grant's UID.", child, child.GetName(), uid)
		}
		if owners := child.GetOwnerReferences(); len(owners) != 0 {
			t.Errorf("%T %s has owners %v, want none.", child, child.GetName(), owners)
		}
	}
	if want := map[string]string{"label": "red", "level": "2"}; !maps.Equal(configMap.Data, want) {
		t.Errorf("The ConfigMap holds %v, want %v.", configMap.Data, want)
	}
	observed := readGrant(t, r, grant)
	if !controllerutil.ContainsFinalizer(observed, GrantFinalizer) {
		t.Errorf("The Grant has finalizers %v, want %s.", observed.Finalizers, GrantFinalizer)
	}
	if observed.Status != (toyv1.ReadyStatus{ObservedGeneration: 1, Ready: true}) {
		t.Errorf("The Grant has status %+v, want ready at generation 1.", observed.Status)
	}
}

func TestAGrantsConfigMapFollowsItsLevelAndGrantConfig(t *testing.T) {
	grant, config := newGrant(1), grantConfig("red")
	r := grantFixture(t, 0, grant, config)
	mustReconcileGrant(t, r, grant)

	grant = readGrant(t, r, grant)
	grant.Spec.Level = 3
	if err := r.Update(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	config.Data["label"] = "blue"
	if err := r.Update(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	mustReconcileGrant(t, r, grant)

	if _, configMap := grantChildren(t, r); !maps.Equal(configMap.Data, map[string]string{"label": "blue", "level": "3"}) {
		t.Errorf("The ConfigMap holds %v, want label blue and level 3.", configMap.Data)
	}
}

func TestADeletedGrantKeepsItsFinalizerUntilBothChildrenAreGone(t *testing.T) {
	grant := deletingGrant()
	held := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: mirror, Name: "ns.g", Finalizers: []string{"test/hold"}}}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "grant-ns-g"}}
	r := grantFixture(t, 0, grant, held, clusterRole)

	mustReconcileGrant(t, r, grant)
	if !gone(t, r, grantClusterRoleKey, &rbacv1.ClusterRole{}) {
		t.Error("The cleanup kept the ClusterRole.")
	}
	if gone(t, r, client.ObjectKeyFromObject(grant), &toyv1.Grant{}) {
		t.Fatal("The cleanup released the Grant while its ConfigMap remained.")
	}

	if err := r.Get(t.Context(), grantConfigMapKey, held); err != nil {
		t.Fatal(err)
	}
	held.Finalizers = nil
	if err := r.Update(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	mustReconcileGrant(t, r, grant)
	if !gone(t, r, client.ObjectKeyFromObject(grant), &toyv1.Grant{}) {
		t.Error("The cleanup kept the Grant after both children were gone.")
	}
}

func TestAGrantsChildrenAndGrantConfigEnqueueGrants(t *testing.T) {
	grant, other := newGrant(0), newGrant(0)
	other.Name, other.UID = "h", "other-uid"
	r := grantFixture(t, 0, grant, other)
	labelled := func(uid string) client.Object {
		return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "x", Labels: map[string]string{GrantUIDLabel: uid}}}
	}

	for _, testCase := range []struct {
		object client.Object
		want   []string
	}{
		{labelled("grant-uid"), []string{"g"}},
		{labelled("unknown"), nil},
		{&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "unlabelled"}}, nil},
		{grantConfig("red"), []string{"g", "h"}},
		{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: GrantConfig}}, nil},
	} {
		var names []string
		for _, request := range r.grantsFor(t.Context(), testCase.object) {
			names = append(names, request.Name)
		}
		if !reflect.DeepEqual(names, testCase.want) {
			t.Errorf("%s/%s enqueues %v, want %v.", testCase.object.GetNamespace(), testCase.object.GetName(), names, testCase.want)
		}
	}
}

func TestB27sCleanupLeavesTheMirrorConfigMap(t *testing.T) {
	grant := newGrant(1)
	r := grantFixture(t, B27, grant)
	mustReconcileGrant(t, r, grant)
	deleteGrant(t, r, grant)
	mustReconcileGrant(t, r, grant)

	if gone(t, r, grantConfigMapKey, &corev1.ConfigMap{}) {
		t.Error("The cleanup deleted the ConfigMap.")
	}
	if !gone(t, r, grantClusterRoleKey, &rbacv1.ClusterRole{}) {
		t.Error("The cleanup kept the ClusterRole.")
	}
	if !gone(t, r, client.ObjectKeyFromObject(grant), &toyv1.Grant{}) {
		t.Error("The cleanup kept the Grant.")
	}
}

func TestB28LeavesTheClusterRoleToAGarbageCollectorThatNeverComes(t *testing.T) {
	grant := newGrant(1)
	r := grantFixture(t, B28, grant)
	mustReconcileGrant(t, r, grant)

	clusterRole, configMap := grantChildren(t, r)
	if !blockingControllerOf(clusterRole, grant) {
		t.Errorf("The ClusterRole has owners %v, want the Grant.", clusterRole.OwnerReferences)
	}
	if owners := configMap.OwnerReferences; len(owners) != 0 {
		t.Errorf("The ConfigMap has owners %v, want none.", owners)
	}

	deleteGrant(t, r, grant)
	mustReconcileGrant(t, r, grant)
	if gone(t, r, grantClusterRoleKey, &rbacv1.ClusterRole{}) {
		t.Error("The cleanup deleted the ClusterRole.")
	}
	if !gone(t, r, grantConfigMapKey, &corev1.ConfigMap{}) {
		t.Error("The cleanup kept the ConfigMap.")
	}
	if !gone(t, r, client.ObjectKeyFromObject(grant), &toyv1.Grant{}) {
		t.Error("The cleanup kept the Grant.")
	}
}

func deleteGrant(t *testing.T, r *GrantReconciler, grant *toyv1.Grant) {
	t.Helper()
	if err := r.Delete(t.Context(), readGrant(t, r, grant)); err != nil {
		t.Fatal(err)
	}
}
