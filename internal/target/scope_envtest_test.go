//go:build envtest

package target_test

import (
	"strings"
	"testing"

	"k8s.io/client-go/discovery"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestLoadKnowsTheScopeOfEveryKindABareAPIServerServes(t *testing.T) {
	clusterScoped, namespaced := servedKinds(t)
	if len(clusterScoped) == 0 || len(namespaced) == 0 {
		t.Fatalf("The API server serves %d cluster-scoped and %d namespaced kinds.", len(clusterScoped), len(namespaced))
	}

	for _, kind := range clusterScoped {
		if err := loadFixtureInDefault(t, kind); err == nil || !strings.Contains(err.Error(), "has no namespace; drop it") {
			t.Errorf("Load returned %v for a fixture of the cluster-scoped %s.", err, kind)
		}
	}
	for _, kind := range namespaced {
		if err := loadFixtureInDefault(t, kind); err == nil || !strings.Contains(err.Error(), "the fixture "+kind.String()+" x sets metadata.namespace") {
			t.Errorf("Load returned %v for a fixture of the namespaced %s.", err, kind)
		}
	}
}

type servedKind struct{ apiVersion, kind string }

func (k servedKind) String() string { return k.apiVersion + "/" + k.kind }

func loadFixtureInDefault(t *testing.T, kind servedKind) error {
	t.Helper()
	path := writeTarget(t, minimalTarget+"fixtures: [x.yaml]\n", map[string]string{
		"widget.yaml": sampleWidget,
		"x.yaml":      "apiVersion: " + kind.apiVersion + "\nkind: " + kind.kind + "\nmetadata:\n  name: x\n  namespace: default\n",
	})
	_, err := target.Load(path)
	return err
}

// servedKinds starts a control plane with no CRDs and lists the kinds it
// serves, at every version.
func servedKinds(t *testing.T) (clusterScoped, namespaced []servedKind) {
	t.Helper()
	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Starting the cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stopping the cluster failed: %v", err)
		}
	})
	client, err := discovery.NewDiscoveryClientForConfig(c.Config())
	if err != nil {
		t.Fatal(err)
	}
	_, lists, err := client.ServerGroupsAndResources()
	if err != nil {
		t.Fatalf("Discovery failed: %v", err)
	}
	for _, list := range lists {
		for _, resource := range list.APIResources {
			if strings.Contains(resource.Name, "/") {
				continue
			}
			kind := servedKind{apiVersion: list.GroupVersion, kind: resource.Kind}
			if resource.Namespaced {
				namespaced = append(namespaced, kind)
			} else {
				clusterScoped = append(clusterScoped, kind)
			}
		}
	}
	return clusterScoped, namespaced
}
