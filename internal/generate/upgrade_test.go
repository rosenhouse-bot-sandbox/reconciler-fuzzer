package generate

import (
	"reflect"
	"slices"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const upgradesConfig = "testdata/upgrades/config.yaml"

func isUpgrade(a plan.Action) bool { return a.Binary != "" || a.Args != nil || a.CRDs != nil }

func upgrades(step plan.Step) []plan.Upgrade {
	var found []plan.Upgrade
	for _, a := range step.Actions {
		if isUpgrade(a) {
			found = append(found, a.Upgrade)
		}
	}
	return found
}

// Each upgrade is a restart that ends its step, 0 to stable after the
// action before it, in a step no earlier than the previous upgrade's.
func TestEveryPlanAppliesEachUpgradeOnceInOrder(t *testing.T) {
	t.Skip("enabled by the load package")
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
				if a.Do != plan.Restart || !isUpgrade(a) || a.At < before || a.At > before+plan.Duration(cfg.Timeouts.Stable) {
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
	t.Skip("enabled by the load package")
	inside := 0
	for _, p := range plans(t, upgradesConfig, 20) {
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if isUpgrade(a) && len(step.Faults) > 0 && a.At < step.Period {
					inside++
				}
			}
		}
	}
	if inside == 0 {
		t.Error("No upgrade of 20 plans comes inside a fault period.")
	}
}

// The new version must finalize CRs the old one made.
func TestSomeUpgradesComeBeforeADelete(t *testing.T) {
	t.Skip("enabled by the load package")
	before := 0
	for _, p := range plans(t, upgradesConfig, 20) {
		upgraded := false
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if upgraded && a.Do == plan.Delete {
					before++
				}
				upgraded = upgraded || isUpgrade(a)
			}
		}
	}
	if before == 0 {
		t.Error("No delete of 20 plans comes after an upgrade.")
	}
}

// Upgrades are drawn after the rest of the plan, so a seed's plan only
// gains the restarts.
func TestUpgradesOnlyAddRestarts(t *testing.T) {
	t.Skip("enabled by the load package")
	cfg := loadConfig(t, upgradesConfig)
	without := *cfg
	without.Launch.Upgrades = nil
	upgraded, plain := drawPlans(t, newGenerator(t, cfg), 20), drawPlans(t, newGenerator(t, &without), 20)
	for i, p := range upgraded {
		for n := range p.Steps {
			p.Steps[n].Actions = slices.DeleteFunc(p.Steps[n].Actions, isUpgrade)
		}
		if !reflect.DeepEqual(p, plain[i]) {
			t.Errorf("Seed %d's plan without its upgrades differs from the plan of the config without upgrades.", i+1)
		}
	}
}
