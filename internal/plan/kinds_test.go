package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func kinded(slot, apiVersion, kind, name string) Action {
	return Action{Do: Create, Slot: slot, Object: json.RawMessage(`{"apiVersion":"` + apiVersion + `","kind":"` + kind + `","metadata":{"name":"` + name + `"}}`)}
}

func TestSlotsClashOnlyOnGroupKindNamespaceAndName(t *testing.T) {
	t.Skip("enabled by the config package")
	for name, actions := range map[string][]Action{
		"a Database and a User": {kinded("a", "toy/v1", "Database", "app"), kinded("b", "toy/v1", "User", "app")},
		"two groups' Widgets":   {kinded("a", "toy/v1", "Widget", "app"), kinded("b", "other/v1", "Widget", "app")},
	} {
		if err := (&Plan{Steps: []Step{{Actions: actions}}}).Validate(); err != nil {
			t.Errorf("%s named app: Validate = %v, want nil.", name, err)
		}
	}
	twoWidgets := &Plan{Steps: []Step{{Actions: []Action{kinded("a", "toy/v1", "Widget", "app"), kinded("b", "toy/v2", "Widget", "app")}}}}
	if err := twoWidgets.Validate(); err == nil || !strings.Contains(err.Error(), `gives its CR the name of slot "a"'s CR`) {
		t.Errorf("Two Widgets named app: Validate = %v, want a clash.", err)
	}
}

func TestValidateKindsRefusesAnUnlistedKind(t *testing.T) {
	t.Skip("enabled by the config package")
	cluster, database := schema.GroupKind{Group: "toy", Kind: "Cluster"}, schema.GroupKind{Group: "toy", Kind: "Database"}
	p := &Plan{Steps: []Step{
		{Actions: []Action{{Do: Create, Slot: "s"}, kinded("a", "toy/v1", "Cluster", "c")}},
		{Actions: []Action{kinded("b", "toy/v1", "Database", "db")}},
	}}
	if err := p.ValidateKinds([]schema.GroupKind{cluster, database}); err != nil {
		t.Errorf("ValidateKinds of listed kinds = %v, want nil.", err)
	}
	err := p.ValidateKinds([]schema.GroupKind{cluster})
	if err == nil || !strings.Contains(err.Error(), "steps[1].actions[0]") || !strings.Contains(err.Error(), "Database") {
		t.Errorf("ValidateKinds without Database = %v, want an error that names steps[1].actions[0] and Database.", err)
	}
	if err := p.ValidateKinds([]schema.GroupKind{{Group: "other", Kind: "Cluster"}, database}); err == nil {
		t.Error("ValidateKinds accepts a Cluster of another group.")
	}
}
