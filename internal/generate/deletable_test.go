package generate

import (
	"reflect"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// deleting lets cfg's plans delete the objects of each kube fixture file.
func deleting(cfg *config.Config) *config.Config {
	cfg.Deletable = cfg.Fixtures
	return cfg
}

// The fixture delete is drawn after the rest of the plan, so a seed's plan
// only gains a last step. The step deletes the fixture, then restarts,
// kills or does neither.
func TestValuesDeletableOnlyAddsALastStepThatDeletesAFixture(t *testing.T) {
	plain := plans(t, noFixtureValues, 100)
	fixture := plan.Target{Resource: "configmaps", Name: "widget-config"}
	then := map[plan.Do]int{}
	for i, p := range drawPlans(t, newGenerator(t, deleting(loadConfig(t, noFixtureValues))), 100) {
		if len(p.Steps) > len(plain[i].Steps) {
			last := p.Steps[len(p.Steps)-1]
			p.Steps = p.Steps[:len(p.Steps)-1]
			if a := last.Actions[0]; a.Do != plan.ChangeFixture || *a.Target != fixture || a.Patch != nil || len(last.Faults) > 0 {
				t.Errorf("Seed %d's last step is %+v, want one that deletes %+v first.", p.Seed, last, fixture)
			}
			var do plan.Do
			if len(last.Actions) > 1 {
				do = last.Actions[1].Do
			}
			if len(last.Actions) > 2 || do != "" && do != plan.Restart && do != plan.Kill || (do == plan.Kill) != (last.Period > 0) {
				t.Errorf("Seed %d's last step is %+v, want a delete then a restart, a kill with a period, or neither.", p.Seed, last)
			}
			then[do]++
		}
		if !reflect.DeepEqual(p, plain[i]) {
			t.Errorf("Seed %d's plan differs from the plan without values.deletable by more than a last step.", p.Seed)
		}
	}
	if n := then[""] + then[plan.Restart] + then[plan.Kill]; n < 30 || n > 70 || then[""] == 0 || then[plan.Restart] == 0 || then[plan.Kill] == 0 {
		t.Errorf("%d of 100 plans end with a fixture delete, then %v; want about half, then each of nothing, a restart and a kill.", n, then)
	}
}

// A plan may end with a referenced fixture deleted already. Its last step
// then deletes no fixture again.
func TestTheLastStepDeletesOnlyALiveFixture(t *testing.T) {
	ended := 0
	for _, p := range plans(t, referencesConfig, 200) {
		if endsDeleted(t, p) {
			ended++
		}
	}
	for _, p := range drawPlans(t, newGenerator(t, deleting(loadConfig(t, referencesConfig))), 200) {
		endsDeleted(t, p)
	}
	if ended < 10 {
		t.Errorf("%d of 200 plans end with the Secret deleted, want 10 or more.", ended)
	}
}

// endsDeleted says whether p ends with its one fixture deleted. A delete of
// the deleted fixture fails the test.
func endsDeleted(t *testing.T, p *plan.Plan) bool {
	t.Helper()
	deleted := false
	for n, step := range p.Steps {
		for _, a := range step.Actions {
			switch {
			case a.Do == plan.ChangeFixture && deleted:
				t.Errorf("Seed %d's step %d deletes the fixture it already deleted.", p.Seed, n)
			case a.Do == plan.ChangeFixture:
				deleted = true
			case a.Do == plan.CreateFixture:
				deleted = false
			}
		}
	}
	return deleted
}
