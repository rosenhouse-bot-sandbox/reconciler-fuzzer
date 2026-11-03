package generate

import (
	"reflect"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// deleting lets cfg's plans delete the objects of each kube fixture file.
func deleting(cfg *config.Config) *config.Config {
	cfg.Deletable = cfg.Fixtures
	return cfg
}

// The fixture delete is drawn after the rest of the plan, so a seed's plan
// only gains a last step.
func TestValuesDeletableOnlyAddsALastStep(t *testing.T) {
	plain := plans(t, noFixtureValues, 100)
	gained := 0
	for i, p := range drawPlans(t, newGenerator(t, deleting(loadConfig(t, noFixtureValues))), 100) {
		if len(p.Steps) == len(plain[i].Steps)+1 {
			gained++
			p.Steps = p.Steps[:len(p.Steps)-1]
		}
		if !reflect.DeepEqual(p, plain[i]) {
			t.Errorf("Seed %d's plan differs from the plan without values.deletable by more than a last step.", p.Seed)
		}
	}
	if gained < 30 || gained > 70 {
		t.Errorf("%d of 100 plans gain a last step, want about half.", gained)
	}
}

// The last step deletes the fixture. Half the time, a restart or kill
// follows 300 ms or more later, within the step's fault period.
func TestTheLastStepDeletesTheFixtureThenMayRestartOrKill(t *testing.T) {
	plain := plans(t, noFixtureValues, 200)
	fixture := plan.Target{Resource: "configmaps", Name: "widget-config"}
	then := map[plan.Do]int{}
	for i, p := range drawPlans(t, newGenerator(t, deleting(loadConfig(t, noFixtureValues))), 200) {
		if len(p.Steps) == len(plain[i].Steps) {
			continue
		}
		last := p.Steps[len(p.Steps)-1]
		if del := (plan.Action{Do: plan.ChangeFixture, Target: &fixture}); !reflect.DeepEqual(last.Actions[0], del) || len(last.Faults) > 0 {
			t.Errorf("Seed %d's last step is %+v, want one without faults that deletes %+v first.", p.Seed, last, fixture)
		}
		switch late := last.Actions[1:]; {
		case len(late) == 0 && last.Period == 0:
			then[""]++
		case len(late) == 1 && (late[0].Do == plan.Restart || late[0].Do == plan.Kill) && late[0].At >= plan.Duration(300*time.Millisecond) && late[0].At < last.Period:
			then[late[0].Do]++
		default:
			t.Errorf("Seed %d's last step is %+v, want the delete alone, or then a restart or kill 300 ms or more later and before Period ends.", p.Seed, last)
		}
	}
	if then[""] == 0 || then[plan.Restart] == 0 || then[plan.Kill] == 0 {
		t.Errorf("The last steps then do %v, want each of nothing, a restart and a kill.", then)
	}
}
