package generate

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// deletableConfig lists two fixture files. Its values.deletable lists the
// first, which holds the ConfigMap settings and the Secret password.
const deletableConfig = "testdata/deletable/config.yaml"

// addedSteps draws the plans of seeds 1 to n of deletableConfig, with and
// without values.deletable. It fails the test unless each plan is the plain
// one, maybe with one more step, and returns the added steps.
func addedSteps(t *testing.T, n int) []plan.Step {
	t.Helper()
	cfg := loadConfig(t, deletableConfig)
	without := *cfg
	without.Deletable = nil
	plain := drawPlans(t, newGenerator(t, &without), n)
	var added []plan.Step
	for i, p := range drawPlans(t, newGenerator(t, cfg), n) {
		if len(p.Steps) == len(plain[i].Steps)+1 {
			added = append(added, p.Steps[len(p.Steps)-1])
			p.Steps = p.Steps[:len(p.Steps)-1]
		}
		if !reflect.DeepEqual(p, plain[i]) {
			t.Errorf("Seed %d's plan differs from the plan without values.deletable by more than a last step.", p.Seed)
		}
	}
	return added
}

// The fixture delete is drawn after the rest of the plan, so a seed's plan
// only gains a last step.
func TestValuesDeletableOnlyAddsALastStep(t *testing.T) {
	if added := len(addedSteps(t, 100)); added < 30 || added > 70 {
		t.Errorf("%d of 100 plans gain a last step, want about half.", added)
	}
}

func TestTheLastStepDeletesEachObjectOfTheListedFileAndNoOther(t *testing.T) {
	listed := []plan.Target{{Resource: "configmaps", Name: "settings"}, {Resource: "secrets", Name: "password"}}
	deleted := map[plan.Target]bool{}
	for _, step := range addedSteps(t, 200) {
		del := step.Actions[0]
		if del.Target == nil || !reflect.DeepEqual(del, plan.Action{Do: plan.ChangeFixture, Target: del.Target}) || !slices.Contains(listed, *del.Target) {
			t.Errorf("The last step starts with %+v, want a delete of an object of deletable.yaml.", del)
			continue
		}
		deleted[*del.Target] = true
	}
	if len(deleted) != len(listed) {
		t.Errorf("The last steps delete %v, want each of %v.", deleted, listed)
	}
}

// Half the last steps then restart or kill the controller 300 ms or more
// after the delete, within a fault period that period draws.
func TestTheLastStepThenMayRestartOrKillInItsFaultPeriod(t *testing.T) {
	least, most := plan.Duration(500*time.Millisecond), plan.Duration(2*loadConfig(t, deletableConfig).Timeouts.Stable)
	added := addedSteps(t, 200)
	then, periods := map[plan.Do]int{}, map[plan.Duration]bool{}
	for _, step := range added {
		switch late := step.Actions[1:]; {
		case len(step.Faults) > 0:
			t.Errorf("The last step %+v has faults, want none.", step)
		case len(late) == 0 && step.Period == 0:
			then[""]++
		case len(late) == 1 && (late[0].Do == plan.Restart || late[0].Do == plan.Kill) && late[0].At >= plan.Duration(300*time.Millisecond) && late[0].At < step.Period && step.Period >= least && step.Period <= most:
			then[late[0].Do]++
			periods[step.Period] = true
		default:
			t.Errorf("The last step is %+v, want the delete alone, or then a restart or kill 300 ms or more later and within a period of %v to %v.", step, time.Duration(least), time.Duration(most))
		}
	}
	if late := then[plan.Restart] + then[plan.Kill]; late*10 < len(added)*3 || late*10 > len(added)*7 || then[plan.Restart] == 0 || then[plan.Kill] == 0 {
		t.Errorf("Of %d last steps, %d then restart and %d kill, want about half to do either and each to occur.", len(added), then[plan.Restart], then[plan.Kill])
	}
	if len(periods) < 2 {
		t.Errorf("The last steps draw the fault periods %v, want several.", periods)
	}
}
