//go:build envtest

package toywidget_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/hunt"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/run"
)

func generator(t *testing.T) (*generate.Generator, map[string]any) {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, readSample(t, cfg.Primary.Sample)
}

// TestGeneratedPlansWithFaultsPassB0 also requires some plans to have a
// fault that picked a request, so the gate fails if faults stop firing.
func TestGeneratedPlansWithFaultsPassB0(t *testing.T) {
	const plans, firing = 20, 3
	g, _ := generator(t)
	var fired atomic.Int32
	t.Cleanup(func() {
		if n := fired.Load(); n < firing {
			t.Errorf("A fault picked a request in %d of %d plans, want at least %d.", n, plans, firing)
		}
	})
	for seed, n := uint64(1), 0; n < plans; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		if faults(p) == 0 {
			continue
		}
		n++
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, run.Options{Plan: p}, 0)
			if len(report.Findings) != 0 {
				t.Errorf("Found %d, want none. report.md:\n%s", len(report.Findings), text)
			}
			unpicked := 0
			for _, note := range report.Notes {
				if strings.HasSuffix(note, "picked no request.") {
					unpicked++
				}
			}
			if unpicked < faults(p) {
				fired.Add(1)
			}
		})
	}
}

func faults(p *plan.Plan) int {
	n := 0
	for _, s := range p.Steps {
		n += len(s.Faults)
	}
	return n
}

// TestAGeneratedFailingPlanShrinks shrinks seed 4's four-step plan under B3.
func TestAGeneratedFailingPlanShrinks(t *testing.T) {
	t.Parallel()
	const seed, bug = 4, 3
	g, sample := generator(t)
	p, err := g.Plan(seed)
	if err != nil {
		t.Fatal(err)
	}
	report, text := run1(t, run.Options{Plan: p}, bug)
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
			if r, _ := run1(t, run.Options{Plan: c}, bug); gives(r) {
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
	if r, text := run1(t, run.Options{Plan: shrunk}, bug); !gives(r) {
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
