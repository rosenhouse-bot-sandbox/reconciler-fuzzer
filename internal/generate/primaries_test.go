package generate

import (
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const fleetConfig = "testdata/fleet/config.yaml"

func TestPlansCreateAndUpdateEachListedKindWithItsValues(t *testing.T) {
	mutable := map[string][]string{"Cluster": {"volumes", "volumeSize"}, "Database": {"engine"}}
	created, updated := map[string]bool{}, map[string]bool{}
	for _, p := range plans(t, fleetConfig, 50) {
		kinds := map[string]string{}
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				switch a.Do {
				case plan.Create:
					kind, _ := decode(t, a.Object)["kind"].(string)
					kinds[a.Slot], created[kind] = kind, true
				case plan.Update:
					kind := kinds[a.Slot]
					updated[kind] = true
					spec, _ := decode(t, a.Patch)["spec"].(map[string]any)
					for field := range spec {
						if !slices.Contains(mutable[kind], field) {
							t.Errorf("seed %d updates spec.%s of slot %s's %s, which its values do not mutate.", p.Seed, field, a.Slot, kind)
						}
					}
				}
			}
		}
	}
	if !created["Cluster"] || !created["Database"] || !updated["Cluster"] || !updated["Database"] {
		t.Errorf("50 seeds create %v and update %v, want both kinds each.", created, updated)
	}
}
