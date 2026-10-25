package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

// referencesHead lists four kinds of group toy, whose CRDs writeReferences
// writes, beside a model and two fakes. Tenant is cluster-scoped.
const referencesHead = `launch: {binary: bin/controller, rbac: [role.yaml]}
crds: [crd.yaml, crds]
fixtures: [fixture.yaml]
models: [{kind: toy/v1/Volume, after: 1s}]
systems:
  - {name: cloud, api: crud, process: {command: [x]}, ready: "self.state == 'ready'"}
  - {name: other, api: crud, process: {command: [x]}}
primaries:
  - {kind: toy/v1/Cluster, sample: sample.yaml, ready: self.status.ready, owns: [{kind: v1/ConfigMap, name: self.metadata.name}]}
  - {kind: toy/v1/Database, sample: sample.yaml, ready: "true"}
  - {kind: toy/v1/User, sample: sample.yaml, ready: "true"}
  - {kind: toy/v1/Tenant, sample: sample.yaml, ready: "true"}
`

// writeReferences writes referencesHead, then rest, with the CRDs of
// Cluster and Database in crd.yaml and of User, Tenant and Volume in crds/.
func writeReferences(t *testing.T, rest string) string {
	t.Helper()
	path := writeConfig(t, referencesHead+rest)
	dir := filepath.Dir(path)
	if err := os.Mkdir(filepath.Join(dir, "crds"), 0o755); err != nil {
		t.Fatal(err)
	}
	for file, content := range map[string]string{
		"crd.yaml":                crd("Cluster", "Namespaced") + "---\n" + crd("Database", "Namespaced"),
		"crds/more.yaml":          crd("User", "Namespaced") + "---\n" + crd("Tenant", "Cluster") + "---\n" + crd("Volume", "Namespaced"),
		"crds/kustomization.yaml": "resources: [more.yaml]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func crd(kind, scope string) string {
	plural := strings.ToLower(kind) + "s"
	return fmt.Sprintf(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: %s.toy}
spec:
  group: toy
  names: {kind: %s, plural: %s}
  scope: %s
  versions: [{name: v1, served: true, storage: true}]
`, plural, kind, plural, scope)
}

func toyKind(group, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: "v1", Kind: kind}
}

func TestReferencesLoad(t *testing.T) {
	c, err := config.Load(writeReferences(t, `references:
  - {from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef}
  - {from: toy/v1/Database, to: v1/Secret, name: spec.passwordSecretRef.name, ready: has(self.data.password), required: false, track: 2s}
  - {from: toy/v1/User, to: toy/v1/Volume, name: "spec.volumes[*].name"}
  - {from: toy/v1/Tenant, to: v1/Secret, name: spec.secretRef.name, namespace: spec.secretRef.namespace, ready: "true"}
  - {from: toy/v1/User, to: "cloud:buckets", name: spec.bucket}
  - {from: toy/v1/Tenant, to: v1/Namespace, name: spec.namespace, ready: "true"}
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Reference{
		{From: toyKind("toy", "Database"), To: toyKind("toy", "Cluster"), Namespaced: true, Name: "spec.clusterRef", Ready: "self.status.ready", Required: true},
		{From: toyKind("toy", "Database"), To: toyKind("", "Secret"), Namespaced: true, Name: "spec.passwordSecretRef.name", Ready: "has(self.data.password)", Track: 2 * time.Second},
		{From: toyKind("toy", "User"), To: toyKind("toy", "Volume"), Namespaced: true, Name: "spec.volumes[*].name", Ready: config.ReadyConditionCEL, Required: true},
		{From: toyKind("toy", "Tenant"), To: toyKind("", "Secret"), Namespaced: true, Name: "spec.secretRef.name", Namespace: "spec.secretRef.namespace", Ready: "true", Required: true},
		{From: toyKind("toy", "User"), System: "cloud", Collection: "buckets", Name: "spec.bucket", Ready: "self.state == 'ready'", Required: true},
		{From: toyKind("toy", "Tenant"), To: toyKind("", "Namespace"), Name: "spec.namespace", Ready: "true", Required: true},
	}
	if !reflect.DeepEqual(c.References, want) {
		t.Errorf("References =\n%+v\nwant\n%+v", c.References, want)
	}
}

func TestReferencesRefusals(t *testing.T) {
	for name, c := range map[string]struct{ references, want string }{
		"no from":                         {"{to: v1/Secret, name: spec.s, ready: \"true\"}", "references[0].from is required"},
		"a malformed from":                {"{from: Database, to: v1/Secret, name: spec.s, ready: \"true\"}", "references[0].from"},
		"an unlisted from":                {"{from: toy/v1/Gadget, to: v1/Secret, name: spec.s, ready: \"true\"}", "references[0].from"},
		"no to":                           {"{from: toy/v1/Database, name: spec.s, ready: \"true\"}", "references[0].to is required"},
		"a malformed to":                  {"{from: toy/v1/Database, to: Secret, name: spec.s, ready: \"true\"}", "references[0].to"},
		"a to of unknown scope":           {"{from: toy/v1/Database, to: example.com/v1/Widget, name: spec.w, ready: \"true\"}", "references[0].to"},
		"a core kind's name in a group":   {"{from: toy/v1/Database, to: example.com/v1/Secret, name: spec.s, ready: \"true\"}", "references[0].to"},
		"a to of an unknown system":       {"{from: toy/v1/Database, to: \"nope:buckets\", name: spec.b, ready: \"true\"}", "references[0].to"},
		"a malformed collection":          {"{from: toy/v1/Database, to: \"cloud:Buckets\", name: spec.b}", "references[0].to"},
		"a to the from entry owns":        {"{from: toy/v1/Cluster, to: v1/ConfigMap, name: spec.config, ready: \"true\"}", "references[0].to"},
		"an unlisted to without ready":    {"{from: toy/v1/Database, to: v1/Secret, name: spec.s}", "references[0].ready"},
		"a fake to without ready":         {"{from: toy/v1/Database, to: \"other:buckets\", name: spec.b}", "references[0].ready"},
		"a ready that does not compile":   {"{from: toy/v1/Database, to: v1/Secret, name: spec.s, ready: \"self.(\"}", "references[0].ready"},
		"a ready that is not bool":        {"{from: toy/v1/Database, to: v1/Secret, name: spec.s, ready: \"'x'\"}", "references[0].ready"},
		"no name":                         {"{from: toy/v1/Database, to: toy/v1/Cluster}", "references[0].name is required"},
		"a name with an index":            {"{from: toy/v1/Database, to: toy/v1/Cluster, name: \"spec.refs[0].name\"}", "references[0].name"},
		"a name with an empty segment":    {"{from: toy/v1/Database, to: toy/v1/Cluster, name: spec..name}", "references[0].name"},
		"a namespace on a cluster scope":  {"{from: toy/v1/Database, to: v1/Namespace, name: spec.ns, namespace: spec.nsns, ready: \"true\"}", "references[0].namespace"},
		"a namespace on a fake":           {"{from: toy/v1/Database, to: \"cloud:buckets\", name: spec.b, namespace: spec.n}", "references[0].namespace"},
		"a cluster-scoped from":           {"{from: toy/v1/Tenant, to: v1/Secret, name: spec.s, ready: \"true\"}", "references[0].namespace"},
		"a namespace of another list":     {"{from: toy/v1/Database, to: v1/Secret, name: \"spec.keys[*].name\", namespace: \"spec.other[*].namespace\", ready: \"true\"}", "references[0].namespace"},
		"a namespace list without a name": {"{from: toy/v1/Database, to: v1/Secret, name: spec.s, namespace: \"spec.keys[*].namespace\", ready: \"true\"}", "references[0].namespace"},
		"a namespace list below the name": {"{from: toy/v1/Database, to: v1/Secret, name: spec.s, namespace: \"spec.a.keys[*].namespace\", ready: \"true\"}", "references[0].namespace"},
		"a namespace with an empty field": {"{from: toy/v1/Database, to: v1/Secret, name: spec.s, namespace: spec..ns, ready: \"true\"}", "references[0].namespace"},
		"a track of zero":                 {"{from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef, track: 0s}", "references[0].track"},
		"a second entry of one name":      {"{from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef}\n  - {from: toy/v1/Database, to: toy/v1/User, name: spec.clusterRef}", "references[1].name"},
		"an unknown key":                  {"{from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef, onDelete: block}", "references[0].onDelete is not a key; references[0] takes from, to, name, namespace, ready, required and track"},
	} {
		_, err := config.Load(writeReferences(t, "references:\n  - "+c.references+"\n"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want an error that names %s.", name, err, c.want)
		}
	}
}

// TestAFromKindOfUnknownScopeNeedsNoNamespace loads a reference from
// Database, whose CRD crds lacks, to a namespaced kind.
func TestAFromKindOfUnknownScopeNeedsNoNamespace(t *testing.T) {
	path := writeReferences(t, "references: [{from: toy/v1/Database, to: v1/Secret, name: spec.s, ready: \"true\"}]\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "crd.yaml"), []byte(crd("Cluster", "Namespaced")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("Load = %v, want no namespace needed for a from kind that crds do not describe.", err)
	}
}

func TestAConfigWithoutReferencesNeedsNoCRDs(t *testing.T) {
	path := writeConfig(t, minimal(t))
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "crd.yaml"), []byte("{not: [yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("Load = %v, want a config without references to load whatever its CRDs hold.", err)
	}
}

func TestReferencedNames(t *testing.T) {
	obj := map[string]any{"spec": map[string]any{
		"clusterRef": "c1",
		"empty":      "",
		"number":     int64(3),
		"keys": []any{
			map[string]any{"secretName": "k1", "namespace": "ns1"},
			map[string]any{"secretName": ""},
			map[string]any{"other": "x"},
			map[string]any{"secretName": "k2", "namespace": int64(1)},
			"not an object",
		},
		"ref": map[string]any{"name": "r", "namespace": "there"},
		"groups": []any{
			map[string]any{"namespace": "g1", "members": []any{map[string]any{"name": "m1"}, map[string]any{"name": "m2"}}},
			map[string]any{"members": []any{map[string]any{"name": "m3"}}},
		},
	}}
	for _, c := range []struct {
		name, namespace string
		want            []types.NamespacedName
	}{
		{"spec.clusterRef", "", []types.NamespacedName{{Name: "c1"}}},
		{"spec.empty", "", nil},
		{"spec.absent", "", nil},
		{"spec.number", "", nil},
		{"spec.keys.secretName", "", nil},
		{"spec.clusterRef[*].name", "", nil},
		{"spec.keys[*].secretName", "", []types.NamespacedName{{Name: "k1"}, {Name: "k2"}}},
		{"spec.keys[*].secretName", "spec.keys[*].namespace", []types.NamespacedName{{Name: "k1", Namespace: "ns1"}, {Name: "k2"}}},
		{"spec.ref.name", "spec.ref.namespace", []types.NamespacedName{{Name: "r", Namespace: "there"}}},
		{"spec.ref.name", "spec.absent", []types.NamespacedName{{Name: "r"}}},
		{"spec.keys[*].secretName", "spec.ref.namespace", []types.NamespacedName{{Name: "k1", Namespace: "there"}, {Name: "k2", Namespace: "there"}}},
		{"spec.groups[*].members[*].name", "spec.groups[*].namespace", []types.NamespacedName{{Name: "m1", Namespace: "g1"}, {Name: "m2", Namespace: "g1"}, {Name: "m3"}}},
	} {
		if got := config.ReferencedNames(c.name, c.namespace, obj); !slices.Equal(got, c.want) {
			t.Errorf("ReferencedNames(%q, %q) = %v, want %v.", c.name, c.namespace, got, c.want)
		}
	}
}

func TestDeleteOrderPutsEachFromKindBeforeItsToKinds(t *testing.T) {
	gk := func(kind string) schema.GroupKind { return schema.GroupKind{Group: "toy", Kind: kind} }
	for name, c := range map[string]struct {
		references string
		want       []schema.GroupKind
	}{
		"none": {"", []schema.GroupKind{gk("Cluster"), gk("Database"), gk("User"), gk("Tenant")}},
		"a chain": {`  - {from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef}
  - {from: toy/v1/User, to: toy/v1/Database, name: spec.databaseRef}
  - {from: toy/v1/Database, to: v1/Secret, name: spec.secretRef, ready: "true"}
`, []schema.GroupKind{gk("User"), gk("Database"), gk("Cluster"), gk("Tenant")}},
		"a cycle": {`  - {from: toy/v1/Cluster, to: toy/v1/Database, name: spec.primary}
  - {from: toy/v1/Database, to: toy/v1/Cluster, name: spec.clusterRef}
  - {from: toy/v1/User, to: toy/v1/Database, name: spec.databaseRef}
`, []schema.GroupKind{gk("User"), gk("Tenant"), gk("Cluster"), gk("Database")}},
		"a self reference": {`  - {from: toy/v1/Cluster, to: toy/v1/Cluster, name: spec.parent}
`, []schema.GroupKind{gk("Cluster"), gk("Database"), gk("User"), gk("Tenant")}},
	} {
		rest := ""
		if c.references != "" {
			rest = "references:\n" + c.references
		}
		cfg, err := config.Load(writeReferences(t, rest))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.DeleteOrder(); !slices.Equal(got, c.want) {
			t.Errorf("%s: DeleteOrder = %v, want %v.", name, got, c.want)
		}
	}
}
