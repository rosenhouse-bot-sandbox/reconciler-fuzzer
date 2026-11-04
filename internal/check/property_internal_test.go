package check

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// A property is judged at most once between two version events, though each
// finding of a property false at each step's end says when it turned false.
func TestAPropertyIsJudgedOnceBetweenVersionEvents(t *testing.T) {
	const steps = 6
	r := tt.NewScript(trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgets"}, steps, time.Second)
	r.Spec.Properties = []trace.Property{
		{Name: "no-data", Expr: "world.kube.configmaps.all(m, !has(m.data))", When: "step"},
		{Name: "one-cr", Expr: "size(crs) == 1", When: "always"},
	}
	cr, child := tt.Ref("toy/widgets", "ns", "a"), tt.Ref("configmaps", "ns", "a-0")
	r.Begin()
	r.CreateCR("a", cr)
	r.Create(trace.Controller, child, tt.Object("a-0"))
	r.Ready(cr)
	for step := range steps {
		if step > 0 {
			r.Begin()
		}
		data := func(value string) map[string]any {
			return map[string]any{"metadata": map[string]any{"name": "a-0"}, "data": map[string]any{fmt.Sprint(step): value}}
		}
		r.Update(trace.Controller, child, data("x"))
		r.Tick(-tt.Tick).Update(trace.Controller, child, data("y"))
		r.EndFaults().Settle()
	}
	ix := r.Index()
	s, err := SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	p := newProperties(ix, s)
	for _, property := range s.Properties {
		eval, err := property.Program.Bind(p.firstWrittenUnder)
		if err != nil {
			t.Fatal(err)
		}
		// judged counts the judgments after each number of version events.
		judged := map[int]int{}
		findings := p.judgeEach(property, func(crs []any, world map[string]map[string][]any) (bool, error) {
			judged[sort.Search(len(p.versionTimes), func(i int) bool { return p.versionTimes[i] > p.t })]++
			return eval(crs, world)
		})
		if property.Always && len(findings) != 0 || !property.Always && (len(findings) != steps || len(findings[steps-1].Details) != 2) {
			t.Errorf("%s: findings = %+v", property.Name, findings)
		}
		for events, n := range judged {
			if n > 1 {
				t.Errorf("%s: judged %d times after %d version events", property.Name, n, events)
			}
		}
	}
}
