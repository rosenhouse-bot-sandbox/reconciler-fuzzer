package generate

import (
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const fakesConfig = "testdata/fakes/config.yaml"

var fakeNames = []string{"cloud", "dns"}

func TestWithFakesHalfTheFaultsNameAFakeWithAnyEffectAndHalfMatchAnyVerb(t *testing.T) {
	drawn := map[string]int{}
	all := faults(plans(t, fakesConfig, 300))
	onFakes, requests := 0, 0
	for _, f := range all {
		if !slices.Contains(fakeNames, f.System) {
			continue
		}
		onFakes++
		drawn[f.System]++
		drawn[f.Effect()]++
		if f.ModelFault() {
			if f.Match != (plan.Match{}) || f.Nth != 0 {
				t.Errorf("Model fault %s on a fake matches something.", f)
			}
			continue
		}
		if !slices.Contains([]string{"", "create", "get", "list", "update", "patch", "delete"}, f.Match.Verb) ||
			f.Match != (plan.Match{Verb: f.Match.Verb}) || f.Nth > 1 {
			t.Errorf("Request fault %s on a fake matches more than a verb, or picks a late request.", f)
		}
		drawn[f.Match.Verb]++
		requests++
	}
	if share := float64(onFakes) / float64(len(all)); share < 0.4 || share > 0.6 {
		t.Errorf("%d of %d faults name a fake, want about half.", onFakes, len(all))
	}
	for _, key := range []string{"cloud", "dns", "error(500)", "error(503)", "error(409)", "dropResponse", "killAfter", "drop", "stuck", "slowDelete", "create", "delete"} {
		if drawn[key] == 0 {
			t.Errorf("No fault on a fake draws %s; drawn %v.", key, drawn)
		}
	}
	if share := float64(drawn[""]) / float64(requests); share < 0.4 || share > 0.6 {
		t.Errorf("%d of %d request faults on a fake match any verb, want about half.", drawn[""], requests)
	}
	if drawn["error(429)"] > 0 {
		t.Error("A fault on a fake answers 429, a Kubernetes code.")
	}
}

func TestWithFakesHalfTheDamageTargetsAFake(t *testing.T) {
	onFakes, damage := 0, 0
	for _, p := range plans(t, fakesConfig, 300) {
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if a.Do != plan.Damage {
					continue
				}
				damage++
				if a.Target.System == "" {
					continue
				}
				onFakes++
				if !slices.Contains(fakeNames, a.Target.System) || *a.Target != (plan.Target{System: a.Target.System}) || a.Patch != nil {
					t.Errorf("Damage of %+v on a fake names more than its system.", *a.Target)
				}
			}
		}
	}
	if share := float64(onFakes) / float64(damage); share < 0.35 || share > 0.65 {
		t.Errorf("%d of %d damage actions target a fake, want about half.", onFakes, damage)
	}
}

func TestMostSeedsDrawAFaultOnAFake(t *testing.T) {
	seeds := 0
	for _, p := range plans(t, fakesConfig, 40) {
		if slices.ContainsFunc(faults([]*plan.Plan{p}), func(f plan.Fault) bool { return f.System != "" }) {
			seeds++
		}
	}
	if seeds < 20 {
		t.Errorf("%d of the first 40 seeds draw a fault on a fake, want at least 20.", seeds)
	}
}

func TestAFakeModelFaultPeriodAddsTheFakesTrack(t *testing.T) {
	const track, stable = 2 * time.Second, time.Second
	for _, p := range plans(t, fakesConfig, 200) {
		for _, step := range p.Steps {
			if len(step.Faults) == 0 || !step.Faults[0].ModelFault() {
				continue
			}
			if period := time.Duration(step.Period); period < 500*time.Millisecond+track || period > 2*stable+track {
				t.Errorf("Seed %d draws %s for %s.", p.Seed, period, step.Faults[0])
			}
		}
	}
}

func TestAFakeFaultComesWithTheActionItExercises(t *testing.T) {
	g := newGenerator(t, loadConfig(t, fakesConfig))
	request := func(verb string) plan.Fault {
		return plan.Fault{System: "cloud", Match: plan.Match{Verb: verb}, Drop: true}
	}
	for _, c := range []struct {
		fault plan.Fault
		live  bool
		want  plan.Do
	}{
		{request("create"), true, plan.Create},
		{request("create"), false, plan.Create},
		{request("delete"), true, plan.Delete},
		{request("delete"), false, plan.Create},
		{request("get"), true, plan.Damage},
		{request(""), false, plan.Create},
		{request(""), true, plan.Create},
		{plan.Fault{System: "cloud", Stuck: true}, true, plan.Create},
		{plan.Fault{System: "cloud", SlowDelete: true}, true, plan.Delete},
		{plan.Fault{System: "cloud", SlowDelete: true}, false, plan.Create},
	} {
		action := rapid.Custom(func(t *rapid.T) plan.Action {
			rapid.Bool().Draw(t, "rapid needs a draw")
			at := state{}
			if c.live {
				at["a"] = g.kinds[0].base("a").Object
			}
			return g.traffic(t, at, c.fault)
		}).Example(0)
		if action.Do != c.want {
			t.Errorf("%s with a live CR %t comes with %s, want %s.", c.fault, c.live, action.Do, c.want)
		}
		if action.Do == plan.Damage && action.Target.System != "cloud" {
			t.Errorf("%s comes with damage of %+v, want the fault's system.", c.fault, *action.Target)
		}
	}
}
