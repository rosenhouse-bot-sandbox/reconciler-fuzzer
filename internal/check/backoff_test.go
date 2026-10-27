package check_test

import (
	"strconv"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const (
	tightLoop = "retried a failing request in a tight loop"
	noOpLoop  = "repeated a no-op write in a tight loop"
)

// repeat has the controller make a copy of call at each start, each
// response taking 2ms. Before the n-th (1-based) it adds between(n), a call
// that starts there or a version.
func repeat(call trace.Event, starts []time.Duration, between func(n int) trace.Event) *tt.Timeline {
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
		c := *call.Call
		c.Latency = 2 * ms
		tl.At(start+2*ms, trace.Event{Call: &c})
	}
	return &tl
}

// retries has the controller fail a get of slot a's Gadget at each start.
func retries(starts []time.Duration, between func(n int) trace.Event) *tt.Timeline {
	return repeat(tt.Call(ctl, "get", gadgetA, 500, ""), starts, between)
}

// noOpDeletes has the controller delete slot a's Gadget at each start, while
// a finalizer holds it. Each delete answers with the Gadget's version 2, so
// none changes it.
func noOpDeletes(starts []time.Duration, between func(n int) trace.Event) *tt.Timeline {
	tl := repeat(tt.Call(ctl, "delete", gadgetA, 200, "2"), starts, between)
	return tl.At(0, tt.Version(gadgetA, "ug", "2", gadgetBody(true, "x")))
}

// widgetGeneration is slot a's Widget at generation n.
func widgetGeneration(n int) trace.Event {
	return tt.Version(widgetA, "ua", "v", generation("a", n))
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
	var starts, twice []time.Duration
	at := 1 * s
	for range 20 {
		starts = append(starts, at)
		twice = append(twice, at, at+ms)
		at += limiter.When("a")
	}
	requireNone(t, check.Backoff(retries(starts, nil).Index(), spec))
	requireNone(t, check.Backoff(noOpDeletes(starts, nil).Index(), spec))
	// A reconcile may write an unchanged status twice, as when a deferred
	// update follows an explicit one.
	update := tt.Call(ctl, "update", gadgetA, 200, "2")
	update.Call.Subresource = "status"
	tl := repeat(update, twice, nil)
	tl.At(0, tt.Version(gadgetA, "ug", "2", gadgetBody(false)))
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffFailsATightLoop(t *testing.T) {
	findings := check.Backoff(retries(every(10*ms, 11), nil).Index(), spec)
	requireFinding(t, findings, "Backoff", 0, tightLoop, "kube/toy/gadgets", "get gadgets.toy ns/a: 500")
	requireNone(t, check.Backoff(retries(every(10*ms, 10), nil).Index(), spec))
}

// A controller that requeues a deleting CR at once deletes its deleting
// dependency again on each pass. Each delete succeeds and changes nothing.
func TestBackoffFailsATightLoopOfNoOpDeletes(t *testing.T) {
	findings := check.Backoff(noOpDeletes(every(10*ms, 21), nil).Index(), spec)
	if len(findings) != 1 || findings[0].Check != "Backoff" {
		t.Fatalf("Want one Backoff finding for 21 no-op deletes 10ms apart, got %+v.", findings)
	}
	requireFinding(t, findings, "Backoff", 0, noOpLoop, "kube/toy/gadgets", "delete gadgets.toy ns/a: 200, 21 calls from 10ms to 210ms")
	requireNone(t, check.Backoff(noOpDeletes(every(10*ms, 20), nil).Index(), spec))
}

// A finding names a run once, with all its calls, though the run outlasts the
// finding's step.
func TestBackoffNamesTheWholeRunOnce(t *testing.T) {
	tl := retries(every(10*ms, 30), nil)
	tl.At(200*ms, tt.Phase(1, trace.FaultPeriod))
	requireFinding(t, check.Backoff(tl.Index(), spec), "Backoff", 0, tightLoop, "kube/toy/gadgets", "get gadgets.toy ns/a: 500, 30 calls from 10ms to 300ms")
}

func TestBackoffGivesAFailingLoopAndANoOpLoopTheirOwnFindings(t *testing.T) {
	tl := noOpDeletes(every(10*ms, 21), nil)
	for _, start := range every(10*ms, 11) {
		tl.At(start, tt.Call(ctl, "get", tt.Ref("toy/gadgets", "ns", "b"), 500, ""))
	}
	findings := check.Backoff(tl.Index(), spec)
	if len(findings) != 2 || findings[0].Summary != tightLoop || findings[1].Summary != noOpLoop {
		t.Fatalf("findings = %+v, want one for the failing gets and one for the no-op deletes", findings)
	}
}

// The delete that marks the Gadget deleting changes it, so the run starts at
// the next delete.
func TestBackoffCountsTheNoOpDeletesAfterTheChangingOne(t *testing.T) {
	for calls, want := range map[int]int{21: 0, 22: 1} {
		tl := repeat(tt.Call(ctl, "delete", gadgetA, 200, "2"), every(10*ms, calls), nil)
		tl.At(0, tt.Version(gadgetA, "ug", "1", tt.Object("a")))
		tl.At(11*ms, tt.Version(gadgetA, "ug", "2", gadgetBody(true, "x")))
		if got := len(check.Backoff(tl.Index(), spec)); got != want {
			t.Errorf("%d deletes 10ms apart: %d findings, want %d", calls, got, want)
		}
	}
}

// A delete with foreground propagation adds a finalizer to the deleting
// Gadget, so it ends the run of no-op deletes before it.
func TestBackoffNoOpRunBreaksOnADeleteThatChangesTheObject(t *testing.T) {
	tl := noOpDeletes(every(10*ms, 15), nil)
	for n, start := range every(10*ms, 15) {
		start += 150 * ms
		if n == 0 {
			tl.At(start+ms, tt.Version(gadgetA, "ug", "3", gadgetBody(true, "x", "foregroundDeletion")))
		}
		del := tt.Call(ctl, "delete", gadgetA, 200, "3")
		del.Call.Latency = 2 * ms
		tl.At(start+2*ms, del)
	}
	requireNone(t, check.Backoff(tl.Index(), spec))
}

// Slow no-op writes, such as one per event of an owned object, do not count
// toward a later burst.
func TestBackoffPassesABurstAfterSlowNoOpWrites(t *testing.T) {
	starts := append(every(time.Second, 11), every(20*ms, 20)...)
	for i := 11; i < len(starts); i++ {
		starts[i] += 20 * time.Second
	}
	requireNone(t, check.Backoff(noOpDeletes(starts, nil).Index(), spec))
}

func TestBackoffPassesATightLoopOfWritesThatChangeTheObject(t *testing.T) {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"), tt.Phase(0, trace.FaultPeriod))
	for n, start := range every(10*ms, 21) {
		rv := strconv.Itoa(n + 2)
		update := tt.Call(ctl, "update", gadgetA, 200, rv)
		update.Call.Latency = 2 * ms
		tl.At(start+ms, tt.Version(gadgetA, "ug", rv, tt.Object("a")))
		tl.At(start+2*ms, update)
	}
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffNoOpRunBreaks(t *testing.T) {
	for name, tc := range map[string]struct {
		event  trace.Event
		breaks bool
	}{
		"a failing delete": {tt.Call(ctl, "delete", gadgetA, 500, ""), false},
		"a new generation": {widgetGeneration(2), true},
	} {
		findings := check.Backoff(noOpDeletes(every(10*ms, 31), func(n int) trace.Event {
			switch n {
			case 1:
				return widgetGeneration(1)
			case 16:
				return tc.event
			}
			return tt.Start(1)
		}).Index(), spec)
		if got := len(findings) == 0; got != tc.breaks {
			t.Errorf("%s: findings = %+v", name, findings)
		}
	}
}

// A reconcile whose write changed nothing may still succeed, and so reset
// controller-runtime's rate limiter. The failures after it then start a fresh
// backoff, during a hold too.
func TestBackoffPassesAFreshBackoffAfterANoOpSuccess(t *testing.T) {
	for _, held := range []bool{false, true} {
		limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
		var tl tt.Timeline
		tl.At(0, tt.Run("toy", "widgets"), tt.Version(gadgetA, "ug", "2", tt.Object("a")))
		if held {
			tl.At(0, tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
			tl.At(2*s, tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
		}
		at := 1 * s
		fail := func(calls int) {
			for range calls {
				tl.At(at, tt.Call(ctl, "update", gadgetA, 500, ""))
				at += limiter.When("a")
			}
		}
		fail(5)
		tl.At(at, tt.Call(ctl, "update", gadgetA, 200, "2"))
		limiter.Forget("a")
		at += 5 * ms
		fail(6)
		if findings := check.Backoff(tl.Index(), spec); len(findings) > 0 {
			t.Errorf("held %v: findings = %+v", held, findings)
		}
	}
}

// A fake's answer carries no resourceVersion, so the observer tells whether
// a write changed its object.
func TestBackoffFailsATightLoopOfWritesToAFakeThatTheObserverSeesNoChangeFrom(t *testing.T) {
	bucket := trace.Ref{System: "s3", Resource: "buckets", Name: "b"}
	for name, observed := range map[string]bool{"no new version": false, "a new version after each": true} {
		var tl tt.Timeline
		tl.At(0, tt.Run("toy", "widgets"), tt.Phase(0, trace.FaultPeriod), tt.Version(bucket, "ub", "1", tt.Object("b")))
		for n, start := range every(10*ms, 21) {
			put := tt.Call(ctl, "update", bucket, 200, "")
			put.Call.Latency = 2 * ms
			tl.At(start+2*ms, put)
			if observed {
				tl.At(start+5*ms, tt.Version(bucket, "ub", strconv.Itoa(n+2), tt.Object("b")))
			}
		}
		tl.At(time.Second, tt.Phase(0, trace.Settled))
		if findings := check.Backoff(tl.Index(), spec); (len(findings) == 1) == observed {
			t.Errorf("%s: findings = %+v", name, findings)
		}
	}
}

func TestBackoffRunBreaksAtAGapOf100ms(t *testing.T) {
	for gap, want := range map[time.Duration]int{99 * ms: 1, 100 * ms: 0} {
		starts := every(10*ms, 20)
		for i := 10; i < len(starts); i++ {
			starts[i] += gap - 10*ms
		}
		if got := len(check.Backoff(retries(starts, nil).Index(), spec)); got != want {
			t.Errorf("10 calls 10ms apart, then 10 more after %v: %d findings, want %d", gap, got, want)
		}
	}
}

func TestBackoffFailsATightLoopAfterASlowGap(t *testing.T) {
	starts := every(10*ms, 21)
	for i := 10; i < len(starts); i++ {
		starts[i] += 150 * ms
	}
	requireFinding(t, check.Backoff(retries(starts, nil).Index(), spec), "Backoff", 0, tightLoop, "kube/toy/gadgets", "get gadgets.toy ns/a: 500, 11 calls from 260ms to 360ms")
}

func TestBackoffNamesARequestOncePerFinding(t *testing.T) {
	starts := every(10*ms, 22)
	for i := 11; i < len(starts); i++ {
		starts[i] += 150 * ms
	}
	findings := check.Backoff(retries(starts, nil).Index(), spec)
	requireFinding(t, findings, "Backoff", 0, tightLoop, "kube/toy/gadgets", "get gadgets.toy ns/a: 500, 11 calls from 10ms to 110ms")
	if details := findings[0].Details; len(details) != 1 {
		t.Errorf("details = %q, want one line for two tight runs of one request", details)
	}
}

func TestBackoffNamesARequestAgainInALaterStep(t *testing.T) {
	tl := retries(every(10*ms, 11), nil)
	tl.At(1*s, tt.Phase(1, trace.FaultPeriod))
	for _, start := range every(10*ms, 11) {
		tl.At(1*s+start, tt.Call(ctl, "get", gadgetA, 500, ""))
	}
	if findings := check.Backoff(tl.Index(), spec); len(findings) != 2 || findings[1].Step != 1 {
		t.Fatalf("findings = %+v, want one in each step", findings)
	}
}

func TestBackoffRunBreaks(t *testing.T) {
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
		"a new generation":            {widgetGeneration(2), true},
		"the same generation":         {widgetGeneration(1), false},
	} {
		findings := check.Backoff(retries(every(10*ms, 16), func(n int) trace.Event {
			switch n {
			case 1:
				return widgetGeneration(1)
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
		"watches":          func(c *trace.Call) { c.Watch = true },
		"unknown outcomes": func(c *trace.Call) { c.Injected = trace.DropResponse },
		"ignored kinds":    func(c *trace.Call) { c.Ref = lease },
		"another caller":   func(c *trace.Call) { c.Caller = trace.Harness },
		"successful reads": func(c *trace.Call) { c.Status = 200 },
	} {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"))
		for _, start := range every(10*ms, 11) {
			call := tt.Call(ctl, "get", gadgetA, 500, "")
			edit(call.Call)
			b.At(start).Add(call)
		}
		if findings := check.Backoff(b.Index(), spec); len(findings) != 0 {
			t.Errorf("Backoff counts %s: %+v", name, findings)
		}
	}
}

func TestBackoffCountsOtherCallsDuringAHold(t *testing.T) {
	for name, call := range map[string]func() trace.Event{
		"a 500 on the held kind":        func() trace.Event { return tt.Call(ctl, "update", gadgetA, 500, "") },
		"a 409 on another kind":         func() trace.Event { return tt.Call(ctl, "update", widgetA, 409, "") },
		"a no-op write on another kind": func() trace.Event { return tt.Call(ctl, "patch", widgetA, 200, "2") },
	} {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"), tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")), tt.Version(widgetA, "ua", "2", tt.Object("a")))
		for _, start := range every(10*ms, 21) {
			b.At(start).Add(call())
		}
		b.Add(tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
		if findings := check.Backoff(b.Index(), spec); len(findings) != 1 {
			t.Errorf("%s: findings = %+v", name, findings)
		}
	}
}

// A trace that stops during a hold records no end for it.
func TestBackoffSkipsA409DuringAHoldWithNoEnd(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
	for _, start := range every(10*ms, 11) {
		b.At(start).Add(tt.Call(ctl, "update", gadgetA, 409, ""))
	}
	requireNone(t, check.Backoff(b.Index(), spec))
}

// A holdWatch on Gadgets lasts from 1s to 2s. The controller sends 21 calls
// 10ms apart, from just after first.
func TestBackoffAllowsA409OrANoOpWriteOnAHeldKindUntilStableOver4AfterTheHold(t *testing.T) {
	for name, c := range map[string]struct {
		first, latency time.Duration
		finds          bool
	}{
		"sent before the hold":                    {1*s - 300*ms, 0, true},
		"sent during the hold":                    {1*s + 100*ms, 0, false},
		"sent within stable/4 after it":           {2*s + spec.Stable/4 - 300*ms, 0, false},
		"sent within stable/4 and answered after": {2*s + spec.Stable/4 - 300*ms, 400 * ms, false},
		"sent after stable/4":                     {2*s + spec.Stable/4 + 10*ms, 0, true},
	} {
		for what, call := range map[string]func() trace.Event{
			"a 409":         func() trace.Event { return tt.Call(ctl, "update", gadgetA, 409, "") },
			"a no-op write": func() trace.Event { return tt.Call(ctl, "patch", gadgetA, 200, "2") },
		} {
			var tl tt.Timeline
			tl.At(0, tt.Run("toy", "widgets"), tt.Version(gadgetA, "ug", "2", gadgetBody(false)))
			tl.At(1*s, tt.HoldFault(0, 0, false, tt.Ref("toy/gadgets", "", "")))
			tl.At(2*s, tt.HoldFault(0, 0, true, tt.Ref("toy/gadgets", "", "")))
			for _, start := range every(10*ms, 21) {
				tl.At(c.first+start+c.latency, sent(call(), c.latency))
			}
			if findings := check.Backoff(tl.Index(), spec); (len(findings) > 0) != c.finds {
				t.Errorf("%s %s: findings = %+v", what, name, findings)
			}
		}
	}
}

// TestBackoffCountsAWriteThatDeletesItsObjectAsAChange has the controller
// write slot a's Gadget 21 times. Each answers at the Gadget's version 2.
func TestBackoffCountsAWriteThatDeletesItsObjectAsAChange(t *testing.T) {
	for name, c := range map[string]struct {
		verb, subresource string
		held, answered    []string
		finds             bool
	}{
		"an update that empties a deleting object's finalizers": {"update", "", []string{own}, nil, false},
		"a patch that empties them":                             {"patch", "", []string{own}, nil, false},
		"an update that keeps them":                             {"update", "", []string{own}, []string{own}, true},
		"an update of an object without finalizers":             {"update", "", nil, nil, true},
		"a scale update, whose answer holds no finalizers":      {"update", "scale", []string{own}, nil, true},
	} {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"), tt.Version(gadgetA, "ug", "2", gadgetBody(c.held != nil, c.held...)))
		for _, start := range every(10*ms, 21) {
			w := write(ctl, c.verb, gadgetA, "2", c.answered...)
			w.Call.Subresource = c.subresource
			b.At(start).Add(w)
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

// A CR deleted during a run still counts, since its retries joined the run.
func TestBackoffCountsACRDeletedDuringTheRun(t *testing.T) {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets"))
	for n, cr := range []trace.Ref{widgetA, widgetB} {
		tl.At(0, tt.Version(cr, "u"+cr.Name, "1", generation(cr.Name, 1)))
		limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
		at := 1*s + time.Duration(n)*3*ms
		for range 6 - n {
			tl.At(at, tt.Call(ctl, "get", gadgetA, 500, ""))
			at += limiter.When(cr.Name)
		}
	}
	tl.At(1*s+80*ms, tt.Deleted(widgetB, "ub", "2", generation("b", 1)))
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffFailsATightLoopWhileSeveralCRsLive(t *testing.T) {
	tl := retries(every(10*ms, 21), nil)
	tl.At(0, tt.Version(widgetA, "ua", "1", map[string]any{}), tt.Version(widgetB, "ub", "1", map[string]any{}))
	requireFinding(t, check.Backoff(tl.Index(), spec), "Backoff", 0, tightLoop, "kube/toy/gadgets", "21 calls")

	tl = retries(every(10*ms, 20), nil)
	tl.At(0, tt.Version(widgetA, "ua", "1", map[string]any{}), tt.Version(widgetB, "ub", "1", map[string]any{}))
	requireNone(t, check.Backoff(tl.Index(), spec))
}

func TestBackoffRunBreaksOnItsCRsGenerationOrAnyCRsWhenUnowned(t *testing.T) {
	cr := func(ref trace.Ref, generation int) trace.Event {
		return tt.Version(ref, "u"+ref.Name, "v", map[string]any{"metadata": map[string]any{"name": ref.Name, "generation": generation}})
	}
	owned := tt.OwnedBy(map[string]any{}, "ua", true)
	for name, tc := range map[string]struct {
		gadget map[string]any
		event  trace.Event
		breaks bool
	}{
		"its CR's":                       {owned, cr(widgetA, 2), true},
		"another CR's":                   {owned, cr(widgetB, 2), false},
		"any CR's, of an unowned gadget": {map[string]any{}, cr(widgetB, 2), true},
	} {
		tl := retries(every(10*ms, 21), func(n int) trace.Event {
			switch n {
			case 1:
				return tt.Version(gadgetA, "ug", "g", tc.gadget)
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

func TestBackoffRunOnACRBreaksOnItsOwnAndItsOwnersNewGeneration(t *testing.T) {
	for name, tc := range map[string]struct {
		run, bumped trace.Ref
		breaks      bool
	}{
		"an unowned CR's own": {clusterA, clusterA, true},
		"an owned CR's own":   {databaseB, databaseB, true},
		"its owner's":         {databaseB, clusterA, true},
		"a CR it owns":        {clusterA, databaseB, false},
	} {
		r := fleetRun(1, clusters, databases)
		r.Begin()
		uid := r.Create(trace.Harness, clusterA, generation("a", 1))
		r.Create(ctl, databaseB, tt.OwnedBy(generation("b", 1), uid, true))
		bumps := map[trace.Ref]map[string]any{clusterA: generation("a", 2), databaseB: tt.OwnedBy(generation("b", 2), uid, true)}
		for range 12 {
			r.Call(ctl, "update", tc.run, 422)
		}
		r.Update(trace.Harness, tc.bumped, bumps[tc.bumped])
		for range 12 {
			r.Call(ctl, "update", tc.run, 422)
		}
		if got := len(judgeWith(t, r, check.Backoff)) == 0; got != tc.breaks {
			t.Errorf("a run on %s, %s generation: breaks = %v", tc.run.Name, name, got)
		}
	}
}

func generation(name string, n int) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "generation": n}}
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
