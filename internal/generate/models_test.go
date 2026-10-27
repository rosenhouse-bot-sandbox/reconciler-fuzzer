package generate

import (
	"slices"
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

// A restart or kill while slowDelete holds a deletion finds a controller
// that keeps finalizer state in memory.
func TestAQuarterOfSlowDeleteStepsRestartOrKillAfterTheDelete(t *testing.T) {
	var held, restarted int
	for _, p := range plans(t, modelsConfig, 400) {
		for _, step := range p.Steps {
			if !slices.ContainsFunc(step.Faults, func(f plan.Fault) bool { return f.SlowDelete }) {
				continue
			}
			del := slices.IndexFunc(step.Actions, func(a plan.Action) bool { return a.Do == plan.Delete })
			if del < 0 {
				continue
			}
			held++
			if slices.ContainsFunc(step.Actions[del:], func(a plan.Action) bool { return a.Do == plan.Restart || a.Do == plan.Kill }) {
				restarted++
			}
		}
	}
	if restarted*4 < held {
		t.Errorf("%d of %d slowDelete steps with a delete restart or kill after it, want a quarter.", restarted, held)
	}
}

// A restart that follows a delete within milliseconds comes before the
// controller has begun to finalize, so it tests nothing about finalizers.
func TestSomeRestartsOrKillsComeWhileADeleteIsFinalizing(t *testing.T) {
	var late int
	for _, p := range plans(t, modelsConfig, 400) {
		for _, step := range p.Steps {
			del := slices.IndexFunc(step.Actions, func(a plan.Action) bool { return a.Do == plan.Delete })
			if del < 0 {
				continue
			}
			for _, a := range step.Actions[del:] {
				if (a.Do == plan.Restart || a.Do == plan.Kill) && time.Duration(a.At-step.Actions[del].At) >= 300*time.Millisecond {
					late++
				}
			}
		}
	}
	if late < 20 {
		t.Errorf("%d restarts or kills over 400 plans come 300ms or more after a delete in their step, want 20.", late)
	}
}

// A restart or kill at an offset lands while a model fault holds the delete
// before it.
func TestARestartOrKillAtAnOffsetComesWhileAModelFaultHoldsADelete(t *testing.T) {
	drawn := map[plan.Do]int{}
	for _, p := range plans(t, modelsConfig, 400) {
		for _, step := range p.Steps {
			for i, a := range step.Actions {
				if a.At == 0 || a.Do != plan.Restart && a.Do != plan.Kill {
					continue
				}
				drawn[a.Do]++
				del := slices.IndexFunc(step.Actions[:i], func(a plan.Action) bool { return a.Do == plan.Delete })
				if del < 0 || len(step.Faults) == 0 || !step.Faults[0].ModelFault() || time.Duration(a.At-step.Actions[del].At) < 300*time.Millisecond || a.At >= step.Period {
					t.Errorf("Seed %d draws a %s at %v, with faults %v, actions %+v and period %v.", p.Seed, a.Do, time.Duration(a.At), step.Faults, step.Actions, time.Duration(step.Period))
				}
			}
		}
	}
	if drawn[plan.Restart] == 0 || drawn[plan.Kill] == 0 {
		t.Errorf("Restarts and kills at an offset are %v, want both.", drawn)
	}
}

// Half the model-fault steps that delete leave the deletion to one process.
func TestSomeModelFaultStepsOnlyDelete(t *testing.T) {
	for _, p := range plans(t, modelsConfig, 400) {
		for _, step := range p.Steps {
			if len(step.Faults) == 1 && step.Faults[0].ModelFault() && len(step.Actions) == 1 && step.Actions[0].Do == plan.Delete {
				return
			}
		}
	}
	t.Error("No model-fault step over 400 plans holds a delete alone.")
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
