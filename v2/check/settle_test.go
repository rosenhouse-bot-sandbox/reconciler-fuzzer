package check_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace/tracetest"
)

const (
	ms = time.Millisecond
	s  = time.Second
)

var (
	widgetA = tt.Ref("toy/widgets", "ns", "a")
	widgetB = tt.Ref("toy/widgets", "ns", "b")
	cm      = tt.Ref("configmaps", "ns", "c")
	lease   = tt.Ref("coordination.k8s.io/leases", "ns", "l")
	ctl     = trace.Controller
)

var spec = check.Spec{
	PrimaryGroup:    "toy",
	PrimaryResource: "widgets",
	Ready: func(obj map[string]any) (bool, error) {
		status, _ := obj["status"].(map[string]any)
		ready, isBool := status["ready"].(bool)
		if !isBool {
			return false, errors.New("status.ready is not a bool")
		}
		return ready, nil
	},
	Stable:  2 * s,
	Budgets: config.Budgets{Reads: 2, Writes: 4},
}

func ready(ready any) map[string]any { return map[string]any{"status": map[string]any{"ready": ready}} }

func deleting() map[string]any {
	body := ready(true)
	body["metadata"] = map[string]any{"deletionTimestamp": "2026-01-01T00:00:00Z"}
	return body
}

// settledAt3s is a trace that settles at 3s: the controller started at 0,
// read its widget at 0.5s, and the widget was ready at 1s.
func settledAt3s() *tt.Builder {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(500 * ms).Add(tt.Call(ctl, "get", widgetA, 200, "")).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(true)))
	return &b
}

func TestSettledAt3s(t *testing.T) {
	if unmet := check.Settled(settledAt3s().Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func requireUnmet(t *testing.T, b *tt.Builder, at time.Duration, want string) {
	t.Helper()
	unmet := check.Settled(b.Index(), at, spec)
	if !slices.ContainsFunc(unmet, func(u string) bool { return strings.Contains(u, want) }) {
		t.Fatalf("unmet = %q, want one that says %q", unmet, want)
	}
}

func TestClause1EveryLiveCRIsReady(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Version(widgetB, "ub", "2", ready(false)))
	requireUnmet(t, b, 3*s, "ns/b is not ready")
}

func TestClause1QuotesAReadyError(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Version(widgetB, "ub", "2", ready("yes")))
	requireUnmet(t, b, 3*s, "status.ready is not a bool")
}

func TestClause1IgnoresOtherKindsAndDeletedCRs(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Version(cm, "uc", "2", ready(false))).
		At(1 * s).Add(tt.Version(widgetB, "ub", "3", ready(false))).
		At(1 * s).Add(tt.Deleted(widgetB, "ub", "4", ready(false)))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestClause2NoCRIsBeingDeleted(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Version(widgetA, "ua", "2", deleting()))
	requireUnmet(t, b, 3*s, "ns/a is being deleted")
}

func TestClause2ADeletedCRIsNotAlsoUnready(t *testing.T) {
	body := deleting()
	body["status"] = map[string]any{"ready": false}
	b := settledAt3s()
	b.At(1 * s).Add(tt.Version(widgetA, "ua", "2", body))
	if unmet := check.Settled(b.Index(), 3*s, spec); !slices.Equal(unmet, []string{"widgets.toy ns/a is being deleted"}) {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestClause3ControllerUpForStable(t *testing.T) {
	b := settledAt3s()
	b.At(1500 * ms).Add(tt.Exit("signal: killed", true, "")).Add(tt.Start(2)).
		At(1600 * ms).Add(tt.Call(ctl, "get", widgetA, 200, ""))
	requireUnmet(t, b, 3*s, "has run for 1.5s")
	if unmet := check.Settled(b.Index(), 3500*ms, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestClause3ControllerUp(t *testing.T) {
	b := settledAt3s()
	b.At(2500 * ms).Add(tt.Exit("signal: killed", true, ""))
	requireUnmet(t, b, 3*s, "controller is down")
}

func TestClause4ControllerMadeASuccessfulRequest(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(500 * ms).Add(tt.Call(ctl, "get", widgetA, 500, "")).
		At(500 * ms).Add(tt.Call(ctl, "update", lease, 200, "9")).
		At(500 * ms).Add(tt.Call(trace.Harness, "get", widgetA, 200, "")).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(true)))
	requireUnmet(t, &b, 3*s, "no successful request")
}

func TestClause4CountsOnlySinceTheLastStart(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Exit("signal: killed", true, "")).Add(tt.Start(2))
	requireUnmet(t, b, 3500*ms, "no successful request")
}

func TestClause7NoChangedWrite(t *testing.T) {
	b := settledAt3s()
	b.At(1500 * ms).Add(tt.Call(ctl, "update", cm, 200, "5"))
	requireUnmet(t, b, 3*s, "changed configmaps ns/c")
	if unmet := check.Settled(b.Index(), 3500*ms, spec); len(unmet) != 0 {
		t.Fatalf("the change left the window, but unmet = %q", unmet)
	}
}

func TestClause7NamesACreatedObject(t *testing.T) {
	b := settledAt3s()
	create := tt.Call(ctl, "create", tt.Ref("configmaps", "ns", ""), 201, "5")
	create.Call.ResultName = "c"
	b.At(1500 * ms).Add(create)
	requireUnmet(t, b, 3*s, "changed configmaps ns/c with create")
}

func TestClause7NoOpPatchIsNoChange(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Call(ctl, "patch", widgetA, 200, "1"))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestClause7IgnoresLeaseWrites(t *testing.T) {
	b := settledAt3s()
	for i := range 10 {
		b.At(2 * s).Add(tt.Call(ctl, "update", lease, 200, string(rune('a'+i))))
	}
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestClause7ReadBudget(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Call(ctl, "get", widgetA, 200, "")).Add(tt.Call(ctl, "list", widgetA, 200, ""))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("two reads with one CR: unmet = %q", unmet)
	}
	b.Add(tt.Call(ctl, "get", cm, 404, ""))
	requireUnmet(t, b, 3*s, "3 gets and lists")
	b.Add(tt.Version(widgetB, "ub", "2", ready(true)))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("three reads with two CRs: unmet = %q", unmet)
	}
}

func TestClause7ReadBudgetWithNoCRs(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(2 * s).Add(tt.Call(ctl, "list", widgetA, 200, "")).Add(tt.Call(ctl, "list", widgetA, 200, ""))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
	b.Add(tt.Call(ctl, "list", widgetA, 200, ""))
	requireUnmet(t, &b, 3*s, "3 gets and lists")
}

func TestClause7WriteBudgetCountsNoOps(t *testing.T) {
	b := settledAt3s()
	for range 4 {
		b.At(2 * s).Add(tt.Call(ctl, "patch", widgetA, 200, "1"))
	}
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("four writes: unmet = %q", unmet)
	}
	b.Add(tt.Call(ctl, "patch", widgetA, 409, ""))
	requireUnmet(t, b, 3*s, "5 writes")
}

func TestClause7IgnoresWatchesAndHarnessCalls(t *testing.T) {
	b := settledAt3s()
	for range 5 {
		b.At(2 * s).Add(tt.Call(ctl, "watch", widgetA, 200, "")).
			Add(tt.Call(trace.Harness, "get", widgetA, 200, "")).
			Add(tt.Call(trace.Harness, "update", widgetA, 200, "7"))
	}
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q", unmet)
	}
}

func TestFirstSettledIsAnEventPlusStable(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(500 * ms).Add(tt.Call(ctl, "get", widgetA, 200, "")).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(false))).
		At(4100 * ms).Add(tt.Call(ctl, "update", widgetA, 200, "2")).
		At(4200 * ms).Add(tt.Version(widgetA, "ua", "2", ready(true)))
	ix := b.Index()
	for _, c := range []struct {
		from, to, want time.Duration
		ok             bool
	}{
		{0, 10 * s, 6100 * ms, true},
		{7 * s, 10 * s, 7 * s, true},
		{5 * s, 10 * s, 6100 * ms, true},
		{0, 6099 * ms, 0, false},
	} {
		got, ok := check.FirstSettled(ix, c.from, c.to, spec)
		if ok != c.ok || got != c.want {
			t.Errorf("FirstSettled(%v, %v) = %v, %v; want %v, %v", c.from, c.to, got, ok, c.want, c.ok)
		}
	}
}

func TestFirstSettledAtAnEventTime(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(500 * ms).Add(tt.Call(ctl, "get", widgetA, 200, "")).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(false))).
		At(3700 * ms).Add(tt.Version(widgetA, "ua", "2", ready(true)))
	if got, ok := check.FirstSettled(b.Index(), 0, 10*s, spec); !ok || got != 3700*ms {
		t.Fatalf("FirstSettled = %v, %v; want 3.7s", got, ok)
	}
}
