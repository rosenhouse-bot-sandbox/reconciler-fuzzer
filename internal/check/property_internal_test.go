package check

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// In a run whose every step writes, a property is judged at most once
// between two version events, though each finding says when it turned false.
func TestAPropertyIsJudgedOnceBetweenVersionEvents(t *testing.T) {
	// Each step writes the child's data.v twice. The property cannot be
	// evaluated while the child has no data, written as "".
	writes := [][2]string{{"x", "x"}, {"x", "y"}, {"y", "y"}, {"x", "x"}, {"y", "y"}, {"x", ""}, {"y", "y"}}
	r := tt.NewScript(trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgets"}, len(writes), time.Second)
	const noY = "world.kube.configmaps.all(m, m.data.v != 'y')"
	r.Spec.Properties = []trace.Property{{Name: "step", Expr: noY, When: "step"}, {Name: "always", Expr: noY, When: "always"}}
	cr, child := tt.Ref("toy/widgets", "ns", "a"), tt.Ref("configmaps", "ns", "a-0")
	body := func(v string) map[string]any {
		b := tt.Object("a-0")
		if v != "" {
			b["data"] = map[string]any{"v": v}
		}
		return b
	}
	r.Begin()
	r.CreateCR("a", cr)
	r.Create(trace.Controller, child, body("x"))
	r.Ready(cr)
	at := make([][2]time.Duration, len(writes))
	for step, values := range writes {
		if step > 0 {
			r.Begin()
		}
		at[step][0] = r.Now
		r.Update(trace.Controller, child, body(values[0]))
		at[step][1] = r.Now
		// Two writes at one time share a version event time.
		r.Update(trace.Controller, child, body(values[1]))
		r.Tick(-tt.Tick).Update(trace.Controller, child, body(values[1]))
		r.EndFaults().Settle()
	}
	turn := func(at time.Duration) string {
		return fmt.Sprintf("does not hold: it turned false at %v: controller updated configmaps ns/a-0", at)
	}
	want := []string{"1 " + turn(at[1][1]), "2 " + turn(at[1][1]), "4 " + turn(at[4][0]), "5 could not be evaluated", "6 " + turn(at[6][0])}
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
		// judged counts the judgments after each number of version event times.
		judged := map[int]int{}
		var got []string
		for _, f := range p.judgeEach(property, func(crs []any, world map[string]map[string][]any) (bool, error) {
			judged[sort.Search(len(p.versionTimes), func(i int) bool { return p.versionTimes[i] > p.t })]++
			return eval(crs, world)
		}) {
			line := fmt.Sprintf("%d %s", f.Step, strings.TrimPrefix(f.Summary, "property "+property.Name+" "))
			if len(f.Details) > 1 {
				line += ": " + f.Details[1]
			}
			got = append(got, line)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: findings = %q, want %q", property.Name, got, want)
		}
		for times, n := range judged {
			if n > 1 {
				t.Errorf("%s: judged %d times after %d version event times", property.Name, n, times)
			}
		}
	}
}
