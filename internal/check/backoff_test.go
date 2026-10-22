package check_test

import (
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const tightLoop = "retried a failing request in a tight loop"

// retries has the controller fail a get of slot a's Gadget at each start,
// each response taking 2ms. Before the n-th (1-based) it adds between(n),
// a call that starts there or a version.
func retries(starts []time.Duration, between func(n int) trace.Event) *tt.Timeline {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"), tt.Start(1), tt.Phase(0, trace.FaultPeriod))
	for n, start := range starts {
		if between != nil {
			e := between(n + 1)
			if e.Call != nil {
				tl.At(start+e.Call.Latency, e)
			} else {
				tl.At(start, e)
			}
		}
		get := tt.Call(ctl, "get", gadgetA, 500, "")
		get.Call.Latency = 2 * ms
		tl.At(start+2*ms, get)
	}
	return &tl
}

func every(gap time.Duration, n int) []time.Duration {
	starts := make([]time.Duration, n)
	for i := range starts {
		starts[i] = time.Duration(i+1) * gap
	}
	return starts
}

func TestBackoffPassesControllerRuntimesDefaultBackoff(t *testing.T) {
	limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
	var starts []time.Duration
	at := 1 * s
	for range 20 {
		starts = append(starts, at)
		at += limiter.When("a")
	}
	requireNone(t, check.Backoff(retries(starts, nil).Index(), spec))
}

func TestBackoffFailsATightLoop(t *testing.T) {
	findings := check.Backoff(retries(every(10*ms, 11), nil).Index(), spec)
	requireFinding(t, findings, "Backoff", 0, tightLoop, "kube/toy/gadgets", "get gadgets.toy ns/a: 500")
	requireNone(t, check.Backoff(retries(every(10*ms, 10), nil).Index(), spec))
}

func TestBackoffJudgesOnlyCallsSixToEleven(t *testing.T) {
	starts := append(every(10*ms, 5), every(100*ms, 6)...)
	for i := 5; i < len(starts); i++ {
		starts[i] += time.Second
	}
	requireNone(t, check.Backoff(retries(starts, nil).Index(), spec))
	starts = append(every(time.Second, 5), every(10*ms, 6)...)
	for i := 5; i < len(starts); i++ {
		starts[i] += 10 * time.Second
	}
	if len(check.Backoff(retries(starts, nil).Index(), spec)) != 1 {
		t.Fatal("a slow start excused calls 6 to 11")
	}
}

func TestBackoffRunBreaks(t *testing.T) {
	widget := func(generation int) trace.Event {
		return tt.Version(widgetA, "ua", "v", map[string]any{"metadata": map[string]any{"name": "a", "generation": generation}})
	}
	other := tt.Ref("toy/gadgets", "ns", "b")
	slowSuccess := tt.Call(ctl, "get", gadgetA, 200, "")
	slowSuccess.Call.Latency = time.Second
	for name, tc := range map[string]struct {
		event  trace.Event
		breaks bool
	}{
		"a success":                   {tt.Call(ctl, "get", gadgetA, 200, ""), true},
		"a slow success":              {slowSuccess, true},
		"a success on another object": {tt.Call(ctl, "get", other, 200, ""), false},
		"another status":              {tt.Call(ctl, "get", gadgetA, 503, ""), false},
		"a new generation":            {widget(2), true},
		"the same generation":         {widget(1), false},
	} {
		findings := check.Backoff(retries(every(10*ms, 16), func(n int) trace.Event {
			switch n {
			case 1:
				return widget(1)
			case 9:
				return tc.event
			}
			return tt.Start(1)
		}).Index(), spec)
		if got := len(findings) == 0; got != tc.breaks {
			t.Errorf("%s: findings = %+v", name, findings)
		}
	}
}

func TestBackoffSkips(t *testing.T) {
	for name, edit := range map[string]func(c *trace.Call){
		"watches":             func(c *trace.Call) { c.Watch = true },
		"unknown outcomes":    func(c *trace.Call) { c.Injected = trace.DropResponse },
		"ignored kinds":       func(c *trace.Call) { c.Ref = lease },
		"another caller":      func(c *trace.Call) { c.Caller = trace.Harness },
		"successes":           func(c *trace.Call) { c.Status = 200 },
		"409s on a held kind": func(c *trace.Call) { c.Verb, c.Status = "update", 409 },
	} {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"), tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
		for _, start := range every(10*ms, 11) {
			call := tt.Call(ctl, "get", gadgetA, 500, "")
			edit(call.Call)
			b.At(start).Add(call)
		}
		b.Add(tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
		if findings := check.Backoff(b.Index(), spec); len(findings) != 0 {
			t.Errorf("Backoff counts %s: %+v", name, findings)
		}
	}
}

func TestBackoffHoldsOnlyA409DuringTheHold(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
	for _, start := range every(10*ms, 11) {
		b.At(start).Add(tt.Call(ctl, "update", gadgetA, 500, ""))
	}
	b.Add(tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
	for _, start := range every(10*ms, 11) {
		b.At(time.Second + start).Add(tt.Call(ctl, "update", gadgetA, 409, ""))
	}
	b.Add(tt.HoldFault(0, 1, false, tt.Ref("configmaps", "", "")))
	if findings := check.Backoff(b.Index(), spec); len(findings) != 1 || len(findings[0].Details) != 2 {
		t.Fatalf("findings = %+v, want the 500s during the hold and the 409s after it", findings)
	}
}

func TestBackoffAllowsA409JustAfterTheHold(t *testing.T) {
	for name, c := range map[string]struct {
		after time.Duration
		finds bool
	}{
		"within stable/4": {spec.Stable/4 - 200*ms, false},
		"after stable/4":  {spec.Stable/4 + 10*ms, true},
	} {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"), tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
		b.At(time.Second).Add(tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
		for _, start := range every(10*ms, 11) {
			b.At(time.Second + c.after + start).Add(tt.Call(ctl, "update", gadgetA, 409, ""))
		}
		if findings := check.Backoff(b.Index(), spec); (len(findings) > 0) != c.finds {
			t.Errorf("%s: findings = %+v", name, findings)
		}
	}
}

func TestBackoffTimesRequestStarts(t *testing.T) {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"))
	for i := range 11 {
		get := tt.Call(ctl, "get", gadgetA, 500, "")
		get.Call.Latency = 2*s - time.Duration(i)*140*ms
		tl.At(4*s+time.Duration(i)*10*ms, get)
	}
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffGivesOneFindingPerStepAndKind(t *testing.T) {
	tl := retries(every(10*ms, 11), nil)
	for _, start := range every(10*ms, 11) {
		tl.At(start, tt.Call(ctl, "get", tt.Ref("toy/gadgets", "ns", "b"), 500, ""))
	}
	tl.At(1*s, tt.Phase(1, trace.FaultPeriod))
	for _, start := range every(10*ms, 11) {
		tl.At(1*s+start, tt.Call(ctl, "get", cm, 500, ""))
	}
	findings := check.Backoff(tl.Index(), spec)
	if len(findings) != 2 || len(findings[0].Details) != 2 || findings[1].Step != 1 || findings[1].Kind != "kube/configmaps" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestBackoffKeysAFailedCreateOnItsRequestedName(t *testing.T) {
	limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"))
	for offset, name := range []string{"a", "b"} {
		at := 1*s + time.Duration(offset)*3*ms
		for range 12 {
			create := tt.Call(ctl, "create", tt.Ref("toy/gadgets", "ns", ""), 500, "")
			create.Call.RequestName = name
			tl.At(at, create)
			at += limiter.When(name)
		}
	}
	requireNone(t, check.Backoff(tl.Index(), spec))
}

// limited has one default limiter per CR fail a get of gadgetA, each
// starting offset after the last, while every CR lives.
func limited(crs []trace.Ref, offset time.Duration, calls int) *tt.Timeline {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"))
	for n, cr := range crs {
		tl.At(0, tt.Version(cr, "u"+cr.Name, "1", map[string]any{"metadata": map[string]any{"name": cr.Name, "generation": 1}}))
		limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
		at := 1*s + time.Duration(n)*offset
		for range calls {
			tl.At(at, tt.Call(ctl, "get", gadgetA, 500, ""))
			at += limiter.When(cr.Name)
		}
	}
	return &tl
}

func TestBackoffPassesOneDefaultBackoffPerCROnASharedRequest(t *testing.T) {
	widgetC := tt.Ref("toy/widgets", "ns", "c")
	for _, offset := range []time.Duration{3 * ms, 40 * ms} {
		requireNone(t, check.Backoff(limited([]trace.Ref{widgetA, widgetB}, offset, 14).Index(), spec))
		requireNone(t, check.Backoff(limited([]trace.Ref{widgetA, widgetB, widgetC}, offset, 14).Index(), spec))
	}
}

func TestBackoffFailsATightLoopWhileSeveralCRsLive(t *testing.T) {
	tl := retries(every(10*ms, 21), nil)
	tl.At(0, tt.Version(widgetA, "ua", "1", map[string]any{}), tt.Version(widgetB, "ub", "1", map[string]any{}))
	requireFinding(t, check.Backoff(tl.Index(), spec), "Backoff", 0, tightLoop, "kube/toy/gadgets", "21 calls")

	tl = retries(every(10*ms, 20), nil)
	tl.At(0, tt.Version(widgetA, "ua", "1", map[string]any{}), tt.Version(widgetB, "ub", "1", map[string]any{}))
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffRunBreaksOnlyOnItsOwnCRsGeneration(t *testing.T) {
	cr := func(ref trace.Ref, generation int) trace.Event {
		return tt.Version(ref, "u"+ref.Name, "v", map[string]any{"metadata": map[string]any{"name": ref.Name, "generation": generation}})
	}
	for name, tc := range map[string]struct {
		event  trace.Event
		breaks bool
	}{
		"its CR's":     {cr(widgetA, 2), true},
		"another CR's": {cr(widgetB, 2), false},
	} {
		tl := retries(every(10*ms, 21), func(n int) trace.Event {
			switch n {
			case 1:
				return tt.Version(gadgetA, "ug", "g", tt.OwnedBy(map[string]any{}, "ua", true))
			case 2:
				return cr(widgetA, 1)
			case 3:
				return cr(widgetB, 1)
			case 15:
				return tc.event
			}
			return tt.Start(1)
		})
		if got := len(check.Backoff(tl.Index(), spec)) == 0; got != tc.breaks {
			t.Errorf("%s generation: breaks = %v", name, got)
		}
	}
}

func TestBackoffRunBreaksOnlyOnASuccessOfTheSameRequest(t *testing.T) {
	for name, edit := range map[string]func(c *trace.Call){
		"another verb":        func(c *trace.Call) { c.Verb = "update" },
		"another subresource": func(c *trace.Call) { c.Subresource = "status" },
		"another system":      func(c *trace.Call) { c.Ref.System = "other" },
	} {
		success := tt.Call(ctl, "get", gadgetA, 200, "")
		edit(success.Call)
		tl := retries(every(10*ms, 11), func(n int) trace.Event {
			if n == 9 {
				return success
			}
			return tt.Start(1)
		})
		if len(check.Backoff(tl.Index(), spec)) == 0 {
			t.Errorf("a success on %s broke the run", name)
		}
	}
}

func TestBackoffNamesTheSubresource(t *testing.T) {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"), tt.Phase(0, trace.FaultPeriod))
	for _, start := range every(10*ms, 11) {
		patch := tt.Call(ctl, "patch", gadgetA, 403, "")
		patch.Call.Subresource = "status"
		tl.At(start, patch)
	}
	requireFinding(t, check.Backoff(tl.Index(), spec), "Backoff", 0, tightLoop, "kube/toy/gadgets", "patch gadgets.toy ns/a/status: 403")
}
