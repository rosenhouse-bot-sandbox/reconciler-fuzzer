package run

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// upgradeFixture logs the process's calls with the cluster's, in one order.
func upgradeFixture(t *testing.T, launchArgs ...string) *fixture {
	f := newFixture(t, nil)
	f.runner.Process = fakeProcess{f.cluster.calls}
	f.runner.Vars = map[string]string{"systems.kube.namespace": "ns1"}
	f.runner.LaunchArgs = launchArgs
	return f
}

// upgradeCalls runs one restart with u and lists the calls but supervise.
func upgradeCalls(t *testing.T, f *fixture, u plan.Upgrade) ([]string, error) {
	t.Helper()
	err := f.runner.Execute(t.Context(), &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Restart, Upgrade: u}}}}})
	return slices.DeleteFunc(f.cluster.all(), func(c string) bool { return strings.HasPrefix(c, "supervise") }), err
}

// The appended restart keeps the command the upgrade set.
func TestARestartInstallsItsCRDsAndThenStartsItsCommand(t *testing.T) {
	f := upgradeFixture(t, "--bug=${systems.kube.namespace}")
	got, err := upgradeCalls(t, f, plan.Upgrade{Binary: "/v2", Args: []string{"--version=2", "--ns=${systems.kube.namespace}"}, CRDs: []string{"/crds/a", "/crds/b"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`install ["/crds/a" "/crds/b"]`, "stop", `setCommand "/v2" []string{"--version=2", "--ns=ns1", "--bug=ns1"}`, "start", "stop", "start"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %q, want %q", got, want)
	}
}

func TestARestartKeepsWhatItsUpgradeLeavesUnset(t *testing.T) {
	for _, c := range []struct {
		name       string
		upgrade    plan.Upgrade
		launchArgs []string
		want       string
	}{
		{"a binary keeps the args", plan.Upgrade{Binary: "/v2"}, []string{"--bug=1"}, `setCommand "/v2" []string(nil)`},
		{"empty args keep only --launch-args", plan.Upgrade{Args: []string{}}, []string{"--bug=1"}, `setCommand "" []string{"--bug=1"}`},
		{"empty args and no --launch-args", plan.Upgrade{Args: []string{}}, nil, `setCommand "" []string{}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := upgradeCalls(t, upgradeFixture(t, c.launchArgs...), c.upgrade)
			if want := []string{"stop", c.want, "start", "stop", "start"}; err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("calls = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestARestartWhoseInstallFailsLeavesTheControllerRunning(t *testing.T) {
	f := upgradeFixture(t)
	f.cluster.installErr = errors.New("no such CRD")
	got, err := upgradeCalls(t, f, plan.Upgrade{CRDs: []string{"/crds"}})
	if err == nil || !strings.Contains(err.Error(), "no such CRD") {
		t.Errorf("Execute = %v, want the install's error", err)
	}
	if slices.Contains(got, "stop") {
		t.Errorf("calls = %q, want no stop", got)
	}
}

func TestARestartWithAnUnknownKeyChangesNothing(t *testing.T) {
	got, err := upgradeCalls(t, upgradeFixture(t), plan.Upgrade{Args: []string{"--url=${systems.cloud.url}"}, CRDs: []string{"/crds"}})
	if err == nil || !strings.Contains(err.Error(), "systems.cloud.url") {
		t.Errorf("Execute = %v, want an error that names the key", err)
	}
	if len(got) > 0 {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestRunRefusesARestartWithABadPath(t *testing.T) {
	notExecutable := filepath.Join(t.TempDir(), "toy-widget")
	if err := os.WriteFile(notExecutable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		upgrade plan.Upgrade
		want    string
	}{
		"missing binary":        {plan.Upgrade{Binary: "/missing/toy-widget"}, "/missing/toy-widget"},
		"directory binary":      {plan.Upgrade{Binary: "../../targets/toy-widget/"}, "is a directory"},
		"non-executable binary": {plan.Upgrade{Binary: notExecutable}, "permission denied"},
		"missing crds":          {plan.Upgrade{CRDs: []string{"../../targets/toy-widget/crds/", "/missing/crds/"}}, "/missing/crds/"},
		"crds without a CRD":    {plan.Upgrade{CRDs: []string{"../../targets/toy-widget/plans/"}}, "plans/ holds no CustomResourceDefinition"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}, {Actions: []plan.Action{{Do: plan.Restart, Upgrade: c.upgrade}}}}}
			out := filepath.Join(t.TempDir(), "out")
			_, err := Run(t.Context(), Options{ConfigPath: "../../targets/toy-widget/config.yaml", Plan: p, Out: out})
			if err == nil || !strings.Contains(err.Error(), "steps[1].actions[0]: ") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Run = %v, want a refusal that names the action and contains %q.", err, c.want)
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Run made %s before it refused the plan: %v", out, err)
			}
		})
	}
}

func TestRunRefusesARestartArgWithAnUnknownKey(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Restart, Upgrade: plan.Upgrade{Args: []string{"--v2", "--url=${systems.cloud.url}"}}}}}}}
	out := filepath.Join(t.TempDir(), "out")
	_, err := Run(t.Context(), Options{ConfigPath: "../../targets/toy-widget/config.yaml", Plan: p, Out: out})
	if want := "steps[0].actions[1].args[1]: no system cloud"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Run = %v, want a refusal containing %q.", err, want)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Run made %s before it refused the plan: %v", out, err)
	}
}
