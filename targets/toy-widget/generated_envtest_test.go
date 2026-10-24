//go:build envtest

package main_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/hunt"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

func generator(t *testing.T, path string) (*generate.Generator, map[string]any) {
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, readSample(t, cfg.Primary.Sample)
}

// TestGeneratedPlansWithFaultsPassB0 runs plans with a fault, or with a
// model fault for the gadget config. It also requires 3 plans of each config
// to have a request or model fault that picked a request or held an object,
// so the gate fails if faults stop firing.
func TestGeneratedPlansWithFaultsPassB0(t *testing.T) {
	for _, cfg := range configs {
		t.Run(cfg, func(t *testing.T) {
			t.Parallel()
			generatedPlansPassB0(t, cfg)
		})
	}
}

func generatedPlansPassB0(t *testing.T, cfg string) {
	const plans, firing = 20, 3
	g, _ := generator(t, cfg)
	var fired atomic.Int32
	drawn := faults
	if cfg == gadgetConfig {
		drawn = modelFaults
	}
	t.Cleanup(func() {
		if n := fired.Load(); n < firing {
			t.Errorf("A request or model fault fired in %d of %d plans, want at least %d.", n, plans, firing)
		}
	})
	for seed, n := uint64(1), 0; n < plans; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		if drawn(p) == 0 {
			continue
		}
		n++
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, cfg, run.Options{Plan: p}, 0)
			if len(report.Findings) != 0 {
				t.Errorf("Found %d, want none. report.md:\n%s", len(report.Findings), text)
			}
			unpicked := 0
			for _, note := range report.Notes {
				if strings.HasSuffix(note, "picked no request.") || strings.HasSuffix(note, "held no object.") {
					unpicked++
				}
			}
			if unpicked < requestAndModelFaults(p) {
				fired.Add(1)
			}
		})
	}
}

func modelFaults(p *plan.Plan) int {
	n := 0
	for _, s := range p.Steps {
		for _, f := range s.Faults {
			if f.ModelFault() {
				n++
			}
		}
	}
	return n
}

func faults(p *plan.Plan) int {
	n := 0
	for _, s := range p.Steps {
		n += len(s.Faults)
	}
	return n
}

// requestAndModelFaults counts p's faults other than holdWatch. With no fake
// services, each such fault gets a note if it picked no request or held no
// object. A holdWatch gets a note only for a kind the controller did not
// watch, so it cannot show that it fired.
func requestAndModelFaults(p *plan.Plan) int {
	n := 0
	for _, s := range p.Steps {
		for _, f := range s.Faults {
			if !f.HoldWatch {
				n++
			}
		}
	}
	return n
}

// TestAGeneratedFailingPlanShrinks shrinks seed 5's four-step plan under B3.
func TestAGeneratedFailingPlanShrinks(t *testing.T) {
	t.Parallel()
	const seed, bug = 5, 3
	g, sample := generator(t, "config.yaml")
	p, err := g.Plan(seed)
	if err != nil {
		t.Fatal(err)
	}
	report, text := run1(t, "config.yaml", run.Options{Plan: p}, bug)
	if len(report.Findings) == 0 {
		t.Fatalf("Seed %d finds nothing under B%d. report.md:\n%s", seed, bug, text)
	}
	want := report.Findings[0]
	gives := func(r check.Report) bool {
		for _, f := range r.Findings {
			if f.Signature() == want.Signature() {
				return true
			}
		}
		return false
	}
	shrunk, err := hunt.Shrink(t.Context(), p, sample, func(_ context.Context, c *plan.Plan) (bool, error) {
		for range 2 {
			if r, _ := run1(t, "config.yaml", run.Options{Plan: c}, bug); gives(r) {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(shrunk.Steps) > 3 || len(shrunk.Steps) >= len(p.Steps) {
		t.Errorf("Shrank %d steps to %d, want at most 3.", len(p.Steps), len(shrunk.Steps))
	}
	t.Logf("Shrank %d steps to %d.", len(p.Steps), len(shrunk.Steps))
	if r, text := run1(t, "config.yaml", run.Options{Plan: shrunk}, bug); !gives(r) {
		t.Errorf("The shrunk plan does not fail with %q. report.md:\n%s", want.Signature(), text)
	}
}

func readSample(t *testing.T, path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sample map[string]any
	if err := yaml.Unmarshal(data, &sample); err != nil {
		t.Fatal(err)
	}
	return sample
}
