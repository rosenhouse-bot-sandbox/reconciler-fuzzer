//go:build envtest

package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestRunNamesTheConfigKeyOfAKindNoCRDServes(t *testing.T) {
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ primary, models string }{
		"primary.kind": {primary: "toy.reconciler-fuzzer/v1alpha1/Widget"},
		"models[0].kind": {
			primary: "toy.reconciler-fuzzer/v1/Widget",
			models:  "models: [{kind: toy.reconciler-fuzzer/v1/Gizmo, stages: readyCondition, after: 1s}]\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			config := fmt.Sprintf("launch: {binary: /bin/true}\ncrds: [%[1]s/crds/]\nprimary: {kind: %[2]s, sample: %[1]s/widget.yaml, ready: \"true\"}\n%[3]s",
				toyDir, c.primary, c.models)
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
			_, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: filepath.Join(dir, "out")})
			// The error lists the kinds the CRDs serve, Widget among them.
			want := []string{name, "no CRD under crds", "toy.reconciler-fuzzer/v1/Widget"}
			for _, w := range want {
				if err == nil || !strings.Contains(err.Error(), w) {
					t.Fatalf("err = %v, want it to name each of %q", err, want)
				}
			}
		})
	}
}

// A launch.env without KUBECONFIG must not leave the controller on the
// shell's cluster.
func TestTheControllerGetsTheHarnessKubeconfigByDefault(t *testing.T) {
	t.Setenv("KUBECONFIG", "/home/me/.kube/production")
	toyDir, err := filepath.Abs(toy)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	got := filepath.Join(dir, "got")
	config := fmt.Sprintf("launch: {binary: /bin/sh, args: [-c, 'echo \"$KUBECONFIG\" > %[2]s; sleep 60']}\ncrds: [%[1]s/crds/]\nprimary: {kind: toy.reconciler-fuzzer/v1/Widget, sample: %[1]s/widget.yaml, ready: \"true\"}\ntimeouts: {settle: 3s, stable: 1s}\n",
		toyDir, got)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}}}}}
	if _, err := Run(context.Background(), Options{ConfigPath: filepath.Join(dir, "config.yaml"), Plan: p, Out: out}); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(out, "kubeconfig") + "\n"; string(text) != want {
		t.Errorf("The controller got KUBECONFIG %q, want %q.", text, want)
	}
}
