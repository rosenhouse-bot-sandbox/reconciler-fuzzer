//go:build envtest

package cluster_test

import (
	"os"
	"path/filepath"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

func TestIdentityTokenReachesTheAPIServer(t *testing.T) {
	c, err := cluster.Start(cluster.Options{CRDPaths: []string{thingCRDDir(t)}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop() })

	admin, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building admin client: %v", err)
	}
	namespace := newNamespace(t, admin)

	role := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "thing-reader"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"test.reconciler-fuzzer"},
			Resources: []string{"things"},
			Verbs:     []string{"get", "list"},
		}},
	}
	id, err := cluster.NewIdentity(cluster.IdentityOptions{
		Config:    c.Config(),
		Namespace: namespace,
		Roles:     []rbacv1.Role{role},
	})
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	t.Cleanup(func() { _ = id.Delete(t.Context()) })

	client, err := kubernetes.NewForConfig(id.Config())
	if err != nil {
		t.Fatalf("Building SA client: %v", err)
	}

	// The SA can list things in its namespace.
	_, err = client.CoreV1().ConfigMaps(namespace).List(t.Context(), metav1.ListOptions{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("Listing ConfigMaps returned %v, want Forbidden.", err)
	}
}

func TestIdentityClusterRoleBindsClusterWide(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(thingCRD), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := cluster.Start(cluster.Options{CRDPaths: []string{dir}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = c.Stop() })

	admin, err := kubernetes.NewForConfig(c.Config())
	if err != nil {
		t.Fatalf("Building admin client: %v", err)
	}
	namespace := newNamespace(t, admin)

	cr := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "thing-viewer"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"test.reconciler-fuzzer"},
			Resources: []string{"things"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
	id, err := cluster.NewIdentity(cluster.IdentityOptions{
		Config:       c.Config(),
		Namespace:    namespace,
		ClusterRoles: []rbacv1.ClusterRole{cr},
	})
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	t.Cleanup(func() { _ = id.Delete(t.Context()) })

	// The per-run ClusterRole copy exists.
	wantName := "thing-viewer-" + namespace
	_, err = admin.RbacV1().ClusterRoles().Get(t.Context(), wantName, metav1.GetOptions{})
	if err != nil {
		t.Errorf("The per-run ClusterRole %s does not exist: %v", wantName, err)
	}

	// Delete cleans up the cluster-scoped resources.
	if err := id.Delete(t.Context()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = admin.RbacV1().ClusterRoles().Get(t.Context(), wantName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("After Delete, reading the ClusterRole %s returned %v, want NotFound.", wantName, err)
	}
	_, err = admin.RbacV1().ClusterRoleBindings().Get(t.Context(), wantName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("After Delete, reading the ClusterRoleBinding %s returned %v, want NotFound.", wantName, err)
	}
}
