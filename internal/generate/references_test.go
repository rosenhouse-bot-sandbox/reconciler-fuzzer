package generate

import (
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const referencesConfig = "testdata/references/config.yaml"

// clusterNames are the names of a Cluster in each slot.
var clusterNames = []string{"cluster", "cluster-b", "cluster-c"}

// replay calls visit with each action of p and what the slots hold before
// it.
func replay(t *testing.T, p *plan.Plan, visit func(a plan.Action, at state)) {
	t.Helper()
	at := state{}
	for _, step := range p.Steps {
		for _, a := range step.Actions {
			visit(a, at)
			switch a.Do {
			case plan.Create:
				at[a.Slot] = decode(t, a.Object)
			case plan.Update:
				at[a.Slot] = mergePatch(at[a.Slot], decode(t, a.Patch))
			case plan.Delete:
				at[a.Slot] = nil
			}
		}
	}
}

func kindName(cr map[string]any) string {
	kind, _ := cr["kind"].(string)
	return kind
}

func clusterRef(cr map[string]any) string {
	spec, _ := cr["spec"].(map[string]any)
	ref, _ := spec["clusterRef"].(string)
	return ref
}

func liveClusters(at state) []string {
	var names []string
	for _, slot := range at.live() {
		if kindName(at[slot]) == "Cluster" {
			names = append(names, at[slot]["metadata"].(map[string]any)["name"].(string))
		}
	}
	return names
}

func TestCreatesAndUpdatesWriteEachReferenceWithACandidate(t *testing.T) {
	t.Skip("enabled by the config package")
	counts := map[string]int{}
	for _, p := range plans(t, referencesConfig, 50) {
		replay(t, p, func(a plan.Action, at state) {
			var spec map[string]any
			switch {
			case a.Do == plan.Create && kindName(decode(t, a.Object)) == "Database":
				spec, _ = decode(t, a.Object)["spec"].(map[string]any)
			case a.Do == plan.Update && kindName(at[a.Slot]) == "Database":
				spec, _ = decode(t, a.Patch)["spec"].(map[string]any)
			default:
				return
			}
			if ref, set := spec["clusterRef"]; set {
				name, _ := ref.(string)
				switch {
				case !slices.Contains(clusterNames, name):
					t.Errorf("Seed %d's %s of slot %s writes clusterRef %v, want one of %v.", p.Seed, a.Do, a.Slot, ref, clusterNames)
				case slices.Contains(liveClusters(at), name):
					counts[string(a.Do)+" naming a live Cluster"]++
				default:
					counts[string(a.Do)+" naming no live Cluster"]++
				}
			}
			if secretRef, _ := spec["passwordSecretRef"].(map[string]any); secretRef != nil {
				switch name := secretRef["name"]; name {
				case "admin-password", "missing":
					counts["passwordSecretRef "+name.(string)]++
				default:
					t.Errorf("Seed %d's %s of slot %s writes passwordSecretRef.name %v, want admin-password or missing.", p.Seed, a.Do, a.Slot, name)
				}
			}
		})
	}
	for _, want := range []string{"create naming a live Cluster", "create naming no live Cluster", "update naming a live Cluster", "passwordSecretRef admin-password", "passwordSecretRef missing"} {
		if counts[want] == 0 {
			t.Errorf("50 seeds draw %v, want a %s.", counts, want)
		}
	}
}

func TestAReferencedFixtureIsDeletedAndCreatedAgain(t *testing.T) {
	t.Skip("enabled by the config package")
	secret := plan.Target{Resource: "secrets", Name: "admin-password"}
	counts := map[plan.Do]int{}
	for _, p := range plans(t, referencesConfig, 100) {
		deleted := false
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if a.Do != plan.ChangeFixture && a.Do != plan.CreateFixture {
					continue
				}
				switch {
				case *a.Target != secret || a.Patch != nil:
					t.Errorf("Seed %d draws %s of %+v with patch %s, want only a delete or create of the Secret.", p.Seed, a.Do, *a.Target, a.Patch)
				case a.Do == plan.ChangeFixture && deleted:
					t.Errorf("Seed %d deletes the Secret it already deleted.", p.Seed)
				}
				deleted = a.Do == plan.ChangeFixture
				counts[a.Do]++
			}
		}
	}
	if counts[plan.ChangeFixture] == 0 || counts[plan.CreateFixture] == 0 {
		t.Errorf("100 seeds draw %v, want deletes and creates of the Secret.", counts)
	}
}

func TestDeletesComeInBothOrders(t *testing.T) {
	t.Skip("enabled by the config package")
	var toFirst, fromFirst int
	for _, p := range plans(t, referencesConfig, 200) {
		replay(t, p, func(a plan.Action, at state) {
			if a.Do != plan.Delete {
				return
			}
			switch cr := at[a.Slot]; kindName(cr) {
			case "Cluster":
				name := cr["metadata"].(map[string]any)["name"]
				if slices.ContainsFunc(at.live(), func(slot string) bool { return clusterRef(at[slot]) == name }) {
					toFirst++
				}
			case "Database":
				if slices.Contains(liveClusters(at), clusterRef(cr)) {
					fromFirst++
				}
			}
		})
	}
	if toFirst == 0 || fromFirst == 0 {
		t.Errorf("200 seeds delete %d Clusters that a live Database names, and %d Databases that name a live Cluster; want some of each.", toFirst, fromFirst)
	}
}
