package generate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gvk "k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	toyConfig       = "../../targets/toy-widget/config.yaml"
	noFixtureValues = "testdata/no-fixture-values.yaml"
	rulesConfig     = "testdata/rules/config.yaml"
)

func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newGenerator(t *testing.T, cfg *config.Config) *Generator {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New failed: %v.", err)
	}
	return g
}

// plans draws the plans of seeds 1 to n.
func plans(t *testing.T, path string, n int) []*plan.Plan {
	t.Helper()
	return drawPlans(t, newGenerator(t, loadConfig(t, path)), n)
}

// drawPlans draws g's plans of seeds 1 to n. A draw that panics fails the
// test.
func drawPlans(t *testing.T, g *Generator, n int) []*plan.Plan {
	t.Helper()
	var drawn []*plan.Plan
	for seed := uint64(1); seed <= uint64(n); seed++ {
		example(t, int(seed), g.plan)
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatalf("Plan(%d) failed: %v.", seed, err)
		}
		drawn = append(drawn, p)
	}
	return drawn
}

// example is draw's value for seed. rapid's Example, and so Plan, hides a
// draw that panics by drawing again. example fails the test on any panic
// but rapid's invalidData, which asks for another draw.
func example[V any](t *testing.T, seed int, draw func(*rapid.T) V) V {
	t.Helper()
	return rapid.Custom(func(rt *rapid.T) V {
		defer func() {
			if r := recover(); r != nil {
				if fmt.Sprintf("%T", r) != "rapid.invalidData" {
					t.Errorf("Seed %d's draw panics with a %T: %v", seed, r, r)
				}
				panic(r)
			}
		}()
		return draw(rt)
	}).Example(seed)
}

func actions(plans []*plan.Plan) []plan.Action {
	var all []plan.Action
	for _, p := range plans {
		for _, step := range p.Steps {
			all = append(all, step.Actions...)
		}
	}
	return all
}

func faults(plans []*plan.Plan) []plan.Fault {
	var all []plan.Fault
	for _, p := range plans {
		for _, step := range p.Steps {
			all = append(all, step.Faults...)
		}
	}
	return all
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func TestPlansHaveTheShapeOfSection14(t *testing.T) {
	for _, path := range []string{toyConfig, fleetConfig, rulesConfig, modelsConfig} {
		for _, p := range plans(t, path, 200) {
			if err := p.Validate(); err != nil {
				t.Fatalf("%s seed %d: %v", path, p.Seed, err)
			}
			if n := len(p.Steps); n < 3 || n > 8 {
				t.Errorf("seed %d has %d steps, want 3 to 8.", p.Seed, n)
			}
			first := p.Steps[0].Actions
			if len(first) != 1 || first[0].Do != plan.Create || first[0].Slot != "a" {
				t.Errorf("seed %d starts with %+v, want a create of slot a.", p.Seed, first)
			}
			for i, step := range p.Steps {
				if len(step.Actions) > 4 || len(step.Faults) > 1 || len(step.Actions)+len(step.Faults) == 0 {
					t.Errorf("seed %d step %d has %d actions and %d faults.", p.Seed, i, len(step.Actions), len(step.Faults))
				}
				for _, a := range step.Actions {
					if a.Slot != "" && !strings.Contains("abc", a.Slot) {
						t.Errorf("seed %d uses slot %q.", p.Seed, a.Slot)
					}
				}
			}
		}
	}
}

func TestOneSeedDrawsOnePlan(t *testing.T) {
	g := newGenerator(t, loadConfig(t, toyConfig))
	first, _ := g.Plan(7)
	again, _ := g.Plan(7)
	other, _ := g.Plan(8)
	if !reflect.DeepEqual(first, again) {
		t.Error("Seed 7 drew two different plans.")
	}
	if reflect.DeepEqual(first.Steps, other.Steps) {
		t.Error("Seeds 7 and 8 drew the same steps.")
	}
	if first.Seed != 7 {
		t.Errorf("Plan(7) records seed %d.", first.Seed)
	}
}

func TestPlansDrawEveryActionAndFaultKind(t *testing.T) {
	drawn := plans(t, toyConfig, 200)
	var dos, effects, verbs []string
	for _, a := range actions(drawn) {
		dos = append(dos, string(a.Do))
	}
	for _, f := range faults(drawn) {
		effect, _, _ := strings.Cut(f.Effect(), "(")
		effects = append(effects, effect)
		if f.Error != nil {
			effects = append(effects, f.Effect())
		}
		verbs = append(verbs, f.Match.Verb)
		if f.Delay != nil {
			effects = append(effects, f.Effect())
		}
	}
	for _, want := range []string{"create", "update", "delete", "restart", "kill", "damage", "changeFixture"} {
		if !slices.Contains(dos, want) {
			t.Errorf("No plan draws a %s.", want)
		}
	}
	for _, want := range []string{"error", "delay", "drop", "dropResponse", "killAfter", "holdWatch",
		"error(500)", "error(503)", "error(429)", "delay(500ms)", "delay(1s)", "delay(2s)"} {
		if !slices.Contains(effects, want) {
			t.Errorf("No plan draws a %s fault.", want)
		}
	}
	for _, want := range []string{"get", "list", "create", "update", "patch", "delete"} {
		if !slices.Contains(verbs, want) {
			t.Errorf("No fault matches verb %q.", want)
		}
	}
}

func TestFaultsMatchRequestsTheRBACGrants(t *testing.T) {
	granted := map[string][]string{
		"toy.reconciler-fuzzer/widgets":        {"get", "list", "update", "patch"},
		"toy.reconciler-fuzzer/widgets/status": {"get", "patch"},
		"/configmaps":                          {"get", "list", "create", "update", "patch", "delete"},
	}
	statuses := 0
	for _, f := range faults(plans(t, toyConfig, 200)) {
		if f.HoldWatch {
			continue
		}
		key := f.Match.Group + "/" + f.Match.Resource
		if f.Match.Subresource != "" {
			key += "/" + f.Match.Subresource
			statuses++
		}
		if !slices.Contains(granted[key], f.Match.Verb) {
			t.Errorf("A fault matches %s %s, which the RBAC does not grant.", f.Match.Verb, key)
		}
	}
	if statuses == 0 {
		t.Error("No fault matches a status write.")
	}
}

func TestFaultsFavorCreates(t *testing.T) {
	counts := map[string]int{}
	var all []plan.Fault
	for _, f := range faults(plans(t, toyConfig, 200)) {
		if !f.HoldWatch {
			all = append(all, f)
			counts[f.Match.Verb]++
		}
	}
	if counts["create"] < len(all)/5 || counts["create"] <= counts["get"] {
		t.Errorf("Faults match verbs %v, want creates to be at least a fifth and more than the gets.", counts)
	}
}

func TestEveryFaultStepHasAnAction(t *testing.T) {
	alone := 0
	for _, p := range plans(t, toyConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Faults) > 0 && len(step.Actions) == 0 {
				alone++
			}
		}
	}
	if alone > 0 {
		t.Errorf("%d fault steps hold no action, so nothing makes the controller send a request.", alone)
	}
}

func TestFaultsAndDamageDrawThePrimaryFixtureAndRBACResources(t *testing.T) {
	drawn := plans(t, toyConfig, 200)
	want := []string{"configmaps", "toy.reconciler-fuzzer/widgets"}
	var matched, damaged, nths []string
	for _, f := range faults(drawn) {
		matched = append(matched, strings.TrimPrefix(f.Match.Group+"/"+f.Match.Resource, "/"))
		nths = append(nths, string(rune('0'+f.Nth)))
	}
	for _, a := range actions(drawn) {
		if a.Do == plan.Damage {
			damaged = append(damaged, strings.TrimPrefix(a.Target.Group+"/"+a.Target.Resource, "/"))
			if a.Patch != nil || a.Target.Name != "" {
				t.Errorf("A damage %+v is not a delete by resource.", a)
			}
		}
	}
	// The controller creates no primary, so damage leaves it out.
	for name, want := range map[string][]string{"faults": want, "damage": want[:1]} {
		got := map[string][]string{"faults": matched, "damage": damaged}[name]
		if got = slices.Compact(slices.Sorted(slices.Values(got))); !slices.Equal(got, want) {
			t.Errorf("The %s draw %v, want %v.", name, got, want)
		}
	}
	if got := slices.Compact(slices.Sorted(slices.Values(nths))); !slices.Equal(got, []string{"0", "1", "2", "3"}) {
		t.Errorf("Faults draw nth %v, want 0 to 3.", got)
	}
}

func TestFaultPeriodsLastHalfASecondToTwiceStable(t *testing.T) {
	var periods []time.Duration
	for _, p := range plans(t, toyConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Faults) == 0 {
				continue
			}
			period := time.Duration(step.Period)
			if period < 500*time.Millisecond || period > 4*time.Second {
				t.Errorf("seed %d has a fault period of %s.", p.Seed, period)
			}
			periods = append(periods, period)
		}
	}
	if slices.Min(periods) > time.Second || slices.Max(periods) < 3*time.Second {
		t.Errorf("Fault periods span %s to %s, want most of 0.5s to 4s.", slices.Min(periods), slices.Max(periods))
	}
}

func TestAboutAThirdOfStepsHoldOneRestartKillDamageOrFault(t *testing.T) {
	single, all := 0, 0
	for _, p := range plans(t, toyConfig, 200) {
		for _, step := range p.Steps[1:] {
			all++
			if len(step.Actions) == 1 && len(step.Faults) <= 1 && slices.Contains([]plan.Do{plan.Restart, plan.Kill, plan.Damage}, step.Actions[0].Do) {
				single++
			}
		}
	}
	if share := float64(single) / float64(all); share < 0.25 || share > 0.45 {
		t.Errorf("%d of %d steps hold one restart, kill, damage or fault.", single, all)
	}
}

func TestBurstsPutADeleteAndCreateOrTwoUpdatesWithin50ms(t *testing.T) {
	bursts := map[string]int{}
	for _, p := range plans(t, toyConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Actions) != 2 || step.Actions[1].At == 0 {
				continue
			}
			first, second := step.Actions[0], step.Actions[1]
			if first.At > second.At || time.Duration(second.At) > 50*time.Millisecond || first.Slot != second.Slot {
				t.Errorf("seed %d bursts %+v.", p.Seed, step.Actions)
			}
			bursts[string(first.Do)+" "+string(second.Do)]++
		}
	}
	if bursts["delete create"] == 0 || bursts["update update"] == 0 || len(bursts) != 2 {
		t.Errorf("Bursts are %v, want deletes then creates and pairs of updates.", bursts)
	}
}

func TestCreatesAndUpdatesDrawCRDValidValues(t *testing.T) {
	g := newGenerator(t, loadConfig(t, toyConfig))
	counts := map[float64]bool{}
	for _, a := range actions(plans(t, toyConfig, 100)) {
		var count any
		switch a.Do {
		case plan.Create:
			object := decode(t, a.Object)
			count = object["spec"].(map[string]any)["count"]
			if err := g.kinds[0].rules.refusal(object, nil); err != nil {
				t.Errorf("The CRD refuses the create %s: %v.", a.Object, err)
			}
		case plan.Update:
			patch := decode(t, a.Patch)
			if spec := patch["spec"].(map[string]any); len(patch) != 1 || len(spec) != 1 {
				t.Errorf("The update %s changes more than one path.", a.Patch)
			}
			count = patch["spec"].(map[string]any)["count"]
		default:
			continue
		}
		n, _ := count.(float64)
		if n < 0 || n > 10 {
			t.Errorf("spec.count is %v.", count)
		}
		counts[n] = true
	}
	if len(counts) < 8 {
		t.Errorf("spec.count takes only %v.", counts)
	}
}

func TestEachSlotHasItsOwnNameAndDistinctValue(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	cfg.Primaries[0].Values.Distinct = []string{"spec.left"}
	g := newGenerator(t, cfg)
	seen := map[string]string{}
	for seed := uint64(1); seed <= 100; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range actions([]*plan.Plan{p}) {
			if a.Do != plan.Create {
				continue
			}
			object := decode(t, a.Object)
			name := object["metadata"].(map[string]any)["name"].(string)
			seen[a.Slot] = name + " " + object["spec"].(map[string]any)["left"].(string)
		}
	}
	want := map[string]string{"a": "gadget one", "b": "gadget-b one-b", "c": "gadget-c one-c"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("Slots create %v, want %v.", seen, want)
	}
}

func TestChangeFixtureSetsAWordAtAValuesFixturesPath(t *testing.T) {
	changed := 0
	for _, a := range actions(plans(t, toyConfig, 100)) {
		if a.Do != plan.ChangeFixture {
			continue
		}
		changed++
		label, _ := decode(t, a.Patch)["data"].(map[string]any)["label"].(string)
		if *a.Target != (plan.Target{Resource: "configmaps", Name: "widget-config"}) || len(label)%4 != 0 || label == "" {
			t.Errorf("changeFixture %+v sets %s.", a.Target, a.Patch)
		}
	}
	if changed == 0 {
		t.Error("No plan changes a fixture.")
	}
}

func TestChangeFixtureTargetsTheFixturesNamespace(t *testing.T) {
	for _, a := range actions(plans(t, "../../targets/toy-widget/grant/config.yaml", 100)) {
		if want := (plan.Target{Resource: "configmaps", Namespace: "rf-shared", Name: "grant-config"}); a.Do == plan.ChangeFixture && *a.Target != want {
			t.Fatalf("changeFixture targets %+v, want %+v.", a.Target, want)
		}
	}
}

func TestWithoutValuesFixturesNoPlanChangesAFixture(t *testing.T) {
	for _, a := range actions(plans(t, noFixtureValues, 100)) {
		if a.Do == plan.ChangeFixture {
			t.Fatalf("A plan changes a fixture: %+v.", a)
		}
	}
}

func TestFixtureKindsAreResourcesWithoutRBAC(t *testing.T) {
	g := newGenerator(t, loadConfig(t, rulesConfig))
	want := []gvk.GroupResource{{Group: "rules.reconciler-fuzzer", Resource: "gadgets"}, {Resource: "secrets"}}
	if !slices.Equal(g.resources, want) {
		t.Errorf("Resources are %v, want %v.", g.resources, want)
	}
}

func TestSomeStepsHoldActionsAndAFault(t *testing.T) {
	for _, p := range plans(t, toyConfig, 50) {
		for _, step := range p.Steps {
			if len(step.Actions) > 0 && len(step.Faults) > 0 {
				return
			}
		}
	}
	t.Error("No step holds both actions and a fault.")
}

func TestDamageNamesALiveSlot(t *testing.T) {
	for _, a := range actions(plans(t, toyConfig, 50)) {
		if a.Do == plan.Damage && a.Slot != "" {
			return
		}
	}
	t.Error("No damage names a slot.")
}

func TestFaultsFavorThePrimaryAndTheCoreKindsTheControllerCreates(t *testing.T) {
	counts := map[string]int{}
	for _, f := range faults(plans(t, "../../examples/cert-manager/config.yaml", 200)) {
		if !f.HoldWatch {
			counts[f.Match.Resource]++
		}
	}
	for _, favored := range []string{"secrets", "certificates"} {
		if counts[favored] < 2*counts["orders"] {
			t.Errorf("Faults match resources %v, want %s over twice as often as orders.", counts, favored)
		}
	}
}

func TestFaultsPickTheFirstOrEveryMatchMostOften(t *testing.T) {
	counts := map[int]int{}
	for _, f := range faults(plans(t, toyConfig, 200)) {
		counts[f.Nth]++
	}
	if counts[0]+counts[1] < 2*(counts[2]+counts[3]) {
		t.Errorf("Faults draw nth %v, want 0 and 1 twice as often as 2 and 3.", counts)
	}
}

func TestAFaultOnThePrimaryComesWithAnUpdate(t *testing.T) {
	updates := 0
	for _, p := range plans(t, toyConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Faults) == 1 && step.Faults[0].Match.Group == "toy.reconciler-fuzzer" && len(step.Actions) == 1 &&
				step.Actions[0].Do == plan.Update {
				updates++
			}
		}
	}
	if updates == 0 {
		t.Error("No step holds a fault on the primary with an update alone.")
	}
}

func TestAboutAQuarterOfDeletesAreForeground(t *testing.T) {
	counts := map[plan.Propagation]int{}
	for _, a := range actions(plans(t, toyConfig, 200)) {
		if a.Do == plan.Delete {
			counts[a.Propagation]++
		} else if a.Propagation != "" {
			t.Errorf("A %s carries propagation %q.", a.Do, a.Propagation)
		}
	}
	all := counts[""] + counts[plan.Foreground]
	if share := float64(counts[plan.Foreground]) / float64(all); share < 0.15 || share > 0.35 || len(counts) != 2 {
		t.Errorf("Deletes have propagations %v, want about 1 in 4 Foreground.", counts)
	}
}

func TestAClusterScopedPrimaryDrawsObjectsWithoutANamespace(t *testing.T) {
	sizes := map[any]bool{}
	for _, a := range actions(plans(t, "testdata/cluster/config.yaml", 50)) {
		if a.Do != plan.Create {
			continue
		}
		object := decode(t, a.Object)
		metadata := object["metadata"].(map[string]any)
		if _, set := metadata["namespace"]; set {
			t.Errorf("A create of a cluster-scoped Realm names a namespace: %s.", a.Object)
		}
		spec, _ := object["spec"].(map[string]any)
		sizes[spec["size"]] = true
	}
	if len(sizes) < 2 {
		t.Errorf("Creates drew sizes %v, want several.", sizes)
	}
}
