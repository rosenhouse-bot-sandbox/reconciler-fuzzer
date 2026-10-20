package check_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// generatedChildren settles a step that creates slot a, whose controller
// creates a child from the prefix a- for each of names.
func generatedChildren(names ...string) *tt.Script {
	r := newRun(1)
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	for _, name := range names {
		ref := tt.Ref("configmaps", "ns", name)
		if name == "dropped" {
			lost := tt.Create(ctl, ref, "u-dropped", "d1")
			lost.Call.Injected = trace.DropResponse
			r.Add(lost, tt.Version(ref, "u-dropped", "d1", tt.OwnedBy(tt.Generated("a-", name), uid, true)))
			continue
		}
		r.Create(ctl, ref, tt.OwnedBy(tt.Generated("a-", name), uid, true))
	}
	return r.Ready(widgetA).EndFaults().Settle()
}

func TestUniqueFailsTwoInstancesOfOneRole(t *testing.T) {
	requireFinding(t, judgeWith(t, generatedChildren("dropped", "a-x2"), check.Unique), "Unique", 0, "duplicate resources",
		"kube/configmaps", "configmaps ns/dropped (rule 1, slot a), configmaps ns/a-x2 (rule 1, slot a)")
}

func TestUniquePassesOneInstancePerGroup(t *testing.T) {
	r := generatedChildren("a-x1")
	r.Begin()
	uid := r.CreateCR("b", widgetB)
	r.Create(ctl, tt.Ref("configmaps", "ns", "b-x1"), tt.OwnedBy(tt.Generated("a-", "b-x1"), uid, true))
	r.Create(ctl, tt.Ref("configmaps", "ns", "b-0"), tt.OwnedBy(tt.Object("b-0"), uid, true))
	r.Create(ctl, tt.Ref("configmaps", "other", "b-0"), tt.OwnedBy(tt.Object("b-0"), uid, true))
	r.Ready(widgetB).EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Unique))
}

func TestUniqueJudgesOnlySettledSteps(t *testing.T) {
	r := generatedChildren("a-x1")
	r.Begin().Restart().EndFaults().At(r.Now, tt.Phase(1, trace.Settled))
	r.Tick(500*ms).Create(ctl, tt.Ref("configmaps", "ns", "a-x2"), tt.OwnedBy(tt.Generated("a-", "a-x2"), r.UID(widgetA), true))
	r.Tick(500*ms).At(r.Now, tt.Phase(1, trace.Confirmed))
	requireNone(t, judgeWith(t, r, check.Unique))
}
