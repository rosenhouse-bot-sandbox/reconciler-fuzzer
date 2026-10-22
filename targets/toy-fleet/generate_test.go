package main_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// A Database's Cluster is a fixture. Deleting it while the Database lives
// is how a seed reaches B50, as plans/restart-cluster-delete does.
func TestSomeSeedDeletesTheFixtureCluster(t *testing.T) {
	c, err := config.Load(databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generate.New(c)
	if err != nil {
		t.Fatal(err)
	}
	for seed := uint64(1); seed <= 200; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Steps {
			for _, a := range s.Actions {
				if a.Do == plan.ChangeFixture && a.Target.Resource == "clusters" && len(a.Patch) == 0 {
					return
				}
			}
		}
	}
	t.Error("No seed from 1 to 200 deletes the fixture Cluster.")
}
