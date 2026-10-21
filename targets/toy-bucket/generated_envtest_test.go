//go:build envtest

package main_test

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// TestGeneratedBucketPlansWithCloudFaultsPassB0 runs the first 20 plans
// that draw a fault on cloud. It fails if fewer than a third of the plans
// with a cloud request fault had one pick a request, or if a request goes
// unmapped. A drawn kill or a short period leaves some faults no request.
func TestGeneratedBucketPlansWithCloudFaultsPassB0(t *testing.T) {
	const plans = 20
	cfg, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var requesting, fired atomic.Int32
	t.Cleanup(func() {
		n, of := fired.Load(), requesting.Load()
		t.Logf("A cloud request fault fired in %d of %d plans with one.", n, of)
		if of == 0 || 3*n < of {
			t.Errorf("A cloud request fault fired in %d of %d plans with one, want at least a third.", n, of)
		}
	})
	for seed, n := uint64(1), 0; n < plans; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		all, requests := cloudFaults(p)
		if all == 0 {
			continue
		}
		n++
		if requests > 0 {
			requesting.Add(1)
		}
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, "config.yaml", run.Options{Plan: p}, 0)
			if len(report.Findings) != 0 {
				t.Errorf("Found %d, want none. report.md:\n%s", len(report.Findings), text)
			}
			unpicked := 0
			for _, note := range report.Notes {
				if strings.Contains(note, "unmapped request") {
					t.Errorf("Note %q, want every request mapped.", note)
				}
				if strings.Contains(note, "system=cloud") && strings.HasSuffix(note, "picked no request.") {
					unpicked++
				}
			}
			if unpicked < requests {
				fired.Add(1)
			}
		})
	}
}

// cloudFaults counts p's faults on cloud, and those that are request
// faults.
func cloudFaults(p *plan.Plan) (all, requests int) {
	for _, s := range p.Steps {
		for _, f := range s.Faults {
			if f.System == "cloud" {
				all++
				if !f.ModelFault() {
					requests++
				}
			}
		}
	}
	return all, requests
}
