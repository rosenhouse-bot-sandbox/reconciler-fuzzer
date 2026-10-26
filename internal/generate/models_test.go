package generate

import (
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const modelsConfig = "testdata/models/config.yaml"

var modelMatches = []plan.Match{
	{Group: "models.reconciler-fuzzer", Resource: "mice"},
	{Group: "models.reconciler-fuzzer", Resource: "gizmos"},
}

func TestWithModelsFaultsIncludeStuckAndSlowDeleteOnEachModelKind(t *testing.T) {
	drawn := map[string]int{}
	requests := 0
	for _, f := range faults(plans(t, modelsConfig, 200)) {
		if !f.ModelFault() {
			requests++
			continue
		}
		if f.Match != modelMatches[0] && f.Match != modelMatches[1] || f.Nth != 0 || f.System != "" {
			t.Errorf("Model fault %s matches more than a model kind.", f)
		}
		drawn[f.Effect()+" "+f.Match.Resource]++
	}
	for _, key := range []string{"stuck mice", "stuck gizmos", "slowDelete mice", "slowDelete gizmos"} {
		if drawn[key] == 0 {
			t.Errorf("No plan draws %s; drawn %v.", key, drawn)
		}
	}
	if requests == 0 {
		t.Error("No plan draws a request fault.")
	}
}

func TestAModelFaultPeriodAddsTheLongestTrack(t *testing.T) {
	const longestTrack, stable = 3 * time.Second, time.Second
	longest := time.Duration(0)
	for _, p := range plans(t, modelsConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Faults) == 0 {
				continue
			}
			least, most := 500*time.Millisecond, 2*stable
			if step.Faults[0].ModelFault() {
				least, most = least+longestTrack, most+longestTrack
			}
			if period := time.Duration(step.Period); period < least || period > most {
				t.Errorf("Seed %d draws %s for %s, want %s to %s.", p.Seed, period, step.Faults[0], least, most)
			}
			longest = max(longest, time.Duration(step.Period))
		}
	}
	if longest <= 2*stable+longestTrack-500*time.Millisecond {
		t.Errorf("The longest period is %s.", longest)
	}
}

func TestAModelFaultComesWithTheActionItExercises(t *testing.T) {
	g := newGenerator(t, loadConfig(t, modelsConfig))
	stuck := plan.Fault{Match: modelMatches[0], Stuck: true}
	slowDelete := plan.Fault{Match: modelMatches[0], SlowDelete: true}
	for _, c := range []struct {
		fault plan.Fault
		live  bool
		want  plan.Do
	}{
		{stuck, true, plan.Update},
		{stuck, false, plan.Create},
		{slowDelete, true, plan.Delete},
		{slowDelete, false, plan.Restart},
	} {
		action := example(t, 0, func(rt *rapid.T) plan.Action {
			rapid.Bool().Draw(rt, "rapid needs a draw")
			at := state{}
			if c.live {
				at["a"] = g.kinds[0].base("a").Object
			}
			return g.traffic(rt, at, c.fault)
		})
		if action.Do != c.want {
			t.Errorf("%s with a live CR %t comes with %s, want %s.", c.fault, c.live, action.Do, c.want)
		}
	}
}

// toy-fleet's Role serves the reconcilers of three kinds, but a Cluster owns
// only Volumes, which a model plays. Faults and damage should favor them over
// the Secrets and Users a Cluster never touches.
func TestTheFleetToyFavorsTheModelKindOverOtherRBACKinds(t *testing.T) {
	ps := plans(t, fleetClusterConfig, 200)
	counts := map[string]int{}
	for _, f := range faults(ps) {
		if !f.ModelFault() {
			counts[f.Match.Resource]++
		}
	}
	for _, a := range actions(ps) {
		if a.Do == plan.Damage {
			counts[a.Target.Resource]++
		}
	}
	for _, other := range []string{"secrets", "configmaps", "users", "databases"} {
		if counts["volumes"] < 2*counts[other] {
			t.Errorf("Faults and damage name resources %v, want volumes over twice as often as %s.", counts, other)
		}
	}
}

func TestTheGadgetToyDrawsBothModelFaultsOnGadgets(t *testing.T) {
	gadgets := plan.Match{Group: "toy.reconciler-fuzzer", Resource: "gadgets"}
	drawn := map[string]bool{}
	for _, f := range faults(plans(t, "../../targets/toy-widget/gadget/config.yaml", 50)) {
		if f.ModelFault() {
			if f.Match != gadgets {
				t.Errorf("Model fault %s matches no Gadget.", f)
			}
			drawn[f.Effect()] = true
		}
	}
	if !drawn["stuck"] || !drawn["slowDelete"] {
		t.Errorf("The gadget toy draws model faults %v, want stuck and slowDelete.", drawn)
	}
}
