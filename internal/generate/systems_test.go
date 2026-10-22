package generate

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const systemsConfig = "testdata/systems/config.yaml"

func TestAboutOneKubeFaultInFourHoldsTheWatchOfAKindTheRBACLetsItWatch(t *testing.T) {
	watchable := []plan.Match{{Group: "toy.reconciler-fuzzer", Resource: "widgets"}, {Resource: "configmaps"}}
	holds, kube := map[plan.Match]int{}, 0
	for _, f := range faults(plans(t, toyConfig, 300)) {
		kube++
		if !f.HoldWatch {
			continue
		}
		if !slices.Contains(watchable, f.Match) || f.Nth != 0 {
			t.Errorf("Drew %s, want a holdWatch of a kind the RBAC lets the controller watch.", f)
		}
		holds[f.Match]++
	}
	n := holds[watchable[0]] + holds[watchable[1]]
	if share := float64(n) / float64(kube); share < 0.2 || share > 0.35 {
		t.Errorf("%d of %d kube faults hold a watch, want about 2 in 7.", n, kube)
	}
	for _, m := range watchable {
		if holds[m] == 0 {
			t.Errorf("No holdWatch holds %s.", m.Resource)
		}
	}
}

func TestWithoutAWatchableKindNoFaultHoldsAWatch(t *testing.T) {
	for _, f := range faults(plans(t, systemsConfig, 200)) {
		if f.HoldWatch {
			t.Fatalf("Drew %s, but the RBAC grants no watch.", f)
		}
	}
}

// A held watch hides a delete, which matters when a CR comes back as it was.
func TestAHoldWatchComesWithARecreateOfALiveCRAsItWas(t *testing.T) {
	g := newGenerator(t, loadConfig(t, toyConfig))
	cr := g.base("a").Object
	actions := rapid.Custom(func(t *rapid.T) []plan.Action {
		return g.holdTraffic(t, state{"a": cr})
	}).Example(0)
	if len(actions) != 2 || actions[0].Do != plan.Delete || actions[1].Do != plan.Create ||
		actions[0].Slot != "a" || actions[1].Slot != "a" || !reflect.DeepEqual(decode(t, actions[1].Object), decode(t, marshal(cr))) {
		t.Errorf("A holdWatch comes with %+v, want slot a deleted and created as it was.", actions)
	}
	if actions[1].At > plan.Duration(50*time.Millisecond) {
		t.Errorf("The create comes at %v, want within 50ms.", time.Duration(actions[1].At))
	}
}

func TestAHoldWatchWithoutALiveCRComesWithACreate(t *testing.T) {
	g := newGenerator(t, loadConfig(t, toyConfig))
	actions := rapid.Custom(func(t *rapid.T) []plan.Action {
		return g.holdTraffic(t, state{})
	}).Example(0)
	if len(actions) != 1 || actions[0].Do != plan.Create {
		t.Errorf("A holdWatch with no live CR comes with %+v, want a create.", actions)
	}
}

func TestHoldWatchesFavorThePrimaryAndTheCoreKindsTheControllerCreates(t *testing.T) {
	counts := map[string]int{}
	for _, f := range faults(plans(t, "../../examples/cert-manager/config.yaml", 400)) {
		if f.HoldWatch {
			counts[f.Match.Resource]++
		}
	}
	for _, favored := range []string{"secrets", "certificates"} {
		if counts[favored] < 2*counts["orders"] || counts[favored] < 2*counts["pods"] {
			t.Errorf("Holds of %v, want %s over twice as often as orders and pods.", counts, favored)
		}
	}
}

func TestAFakeWithoutModelFaultsDrawsOnlyRequestFaults(t *testing.T) {
	drawn := map[string]int{}
	for _, f := range faults(plans(t, systemsConfig, 300)) {
		if f.System != "vault" {
			continue
		}
		if f.ModelFault() {
			t.Fatalf("Drew %s on a fake without model faults.", f)
		}
		drawn[f.Effect()]++
	}
	for _, effect := range []string{"drop", "dropResponse", "killAfter", "error(500)"} {
		if drawn[effect] == 0 {
			t.Errorf("No fault on vault draws %s; drawn %v.", effect, drawn)
		}
	}
}

func TestChangeFixtureDrawsASystemFixture(t *testing.T) {
	paths := map[string]int{}
	for _, a := range actions(plans(t, systemsConfig, 300)) {
		if a.Do != plan.ChangeFixture {
			continue
		}
		if want := (plan.Target{System: "vault", Resource: "secret", Name: "app"}); *a.Target != want {
			t.Fatalf("changeFixture targets %+v, want %+v.", *a.Target, want)
		}
		patch := decode(t, a.Patch)
		if word, isString := patch["password"].(string); isString && word != "" && len(patch) == 1 {
			paths["password"]++
		} else if nested, _ := patch["nested"].(map[string]any); len(patch) == 1 && reflect.TypeOf(nested["key"]) == reflect.TypeOf("") {
			paths["nested.key"]++
		} else {
			t.Errorf("changeFixture patches %s, want a word at a values.fixtures path.", a.Patch)
		}
	}
	if paths["password"] == 0 || paths["nested.key"] == 0 {
		t.Errorf("changeFixture drew paths %v, want both.", paths)
	}
}

func TestChangeFixtureDrawsEachEntryOfASystemFixturesFile(t *testing.T) {
	names := map[string]bool{}
	for _, a := range actions(plans(t, "testdata/systems/two-entries.yaml", 100)) {
		if a.Do == plan.ChangeFixture {
			names[a.Target.Name] = true
		}
	}
	if !names["one"] || !names["two"] || len(names) != 2 {
		t.Errorf("changeFixture names %v, want both entries.", names)
	}
}

func TestEveryHoldWatchComesWithARecreateOrACreate(t *testing.T) {
	for _, p := range plans(t, toyConfig, 300) {
		for _, step := range p.Steps {
			if len(step.Faults) == 0 || !step.Faults[0].HoldWatch {
				continue
			}
			a := step.Actions
			recreate := len(a) == 2 && a[0].Do == plan.Delete && a[1].Do == plan.Create && a[0].Slot == a[1].Slot
			if !recreate && (len(a) != 1 || a[0].Do != plan.Create) {
				t.Errorf("Seed %d holds a watch with %+v, want a recreate or a create.", p.Seed, a)
			}
		}
	}
}

func TestHoldWatchesFavorThePrimaryAndTheOwnedKinds(t *testing.T) {
	cfg := loadConfig(t, "../../examples/cert-manager/config.yaml")
	g := newGenerator(t, cfg)
	counts := map[string]int{}
	for _, gr := range g.watchable {
		counts[gr.Resource]++
	}
	if counts["secrets"] != favored || counts["certificates"] != favored || counts["orders"] != 1 {
		t.Errorf("watchable = %v, want secrets and certificates %d times and orders once", counts, favored)
	}
}

func TestAWildcardVerbLetsTheControllerWatch(t *testing.T) {
	for _, f := range faults(plans(t, "testdata/systems/star.yaml", 100)) {
		if f.HoldWatch && f.Match == (plan.Match{Resource: "secrets"}) {
			return
		}
	}
	t.Error("No fault holds the watch of secrets, which the RBAC grants with *.")
}

func TestAReadOnlyFakeTakesNoDamageAndOnlyReadFaults(t *testing.T) {
	drawn := plans(t, "testdata/systems/read-only.yaml", 300)
	reads := 0
	for _, a := range actions(drawn) {
		if a.Do == plan.Damage && a.Target.System != "" {
			t.Fatalf("Drew damage of %+v, a read-only fake.", *a.Target)
		}
	}
	for _, p := range drawn {
		for _, step := range p.Steps {
			if len(step.Faults) == 0 || step.Faults[0].System != "vault" {
				continue
			}
			if f := step.Faults[0]; !slices.Contains(readVerbs, f.Match.Verb) {
				t.Errorf("Drew %s on a read-only fake, want a read.", f)
			}
			reads++
		}
	}
	if reads == 0 {
		t.Error("No fault names the read-only fake.")
	}
}

func TestAReadOnlyFakesFaultComesWithACreateOrAnUpdate(t *testing.T) {
	g := newGenerator(t, loadConfig(t, "testdata/systems/read-only.yaml"))
	read := plan.Fault{System: "vault", Match: plan.Match{Verb: "get"}, Drop: true}
	for _, live := range []bool{false, true} {
		action := rapid.Custom(func(t *rapid.T) plan.Action {
			at := state{}
			if live {
				for _, slot := range slots {
					at[slot] = g.base(slot).Object
				}
			}
			return g.traffic(t, at, read)
		}).Example(0)
		if want := map[bool]plan.Do{false: plan.Create, true: plan.Update}[live]; action.Do != want {
			t.Errorf("live %v: a read fault comes with %s, want %s.", live, action.Do, want)
		}
	}
}
