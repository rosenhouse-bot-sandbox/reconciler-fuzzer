package generate

import (
	"reflect"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const upgradesConfig = "testdata/upgrades/config.yaml"

func upgrades(step plan.Step) []plan.Upgrade {
	var found []plan.Upgrade
	for _, a := range step.Actions {
		if a.Binary != "" || a.Args != nil || a.CRDs != nil {
			found = append(found, a.Upgrade)
		}
	}
	return found
}

// Each upgrade is a restart that ends its step, 0 to stable after the
// action before it, in a step no earlier than the previous upgrade's.
func TestEveryPlanAppliesEachUpgradeOnceInOrder(t *testing.T) {
	t.Skip("enabled by the generate package")
	cfg := loadConfig(t, upgradesConfig)
	for i, p := range drawPlans(t, newGenerator(t, cfg), 20) {
		var got []plan.Upgrade
		for n, step := range p.Steps {
			found := upgrades(step)
			got = append(got, found...)
			if len(found) == 0 {
				continue
			}
			first := len(step.Actions) - len(found)
			for j, a := range step.Actions[first:] {
				var before plan.Duration
				if first+j > 0 {
					before = step.Actions[first+j-1].At
				}
				if a.Do != plan.Restart || len(upgrades(plan.Step{Actions: []plan.Action{a}})) == 0 || a.At < before || a.At > before+plan.Duration(cfg.Timeouts.Stable) {
					t.Errorf("Seed %d's step %d ends with %+v after an action at %v, want a restart 0 to %v later.", i+1, n, a, before, cfg.Timeouts.Stable)
				}
			}
		}
		if !reflect.DeepEqual(got, cfg.Launch.Upgrades) {
			t.Errorf("Seed %d's plan upgrades to %+v, want %+v.", i+1, got, cfg.Launch.Upgrades)
		}
	}
}

// An upgrade during a fault meets the controller mid-reconcile.
func TestSomeUpgradesComeInsideAFaultPeriod(t *testing.T) {
	t.Skip("enabled by the generate package")
	inside := 0
	for _, p := range plans(t, upgradesConfig, 20) {
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if len(upgrades(plan.Step{Actions: []plan.Action{a}})) > 0 && len(step.Faults) > 0 && a.At < step.Period {
					inside++
				}
			}
		}
	}
	if inside == 0 {
		t.Error("No upgrade of 20 plans comes inside a fault period.")
	}
}
