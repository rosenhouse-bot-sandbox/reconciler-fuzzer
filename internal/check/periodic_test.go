package check_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const refreshEvery = 400 * ms

// refreshed is widget a's ready body, with status.refreshTime at and the
// fields refreshEvery write changes set from n. status holds more fields.
func refreshed(n int, at string, status map[string]any) map[string]any {
	body := tt.CR("a", true)
	body["metadata"] = map[string]any{"name": "a", "resourceVersion": n, "generation": n, "managedFields": []any{n}}
	s := body["status"].(map[string]any)
	s["refreshTime"] = at
	maps.Copy(s, status)
	return body
}

// refreshing settles step 0 with widget a ready, and then has the
// controller write status.refreshTime once more than stable later.
func refreshing() *tt.Script {
	r := newRun(0)
	r.Spec.Primaries[0].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"status.refreshTime"}}
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Update(ctl, widgetA, refreshed(1, "t1", nil))
	r.EndFaults().Tick(2 * s)
	return r.Update(ctl, widgetA, refreshed(2, "t2", nil))
}

func changedWrites(t *testing.T, r *tt.Script) []string {
	t.Helper()
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	var unmet []string
	for _, text := range check.Settled(ix, r.Now, spec) {
		if strings.Contains(text, "changed write") || strings.Contains(text, "budget") {
			unmet = append(unmet, text)
		}
	}
	return unmet
}

func TestPeriodicExemptsRefreshOnlyWrites(t *testing.T) {
	r := refreshing()
	if unmet := changedWrites(t, r); len(unmet) != 0 {
		t.Errorf("first refresh: unmet = %q", unmet)
	}
	r.Tick(refreshEvery/2-tt.Tick).Update(ctl, widgetA, refreshed(3, "t3", nil))
	if unmet := changedWrites(t, r); len(unmet) != 0 {
		t.Errorf("refresh refreshEvery/2 later: unmet = %q", unmet)
	}
}

func TestPeriodicCountsFasterRefreshes(t *testing.T) {
	r := refreshing()
	r.Tick(refreshEvery/2-2*tt.Tick).Update(ctl, widgetA, refreshed(3, "t3", nil))
	if unmet := changedWrites(t, r); len(unmet) != 1 || !strings.Contains(unmet[0], "1 changed writes") {
		t.Errorf("unmet = %q, want one changed write", unmet)
	}
}

func TestPeriodicCountsWritesOfOtherFields(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"spec":          func() map[string]any { b := refreshed(2, "t2", nil); b["spec"] = map[string]any{"n": 1}; return b }(),
		"status":        refreshed(2, "t2", map[string]any{"message": "synced"}),
		"prefix":        refreshed(2, "t1", map[string]any{"refreshTimes": 2}),
		"no refresh":    refreshed(2, "t1", map[string]any{"message": "synced"}),
		"metadata only": refreshed(2, "t1", nil),
	} {
		r := newRun(0)
		r.Spec.Primaries[0].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"status.refreshTime"}}
		r.Begin()
		r.CreateCR("a", widgetA)
		r.Update(ctl, widgetA, refreshed(1, "t1", nil))
		r.EndFaults().Tick(2 * s)
		r.Update(ctl, widgetA, body)
		if unmet := changedWrites(t, r); len(unmet) != 1 {
			t.Errorf("%s: unmet = %q, want one changed write", name, unmet)
		}
	}
}

func TestPeriodicWritesStillCountTowardTheBudget(t *testing.T) {
	r := refreshing()
	r.Spec.Writes = 1
	r.Tick(refreshEvery).Update(ctl, widgetA, refreshed(3, "t3", nil))
	unmet := changedWrites(t, r)
	if len(unmet) != 1 || !strings.Contains(unmet[0], "2 writes") {
		t.Errorf("unmet = %q, want the write budget", unmet)
	}
}

func TestPeriodicCoversOnlyThePrimary(t *testing.T) {
	for _, other := range []trace.Ref{cm, tt.Ref("other/widgets", "ns", "a")} {
		r := refreshing()
		r.Create(ctl, other, map[string]any{"status": map[string]any{"refreshTime": "t1"}})
		r.Tick(2*s).Update(ctl, other, map[string]any{"status": map[string]any{"refreshTime": "t2"}})
		if unmet := changedWrites(t, r); len(unmet) != 1 {
			t.Errorf("%s: unmet = %q, want its write", other.Resource, unmet)
		}
	}
}

func TestPeriodicExemptsARefreshInFlight(t *testing.T) {
	r := refreshing()
	r.Tick(refreshEvery).UpdateInFlight(ctl, widgetA, refreshed(3, "t3", nil), 200*ms)
	if unmet := changedWrites(t, r); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

func TestPeriodicNamesAFieldOfEveryListItem(t *testing.T) {
	conditions := func(stamp, status string) map[string]any {
		return map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": stamp},
			map[string]any{"type": "Synced", "status": status, "lastTransitionTime": stamp},
		}}
	}
	for name, c := range map[string]struct {
		status  map[string]any
		changed int
	}{
		"only the stamps":      {conditions("t2", "True"), 0},
		"a condition's status": {conditions("t2", "False"), 1},
		"one condition fewer":  {map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "t2"}}}, 1},
	} {
		r := newRun(0)
		r.Spec.Primaries[0].Periodic = &trace.Periodic{Every: refreshEvery, Paths: []string{"status.conditions[*].lastTransitionTime"}}
		r.Begin()
		r.CreateCR("a", widgetA)
		r.Update(ctl, widgetA, refreshed(1, "t", conditions("t1", "True")))
		r.EndFaults().Tick(2 * s)
		r.Update(ctl, widgetA, refreshed(2, "t", c.status))
		if unmet := changedWrites(t, r); len(unmet) != c.changed {
			t.Errorf("%s: unmet = %q, want %d changed writes", name, unmet, c.changed)
		}
	}
}
