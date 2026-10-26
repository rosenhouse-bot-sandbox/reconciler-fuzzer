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

// at walks m down the keys through nested maps. A key that holds anything
// else gives nil.
func at(m map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		m, _ = m[key].(map[string]any)
	}
	return m
}

// specFields are the spec fields the CRD's one version describes.
func specFields(t *testing.T, crd map[string]any) map[string]any {
	t.Helper()
	versions, _ := at(crd, "spec")["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("The CRD describes %d versions, want 1.", len(versions))
	}
	version, _ := versions[0].(map[string]any)
	fields := at(version, "schema", "openAPIV3Schema", "properties", "spec", "properties")
	if len(fields) == 0 {
		t.Fatal("The CRD describes no spec fields.")
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
