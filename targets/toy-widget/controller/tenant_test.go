package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	toyv1 "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/targets/toy-widget/api/v1"
)

var baseRules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}

func tenantBase() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: TenantBase}, Rules: baseRules}
}

func newTenant(quota int32) *toyv1.Tenant {
	return &toyv1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Generation: 1, UID: "tenant-uid"},
		Spec:       toyv1.TenantSpec{Quota: quota},
	}
}

func deletingTenant() *toyv1.Tenant {
	tenant := newTenant(1)
	tenant.Finalizers = []string{metav1.FinalizerDeleteDependents}
	deletedAt := metav1.Now()
	tenant.DeletionTimestamp = &deletedAt
	return tenant
}

// fakeClient is a fake API server holding objects.
func fakeClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&toyv1.Tenant{}, &toyv1.Grant{}).
		WithObjects(objects...).
		Build()
}

func tenantFixture(t *testing.T, bug Bug, objects ...client.Object) *TenantReconciler {
	t.Helper()
	c := fakeClient(t, objects...)
	return &TenantReconciler{Client: c, Scheme: c.Scheme(), Bug: bug}
}

func mustReconcileTenant(t *testing.T, r *TenantReconciler, tenant *toyv1.Tenant) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tenant)}); err != nil {
		t.Fatalf("Reconcile returned an error: %v", err)
	}
}

func tenantChildren(t *testing.T, reader client.Reader) (*corev1.Namespace, *rbacv1.ClusterRole) {
	t.Helper()
	namespace, clusterRole := &corev1.Namespace{}, &rbacv1.ClusterRole{}
	if err := reader.Get(t.Context(), client.ObjectKey{Name: "tenant-t"}, namespace); err != nil {
		t.Fatal(err)
	}
	if err := reader.Get(t.Context(), client.ObjectKey{Name: "tenant-t"}, clusterRole); err != nil {
		t.Fatal(err)
	}
	return namespace, clusterRole
}

func readTenant(t *testing.T, reader client.Reader, tenant *toyv1.Tenant) *toyv1.Tenant {
	t.Helper()
	observed := &toyv1.Tenant{}
	if err := reader.Get(t.Context(), client.ObjectKeyFromObject(tenant), observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

// blockingControllerOf says whether object's controller is the owner, with
// blockOwnerDeletion.
func blockingControllerOf(object client.Object, owner client.Object) bool {
	ref := metav1.GetControllerOf(object)
	return ref != nil && ref.UID == owner.GetUID() && ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion
}

func TestATenantOwnsANamespaceAndAClusterRoleLabelledWithItsQuota(t *testing.T) {
	tenant := newTenant(2)
	r := tenantFixture(t, 0, tenant, tenantBase())
	mustReconcileTenant(t, r, tenant)

	namespace, clusterRole := tenantChildren(t, r)
	for _, child := range []client.Object{namespace, clusterRole} {
		if !blockingControllerOf(child, tenant) {
			t.Errorf("%T %s has owners %v, want a blocking controller reference to the Tenant.", child, child.GetName(), child.GetOwnerReferences())
		}
		if quota := child.GetLabels()[QuotaLabel]; quota != "2" {
			t.Errorf("%T %s has quota label %q, want 2.", child, child.GetName(), quota)
		}
	}
	if !reflect.DeepEqual(clusterRole.Rules, baseRules) {
		t.Errorf("The ClusterRole has rules %v, want tenant-base's %v.", clusterRole.Rules, baseRules)
	}
	if status := readTenant(t, r, tenant).Status; status != (toyv1.ReadyStatus{ObservedGeneration: 1, Ready: true}) {
		t.Errorf("The Tenant has status %+v, want ready at generation 1.", status)
	}
}

func TestATenantsChildrenFollowItsQuotaAndTenantBase(t *testing.T) {
	tenant, base := newTenant(1), tenantBase()
	r := tenantFixture(t, 0, tenant, base)
	mustReconcileTenant(t, r, tenant)

	tenant = readTenant(t, r, tenant)
	tenant.Spec.Quota, tenant.Generation = 3, 2
	if err := r.Update(t.Context(), tenant); err != nil {
		t.Fatal(err)
	}
	base.Rules = []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"list"}}}
	if err := r.Update(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	mustReconcileTenant(t, r, tenant)

	namespace, clusterRole := tenantChildren(t, r)
	if namespace.Labels[QuotaLabel] != "3" || clusterRole.Labels[QuotaLabel] != "3" {
		t.Errorf("The children have labels %v and %v, want quota 3.", namespace.Labels, clusterRole.Labels)
	}
	if !reflect.DeepEqual(clusterRole.Rules, base.Rules) {
		t.Errorf("The ClusterRole has rules %v, want tenant-base's new %v.", clusterRole.Rules, base.Rules)
	}
}

func TestATenantIsNotReadyWhileItsNamespaceTerminates(t *testing.T) {
	tenant := newTenant(1)
	deletedAt := metav1.Now()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-t", Finalizers: []string{"kubernetes"}, DeletionTimestamp: &deletedAt}}
	r := tenantFixture(t, 0, tenant, tenantBase(), namespace)
	mustReconcileTenant(t, r, tenant)

	if readTenant(t, r, tenant).Status.Ready {
		t.Error("The Tenant is ready while its Namespace terminates.")
	}
}

func TestADeletedTenantIsLeftAlone(t *testing.T) {
	tenant := deletingTenant()
	r := tenantFixture(t, 0, tenant, tenantBase())
	mustReconcileTenant(t, r, tenant)

	err := r.Get(t.Context(), client.ObjectKey{Name: "tenant-t"}, &rbacv1.ClusterRole{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Reading the ClusterRole of a deleted Tenant returned %v, want NotFound.", err)
	}
	if status := readTenant(t, r, tenant).Status; status != (toyv1.ReadyStatus{}) {
		t.Errorf("A deleted Tenant got status %+v.", status)
	}
}

func TestTenantBaseEnqueuesEveryTenant(t *testing.T) {
	first, second := newTenant(0), newTenant(0)
	second.Name, second.UID = "u", "other-uid"
	r := tenantFixture(t, 0, first, second, tenantBase())

	requests := r.tenantsUsing(t.Context(), tenantBase())
	if want := []ctrl.Request{{NamespacedName: client.ObjectKey{Name: "t"}}, {NamespacedName: client.ObjectKey{Name: "u"}}}; !reflect.DeepEqual(requests, want) {
		t.Errorf("tenant-base enqueues %v, want %v.", requests, want)
	}
	other := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "other"}}
	if requests := r.tenantsUsing(t.Context(), other); len(requests) != 0 {
		t.Errorf("ClusterRole other enqueues %v, want none.", requests)
	}
}

func TestB26GivesTheClusterRoleNoOwner(t *testing.T) {
	tenant := newTenant(1)
	r := tenantFixture(t, B26, tenant, tenantBase())
	mustReconcileTenant(t, r, tenant)

	namespace, clusterRole := tenantChildren(t, r)
	if owners := clusterRole.OwnerReferences; len(owners) != 0 {
		t.Errorf("The ClusterRole has owners %v, want none.", owners)
	}
	if !blockingControllerOf(namespace, tenant) {
		t.Errorf("The Namespace has owners %v, want the Tenant.", namespace.OwnerReferences)
	}
}

func TestATenantHoldsItsChildrenAndReleasesThemWhenDeleted(t *testing.T) {
	tenant := newTenant(1)
	r := tenantFixture(t, 0, tenant, tenantBase())
	mustReconcileTenant(t, r, tenant)

	namespace, clusterRole := tenantChildren(t, r)
	for _, child := range []client.Object{namespace, clusterRole} {
		if !controllerutil.ContainsFinalizer(child, ChildFinalizer) {
			t.Errorf("%T %s has finalizers %v, want %s.", child, child.GetName(), child.GetFinalizers(), ChildFinalizer)
		}
	}
	if err := r.Delete(t.Context(), clusterRole); err != nil {
		t.Fatal(err)
	}
	mustReconcileTenant(t, r, tenant)
	if !gone(t, r, client.ObjectKey{Name: "tenant-t"}, &rbacv1.ClusterRole{}) {
		t.Error("A live Tenant's reconcile kept its deleted ClusterRole.")
	}
}

// heldChildren are a Tenant's children, being deleted and held.
func heldChildren() []client.Object {
	deletedAt := metav1.Now()
	meta := metav1.ObjectMeta{Name: "tenant-t", Finalizers: []string{ChildFinalizer}, DeletionTimestamp: &deletedAt}
	return []client.Object{&corev1.Namespace{ObjectMeta: meta}, &rbacv1.ClusterRole{ObjectMeta: meta}}
}

func TestReleasingTheChildren(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bug      Bug
		tenant   []client.Object
		released bool
	}{
		{"B0 with the Tenant gone", 0, nil, true},
		{"B0 with the Tenant being deleted", 0, []client.Object{deletingTenant()}, true},
		{"B29 with the Tenant gone", B29, nil, true},
		{"B29 with the Tenant being deleted", B29, []client.Object{deletingTenant()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tenantFixture(t, tc.bug, append(heldChildren(), tc.tenant...)...)
			mustReconcileTenant(t, r, newTenant(1))
			for _, child := range heldChildren() {
				if released := gone(t, r, client.ObjectKeyFromObject(child), child); released != tc.released {
					t.Errorf("%T released: %v, want %v.", child, released, tc.released)
				}
			}
		})
	}
}
