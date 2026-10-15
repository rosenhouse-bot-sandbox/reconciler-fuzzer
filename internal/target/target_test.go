package target_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestWatchedKindsHoldThePrimaryOnce(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

	watched := (&target.Target{
		Primary: widget,
		Manages: []schema.GroupVersionKind{configMap, widget},
	}).WatchedKinds()

	if want := []schema.GroupVersionKind{widget, configMap}; !slices.Equal(watched, want) {
		t.Errorf("reconciler-fuzzer watches %v, want %v.", watched, want)
	}
}

func TestCheckScopesRefusesPrimaryAndManagedButAcceptsFixtures(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	clusterRole := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	webhook := schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration"}
	unserved := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Unserved"}
	namespace := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widget, meta.RESTScopeNamespace)
	mapper.Add(clusterRole, meta.RESTScopeRoot)
	mapper.Add(namespace, meta.RESTScopeRoot)
	mapper.Add(webhook, meta.RESTScopeRoot)
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(webhook)
	fixture.SetName("widget-validator")

	err := (&target.Target{
		Primary:  widget,
		Manages:  []schema.GroupVersionKind{clusterRole, unserved, namespace},
		Fixtures: []*unstructured.Unstructured{fixture},
	}).CheckScopes(mapper)

	if err == nil {
		t.Fatal("CheckScopes accepted cluster-scoped primary and managed kinds.")
	}
	for _, want := range []string{"managed rbac.authorization.k8s.io/v1/ClusterRole", "managed v1/Namespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckScopes returned %q, which does not name %q.", err, want)
		}
	}
	for _, other := range []string{"Widget", "Unserved", "fixture", "ValidatingWebhookConfiguration", "widget-validator"} {
		if strings.Contains(err.Error(), other) {
			t.Errorf("CheckScopes returned %q, which names %s.", err, other)
		}
	}
	if err := (&target.Target{Primary: widget}).CheckScopes(mapper); err != nil {
		t.Errorf("CheckScopes refused a namespaced primary: %v", err)
	}
	// A cluster-scoped fixture is accepted.
	if err := (&target.Target{Primary: widget, Fixtures: []*unstructured.Unstructured{fixture}}).CheckScopes(mapper); err != nil {
		t.Errorf("CheckScopes refused a cluster-scoped fixture: %v", err)
	}
}

func TestCheckScopesJudgesFixtureScopesAndNamespaces(t *testing.T) {
	widget := schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	secret := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	gadget := schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}
	unserved := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Unserved"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widget, meta.RESTScopeNamespace)
	mapper.Add(secret, meta.RESTScopeNamespace)
	mapper.Add(gadget, meta.RESTScopeRoot)
	fixture := func(gvk schema.GroupVersionKind, name, namespace string) *unstructured.Unstructured {
		object := &unstructured.Unstructured{}
		object.SetGroupVersionKind(gvk)
		object.SetName(name)
		object.SetNamespace(namespace)
		return object
	}

	for _, test := range []struct {
		fixtures []*unstructured.Unstructured
		want     string
	}{
		{fixtures: []*unstructured.Unstructured{fixture(unserved, "elsewhere", "default")}},
		// A cluster-scoped fixture without a namespace is accepted.
		{fixtures: []*unstructured.Unstructured{fixture(gadget, "shared", "")}},
		// A cluster-scoped fixture with a namespace is refused.
		{
			fixtures: []*unstructured.Unstructured{fixture(gadget, "shared", "default")},
			want:     "Gadget has no namespace; drop it",
		},
		{
			fixtures: []*unstructured.Unstructured{fixture(secret, "settings", ""), fixture(secret, "ca", "default"), fixture(secret, "token", "")},
			want:     "the fixture v1/Secret ca sets metadata.namespace default; drop it",
		},
	} {
		err := (&target.Target{Primary: widget, Fixtures: test.fixtures}).CheckScopes(mapper)

		switch {
		case test.want == "" && err != nil:
			t.Errorf("CheckScopes refused fixtures %v: %v", test.fixtures, err)
		case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
			t.Errorf("CheckScopes returned %v, want %q.", err, test.want)
		}
	}
}

func TestLaunchCheckFindsTheBinaryWhereReconcilerFuzzerRuns(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for name, mode := range map[string]os.FileMode{"operator": 0o755, "notes": 0o644} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		binary string
		want   []string
		// hint is whether the error names the working directory.
		hint bool
	}{
		{binary: "./operator"},
		{binary: filepath.Join(dir, "operator")},
		{binary: "sh"},
		{binary: "bin/operator", want: []string{"launch.binary", "bin/operator", "working directory " + dir, "target.yaml"}, hint: true},
		{binary: "./notes", want: []string{"launch.binary", "./notes", "permission denied"}},
		{binary: "./tools", want: []string{"launch.binary", "./tools", "is a directory"}},
		{binary: "no-such-program-on-path", want: []string{"launch.binary", "no-such-program-on-path", "PATH"}},
		{binary: filepath.Join(dir, "no-such-operator"), want: []string{"launch.binary", filepath.Join(dir, "no-such-operator")}},
	} {
		t.Run(test.binary, func(t *testing.T) {
			err := target.LaunchSpec{Binary: test.binary}.Check()

			if len(test.want) == 0 {
				if err != nil {
					t.Errorf("Check refused %s: %v", test.binary, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check accepted %s.", test.binary)
			}
			for _, said := range test.want {
				if !strings.Contains(err.Error(), said) {
					t.Errorf("Check returned %q, which does not say %q.", err, said)
				}
			}
			if named := strings.Contains(err.Error(), "working directory"); named != test.hint {
				t.Errorf("Check returned %q; want it to name the working directory only for a relative path it found nothing at.", err)
			}
		})
	}
}
