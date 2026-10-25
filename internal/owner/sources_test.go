package owner_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var (
	widgetKind      = schema.GroupVersionKind{Group: "toy", Version: "v1", Kind: "Widget"}
	databaseKind    = schema.GroupVersionKind{Group: "toy", Version: "v1", Kind: "Database"}
	clusterRoleKind = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
)

// resolver maps each Kind that resources holds to its resource, cluster
// scoped where clusterScoped names it, and refuses other kinds.
func resolver(resources map[string]string, clusterScoped ...string) func(schema.GroupVersionKind) (string, bool, error) {
	return func(gvk schema.GroupVersionKind) (string, bool, error) {
		resource, found := resources[gvk.Kind]
		if !found {
			return "", false, errors.New("no kind")
		}
		return resource, !slices.Contains(clusterScoped, gvk.Kind), nil
	}
}

func TestSourcesDescribeEachPrimariesEntry(t *testing.T) {
	c := &config.Config{
		Systems: []config.System{{Name: "kube", Owner: "self.metadata.labels.owner"}, {Name: "s3"}},
		Primaries: []config.Primary{
			{Kind: widgetKind, Ready: "self.ok", Retain: "self.keep", Halted: "self.halted",
				Periodic: &config.Periodic{Every: time.Second, Paths: []string{"status.refreshTime"}},
				Owns:     []config.Owns{{Kind: clusterRoleKind, Name: "self.metadata.name", Namespace: "'other'"}}},
			{Kind: databaseKind, Ready: "true"},
		},
	}
	owners, primaries, err := owner.Sources(c, resolver(map[string]string{"Widget": "widgetries", "Database": "databases", "ClusterRole": "clusterroleries"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, map[string]string{"kube": "self.metadata.labels.owner"}) {
		t.Errorf("owners = %v", owners)
	}
	want := []trace.Primary{
		{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgetries"}, Ready: "self.ok", Retain: "self.keep", Halted: "self.halted",
			Periodic: &trace.Periodic{Every: time.Second, Paths: []string{"status.refreshTime"}},
			Owns: []trace.Owns{{Kind: trace.Kind{Group: clusterRoleKind.Group, Version: "v1", Kind: "ClusterRole", Resource: "clusterroleries"},
				Name: "self.metadata.name", Namespace: "'other'"}}},
		{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Database", Resource: "databases"}, Ready: "true"},
	}
	if !reflect.DeepEqual(primaries, want) {
		t.Errorf("primaries = %+v, want %+v", primaries, want)
	}
}

func TestSourcesRefuseAnUnknownKindAndANamespaceOnAClusterScopedKind(t *testing.T) {
	c := &config.Config{Primaries: []config.Primary{{Kind: widgetKind, Owns: []config.Owns{{Kind: clusterRoleKind, Name: "'x'", Namespace: "'other'"}}}}}
	both := map[string]string{"Widget": "widgets", "ClusterRole": "clusterroles"}
	for name, tc := range map[string]struct {
		resolve func(schema.GroupVersionKind) (string, bool, error)
		want    string
	}{
		"an unknown primaries kind":                 {resolver(map[string]string{"ClusterRole": "clusterroles"}), "primaries kind Widget"},
		"an unknown owns kind":                      {resolver(map[string]string{"Widget": "widgets"}), "owns kind ClusterRole"},
		"a namespace on a cluster-scoped owns kind": {resolver(both, "ClusterRole"), "cluster-scoped"},
	} {
		if _, _, err := owner.Sources(c, tc.resolve); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one that says %q", name, err, tc.want)
		}
	}
	c.Primaries[0].Owns[0].Namespace = ""
	if _, _, err := owner.Sources(c, resolver(both, "ClusterRole")); err != nil {
		t.Error(err)
	}
}
