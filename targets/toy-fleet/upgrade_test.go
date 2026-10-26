package main_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	upgradeConfig           = "upgrade/config.yaml"
	upgradePropertiesConfig = "upgrade/properties/config.yaml"
)

// upgradeRows hold the upgrade plans' rows. Each plan's restart upgrades
// the controller to version 2.
var upgradeRows = []gateRow{
	{plan: "delete", bug: 140, finding: "Converge: did not settle (clause 2)", step: 2, detail: "cluster is being deleted", config: upgradeConfig},
	{plan: "update", bug: 141, finding: "Restore: replaced resources", step: 1, detail: "/cluster-data-0 (rule 1, slot a)", config: upgradeConfig},
	{plan: "mid-provisioning", bug: 142, finding: "Converge: did not settle (clause 1)", step: 0, detail: "cluster is not ready", config: upgradeConfig},
	{plan: "upgrade-and-update", bug: 141, finding: "Property: property volumes-go-first does not hold", step: 2, detail: "is false", config: upgradePropertiesConfig},
	{plan: "upgrade-and-update", config: upgradePropertiesConfig},
}

func TestAConfigCanDeclareAnUpgrade(t *testing.T) {
	c, err := config.Load(upgradeConfig)
	if err != nil {
		t.Fatal(err)
	}
	crds, err := filepath.Abs("upgrade/crds")
	if err != nil {
		t.Fatal(err)
	}
	if want := []plan.Upgrade{{Args: []string{"--bug=0", "--version=2"}, CRDs: []string{crds}}}; !reflect.DeepEqual(c.Launch.Upgrades, want) {
		t.Errorf("upgrades = %#v, want %#v", c.Launch.Upgrades, want)
	}
}

func TestRestartActionsLoadWithUpgradeFields(t *testing.T) {
	p, err := plan.Load(planPath(upgradeConfig, "crds"))
	if err != nil {
		t.Fatal(err)
	}
	crds, err := filepath.Abs("upgrade/crds")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.Steps[1].Actions[0].Upgrade, (plan.Upgrade{Args: []string{"--version=2"}, CRDs: []string{crds}}); !reflect.DeepEqual(got, want) {
		t.Errorf("Upgrade = %#v, want %#v", got, want)
	}
}
