package cluster_test

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

func TestNewIdentityRequiresANamespace(t *testing.T) {
	_, err := cluster.NewIdentity(cluster.IdentityOptions{})
	if err == nil {
		t.Fatal("NewIdentity accepted an empty namespace.")
	}
}

func TestNewIdentityRequiresAConfig(t *testing.T) {
	_, err := cluster.NewIdentity(cluster.IdentityOptions{Namespace: "test"})
	if err == nil {
		t.Fatal("NewIdentity accepted a nil Config.")
	}
}

// The envtest tests exercise the full token and RBAC flow.
var _ *cluster.Identity

// Verify IdentityOptions has the expected fields.
var _ = cluster.IdentityOptions{
	Roles:        []rbacv1.Role{},
	ClusterRoles: []rbacv1.ClusterRole{},
}
