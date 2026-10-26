package main_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

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

func readCRD(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crd := map[string]any{}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

// specFields are the spec fields the CRD's one version describes.
func specFields(t *testing.T, crd map[string]any) map[string]any {
	t.Helper()
	versions, _ := crd["spec"].(map[string]any)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("The CRD describes %d versions, want 1.", len(versions))
	}
	schema, _ := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	fields, ok := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	if !ok {
		t.Fatalf("The CRD describes no spec fields: %v", schema["properties"])
	}
	return fields
}

// The upgrade installs the Cluster CRD plus spec.tier, so the rest of it
// must stay in step with crds/.
func TestTheUpgradeCRDOnlyAddsSpecTier(t *testing.T) {
	const name = "toy.reconciler-fuzzer_clusters.yaml"
	installed, upgraded := readCRD(t, filepath.Join("crds", name)), readCRD(t, filepath.Join("upgrade/crds", name))
	fields := specFields(t, upgraded)
	if _, ok := fields["tier"]; !ok {
		t.Fatalf("upgrade/crds/%s describes no spec.tier.", name)
	}
	delete(fields, "tier")
	if !reflect.DeepEqual(installed, upgraded) {
		t.Errorf("Without spec.tier, upgrade/crds/%s differs from crds/%s.", name, name)
	}
}
