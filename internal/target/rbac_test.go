package target_test

import (
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestLoadRBACRolesAndClusterRoles(t *testing.T) {
	roleYAML := `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: widget-role
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "create", "update", "patch", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: widget-clusterrole
rules:
  - apiGroups: ["toy.reconciler-fuzzer"]
    resources: ["widgets", "widgets/status"]
    verbs: ["get", "list", "watch", "update", "patch"]
`
	path := writeTarget(t, minimalTarget+"rbac:\n  - rbac/role.yaml\n", map[string]string{
		"widget.yaml":    sampleWidget,
		"rbac/role.yaml": roleYAML,
	})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected rbac: %v", err)
	}
	if len(loaded.Roles) != 1 || loaded.Roles[0].Name != "widget-role" {
		t.Errorf("Load read %d roles, want 1 named widget-role.", len(loaded.Roles))
	}
	if len(loaded.ClusterRoles) != 1 || loaded.ClusterRoles[0].Name != "widget-clusterrole" {
		t.Errorf("Load read %d cluster roles, want 1 named widget-clusterrole.", len(loaded.ClusterRoles))
	}
}

func TestLoadNoRBACReturnsNil(t *testing.T) {
	path := writeTarget(t, minimalTarget, map[string]string{"widget.yaml": sampleWidget})
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Roles != nil || loaded.ClusterRoles != nil {
		t.Errorf("Load read rbac %v %v from a target that declares none.", loaded.Roles, loaded.ClusterRoles)
	}
}

func TestLoadRBACRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		yaml  string
		extra map[string]string
		wants []string
	}{
		{"a directory", minimalTarget + "rbac:\n  - rbac/\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n"},
			[]string{"rbac", "directory"}},
		{"a RoleBinding", minimalTarget + "rbac:\n  - rbac/rb.yaml\n",
			map[string]string{"rbac/rb.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: RoleBinding\nmetadata:\n  name: rb\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: Role\n  name: r\n"},
			[]string{"rbac/rb.yaml", "RoleBinding"}},
		{"a ClusterRoleBinding", minimalTarget + "rbac:\n  - rbac/crb.yaml\n",
			map[string]string{"rbac/crb.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: crb\nroleRef:\n  apiGroup: rbac.authorization.k8s.io\n  kind: ClusterRole\n  name: cr\n"},
			[]string{"rbac/crb.yaml", "ClusterRoleBinding"}},
		{"a ServiceAccount", minimalTarget + "rbac:\n  - rbac/sa.yaml\n",
			map[string]string{"rbac/sa.yaml": "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: sa\n"},
			[]string{"rbac/sa.yaml", "ServiceAccount"}},
		{"wrong apiVersion", minimalTarget + "rbac:\n  - rbac/r.yaml\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1beta1\nkind: Role\nmetadata:\n  name: r\n"},
			[]string{"rbac/r.yaml", "apiVersion"}},
		{"a Role with a namespace", minimalTarget + "rbac:\n  - rbac/r.yaml\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n  namespace: default\n"},
			[]string{"rbac/r.yaml", "namespace"}},
		{"an aggregationRule", minimalTarget + "rbac:\n  - rbac/cr.yaml\n",
			map[string]string{"rbac/cr.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: cr\naggregationRule:\n  clusterRoleSelectors:\n    - matchLabels: {app: x}\n"},
			[]string{"rbac/cr.yaml", "aggregationRule"}},
		{"an unknown field", minimalTarget + "rbac:\n  - rbac/r.yaml\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\nextra: true\n"},
			[]string{"rbac/r.yaml"}},
		{"a missing name", minimalTarget + "rbac:\n  - rbac/r.yaml\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata: {}\n"},
			[]string{"rbac/r.yaml", "name"}},
		{"duplicate names", minimalTarget + "rbac:\n  - rbac/r.yaml\n",
			map[string]string{"rbac/r.yaml": "apiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n---\napiVersion: rbac.authorization.k8s.io/v1\nkind: Role\nmetadata:\n  name: r\n"},
			[]string{"rbac/r.yaml", "duplicate", "r"}},
		{"a missing file", minimalTarget + "rbac:\n  - rbac/missing.yaml\n",
			nil,
			[]string{"rbac", "missing.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]string{"widget.yaml": sampleWidget}
			for k, v := range tc.extra {
				extra[k] = v
			}
			path := writeTarget(t, tc.yaml, extra)

			_, err := target.Load(path)
			if err == nil {
				t.Fatal("Load accepted invalid rbac.")
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load reported %q, which does not name %q.", err, want)
				}
			}
		})
	}
}

// Verify that the roles carry the rules from the YAML file.
func TestLoadRBACRulesArePopulated(t *testing.T) {
	roleYAML := `apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: widget-role
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list"]
`
	path := writeTarget(t, minimalTarget+"rbac:\n  - rbac/role.yaml\n", map[string]string{
		"widget.yaml":    sampleWidget,
		"rbac/role.yaml": roleYAML,
	})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected rbac: %v", err)
	}
	if len(loaded.Roles) != 1 {
		t.Fatalf("Load read %d roles, want 1.", len(loaded.Roles))
	}
	rules := loaded.Roles[0].Rules
	if len(rules) != 1 {
		t.Fatalf("Role has %d rules, want 1.", len(rules))
	}
	if !slices.Equal(rules[0].Verbs, []string{"get", "list"}) {
		t.Errorf("Role rule verbs are %v, want [get list].", rules[0].Verbs)
	}
}

// Verify the Target fields are typed correctly.
var _ []rbacv1.Role = target.Target{}.Roles
var _ []rbacv1.ClusterRole = target.Target{}.ClusterRoles
