package examples_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func load(t *testing.T, example string) *config.Config {
	t.Helper()
	c, err := config.Load(filepath.Join(example, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPlansLoad(t *testing.T) {
	for _, example := range []string{"cert-manager", "external-secrets"} {
		load(t, example)
		paths, _ := filepath.Glob(filepath.Join(example, "*", "*.json"))
		if len(paths) < 4 {
			t.Errorf("%s has plans %v", example, paths)
		}
		for _, path := range paths {
			if _, err := plan.Load(path); err != nil {
				t.Error(err)
			}
		}
	}
}

type readiness map[string]struct {
	cr   map[string]any
	want bool
}

func requireReady(t *testing.T, c *config.Config, cases readiness) {
	t.Helper()
	for name, tc := range cases {
		if ready, err := c.Ready(tc.cr); ready != tc.want || err != nil {
			t.Errorf("%s: Ready = %v, %v; want %v", name, ready, err, tc.want)
		}
	}
}

func cr(status map[string]any) map[string]any {
	obj := map[string]any{"metadata": map[string]any{"generation": int64(2)}}
	if status != nil {
		obj["status"] = status
	}
	return obj
}

func conditions(cs ...map[string]any) []any {
	var list []any
	for _, c := range cs {
		list = append(list, c)
	}
	return list
}

func TestCertificateReady(t *testing.T) {
	ready := func(status string, generation int64) map[string]any {
		return map[string]any{"type": "Ready", "status": status, "observedGeneration": generation}
	}
	requireReady(t, load(t, "cert-manager"), readiness{
		"no status":              {cr(nil), false},
		"no conditions":          {cr(map[string]any{}), false},
		"not ready":              {cr(map[string]any{"conditions": conditions(ready("False", 2))}), false},
		"ready for an older gen": {cr(map[string]any{"conditions": conditions(ready("True", 1))}), false},
		"issuing but ready": {cr(map[string]any{"conditions": conditions(
			map[string]any{"type": "Issuing", "status": "True", "observedGeneration": int64(2)}, ready("True", 2))}), true},
		"ready without a generation": {cr(map[string]any{"conditions": conditions(map[string]any{"type": "Ready", "status": "True"})}), false},
	})
}

func TestExternalSecretReady(t *testing.T) {
	readyCondition := map[string]any{"type": "Ready", "status": "True"}
	requireReady(t, load(t, "external-secrets"), readiness{
		"no status":                     {cr(nil), false},
		"no synced version":             {cr(map[string]any{"conditions": conditions(readyCondition)}), false},
		"an older generation":           {cr(map[string]any{"syncedResourceVersion": "1-abc", "conditions": conditions(readyCondition)}), false},
		"a generation sharing a prefix": {cr(map[string]any{"syncedResourceVersion": "22-abc", "conditions": conditions(readyCondition)}), false},
		"not ready": {cr(map[string]any{"syncedResourceVersion": "2-abc",
			"conditions": conditions(map[string]any{"type": "Ready", "status": "False"})}), false},
		"synced and ready": {cr(map[string]any{"syncedResourceVersion": "2-abc", "conditions": conditions(readyCondition)}), true},
	})
}

// TestSomeSeedDropsTheResponseToASecretWrite holds each seeds file to a seed
// whose plan drops the response to the request that makes the Secret: a
// create, or cert-manager's apply patch.
func TestSomeSeedDropsTheResponseToASecretWrite(t *testing.T) {
	for example, verb := range map[string]string{"cert-manager": "patch", "external-secrets": "create"} {
		g, err := generate.New(load(t, example))
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(example, "seeds"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, field := range strings.Fields(string(data)) {
			seed, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			p, err := g.Plan(seed)
			if err != nil {
				t.Fatal(err)
			}
			found = found || slices.ContainsFunc(p.Steps, func(s plan.Step) bool {
				return slices.ContainsFunc(s.Faults, func(f plan.Fault) bool {
					return f.DropResponse && f.Match.Resource == "secrets" && f.Match.Verb == verb
				})
			})
		}
		if !found {
			t.Errorf("No seed in %s/seeds drops the response to a %s of secrets.", example, verb)
		}
	}
}
