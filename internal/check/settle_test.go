package check_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const (
	ms = time.Millisecond
	s  = time.Second
)

var (
	widgets = trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgets"}
	widgetA = tt.Ref("toy/widgets", "ns", "a")
	widgetB = tt.Ref("toy/widgets", "ns", "b")
	cm      = tt.Ref("configmaps", "ns", "c")
	lease   = tt.Ref("coordination.k8s.io/leases", "ns", "l")
	ctl     = trace.Controller
)

var spec = check.Spec{
	Primaries: []check.Primary{{Kind: widgets, Ready: func(obj map[string]any) (bool, error) {
		status, _ := obj["status"].(map[string]any)
		ready, isBool := status["ready"].(bool)
		if !isBool {
			return false, errors.New("status.ready is not a bool")
		}
		return ready, nil
	}}},
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

func TestClause4IgnoresAWriteTheKilledProcessSent(t *testing.T) {
	var b tt.Builder
	write := tt.Call(ctl, "update", widgetA, 200, "")
	write.Call.Latency = 200 * ms
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(true))).
		At(1 * s).Add(tt.Exit("signal: killed", true, "")).Add(tt.Start(2)).
		At(1100 * ms).Add(write)
	requireUnmet(t, &b, 4*s, "no successful request")
}

func TestClause7NoChangedWrite(t *testing.T) {
	b := settledAt3s()
	b.At(1500 * ms).Add(tt.Call(ctl, "update", cm, 200, "5"))
	requireUnmet(t, b, 3*s, "changed configmaps ns/c")
	if unmet := check.Settled(b.Index(), 3500*ms, spec); len(unmet) != 0 {
		t.Fatalf("the change left the window, but unmet = %q", unmet)
	}
}

func TestClause7CountsAChangedWriteInFlight(t *testing.T) {
	b := settledAt3s()
	b.At(3*s - 3*ms).Add(tt.Version(cm, "uc", "5", tt.Object("c")))
	write := tt.Call(ctl, "update", cm, 200, "5")
	write.Call.Latency = 6 * ms
	b.At(3*s + 1*ms).Add(write)
	requireUnmet(t, b, 3*s, "changed configmaps ns/c")
	write.Call.Latency = 500 * time.Microsecond
	b = settledAt3s()
	b.At(3*s + 1*ms).Add(write)
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("a write sent after t is in flight, but unmet = %q", unmet)
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

func TestClause4IgnoresCallsOfUnknownOutcome(t *testing.T) {
	var b tt.Builder
	get := tt.Call(ctl, "get", widgetA, 200, "")
	get.Call.Injected = trace.DropResponse
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(500 * ms).Add(get).
		At(1 * s).Add(tt.Version(widgetA, "ua", "1", ready(true)))
	requireUnmet(t, &b, 3*s, "no successful request")
}

// damagedAt1200ms settles at 3s but for clause 5: step 0 damaged cm at
// 1.2s and ended its faults at 1.3s.
func damagedAt1200ms(do string) *tt.Builder {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1200 * ms).Add(tt.Action(0, do, cm)).
		At(1300 * ms).Add(tt.EndFaults(0, 10*s, false))
	return b
}

func call(verb string, ref trace.Ref, status int, injected string) trace.Event {
	e := tt.Call(ctl, verb, ref, status, "")
	e.Call.Injected = injected
	return e
}

func TestClause5TheControllerLooksAtEachChangedObject(t *testing.T) {
	requireUnmet(t, damagedAt1200ms("damage"), 3200*ms, "has not looked at configmaps ns/c since the damage at 1.2s")
	requireUnmet(t, damagedAt1200ms("changeFixture"), 3200*ms, "has not looked at configmaps ns/c since the changeFixture at 1.2s")
	looks := map[string]trace.Event{
		"a get":                     call("get", cm, 200, ""),
		"a 404":                     call("get", cm, 404, ""),
		"a list of the namespace":   call("list", tt.Ref("configmaps", "ns", ""), 200, ""),
		"a list of every namespace": call("list", tt.Ref("configmaps", "", ""), 200, ""),
		"a watch opened after":      call("watch", tt.Ref("configmaps", "", ""), 200, ""),
	}
	for name, look := range looks {
		b := damagedAt1200ms("damage")
		b.At(1500 * ms).Add(look)
		if unmet := check.Settled(b.Index(), 3500*ms, spec); len(unmet) != 0 {
			t.Errorf("%s: unmet = %q", name, unmet)
		}
	}
}

func TestClause5HoldsStableAfterTheChange(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1100 * ms).Add(watchCall(1)).
		At(1200 * ms).Add(tt.Action(0, "damage", cm)).
		At(1210 * ms).Add(call("get", cm, 404, "")).
		At(1300 * ms).Add(tt.EndFaults(0, 10*s, false))
	requireUnmet(t, b, 3199*ms, "configmaps ns/c was changed at 1.2s, less than stable ago")
	if got, ok := check.FirstSettled(b.Index(), 1300*ms, 10*s, spec); !ok || got != 3200*ms {
		t.Errorf("FirstSettled = %v, %v; want 3.2s, since a watch saw the change", got, ok)
	}
}

// A polling controller repairs just after it looks.
func TestClause5HoldsStableAfterTheFirstLook(t *testing.T) {
	b := damagedAt1200ms("damage")
	b.At(2500 * ms).Add(call("list", tt.Ref("configmaps", "ns", ""), 200, "")).
		At(3 * s).Add(call("get", cm, 404, ""))
	requireUnmet(t, b, 4200*ms, "the controller looked at configmaps ns/c at 2.5s, less than stable ago")
	if got, ok := check.FirstSettled(b.Index(), 1300*ms, 10*s, spec); !ok || got != 4300*ms {
		t.Errorf("FirstSettled = %v, %v; want the lapse at 4.3s", got, ok)
	}
	b = damagedAt1200ms("damage")
	b.At(1500 * ms).Add(call("get", cm, 404, ""))
	if got, ok := check.FirstSettled(b.Index(), 1300*ms, 10*s, spec); !ok || got != 3500*ms {
		t.Errorf("FirstSettled = %v, %v; want stable after the look at 1.5s", got, ok)
	}
}

func TestClause5DoesNotCount(t *testing.T) {
	harnessGet := tt.Call(trace.Harness, "get", cm, 200, "")
	for name, c := range map[string]struct {
		at   time.Duration
		call trace.Event
	}{
		"a call before the change":    {1100 * ms, call("get", cm, 200, "")},
		"a failed call":               {1500 * ms, call("get", cm, 500, "")},
		"a call of unknown outcome":   {1500 * ms, call("update", cm, 200, trace.DropResponse)},
		"a killAfter call":            {1500 * ms, call("update", cm, 200, trace.KillAfter)},
		"a call in another namespace": {1500 * ms, call("list", tt.Ref("configmaps", "other", ""), 200, "")},
		"a call on another name":      {1500 * ms, call("get", tt.Ref("configmaps", "ns", "d"), 200, "")},
		"a call on another kind":      {1500 * ms, call("get", tt.Ref("secrets", "ns", "c"), 200, "")},
		"a harness call":              {1500 * ms, harnessGet},
	} {
		b := damagedAt1200ms("damage")
		b.At(c.at).Add(c.call)
		requireUnmetNamed(t, name, b, 3200*ms, "has not looked at configmaps ns/c")
	}
}

func requireUnmetNamed(t *testing.T, name string, b *tt.Builder, at time.Duration, want string) {
	t.Helper()
	unmet := check.Settled(b.Index(), at, spec)
	if !slices.ContainsFunc(unmet, func(u string) bool { return strings.Contains(u, want) }) {
		t.Errorf("%s: unmet = %q, want one that says %q", name, unmet, want)
	}
}

func watchCall(stream uint64) trace.Event {
	e := call("watch", tt.Ref("configmaps", "ns", ""), 200, "")
	e.Call.Watch, e.Call.Stream = true, stream
	return e
}

func TestClause5CountsAWatchOpenAcrossTheChange(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1100 * ms).Add(watchCall(1)).
		At(1200 * ms).Add(tt.Action(0, "damage", cm)).
		At(1300 * ms).Add(tt.EndFaults(0, 10*s, false))
	if unmet := check.Settled(b.Index(), 3200*ms, spec); len(unmet) != 0 {
		t.Errorf("open watch: unmet = %q", unmet)
	}
	b.At(2 * s).Add(trace.Event{WatchEnd: &trace.WatchEnd{Caller: ctl, System: "kube", Stream: 1}})
	requireUnmet(t, b, 3200*ms, "has not looked at configmaps ns/c")
}

func TestClause5HoldsStableAfterAHoldWatchReleasesTheWatch(t *testing.T) {
	hold := trace.Ref{System: "kube", Resource: "configmaps"}
	b := settledAt3s()
	b.At(1*s).Add(tt.Phase(0, trace.FaultPeriod), tt.HoldFault(0, 0, false, hold)).
		At(1100*ms).Add(watchCall(1)).
		At(1200*ms).Add(tt.Action(0, "damage", cm)).
		At(3500*ms).Add(tt.HoldFault(0, 0, true, hold), tt.EndFaults(0, 10*s, false))
	requireUnmet(t, b, 3400*ms, "has not looked at configmaps ns/c")
	requireUnmet(t, b, 5499*ms, "a holdWatch released the controller's watch of configmaps ns/c at 3.5s, less than stable ago")
	if got, ok := check.FirstSettled(b.Index(), 3500*ms, 10*s, spec); !ok || got != 5500*ms {
		t.Errorf("FirstSettled = %v, %v; want stable after the release", got, ok)
	}
}

func TestClause5LapsesAfterOneAndAHalfPollIntervals(t *testing.T) {
	b := damagedAt1200ms("damage")
	requireUnmet(t, b, 4299*ms, "has not looked at configmaps ns/c")
	if unmet := check.Settled(b.Index(), 4300*ms, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
	if got, ok := check.FirstSettled(b.Index(), 1300*ms, 10*s, spec); !ok || got != 4300*ms {
		t.Errorf("FirstSettled = %v, %v; want the lapse at 4.3s", got, ok)
	}
	if got, ok := check.FirstSettled(b.Index(), 1300*ms, 4299*ms, spec); ok {
		t.Errorf("FirstSettled before the lapse = %v", got)
	}
}

func TestClause5DoesNotLapseBeforeEndFaults(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1200 * ms).Add(tt.Action(0, "damage", cm))
	requireUnmet(t, b, 5*s, "has not looked at configmaps ns/c")
}

func TestClause5JudgesOnlyTheLatestStep(t *testing.T) {
	b := damagedAt1200ms("damage")
	b.At(2 * s).Add(tt.Phase(1, trace.FaultPeriod))
	requireUnmet(t, b, 1900*ms, "configmaps ns/c was changed at 1.2s")
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

func TestClause5SkipsAnUnmatchedTarget(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1200 * ms).Add(trace.Event{Action: &trace.Action{Step: 0, Do: "damage", Error: "no instance matches"}})
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

func TestClause5SkipsAFailedChange(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(1200 * ms).Add(trace.Event{Action: &trace.Action{Step: 0, Do: "damage", Ref: &cm, Error: "conflict"}})
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

func TestClause6NoModelHasWorkPending(t *testing.T) {
	b := settledAt3s()
	b.At(1 * s).Add(trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
	requireUnmet(t, b, 3*s, "model gc has work pending")
	b.At(2 * s).Add(trace.Event{Work: &trace.Work{System: "gc", Pending: false}})
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

// stopped adds, at t, the exit of the controller that a stop action of
// step 1 stopped, and that action.
func stopped(b *tt.Builder, t time.Duration) *tt.Builder {
	return b.At(t).Add(tt.Exit("signal: terminated", true, ""), trace.Event{Action: &trace.Action{Step: 1, Do: "stop"}})
}

func TestAStoppedControllerOwesNoReadyCRAndNoRun(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Version(widgetB, "ub", "2", ready(false)))
	stopped(b, 2500*ms)
	if unmet := check.Settled(b.Index(), 5*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q, want none after the stop", unmet)
	}
}

func TestAStoppedControllerOwesNoLookAtALaterChange(t *testing.T) {
	b := settledAt3s()
	stopped(b, 2*s)
	b.At(2 * s).Add(tt.Phase(2, trace.FaultPeriod)).
		At(2200 * ms).Add(tt.Action(2, "changeFixture", cm)).
		At(2300 * ms).Add(tt.EndFaults(2, 10*s, false))
	if unmet := check.Settled(b.Index(), 3*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q, want none", unmet)
	}
}

func TestAStoppedControllerMustHaveFinalizedTheCRsThePlanDeleted(t *testing.T) {
	b := settledAt3s()
	b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), tt.Version(widgetA, "ua", "2", deleting()),
		trace.Event{Action: &trace.Action{Step: 1, Do: "delete", Slot: "a", Ref: &widgetA}})
	stopped(b, 2500*ms)
	if unmet := check.Settled(b.Index(), 5*s, spec); !slices.Equal(unmet, []string{"widgets.toy ns/a is being deleted"}) {
		t.Fatalf("unmet = %q, want only slot a's CR being deleted", unmet)
	}
}

// The stop waits only for the CRs that delete actions removed, so another
// deletion, such as a cascade's, may outlast it.
func TestAStoppedControllerOwesNoFinalizerTheStopDidNotWaitFor(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Version(widgetA, "ua", "2", deleting()))
	stopped(b, 2500*ms)
	if unmet := check.Settled(b.Index(), 5*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q, want none", unmet)
	}
}

// No controller runs to remove the finalizers of a CR deleted after the
// stop, so the CR may stay.
func TestACRDeletedAfterTheStopMayStayDeleting(t *testing.T) {
	b := settledAt3s()
	stopped(b, 2*s)
	b.At(2500 * ms).Add(tt.Version(widgetA, "ua", "2", deleting()))
	if unmet := check.Settled(b.Index(), 5*s, spec); len(unmet) != 0 {
		t.Fatalf("unmet = %q, want none", unmet)
	}
}

func TestAStoppedRunWaitsForTheControllersLastWritesAndForModels(t *testing.T) {
	b := settledAt3s()
	b.At(2 * s).Add(tt.Call(ctl, "update", cm, 200, "5"))
	stopped(b, 2100*ms)
	b.At(2200 * ms).Add(trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
	requireUnmet(t, b, 3*s, "changed configmaps ns/c")
	requireUnmet(t, b, 4500*ms, "model gc has work pending")
}
