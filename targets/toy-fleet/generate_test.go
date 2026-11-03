package main_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// A Database's Cluster is a fixture. Deleting it while a Database lives,
// then restarting, is how a seed reaches B150, as
// database/plans/restart-cluster-delete does.
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
		live, deleted := map[string]bool{}, false
		for _, s := range p.Steps {
			for _, a := range s.Actions {
				switch {
				case a.Do == plan.Create:
					live[a.Slot] = true
				case a.Do == plan.Delete:
					delete(live, a.Slot)
				case a.Do == plan.ChangeFixture && a.Target.Resource == "clusters" && len(a.Patch) == 0:
					deleted = len(live) > 0
				case a.Do == plan.Restart && deleted:
					return
				}
			}
		}
	}
	t.Error("No seed from 1 to 200 deletes the fixture Cluster while a Database lives, then restarts.")
}
