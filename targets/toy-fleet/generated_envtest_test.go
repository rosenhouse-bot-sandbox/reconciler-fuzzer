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

// TestGeneratedPlansWithFaultsPassB0 runs 20 plans with a fault from each
// config. It fails if fewer than 3 of a config's plans had a request or model
// fault that picked a request or held an object, so it catches faults that
// stop firing.
func TestGeneratedPlansWithFaultsPassB0(t *testing.T) {
	for _, cfg := range configs {
		t.Run(cfg, func(t *testing.T) {
			t.Parallel()
			generatedPlansPassB0(t, cfg)
		})
	}
}

func generatedPlansPassB0(t *testing.T, path string) {
	const plans, firing = 20, 3
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var fired atomic.Int32
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
		// On the database config, seed 22 hits #128's false positive: Clean
		// blames a Volume that the controller recreates for the fixture
		// Cluster on a terminating Database.
		if faults(p) == 0 || path == databaseConfig && seed == 22 {
			continue
		}
		n++
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Parallel()
			report, text := run1(t, path, run.Options{Plan: p}, 0)
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
