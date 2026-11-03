package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestRunRefusesAPlanThatCreatesAnUnlistedKind(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a",
		Object: json.RawMessage(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "settings"}}`)}}}}}
	out := filepath.Join(t.TempDir(), "out")
	_, err := Run(t.Context(), Options{ConfigPath: "../../targets/toy-widget/config.yaml", Plan: p, Out: out})
	if err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("Run = %v, want a refusal that names ConfigMap.", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Run made %s before it refused the plan: %v", out, err)
	}
}

// writeToyConfig writes a config of toy-widget's CRDs and sample whose
// primary is of kind, with launch and rest, and returns its path.
func writeToyConfig(t *testing.T, launch, kind, rest string) string {
	t.Helper()
	toy, err := filepath.Abs("../../targets/toy-widget")
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("launch: %s\ncrds: [%s/crds/]\nprimary: {kind: %s, sample: %s/widget.yaml, ready: \"true\"}\n%s", launch, toy, kind, toy, rest)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var createA = &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}

func TestRunNamesTheConfigKeyOfAKindNoCRDServes(t *testing.T) {
	for name, c := range map[string]struct{ kind, models string }{
		"primary.kind":   {"toy.reconciler-fuzzer/v1alpha1/Widget", ""},
		"models[0].kind": {"toy.reconciler-fuzzer/v1/Widget", "models: [{kind: toy.reconciler-fuzzer/v1/Gizmo, stages: readyCondition, after: 1s}]"},
	} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			_, err := Run(t.Context(), Options{ConfigPath: writeToyConfig(t, "{binary: /bin/true}", c.kind, c.models), Plan: createA, Out: out})
			for _, want := range []string{name, "no CRD under crds", "toy.reconciler-fuzzer/v1/Widget"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("Run = %v, want an error that names %s", err, want)
				}
			}
			if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Run made %s before it refused the config: %v", out, err)
			}
		})
	}
}

func TestRunRefusesAMissingLaunchBinaryBeforeEnvtest(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "controller")
	out := filepath.Join(t.TempDir(), "out")
	_, err := Run(t.Context(), Options{ConfigPath: writeToyConfig(t, "{binary: "+missing+"}", "toy.reconciler-fuzzer/v1/Widget", ""), Plan: createA, Out: out})
	if err == nil || !strings.HasPrefix(err.Error(), "launch.binary: ") || !strings.Contains(err.Error(), missing) {
		t.Errorf("Run = %v, want a refusal that names launch.binary and %s", err, missing)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Run made %s before it refused the config: %v", out, err)
	}
}

func TestRunRefusesAFaultNoModelTakesBeforeEnvtest(t *testing.T) {
	path := writeToyConfig(t, "{binary: /bin/true}", "toy.reconciler-fuzzer/v1/Widget", "models: [{kind: toy.reconciler-fuzzer/v1/Gadget, after: 1s}]")
	p := &plan.Plan{Steps: []plan.Step{createA.Steps[0], {Faults: []plan.Fault{{Match: plan.Match{Resource: "gadget"}, Stuck: true}}}}}
	out := filepath.Join(t.TempDir(), "out")
	_, err := Run(t.Context(), Options{ConfigPath: path, Plan: p, Out: out})
	if want := "steps[1].faults[0]: no model takes stuck on resource=gadget; did you mean resource=gadgets?"; err == nil || err.Error() != want {
		t.Errorf("Run = %v, want %s", err, want)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Run made %s before it refused the plan: %v", out, err)
	}
}

func TestCheckFaultsRefusesAFaultNothingTakes(t *testing.T) {
	cfg := &config.Config{
		Models:  []config.Model{{Kind: gadgetKind, Resource: "gadgets"}},
		Systems: []config.System{{Name: "store", API: "crud", Process: &config.Process{}}, {Name: "secrets", API: "vault", Process: &config.Process{}}},
	}
	for fault, want := range map[string]string{
		`{"stuck": true, "match": {"resource": "gadgets"}}`:                                                          "",
		`{"slowDelete": true, "system": "kube", "match": {"group": "toy.reconciler-fuzzer", "resource": "gadgets"}}`: "",
		`{"stuck": true, "system": "store"}`:                                                                         "",
		`{"drop": true, "system": "secrets"}`:                                                                        "",
		`{"holdWatch": true, "match": {"resource": "widgets"}}`:                                                      "",
		`{"stuck": true, "match": {"resource": "gadget"}}`:                                                           "steps[0].faults[1]: no model takes stuck on resource=gadget; did you mean resource=gadgets?",
		`{"stuck": true, "match": {"group": "other.io", "resource": "gadgets"}}`:                                     "steps[0].faults[1]: no model takes stuck on group=other.io resource=gadgets",
		`{"slowDelete": true, "system": "secrets"}`:                                                                  "steps[0].faults[1]: no model takes slowDelete on system=secrets",
		`{"drop": true, "system": "stor"}`:                                                                           `steps[0].faults[1]: unknown system "stor"; did you mean store?`,
		`{"drop": true, "system": "cloud"}`:                                                                          `steps[0].faults[1]: unknown system "cloud"`,
	} {
		var f plan.Fault
		if err := json.Unmarshal([]byte(fault), &f); err != nil {
			t.Fatal(err)
		}
		p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{{Drop: true}, f}}}}
		err := checkFaults(cfg, p)
		if got := fmt.Sprint(err); want == "" && err != nil || want != "" && got != want {
			t.Errorf("checkFaults(%s) = %v, want %q", fault, err, want)
		}
	}
}
