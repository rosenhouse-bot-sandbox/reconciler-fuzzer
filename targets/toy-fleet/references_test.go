package main_test

import (
	"encoding/json"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestReferencesConfigLoads(t *testing.T) {
	t.Skip("enabled by the toy-fleet package")
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	toy := func(kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: kind}
	}
	type entry struct {
		from, to schema.GroupVersionKind
		name     string
		tracked  bool
	}
	var got []entry
	for _, r := range c.References {
		if !r.Required {
			t.Errorf("The reference from %s at %s is optional, want required.", r.From.Kind, r.Name)
		}
		got = append(got, entry{r.From, r.To, r.Name, r.Track > 0})
	}
	want := []entry{
		{toy("Database"), toy("Cluster"), "spec.clusterRef", false},
		{toy("Database"), schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, "spec.passwordSecretRef.name", true},
		{toy("User"), toy("Database"), "spec.databaseRef", false},
	}
	if len(got) != len(want) || slices.ContainsFunc(want, func(e entry) bool { return !slices.Contains(got, e) }) {
		t.Errorf("References = %+v, want %+v in any order.", got, want)
	}
}

// TestGeneratedPlansCreateAndDeleteReferencingCRs wants generated plans to
// reach the ordering across kinds and the Secret's deletion.
func TestGeneratedPlansCreateAndDeleteReferencingCRs(t *testing.T) {
	t.Skip("enabled by the toy-fleet package")
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(c)
	if err != nil {
		t.Fatal(err)
	}
	made := map[string]bool{}
	for seed := range uint64(50) {
		p, err := g.Plan(seed + 1)
		if err != nil {
			t.Fatal(err)
		}
		created, kinds := map[string]bool{}, map[string]string{}
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				switch a.Do {
				case plan.Create:
					var o struct {
						Kind     string
						Metadata struct{ Name string }
						Spec     struct {
							ClusterRef, DatabaseRef string
							PasswordSecretRef       *struct{ Name string }
						}
					}
					if err := json.Unmarshal(a.Object, &o); err != nil {
						t.Fatal(err)
					}
					made["a Database naming a created Cluster"] = made["a Database naming a created Cluster"] || o.Kind == "Database" && created["Cluster "+o.Spec.ClusterRef]
					made["a User naming a created Database"] = made["a User naming a created Database"] || o.Kind == "User" && created["Database "+o.Spec.DatabaseRef]
					made["a Database naming the Secret"] = made["a Database naming the Secret"] || o.Spec.PasswordSecretRef != nil && o.Spec.PasswordSecretRef.Name == "admin-password"
					created[o.Kind+" "+o.Metadata.Name], kinds[a.Slot] = true, o.Kind
				case plan.Delete:
					made["a delete of a "+kinds[a.Slot]] = true
				case plan.ChangeFixture, plan.CreateFixture:
					if *a.Target == (plan.Target{Resource: "secrets", Name: "admin-password"}) && a.Patch == nil {
						made[string(a.Do)+" of the Secret"] = true
					}
				}
			}
		}
	}
	for _, want := range []string{
		"a Database naming a created Cluster", "a User naming a created Database", "a Database naming the Secret",
		"a delete of a Cluster", "a delete of a Database", "a delete of a User", "changeFixture of the Secret", "createFixture of the Secret",
	} {
		if !made[want] {
			t.Errorf("No plan of seeds 1 to 50 has %s.", want)
		}
	}
}
