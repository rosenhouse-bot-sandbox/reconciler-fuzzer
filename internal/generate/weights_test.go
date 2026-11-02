package generate

import (
	"maps"
	"slices"
	"testing"

	gvk "k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

const (
	// fleetClusterConfig lists only Clusters, whose Volumes a dependency
	// model plays.
	fleetClusterConfig = "../../targets/toy-fleet/cluster/config.yaml"
	// fleetDatabaseConfig lists only Databases. Its Volume model serves the
	// fixture Cluster, so it has no dependency.
	fleetDatabaseConfig = "../../targets/toy-fleet/database/config.yaml"
)

var (
	volume    = gvk.GroupVersionKind{Group: "storage.thirdparty", Version: "v1", Kind: "Volume"}
	configMap = gvk.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
)

// owning gives entry i of cfg an owns entry of the kube kind.
func owning(cfg *config.Config, i int, kind gvk.GroupVersionKind) *config.Config {
	cfg.Primaries[i].Owns = append(cfg.Primaries[i].Owns, config.Owns{Kind: kind, Name: "self.metadata.name"})
	return cfg
}

func TestFaultsFavorTheListedAndOwnedKinds(t *testing.T) {
	for _, c := range []struct {
		name    string
		cfg     *config.Config
		favored []string
	}{
		{"a dependency model", loadConfig(t, fleetClusterConfig), []string{"clusters", "volumes"}},
		{"owns in each entry", owning(owning(loadConfig(t, fleetConfig), 0, volume), 1, configMap), []string{"clusters", "databases", "volumes", "configmaps"}},
		{"a dependency model and owns", owning(loadConfig(t, fleetClusterConfig), 0, configMap), []string{"clusters", "volumes", "configmaps"}},
		// Without owned kinds, the core kinds the RBAC lets the controller
		// create stand in for them.
		{"neither", loadConfig(t, fleetConfig), []string{"clusters", "databases", "configmaps", "secrets"}},
		{"a model without a dependency", loadConfig(t, fleetDatabaseConfig), []string{"databases", "configmaps", "secrets"}},
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
			holds[gr.Resource] += g.kindWeight(gr)
		}
		for _, resource := range []string{"clusters", "databases", "users", "volumes", "configmaps", "secrets"} {
			want[resource] = weight(resource)
		}
		if !maps.Equal(holds, want) {
			t.Errorf("%s: holds weigh %v, want %v.", c.name, holds, want)
		}
	}
}

func TestAFakesOwnsEntryKeepsTheCoreKindsTheControllerMayCreateFavored(t *testing.T) {
	cfg := loadConfig(t, systemsConfig)
	cfg.Primaries[0].Owns = []config.Owns{{System: "vault", Collection: "secret", Name: "self.metadata.name"}}
	if w := newGenerator(t, cfg).kindWeight(gvk.GroupResource{Resource: "configmaps"}); w != favored {
		t.Errorf("ConfigMaps weigh %d, want %d.", w, favored)
	}
}

func TestWeightedDrawsEachElementInProportionToItsWeight(t *testing.T) {
	weights := map[string]int{"light": 1, "heavy": 3}
	counts := map[string]int{}
	for seed := range 400 {
		counts[example(t, seed, func(rt *rapid.T) string {
			return weighted(rt, []string{"light", "heavy"}, func(e string) int { return weights[e] }, "element")
		})]++
	}
	if counts["light"] < 70 || counts["light"] > 130 {
		t.Errorf("400 draws give %v, want about 100 light.", counts)
	}
}

func TestAnOwnedKindIsFavoredUnderThePluralItsCRDNames(t *testing.T) {
	mouse := gvk.GroupVersionKind{Group: "models.reconciler-fuzzer", Version: "v1", Kind: "Mouse"}
	withoutModels := loadConfig(t, modelsConfig)
	withoutModels.Models = nil
	for _, c := range []struct {
		name string
		cfg  *config.Config
	}{
		{"an owns entry", owning(withoutModels, 0, mouse)},
		{"a dependency model", loadConfig(t, modelsConfig)},
	} {
		if w := newGenerator(t, c.cfg).kindWeight(gvk.GroupResource{Group: mouse.Group, Resource: "mice"}); w != favored {
			t.Errorf("%s: mice weigh %d, want %d.", c.name, w, favored)
		}
	}
}
