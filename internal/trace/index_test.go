package trace_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

const s = time.Second

var (
	widgetA = tt.Ref("toy/widgets", "ns", "a")
	widgetB = tt.Ref("toy/widgets", "ns", "b")
	cm      = tt.Ref("configmaps", "ns", "c")
)

func TestRun(t *testing.T) {
	var b tt.Builder
	if _, ok := b.Index().Run(); ok {
		t.Fatal("an empty trace has a Run")
	}
	run, ok := b.Add(tt.Run("toy", "widgets")).Index().Run()
	if !ok || run.Primary.Group != "toy" || run.Primary.Resource != "widgets" {
		t.Fatalf("Run() = %+v, %v", run, ok)
	}
}

func TestAppendMatchesNewIndex(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(1 * s).Add(tt.Version(widgetA, "u", "1", nil)).
		At(2 * s).Add(tt.Call(trace.Controller, "update", widgetA, 200, "2"))
	events := b.Events()
	ix := trace.NewIndex(events[:2])
	ix.Append(events[2:3]...)
	ix.Append(events[3:]...)
	if ix.Len() != 4 || !slices.EqualFunc(ix.Events(), events, func(a, b trace.Event) bool { return a.Seq == b.Seq }) {
		t.Fatalf("Events() = %+v", ix.Events())
	}
	if len(ix.Live("toy", "widgets", 2*s)) != 1 || len(ix.Calls(trace.Controller, 0, 2*s)) != 1 {
		t.Fatal("Append did not index versions or calls")
	}
	if !ix.Changed(events[3]) {
		t.Fatal("Append did not index the call's resourceVersion")
	}
}

func TestSteps(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod)).
		At(2 * s).Add(tt.EndFaults(0, 9*s, true)).
		At(3 * s).Add(tt.Phase(0, trace.Settled)).
		At(5 * s).Add(tt.Phase(0, trace.Confirmed)).
		At(6 * s).Add(tt.Phase(1, trace.FaultPeriod)).
		At(7 * s).Add(tt.EndFaults(1, 20*s, false)).
		At(20 * s).Add(tt.Phase(1, trace.TimedOut))
	steps := b.Index().Steps()
	want := []trace.StepSpan{
		{Step: 0, Start: 1 * s, EndFaults: 2 * s, Deadline: 9 * s, BackoffBound: true, WaitEnd: 3 * s, Settled: true, ConfirmEnd: 5 * s,
			Reached: map[trace.PhaseName]bool{trace.FaultPeriod: true, trace.EndFaults: true, trace.Settled: true, trace.Confirmed: true}},
		{Step: 1, Start: 6 * s, EndFaults: 7 * s, Deadline: 20 * s, WaitEnd: 20 * s,
			Reached: map[trace.PhaseName]bool{trace.FaultPeriod: true, trace.EndFaults: true, trace.TimedOut: true}},
	}
	if len(steps) != len(want) {
		t.Fatalf("Steps() = %+v", steps)
	}
	for i := range want {
		got := steps[i]
		w := want[i]
		if got.Step != w.Step || got.Start != w.Start || got.EndFaults != w.EndFaults || got.Deadline != w.Deadline ||
			got.BackoffBound != w.BackoffBound || got.WaitEnd != w.WaitEnd || got.Settled != w.Settled ||
			got.ConfirmEnd != w.ConfirmEnd || !mapsEqual(got.Reached, w.Reached) {
			t.Errorf("step %d = %+v, want %+v", i, got, w)
		}
	}
}

func mapsEqual(a, b map[trace.PhaseName]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestCallsIsHalfOpenAndPerCaller(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Call(trace.Controller, "get", cm, 200, "")).
		At(2 * s).Add(tt.Call(trace.Harness, "get", cm, 200, "")).
		At(2 * s).Add(tt.Call(trace.Controller, "list", cm, 200, "")).
		At(3 * s).Add(tt.Call(trace.Controller, "watch", cm, 200, ""))
	ix := b.Index()
	got := ix.Calls(trace.Controller, 1*s, 3*s)
	if len(got) != 2 || got[0].Call.Verb != "list" || got[1].Call.Verb != "watch" {
		t.Fatalf("Calls = %+v", got)
	}
	if n := len(ix.Calls(trace.Controller, 0, 1*s)); n != 1 {
		t.Fatalf("Calls(0, 1s) has %d calls, want 1", n)
	}
}

func TestLive(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Version(widgetB, "ub", "1", map[string]any{"v": 1})).
		At(2 * s).Add(tt.Version(widgetA, "ua", "2", map[string]any{"v": 1})).
		At(3 * s).Add(tt.Version(widgetA, "ua", "3", map[string]any{"v": 2})).
		At(4 * s).Add(tt.Deleted(widgetB, "ub", "4", nil)).
		At(4 * s).Add(tt.Version(cm, "uc", "5", nil))
	ix := b.Index()
	names := func(t time.Duration) (out []string) {
		for _, v := range ix.Live("toy", "widgets", t) {
			out = append(out, v.Ref.Name+"@"+v.RV)
		}
		return out
	}
	for at, want := range map[time.Duration][]string{
		0:       nil,
		1 * s:   {"b@1"},
		2 * s:   {"a@2", "b@1"},
		3 * s:   {"a@3", "b@1"},
		4 * s:   {"a@3"},
		100 * s: {"a@3"},
	} {
		if got := names(at); !slices.Equal(got, want) {
			t.Errorf("Live at %v = %v, want %v", at, got, want)
		}
	}
}

func TestWriter(t *testing.T) {
	created := tt.Call(trace.Controller, "create", tt.Ref("configmaps", "ns", ""), 201, "7")
	created.Call.ResultName = "c"
	var b tt.Builder
	b.Add(tt.Call(trace.Harness, "patch", widgetA, 200, "3")).
		Add(created).
		Add(tt.Call(trace.Controller, "update", widgetB, 200, "3")).
		Add(tt.Call(trace.Controller, "patch", widgetA, 200, "3"))
	ix := b.Index()
	for _, c := range []struct {
		v    trace.Version
		want trace.Caller
	}{
		{trace.Version{Ref: widgetA, RV: "3"}, trace.Harness},
		{trace.Version{Ref: widgetB, RV: "3"}, trace.Controller},
		{trace.Version{Ref: cm, RV: "7"}, trace.Controller},
		{trace.Version{Ref: widgetA, RV: "4"}, trace.Cluster},
		{trace.Version{Ref: tt.Ref("configmaps", "other", "c"), RV: "7"}, trace.Cluster},
	} {
		if got := ix.Writer(c.v); got != c.want {
			t.Errorf("Writer(%+v) = %s, want %s", c.v, got, c.want)
		}
	}
}

func TestChanged(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Version(widgetA, "u", "1", nil))
	noOpAfterVersion := add(&b, 2*s, tt.Call(trace.Controller, "patch", widgetA, 200, "1"))
	change := add(&b, 3*s, tt.Call(trace.Controller, "patch", widgetA, 200, "2"))
	noOpAfterResponse := add(&b, 4*s, tt.Call(trace.Controller, "update", widgetA, 200, "2"))
	otherObject := add(&b, 4*s, tt.Call(trace.Controller, "update", widgetB, 200, "2"))
	noRV := add(&b, 5*s, tt.Call(trace.Controller, "delete", widgetA, 200, ""))
	failed := add(&b, 5*s, tt.Call(trace.Controller, "update", widgetA, 409, "9"))
	read := add(&b, 5*s, tt.Call(trace.Controller, "get", widgetA, 200, "10"))
	created := tt.Call(trace.Controller, "create", tt.Ref("configmaps", "ns", ""), 201, "11")
	created.Call.ResultName = "c"
	create := add(&b, 6*s, created)
	b.At(6 * s).Add(tt.Version(cm, "uc", "11", nil))
	slow := tt.Call(trace.Controller, "update", cm, 200, "12")
	slow.Call.Latency = 2 * s
	b.At(7 * s).Add(tt.Version(cm, "uc", "12", nil))
	slowWrite := add(&b, 8*s, slow)
	ix := b.Index()
	for name, c := range map[string]struct {
		seq  int
		want bool
	}{
		"no-op after a version":         {noOpAfterVersion, false},
		"change":                        {change, true},
		"no-op after a response":        {noOpAfterResponse, false},
		"same rv on another object":     {otherObject, true},
		"no resourceVersion":            {noRV, true},
		"failed write":                  {failed, false},
		"read":                          {read, false},
		"create joined by result name":  {create, true},
		"own version seen before reply": {slowWrite, true},
	} {
		if got := ix.Changed(ix.Events()[c.seq]); got != c.want {
			t.Errorf("%s: Changed = %v, want %v", name, got, c.want)
		}
	}
}

// add appends e at t and returns its position.
func add(b *tt.Builder, at time.Duration, e trace.Event) int {
	b.At(at).Add(e)
	return len(b.Events()) - 1
}

func TestUp(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Start(1)).
		At(3 * s).Add(tt.Exit("exit status 1", false, "")).
		At(4 * s).Add(tt.Start(2))
	ix := b.Index()
	for _, c := range []struct {
		at    time.Duration
		since time.Duration
		up    bool
	}{
		{0, 0, false},
		{1 * s, 1 * s, true},
		{2 * s, 1 * s, true},
		{3 * s, 0, false},
		{5 * s, 4 * s, true},
	} {
		since, up := ix.Up(c.at)
		if up != c.up || (up && since != c.since) {
			t.Errorf("Up(%v) = %v, %v; want %v, %v", c.at, since, up, c.since, c.up)
		}
	}
	if n := len(ix.Procs()); n != 3 {
		t.Fatalf("Procs() has %d events", n)
	}
}

func TestEventsInIsHalfOpen(t *testing.T) {
	var b tt.Builder
	b.At(1 * s).Add(tt.Start(1)).At(2 * s).Add(tt.Start(2)).At(3 * s).Add(tt.Start(3))
	got := b.Index().EventsIn(1*s, 2*s)
	if len(got) != 1 || got[0].Proc.PID != 2 {
		t.Fatalf("EventsIn(1s, 2s) = %+v", got)
	}
}

func TestStep(t *testing.T) {
	var b tt.Builder
	ix := b.At(2 * s).Add(tt.EndFaults(1, 5*s, false)).Index()
	if span, ok := ix.Step(1); !ok || span.EndFaults != 2*s {
		t.Fatalf("Step(1) = %+v, %v", span, ok)
	}
	if _, ok := ix.Step(0); ok {
		t.Fatal("Step(0) found a step the trace lacks")
	}
}

func watch(caller trace.Caller, ref trace.Ref, status int, stream uint64) trace.Event {
	return trace.Event{Call: &trace.Call{Caller: caller, Verb: "watch", Watch: true, Ref: ref, Status: status, Stream: stream}}
}

func watchEnd(caller trace.Caller, system string, stream uint64) trace.Event {
	return trace.Event{WatchEnd: &trace.WatchEnd{Caller: caller, System: system, Stream: stream}}
}

func TestWatching(t *testing.T) {
	ctl := trace.Controller
	cms := tt.Ref("configmaps", "", "")
	var b tt.Builder
	b.At(1 * s).Add(watch(ctl, cms, 200, 1)).
		At(3 * s).Add(watchEnd(ctl, "kube", 1))
	ix := b.Index()
	cases := []struct {
		name   string
		ix     *trace.Index
		caller trace.Caller
		ref    trace.Ref
		since  time.Duration
		at     time.Duration
		want   bool
	}{
		{"open", ix, ctl, cm, 2 * s, 2 * s, true},
		{"opened at since", ix, ctl, cm, 1 * s, 2 * s, true},
		{"opened after since", ix, ctl, cm, s - 1, 2 * s, false},
		{"ended at t", ix, ctl, cm, 2 * s, 3 * s, false},
		{"other caller", ix, trace.Harness, cm, 2 * s, 2 * s, false},
		{"other group", ix, ctl, widgetA, 2 * s, 2 * s, false},
		{"other resource", ix, ctl, tt.Ref("secrets", "ns", "c"), 2 * s, 2 * s, false},
		{"other system", ix, ctl, trace.Ref{System: "vault", Resource: "configmaps", Namespace: "ns", Name: "c"}, 2 * s, 2 * s, false},
		{"other group, same resource", ix, ctl, trace.Ref{System: "kube", Group: "g", Resource: "configmaps", Namespace: "ns", Name: "c"}, 2 * s, 2 * s, false},
		{"end of another stream", (&tt.Builder{}).Add(watch(ctl, cms, 200, 1), watchEnd(ctl, "kube", 2)).Index(), ctl, cm, 0, s, true},
		{"end on another system", (&tt.Builder{}).Add(watch(ctl, cms, 200, 1), watchEnd(ctl, "vault", 1)).Index(), ctl, cm, 0, s, true},
		{"end by another caller", (&tt.Builder{}).Add(watch(ctl, cms, 200, 1), watchEnd(trace.Harness, "kube", 1)).Index(), ctl, cm, 0, s, true},
		{"same namespace", (&tt.Builder{}).Add(watch(ctl, tt.Ref("configmaps", "ns", ""), 200, 1)).Index(), ctl, cm, 0, s, true},
		{"other namespace", (&tt.Builder{}).Add(watch(ctl, tt.Ref("configmaps", "other", ""), 200, 1)).Index(), ctl, cm, 0, s, false},
		{"same name", (&tt.Builder{}).Add(watch(ctl, cm, 200, 1)).Index(), ctl, cm, 0, s, true},
		{"other name", (&tt.Builder{}).Add(watch(ctl, tt.Ref("configmaps", "ns", "d"), 200, 1)).Index(), ctl, cm, 0, s, false},
		{"failed watch", (&tt.Builder{}).Add(watch(ctl, cms, 403, 1)).Index(), ctl, cm, 0, s, false},
		{"a get", (&tt.Builder{}).Add(tt.Call(ctl, "list", cms, 200, "")).Index(), ctl, cm, 0, s, false},
	}
	for _, c := range cases {
		if got := c.ix.Watching(c.caller, c.ref, c.since, c.at); got != c.want {
			t.Errorf("%s: Watching = %v, want %v", c.name, got, c.want)
		}
	}
}

func work(system string, pending bool) trace.Event {
	return trace.Event{Work: &trace.Work{System: system, Pending: pending}}
}

func TestPending(t *testing.T) {
	var b tt.Builder
	b.At(1*s).Add(work("namespace", true), work("gc", true)).
		At(2 * s).Add(work("gc", false)).
		At(3 * s).Add(work("gc", true))
	ix := b.Index()
	for at, want := range map[time.Duration][]string{0: nil, 1 * s: {"gc", "namespace"}, 2 * s: {"namespace"}, 3 * s: {"gc", "namespace"}} {
		if got := ix.Pending(at); !slices.Equal(got, want) {
			t.Errorf("Pending(%v) = %q, want %q", at, got, want)
		}
	}
}
