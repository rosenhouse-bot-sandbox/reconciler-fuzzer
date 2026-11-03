//go:build envtest

package config

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestBuiltinsAreTheKindsTheAPIServerServesToWatch(t *testing.T) {
	env := &envtest.Environment{}
	rest, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	client, err := discovery.NewDiscoveryClientForConfig(rest)
	if err != nil {
		t.Fatal(err)
	}
	_, lists, err := client.ServerGroupsAndResources()
	if err != nil {
		t.Fatal(err)
	}
	served := servedKinds{}
	for _, list := range lists {
		version, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range list.APIResources {
			if !strings.Contains(r.Name, "/") && slices.Contains(r.Verbs, "watch") {
				served[version.WithKind(r.Kind)] = servedKind{namespaced: r.Namespaced, resource: r.Name}
			}
		}
	}
	for kind, want := range served {
		if got, found := builtins[kind]; !found || got.namespaced != want.namespaced || got.resource != want.resource {
			t.Errorf("%s: builtins hold %+v, %v; the API server serves %+v", kind, got, found, want)
		}
	}
	for kind := range builtins {
		if _, found := served[kind]; !found {
			t.Errorf("%s: builtins hold a kind that the API server does not serve to watch", kind)
		}
	}
}
