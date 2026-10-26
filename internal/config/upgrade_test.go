package config_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// withUpgrades is the minimal config, with the cloud fake and upgrades.
func withUpgrades(t *testing.T, upgrades string) string {
	return strings.Replace(minimal(t), "launch:\n", "launch:\n  upgrades: "+upgrades+"\n", 1) + "systems: [" + fake + "]\n"
}

func TestLaunchUpgradesResolveAgainstTheConfig(t *testing.T) {
	path := writeConfig(t, withUpgrades(t, "[{binary: bin/v2, args: [--version=2], crds: [crd.yaml]}, {args: []}]"))
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	want := []plan.Upgrade{{Binary: filepath.Join(dir, "bin/v2"), Args: []string{"--version=2"}, CRDs: []string{filepath.Join(dir, "crd.yaml")}}, {Args: []string{}}}
	if !reflect.DeepEqual(c.Launch.Upgrades, want) {
		t.Errorf("upgrades = %#v, want %#v", c.Launch.Upgrades, want)
	}
}

func TestLaunchUpgradesRefuseAMissingCRDPath(t *testing.T) {
	_, err := config.Load(writeConfig(t, withUpgrades(t, "[{crds: [crd.yaml]}, {crds: [missing/]}]")))
	if want := "launch.upgrades[1].crds: stat "; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}

func TestLaunchUpgradeArgsNameDeclaredSystems(t *testing.T) {
	if _, err := config.Load(writeConfig(t, withUpgrades(t, "[{args: ['${systems.cloud.url}']}]"))); err != nil {
		t.Error(err)
	}
	_, err := config.Load(writeConfig(t, withUpgrades(t, "[{args: [--v2]}, {args: [--v3, '${systems.cloud.port}']}]")))
	if want := "launch.upgrades[1].args[1]: system cloud has no port"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}

func TestAnUnknownUpgradeKeyListsTheUpgradeKeys(t *testing.T) {
	_, err := config.Load(writeConfig(t, withUpgrades(t, "[{zzzzzz: x}]")))
	if want := "launch.upgrades[0].zzzzzz is not a key; launch.upgrades[0] takes binary, args and crds"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want one containing %q", err, want)
	}
}

func TestCheckArgsNamesTheArgAndItsKey(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"systems: ["+fake+"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckArgs([]string{"--a", "${systems.kube.namespace}", "--url=${systems.cloud.url}"}); err != nil {
		t.Error(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--a", "${systems.clod.url}"}, "args[1]: no system clod"},
		{[]string{"${other}"}, "args[0]: unknown key other"},
	} {
		if err := c.CheckArgs(tc.args); err == nil || err.Error() != tc.want {
			t.Errorf("CheckArgs(%q) = %v, want %q", tc.args, err, tc.want)
		}
	}
}
