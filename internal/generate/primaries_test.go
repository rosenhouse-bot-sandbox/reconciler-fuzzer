package generate

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
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

func TestCreatesDrawEachKindsOwnFieldsAndValues(t *testing.T) {
	drawn := map[string]map[string]bool{"Cluster": {}, "Database": {}}
	for _, a := range actions(plans(t, fleetConfig, 100)) {
		if a.Do != plan.Create {
			continue
		}
		object := decode(t, a.Object)
		kind, _ := object["kind"].(string)
		for field, value := range object["spec"].(map[string]any) {
			drawn[kind][fmt.Sprintf("%s=%v", field, value)] = true
		}
	}
	want := map[string][]string{
		"Cluster":  {"volumeSize=large", "volumeSize=small", "volumes=1", "volumes=2", "volumes=3"},
		"Database": {"clusterRef=cluster", "engine=mysql", "engine=postgres"},
	}
	for kind, values := range want {
		if got := slices.Sorted(maps.Keys(drawn[kind])); !slices.Equal(got, values) {
			t.Errorf("%s creates draw %v, want %v.", kind, got, values)
		}
	}
}

func TestTheFirstCreateDrawsEachListedKindAboutEqually(t *testing.T) {
	counts := map[string]int{}
	for _, p := range plans(t, fleetConfig, 200) {
		kind, _ := decode(t, p.Steps[0].Actions[0].Object)["kind"].(string)
		counts[kind]++
	}
	if counts["Cluster"] < 70 || counts["Database"] < 70 {
		t.Errorf("200 plans first create %v, want about 100 of each kind.", counts)
	}
}

// toy-fleet's Role grants no create on Users, and a config without RBAC
// grants nothing, so the controller makes no object such a damage could
// delete. external-secrets' Role lets it create the listed ExternalSecrets,
// which `delete` actions cover.
func TestDamageTargetsTheUnlistedKindsTheRBACLetsTheControllerCreate(t *testing.T) {
	for path, want := range map[string][]string{
		fleetConfig: {"configmaps", "secrets", "volumes"},
		"../../examples/external-secrets/config.yaml": {"generatorstates", "pushsecrets", "secrets"},
		rulesConfig: nil,
	} {
		damaged := map[string]bool{}
		for _, a := range actions(plans(t, path, 200)) {
			if a.Do == plan.Damage {
				damaged[a.Target.Resource] = true
			}
		}
		if got := slices.Sorted(maps.Keys(damaged)); !slices.Equal(got, want) {
			t.Errorf("%s: damage targets %v, want %v.", path, got, want)
		}
	}
}

// fleetState holds a Cluster in slot a and a Database in slot b.
func fleetState(g *Generator) state {
	return state{"a": g.kinds[0].base("a").Object, "b": g.kinds[1].base("b").Object}
}

// firstActions sum up the first action of 20 draws of traffic: the slot it
// updates or deletes, the kind it creates, or else its do.
func firstActions(t *testing.T, at state, traffic func(*rapid.T, state) []plan.Action) []string {
	t.Helper()
	var slots []string
	for seed := range 20 {
		drawn := example(t, seed, func(rt *rapid.T) []plan.Action {
			rapid.Bool().Draw(rt, "rapid needs a draw")
			return traffic(rt, maps.Clone(at))
		})
		switch first := drawn[0]; first.Do {
		case plan.Update, plan.Delete:
			slots = append(slots, first.Slot)
		case plan.Create:
			slots = append(slots, fmt.Sprint("create ", decode(t, first.Object)["kind"]))
		default:
			slots = append(slots, string(first.Do))
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(slots)))
}

func TestARequestFaultOnAListedKindComesWithAnUpdateOfThatKind(t *testing.T) {
	g := newGenerator(t, loadConfig(t, fleetConfig))
	onlyCluster := state{"a": g.kinds[0].base("a").Object}
	for _, c := range []struct {
		resource string
		at       state
		want     []string
	}{
		{"clusters", fleetState(g), []string{"a"}},
		{"databases", fleetState(g), []string{"b"}},
		{"databases", onlyCluster, []string{"restart"}},
	} {
		f := plan.Fault{Match: plan.Match{Verb: "update", Group: "toy.reconciler-fuzzer", Resource: c.resource}, Drop: true}
		got := firstActions(t, c.at, func(t *rapid.T, at state) []plan.Action { return []plan.Action{g.traffic(t, at, f)} })
		if !slices.Equal(got, c.want) {
			t.Errorf("A fault on %s with slots %v comes with %v, want %v.", c.resource, slices.Sorted(maps.Keys(c.at)), got, c.want)
		}
	}
}

// A damage makes the controller write the kind again, but it can delete only
// what the controller made. A restart makes the controller read every kind.
func TestARequestFaultOnAnUnlistedKindComesWithADamageOnlyIfTheControllerMayCreateIt(t *testing.T) {
	g := newGenerator(t, loadConfig(t, fleetConfig))
	for _, c := range []struct {
		group, resource string
		want            []string
	}{
		{"storage.thirdparty", "volumes", []string{"damage"}},
		{"toy.reconciler-fuzzer", "users", []string{"restart"}},
	} {
		f := plan.Fault{Match: plan.Match{Verb: "get", Group: c.group, Resource: c.resource}, Drop: true}
		got := firstActions(t, fleetState(g), func(t *rapid.T, at state) []plan.Action { return []plan.Action{g.traffic(t, at, f)} })
		if !slices.Equal(got, c.want) {
			t.Errorf("A fault on %s comes with %v, want %v.", c.resource, got, c.want)
		}
	}
}

func TestAHoldWatchOfAListedKindRecreatesOrCreatesACROfThatKind(t *testing.T) {
	g := newGenerator(t, loadConfig(t, fleetConfig))
	database := g.kinds[1].base("b").Object
	databases := state{"a": g.kinds[1].base("a").Object, "b": database, "c": g.kinds[1].base("c").Object}
	for _, c := range []struct {
		group, resource string
		at              state
		want            []string
	}{
		{"toy.reconciler-fuzzer", "clusters", fleetState(g), []string{"a"}},
		{"toy.reconciler-fuzzer", "databases", fleetState(g), []string{"b"}},
		{"toy.reconciler-fuzzer", "clusters", state{"b": database}, []string{"create Cluster"}},
		{"toy.reconciler-fuzzer", "databases", state{}, []string{"create Database"}},
		{"toy.reconciler-fuzzer", "clusters", databases, []string{"a", "b", "c"}},
		{"toy.reconciler-fuzzer", "users", fleetState(g), []string{"a", "b"}},
		{"toy.reconciler-fuzzer", "users", state{}, []string{"create Cluster", "create Database"}},
		{"", "configmaps", fleetState(g), []string{"a", "b"}},
	} {
		f := plan.Fault{Match: plan.Match{Group: c.group, Resource: c.resource}, HoldWatch: true}
		got := firstActions(t, c.at, func(t *rapid.T, at state) []plan.Action { return g.holdTraffic(t, at, f) })
		if !slices.Equal(got, c.want) {
			t.Errorf("A hold of %s with slots %v comes with %v, want %v.", c.resource, slices.Sorted(maps.Keys(c.at)), got, c.want)
		}
	}
}

func TestABurstCreatesTheKindItDeleted(t *testing.T) {
	recreates := 0
	for _, p := range plans(t, fleetConfig, 200) {
		kinds := map[string]any{}
		for _, step := range p.Steps {
			a := step.Actions
			if len(step.Faults) == 0 && len(a) == 2 && a[1].At != 0 && a[0].Do == plan.Delete && a[1].Do == plan.Create {
				recreates++
				if created := decode(t, a[1].Object)["kind"]; created != kinds[a[0].Slot] {
					t.Errorf("seed %d bursts a delete of a %s and a create of a %s.", p.Seed, kinds[a[0].Slot], created)
				}
			}
			for _, action := range a {
				if action.Do == plan.Create {
					kinds[action.Slot] = decode(t, action.Object)["kind"]
				}
			}
		}
	}
	if recreates == 0 {
		t.Error("No burst deletes and creates a slot.")
	}
}

// A stuck fault, in the models config, and a fault on a read-only fake
// come with an update when a live CR has fields.
func TestNoPlanUpdatesAKindWithoutFields(t *testing.T) {
	for _, c := range []struct {
		path      string
		fieldless int
		want      map[any]bool
	}{
		{fleetConfig, 1, map[any]bool{"Cluster": true}},
		{modelsConfig, 0, map[any]bool{}},
		{"testdata/systems/read-only.yaml", 0, map[any]bool{}},
	} {
		g := newGenerator(t, loadConfig(t, c.path))
		g.kinds[c.fieldless].fields = nil
		updated := map[any]bool{}
		for _, p := range drawPlans(t, g, 200) {
			kinds := map[string]any{}
			for _, a := range actions([]*plan.Plan{p}) {
				switch a.Do {
				case plan.Create:
					kinds[a.Slot] = decode(t, a.Object)["kind"]
				case plan.Update:
					updated[kinds[a.Slot]] = true
				}
			}
		}
		if !maps.Equal(updated, c.want) {
			t.Errorf("%s: plans update %v, want %v.", c.path, updated, c.want)
		}
	}
}

func TestPlanRefusesACreateOfAKindTheConfigDoesNotList(t *testing.T) {
	g := newGenerator(t, loadConfig(t, fleetConfig))
	g.listed = g.listed[:1]
	for seed := uint64(1); seed <= 50; seed++ {
		if _, err := g.Plan(seed); err != nil {
			if !strings.Contains(err.Error(), "makes a Database.toy.reconciler-fuzzer, which primaries does not list") {
				t.Errorf("Plan(%d) = %v, want a refused Database.", seed, err)
			}
			return
		}
	}
	t.Error("Plan accepts 50 plans, though the config lists no Database.")
}

func TestWithSeveralKindsNotesNameTheKindAndErrorsNameTheEntry(t *testing.T) {
	one := loadConfig(t, rulesConfig)
	for _, note := range newGenerator(t, one).LeftAlone() {
		if !strings.HasPrefix(note, "generation leaves spec.") {
			t.Errorf("With one kind, LeftAlone notes %q.", note)
		}
	}
	several := loadConfig(t, fleetConfig)
	several.CRDs = append(several.CRDs, one.CRDs...)
	several.Primaries = append(several.Primaries, one.Primaries[0])
	notes := newGenerator(t, several).LeftAlone()
	for _, note := range notes {
		if !strings.HasPrefix(note, "Gadget: generation leaves spec.") {
			t.Errorf("With several kinds, LeftAlone notes %q.", note)
		}
	}
	if len(notes) != 2 {
		t.Errorf("LeftAlone notes %q, want spec.right and spec.surge.", notes)
	}
	for _, c := range []struct {
		cfg   *config.Config
		entry int
		want  string
	}{
		{one, 0, "generating plans: values.mutate spec.nonesuch"},
		{several, 1, "generating plans: primaries[1]: values.mutate spec.nonesuch"},
	} {
		c.cfg.Primaries[c.entry].Values.Mutate = []string{"spec.nonesuch"}
		if _, err := New(c.cfg); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("New returned %v, want an error that starts %q.", err, c.want)
		}
	}
}

func TestNewRefusesASampleOfAnotherKind(t *testing.T) {
	cfg := loadConfig(t, fleetConfig)
	cfg.Primaries[1].Sample = cfg.Primaries[0].Sample
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "the sample holds a Cluster.toy.reconciler-fuzzer, not a Database.toy.reconciler-fuzzer") {
		t.Errorf("New returned %v, want it to refuse a Cluster as the Database sample.", err)
	}
}

func TestASampleWhoseSlotNameTheCRDRefusesIsAConfigurationError(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	sample, err := readObject(cfg.Primaries[0].Sample)
	if err != nil {
		t.Fatal(err)
	}
	// The CRD takes names of up to 20 characters, so slot b's 21 are refused.
	sample["metadata"].(map[string]any)["name"] = strings.Repeat("g", 19)
	cfg.Primaries[0].Sample = filepath.Join(t.TempDir(), "sample.json")
	if err := os.WriteFile(cfg.Primaries[0].Sample, marshal(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "the CRD refuses the CR of slot b") {
		t.Errorf("New returned %v, want it to say the CRD refuses slot b's CR.", err)
	}
}
