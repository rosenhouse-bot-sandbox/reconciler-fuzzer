package generate

import (
	"maps"
	"slices"
	"testing"

	gvk "k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

// fleetClusterConfig lists only Clusters, whose Volumes a model plays.
const fleetClusterConfig = "../../targets/toy-fleet/cluster/config.yaml"

// fleetOwning is fleetConfig with an owns entry on each kind: a Cluster owns
// a Volume, and a Database a ConfigMap.
func fleetOwning(t *testing.T) *config.Config {
	cfg := loadConfig(t, fleetConfig)
	cfg.Primaries[0].Owns = []config.Owns{{Kind: gvk.GroupVersionKind{Group: "storage.thirdparty", Version: "v1", Kind: "Volume"}, Name: "self.metadata.name"}}
	cfg.Primaries[1].Owns = []config.Owns{{Kind: gvk.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Name: "self.metadata.name"}}
	return cfg
}

func TestFaultsFavorTheListedModelAndOwnsKinds(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     *config.Config
		favored []string
	}{
		{"a model", loadConfig(t, fleetClusterConfig), []string{"clusters", "volumes"}},
		{"owns in each entry", fleetOwning(t), []string{"clusters", "databases", "volumes", "configmaps"}},
		// Without either, the core kinds the RBAC lets the controller create
		// stand in for them.
		{"neither", loadConfig(t, fleetConfig), []string{"clusters", "databases", "configmaps", "secrets"}},
	} {
		g := newGenerator(t, c.cfg)
		weight := func(resource string) int {
			if slices.Contains(c.favored, resource) {
				return favored
			}
			return 1
		}
		for _, r := range g.requests {
			if want := weight(r.match.Resource) * verbWeights[r.match.Verb]; r.weight != want {
				t.Errorf("%s: faults match %+v with weight %d, want %d.", c.name, r.match, r.weight, want)
			}
		}
		holds, want := map[string]int{}, map[string]int{}
		for _, gr := range g.watchable {
			holds[gr.Resource]++
		}
		for _, resource := range []string{"clusters", "databases", "users", "volumes", "configmaps", "secrets"} {
			want[resource] = weight(resource)
		}
		if !maps.Equal(holds, want) {
			t.Errorf("%s: holds are %v, want %v.", c.name, holds, want)
		}
	}
}

func TestAnOwnsKindTakesThePluralItsCRDNames(t *testing.T) {
	cfg := loadConfig(t, modelsConfig)
	cfg.Models = nil
	cfg.Primaries[0].Owns = []config.Owns{{Kind: gvk.GroupVersionKind{Group: "models.reconciler-fuzzer", Version: "v1", Kind: "Mouse"}, Name: "self.metadata.name"}}
	g := newGenerator(t, cfg)
	if w := g.kindWeight(gvk.GroupResource{Group: "models.reconciler-fuzzer", Resource: "mice"}); w != favored {
		t.Errorf("Mice weigh %d, want %d.", w, favored)
	}
}
