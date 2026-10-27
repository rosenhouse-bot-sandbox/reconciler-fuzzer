package check_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

// createStep is step 0 of a run: the harness creates widget a, and the
// controller makes it ready at 1.5s.
func createStep() *tt.Builder {
	run := tt.Run("toy", "widgets")
	run.Run.Primaries[0].Kind = widgets
	var b tt.Builder
	b.Add(run, tt.Start(1)).
		At(0).Add(tt.Phase(0, trace.FaultPeriod)).
		At(100 * ms).Add(tt.Call(trace.Harness, "create", widgetA, 201, "1")).
		At(100 * ms).Add(tt.Version(widgetA, "ua", "1", ready(false))).
		At(1 * s).Add(tt.EndFaults(0, 10*s, false)).
		At(1500 * ms).Add(tt.Call(ctl, "update", widgetA, 200, "2")).
		At(1500 * ms).Add(tt.Version(widgetA, "ua", "2", ready(true)))
	return &b
}

func converge(b *tt.Builder, s check.Spec) []check.Finding {
	findings, _ := check.Converge(b.Index(), s)
	return findings
}

func only(t *testing.T, findings []check.Finding) check.Finding {
	t.Helper()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one", findings)
	}
	return findings[0]
}

func TestConvergeB1DidNotStaySettled(t *testing.T) {
	b := createStep()
	b.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(4500 * ms).Add(tt.Call(ctl, "create", cm, 201, "3")).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed))
	f := only(t, converge(b, spec))
	if f.Check != "Converge" || f.Step != 0 || f.Summary != "did not stay settled (clause 7)" || f.Kind != "kube/configmaps" || f.At != 5500*ms {
		t.Fatalf("finding = %+v", f)
	}
	if !slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, "changed configmaps ns/c") }) {
		t.Fatalf("details = %q", f.Details)
	}
	if len(f.Labels) != 0 {
		t.Fatalf("labels = %q", f.Labels)
	}
}

func churn(backoffBound bool) *tt.Builder {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(0).Add(tt.Phase(0, trace.FaultPeriod)).
		At(100 * ms).Add(tt.Version(widgetA, "ua", "1", ready(true))).
		At(1 * s).Add(tt.EndFaults(0, 10*s, backoffBound))
	for i := range 10 {
		rv := string(rune('b' + i))
		b.At(time.Duration(i+1) * s).Add(tt.Call(ctl, "update", cm, 200, rv)).Add(tt.Version(cm, "uc", rv, nil))
	}
	b.At(10 * s).Add(tt.Phase(0, trace.TimedOut))
	return &b
}

func TestConvergeChurnDidNotSettle(t *testing.T) {
	f := only(t, converge(churn(false), spec))
	if f.Summary != "did not settle (clause 7)" || f.Kind != "kube/configmaps" || f.At != 10*s || len(f.Labels) != 0 {
		t.Fatalf("finding = %+v", f)
	}
	if !slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, "changed configmaps ns/c") }) {
		t.Fatalf("details = %q", f.Details)
	}
}

func TestConvergeLabelsBackoffBound(t *testing.T) {
	f := only(t, converge(churn(true), spec))
	if !slices.Equal(f.Labels, []string{"backoff-bound"}) {
		t.Fatalf("labels = %q", f.Labels)
	}
}

func TestConvergeStuckDeletion(t *testing.T) {
	b := createStep()
	b.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed)).
		At(5500 * ms).Add(tt.Phase(1, trace.FaultPeriod)).
		At(5600 * ms).Add(tt.Call(trace.Harness, "delete", widgetA, 200, "3")).
		At(5600 * ms).Add(tt.Version(widgetA, "ua", "3", deleting())).
		At(5700 * ms).Add(tt.EndFaults(1, 15*s, false)).
		At(15 * s).Add(tt.Phase(1, trace.TimedOut))
	f := only(t, converge(b, spec))
	if f.Step != 1 || f.Summary != "did not settle (clause 2)" || f.Kind != "kube/toy/widgets" ||
		!slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, "ns/a is being deleted") }) {
		t.Fatalf("finding = %+v", f)
	}
}

func TestConvergeReadLoop(t *testing.T) {
	b := createStep()
	for at := 1600 * ms; at < 10*s; at += 300 * ms {
		b.At(at).Add(tt.Call(ctl, "get", widgetA, 200, ""))
	}
	b.At(10 * s).Add(tt.Phase(0, trace.TimedOut))
	f := only(t, converge(b, spec))
	if f.Summary != "did not settle (clause 7)" || f.Kind != "" ||
		!slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, "gets and lists") }) {
		t.Fatalf("finding = %+v", f)
	}
}

func requireNote(t *testing.T, b *tt.Builder, s check.Spec, want string) {
	t.Helper()
	findings, notes := check.Converge(b.Index(), s)
	if len(findings) != 0 || len(notes) != 1 || !strings.Contains(notes[0], want) {
		t.Fatalf("findings = %+v, notes = %q; want only a note that says %q", findings, notes, want)
	}
}

func TestConvergeNotesWhereTheStepsRecordEnds(t *testing.T) {
	confirmed := createStep()
	confirmed.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed)).
		At(9 * s).Add(tt.Call(ctl, "get", widgetA, 200, ""))
	longer := spec
	longer.Stable = 3 * s
	requireNote(t, confirmed, longer, "Step 0 is not judged: it settled at 4.5s and had to stay settled until 7.5s, but its confirm window ends at 5.5s.")

	cut := createStep()
	cut.At(10 * s).Add(tt.Phase(0, trace.TimedOut)).
		At(10 * s).Add(tt.Phase(1, trace.FaultPeriod))
	slow := spec
	slow.Stable = 8 * s
	requireNote(t, cut, slow, "Step 0 is not judged: it settled at 9.5s and had to stay settled until 17.5s, but the next step starts at 10s.")
}

func TestConvergeSearchesOnlyUntilConfirmed(t *testing.T) {
	b := createStep()
	b.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed))
	longer := spec
	longer.Stable = 5 * s
	if f := only(t, converge(b, longer)); f.Summary != "did not settle (clause 7)" || f.At != 5500*ms {
		t.Fatalf("finding = %+v", f)
	}
}

func TestConvergeNotesATraceThatEndsBeforeTheDeadline(t *testing.T) {
	b := createStep()
	b.At(2 * s).Add(tt.Call(ctl, "update", cm, 200, "9"))
	requireNote(t, b, spec, "Step 0 is not judged: the trace ends at 2s, before its deadline 10s.")
}

// cleanRun settles and confirms a create step, then a restart step.
func cleanRun() *tt.Builder {
	b := createStep()
	b.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed)).
		At(5500 * ms).Add(tt.Phase(1, trace.FaultPeriod)).
		At(5600 * ms).Add(tt.Exit("signal: terminated", true, "bye")).
		At(5700 * ms).Add(tt.Start(2)).
		At(5700 * ms).Add(tt.EndFaults(1, 20*s, false)).
		At(5800 * ms).Add(tt.Call(ctl, "list", widgetA, 200, "")).
		At(7800 * ms).Add(tt.Phase(1, trace.Settled)).
		At(9800 * ms).Add(tt.Phase(1, trace.Confirmed))
	return b
}

func TestCleanRunHasNoFindings(t *testing.T) {
	if r := check.Judge(cleanRun().Index(), spec); len(r.Findings) != 0 || len(r.Notes) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestStoredCleanTraceHasNoFindings(t *testing.T) {
	events, err := trace.Load("testdata/clean.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if r := check.Judge(trace.NewIndex(events), spec); len(r.Findings) != 0 {
		t.Fatalf("findings = %+v", r.Findings)
	}
}

// judgeRecorded judges a trace recorded from a toy run, under the spec its
// run event records.
func judgeRecorded(t *testing.T, path string) check.Report {
	t.Helper()
	events, err := trace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ix := trace.NewIndex(events)
	recorded, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	return check.Judge(ix, recorded)
}

func TestRecordedB0RunHasNoFindings(t *testing.T) {
	if r := judgeRecorded(t, "testdata/b0.jsonl"); len(r.Findings) != 0 || len(r.Notes) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestRecordedB16RunDidNotSettle(t *testing.T) {
	r := judgeRecorded(t, "testdata/b16.jsonl")
	if f := only(t, r.Findings); f.Summary != "did not settle (clause 1)" || f.Step != 0 ||
		!slices.ContainsFunc(f.Details, func(d string) bool { return strings.HasSuffix(d, "/widget is not ready") }) {
		t.Fatalf("finding = %+v", f)
	}
}

func TestCrash(t *testing.T) {
	b := cleanRun()
	b.At(6 * s).Add(tt.Exit("exit status 2", false, "panic: boom")).Add(tt.Start(3)).
		At(6100 * ms).Add(tt.Exit("exit status 3", false, "panic: again")).Add(tt.Start(4))
	f := only(t, check.Crash(b.Index()))
	if f.Check != "Crash" || f.Step != 1 || f.At != 6*s || f.Summary != "the controller exited 2 times" {
		t.Fatalf("finding = %+v", f)
	}
	details := strings.Join(f.Details, "\n")
	if !strings.Contains(details, "exit status 2") || !strings.Contains(details, "panic: boom") || strings.Contains(details, "again") {
		t.Fatalf("details = %q", f.Details)
	}
}

func TestCrashAllowsAnExitWithinASecondOfALeaseFault(t *testing.T) {
	lease := trace.Ref{System: "kube", Group: "coordination.k8s.io", Resource: "leases", Namespace: "ns", Name: "l"}
	faulted := func(ref trace.Ref, injected string) trace.Event {
		e := tt.Call(trace.Controller, "update", ref, 500, "")
		e.Call.Injected = injected
		return e
	}
	for name, tc := range map[string]struct {
		call  trace.Event
		after time.Duration
		want  int
	}{
		"lease fault, exit at 1s":     {faulted(lease, "error(500)"), 1 * s, 0},
		"lease fault, exit after 1s":  {faulted(lease, "error(500)"), 1100 * ms, 1},
		"lease failure with no fault": {faulted(lease, ""), 100 * ms, 1},
		"fault on another kind":       {faulted(tt.Ref("configmaps", "ns", "c"), "error(500)"), 100 * ms, 1},
	} {
		b := cleanRun()
		b.At(10 * s).Add(tc.call).At(10*s + tc.after).Add(tt.Exit("exit status 1", false, "")).Add(tt.Start(3))
		if got := check.Crash(b.Index()); len(got) != tc.want {
			t.Errorf("%s: findings = %+v, want %d", name, got, tc.want)
		}
	}
}

func TestCrashOutsideAnyStepIsStepMinusOne(t *testing.T) {
	var b tt.Builder
	b.Add(tt.Run("toy", "widgets"), tt.Start(1), tt.Exit("exit status 1", false, "")).
		At(1 * s).Add(tt.Phase(0, trace.FaultPeriod))
	if f := only(t, check.Crash(b.Index())); f.Step != -1 || f.Summary != "the controller exited 1 time" {
		t.Fatalf("finding = %+v", f)
	}
}

func TestJudgeRunsConvergeThenCrash(t *testing.T) {
	b := churn(false)
	b.At(10 * s).Add(tt.Exit("exit status 2", false, ""))
	findings := check.Judge(b.Index(), spec).Findings
	if len(findings) != 2 || findings[0].Check != "Converge" || findings[1].Check != "Crash" {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestJudgeNotesAFailedAction(t *testing.T) {
	b := cleanRun()
	failed := tt.Action(1, "create", widgetA)
	failed.Action.Index, failed.Action.Error = 2, "the slot's name is still taken after the create waited 1s"
	b.At(10 * s).Add(failed)
	notes := check.Judge(b.Index(), spec).Notes
	if len(notes) != 1 || notes[0] != "Step 1, action 2 (create) failed at 10s: the slot's name is still taken after the create waited 1s" {
		t.Fatalf("notes = %q", notes)
	}
}

// A step without a period ends its faults as its last action returns, so
// a fault meant for the controller's next request never acts. A period
// does not help while a kill or a stop keeps the controller down. Nor does
// it help a hold of a kind that the controller does not watch later.
func TestJudgeNotesAFaultPeriodOfZero(t *testing.T) {
	const what, hint = "killAfter on verb=update resource=clusters nth=2", " because the step has no period. Set one to keep faults on after the last action returns."
	request := func(ended bool) trace.Event { return tt.Fault(1, 1, ended, what) }
	model := func(ended bool) trace.Event { return tt.ModelFault(1, 1, ended, "stuck", "model:gadgets.toy") }
	secrets := trace.Ref{System: "kube", Resource: "secrets"}
	hold := func(ended bool) trace.Event { return tt.HoldFault(1, 1, ended, secrets) }
	kill := []trace.Event{{Action: &trace.Action{Step: 1, Do: "kill"}}, tt.Exit("signal: killed", true, "")}
	stop := []trace.Event{{Action: &trace.Action{Step: 1, Do: "stop"}}, tt.Exit("signal: terminated", true, "")}
	watch := tt.Call(ctl, "watch", secrets, 200, "")
	watch.Call.Watch, watch.Call.Stream = true, 7
	watchLater := []trace.Event{watch}
	for name, c := range map[string]struct {
		phase         trace.Event
		fault         func(ended bool) trace.Event
		during, after []trace.Event
		lasted        time.Duration
		want          string
	}{
		"a request fault":                        {tt.FaultPeriod(1, 0), request, nil, nil, 0, "Step 1, fault 1 (" + what + ") picked no request: it lasted 0s" + hint},
		"a longer request fault":                 {tt.FaultPeriod(1, 0), request, nil, nil, 1234567 * time.Microsecond, "Step 1, fault 1 (" + what + ") picked no request: it lasted 1.235s" + hint},
		"a model fault":                          {tt.FaultPeriod(1, 0), model, nil, nil, s, "Step 1, fault 1 (stuck) held no object: it lasted 1s" + hint},
		"a holdWatch":                            {tt.FaultPeriod(1, 0), hold, nil, watchLater, s, "Step 1, fault 1 (holdWatch) held no watch: it lasted 1s" + hint},
		"a holdWatch of a kind never watched":    {tt.FaultPeriod(1, 0), hold, nil, nil, s, "Step 1, fault 1 (holdWatch) held no watch."},
		"a request fault of a step with period":  {tt.FaultPeriod(1, s), request, nil, nil, s, "Step 1, fault 1 (" + what + ") picked no request."},
		"a model fault of a step with period":    {tt.FaultPeriod(1, s), model, nil, nil, s, "Step 1, fault 1 (stuck) held no object."},
		"a holdWatch of a step with period":      {tt.FaultPeriod(1, s), hold, nil, watchLater, s, "Step 1, fault 1 (holdWatch) held no watch."},
		"a trace that lacks the period":          {tt.Phase(1, trace.FaultPeriod), request, nil, nil, 0, "Step 1, fault 1 (" + what + ") picked no request."},
		"a request fault that ends after a kill": {tt.FaultPeriod(1, 0), request, kill, nil, s, "Step 1, fault 1 (" + what + ") picked no request."},
		"a model fault that ends after a stop":   {tt.FaultPeriod(1, 0), model, stop, nil, s, "Step 1, fault 1 (stuck) held no object."},
	} {
		b := createStep()
		b.At(2*s).Add(c.phase, c.fault(false)).At(2*s + c.lasted/2).Add(c.during...).At(2*s + c.lasted).Add(c.fault(true)).At(3*s + c.lasted).Add(c.after...)
		notes := slices.DeleteFunc(check.Judge(b.Index(), spec).Notes, func(n string) bool { return !strings.HasPrefix(n, "Step 1, fault") })
		if !slices.Equal(notes, []string{c.want}) {
			t.Errorf("%s: fault notes = %q, want %q", name, notes, c.want)
		}
	}
}

// A trace that ends during a hold does not say how long the hold lasted.
func TestJudgeGivesNoLengthOfAHoldThatNeverEnded(t *testing.T) {
	b := createStep()
	b.At(2*s).Add(tt.FaultPeriod(1, 0), tt.HoldFault(1, 0, false, trace.Ref{System: "kube", Resource: "secrets"}))
	if notes := check.Judge(b.Index(), spec).Notes; !slices.Contains(notes, "Step 1, fault 0 (holdWatch) held no watch.") {
		t.Errorf("notes = %q, want the hold's note without a length", notes)
	}
}

func TestJudgeNotesAFaultThatPickedNoRequest(t *testing.T) {
	const what = "dropResponse on verb=create resource=configmaps nth=2"
	fault := func(ended bool) trace.Event {
		return trace.Event{Fault: &trace.Fault{Step: 1, Index: 0, Ended: ended, What: what}}
	}
	picked := tt.Call(ctl, "create", cm, 201, "7")
	picked.Call.Injected = trace.DropResponse
	for name, tc := range map[string]struct {
		call  trace.Event
		notes int
	}{
		"picked":                 {picked, 0},
		"no call":                {tt.Call(ctl, "get", cm, 200, ""), 1},
		"another effect":         {call("get", cm, 500, "error(500)"), 1},
		"picked after the fault": {picked, 1},
		"held past the fault":    {picked, 0},
	} {
		b := createStep()
		b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), fault(false))
		switch name {
		case "picked after the fault":
			b.At(2100 * ms).Add(fault(true)).At(2200 * ms).Add(tc.call)
		case "held past the fault":
			held := *tc.call.Call
			held.Latency = 150 * ms
			tc.call.Call = &held
			b.At(2100 * ms).Add(fault(true)).At(2200 * ms).Add(tc.call)
		default:
			b.At(2100 * ms).Add(tc.call).At(2200 * ms).Add(fault(true))
		}
		var notes []string
		for _, note := range check.Judge(b.Index(), spec).Notes {
			if strings.Contains(note, "fault") {
				notes = append(notes, note)
			}
		}
		if len(notes) != tc.notes {
			t.Errorf("%s: notes = %q, want %d", name, notes, tc.notes)
			continue
		}
		if tc.notes == 1 && notes[0] != "Step 1, fault 0 ("+what+") picked no request." {
			t.Errorf("%s: note = %q", name, notes[0])
		}
	}
}

func TestMarkdown(t *testing.T) {
	var b tt.Builder
	run := tt.Run("toy", "widgets")
	run.Run.PlanSteps = 2
	ix := b.Add(run).Index()
	if got := check.Markdown(ix, check.Report{}); got != "No findings.\n" {
		t.Fatalf("Markdown of nothing = %q", got)
	}
	got := check.Markdown(ix, check.Report{
		Findings: []check.Finding{
			{Check: "Converge", Step: 1, At: 1500 * ms, Labels: []string{"backoff-bound"}, Summary: "did not settle", Details: []string{"ns/a is not ready"}},
			{Check: "Converge", Step: 2, Summary: "did not settle"},
			{Check: "Crash", Step: 3, Summary: "the controller exited 1 time"},
			{Check: "Crash", Step: -1, Summary: "the controller exited 2 times"},
		},
		Notes: []string{"Step 0 is not judged."},
	})
	for _, want := range []string{"# Findings", "## Converge: did not settle\n\nStep 1, at 1.5s.", "backoff-bound", "ns/a is not ready",
		"Step 2 (delete all), at 0s.", "## Crash: the controller exited 1 time\n\nStep 3 (restart)",
		"## Crash: the controller exited 2 times\n\nBefore step 0, at 0s.", "# Notes\n\n- Step 0 is not judged.\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, got)
		}
	}
	if got := check.Markdown(ix, check.Report{Notes: []string{"n"}}); !strings.HasPrefix(got, "No findings.\n") {
		t.Errorf("Markdown with only notes = %q", got)
	}
}

func TestSpecFor(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var b tt.Builder
	if _, err := check.SpecFor(c, b.Index()); err == nil {
		t.Fatal("SpecFor works on a trace with no Run")
	}
	got, err := check.SpecFor(c, b.Add(tt.Run("toy.reconciler-fuzzer", "widgets")).Index())
	if err != nil {
		t.Fatal(err)
	}
	if want := (trace.Kind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget", Resource: "widgets"}); len(got.Primaries) != 1 ||
		got.Primaries[0].Kind != want || got.Stable != 2*s || got.Budgets != c.Budgets {
		t.Fatalf("spec = %+v", got)
	}
	ready := map[string]any{
		"metadata": map[string]any{"generation": int64(1)},
		"spec":     map[string]any{"count": int64(1)},
		"status":   map[string]any{"observedGeneration": int64(1), "ready": int64(1)},
	}
	if ok, err := got.Primaries[0].Ready(ready); !ok || err != nil {
		t.Fatalf("Ready is not the config's: %v, %v", ok, err)
	}
	if ok, _ := got.Primaries[0].Ready(map[string]any{}); ok {
		t.Fatal("Ready holds for an empty object")
	}
}

func TestSpecForRefusesAConfigThatListsOtherKinds(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.Primaries = append(run.Run.Primaries, trace.Primary{Kind: trace.Kind{Group: "toy.reconciler-fuzzer", Resource: "gadgets"}})
	var b tt.Builder
	_, err = check.SpecFor(c, b.Add(run).Index())
	if want := "the config's primaries list widgets.toy.reconciler-fuzzer, but the trace's list widgets.toy.reconciler-fuzzer, gadgets.toy.reconciler-fuzzer"; err == nil || err.Error() != want {
		t.Errorf("SpecFor = %v, want %q", err, want)
	}
}

func TestSpecForTellsToRunAgainATraceThatListsNoPrimaries(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.Primaries, run.Run.Stable, run.Run.Reads, run.Run.Writes = nil, s, 2, 4
	for name, c := range map[string]*config.Config{"with a config": c, "without one": nil} {
		var b tt.Builder
		_, err := check.SpecFor(c, b.Add(run).Index())
		if want := "the trace's run event lists no primaries, so an older reconciler-fuzzer recorded it; run its plan.json again"; err == nil || err.Error() != want {
			t.Errorf("SpecFor %s = %v, want %q", name, err, want)
		}
	}
}

func TestSpecForTakesTheResourceTheRunEventRecordsForAListedKind(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgetries")
	run.Run.Primaries[0].Kind.Kind = "Widget"
	var b tt.Builder
	got, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil || got.Primaries[0].Kind.Resource != "widgetries" {
		t.Errorf("SpecFor = %+v, %v; want the recorded resource widgetries.", got.Primaries, err)
	}
}

func TestSpecForWithoutAConfigReadsTheRunEvent(t *testing.T) {
	var b tt.Builder
	run := tt.Run("toy", "widgets")
	if _, err := check.SpecFor(nil, b.Add(run).Index()); err == nil {
		t.Fatal("SpecFor works on a run event that records no spec")
	}
	run.Run.Primaries[0].Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "self.ok", 4*s, 3, 5
	b = tt.Builder{}
	got, err := check.SpecFor(nil, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Primaries) != 1 || got.Primaries[0].Kind.Resource != "widgets" || got.Stable != 4*s || got.Budgets != (config.Budgets{Reads: 3, Writes: 5}) {
		t.Fatalf("spec = %+v", got)
	}
	if ok, err := got.Primaries[0].Ready(map[string]any{"ok": true}); !ok || err != nil {
		t.Fatalf("Ready = %v, %v", ok, err)
	}
	for name, mistake := range map[string]func(r *trace.Run){
		"no primaries": func(r *trace.Run) { r.Primaries = nil },
		"a bad ready":  func(r *trace.Run) { r.Primaries[0].Ready = "self.(" },
		"a bad retain": func(r *trace.Run) { r.Primaries[0].Retain = "self.(" },
	} {
		bad := *run.Run
		bad.Primaries = slices.Clone(run.Run.Primaries)
		mistake(&bad)
		b = tt.Builder{}
		if _, err := check.SpecFor(nil, b.Add(trace.Event{Run: &bad}).Index()); err == nil {
			t.Errorf("SpecFor accepts a run event with %s.", name)
		}
	}
}

// TestWriteCleanTrace regenerates testdata/clean.jsonl when
// RF_WRITE_TESTDATA is set.
func TestWriteCleanTrace(t *testing.T) {
	if os.Getenv("RF_WRITE_TESTDATA") == "" {
		t.Skip("RF_WRITE_TESTDATA is not set")
	}
	var lines []byte
	for _, e := range cleanRun().Events() {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(append(lines, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join("testdata", "clean.jsonl"), lines, 0o644); err != nil {
		t.Fatal(err)
	}
}

var gadgetKind = trace.Kind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget", Resource: "gadgets"}

func TestSpecForTakesModelsFromTheConfig(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dep := &config.Dependency{Required: true, Track: s}
	c.Models = []config.Model{
		{Kind: schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}, Dependency: dep},
		{Kind: schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gizmo"}},
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	recorded := gadgetKind
	recorded.Resource = "gadgetries"
	run.Run.Models = []trace.Model{{Kind: recorded, Ready: "false", Dependency: &trace.Dependency{}}}
	run.Run.StatusKinds = []trace.Kind{gadgetKind}
	var b tt.Builder
	got, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 || got.Models[0].Kind != recorded || *got.Models[0].Dependency != (trace.Dependency{Required: true, Track: s}) ||
		got.Models[1].Kind.Resource != "gizmos" || got.Models[1].Kind.Kind != "Gizmo" || got.Models[1].Dependency != nil {
		t.Fatalf("models = %+v", got.Models)
	}
	ready := map[string]any{"metadata": map[string]any{"generation": int64(2)},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(2)}}}}
	if ok, err := got.Models[0].Ready(ready); !ok || err != nil {
		t.Fatalf("model Ready is not config.ReadyConditionCEL: %v, %v", ok, err)
	}
	if !slices.Equal(got.StatusKinds, run.Run.StatusKinds) {
		t.Fatalf("status kinds = %+v", got.StatusKinds)
	}
}

func TestSpecForWithoutAConfigTakesModelsFromTheRunEvent(t *testing.T) {
	run := tt.Run("toy", "widgets")
	run.Run.Primaries[0].Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "self.ok", 4*s, 3, 5
	run.Run.Models = []trace.Model{{Kind: gadgetKind, Ready: "self.ok", Dependency: &trace.Dependency{Track: s}}}
	run.Run.StatusKinds = []trace.Kind{gadgetKind}
	var b tt.Builder
	got, err := check.SpecFor(nil, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].Kind != gadgetKind || got.Models[0].Dependency.Track != s || len(got.StatusKinds) != 1 {
		t.Fatalf("spec = %+v", got)
	}
	if ok, err := got.Models[0].Ready(map[string]any{"ok": true}); !ok || err != nil {
		t.Fatalf("model Ready = %v, %v", ok, err)
	}
	run.Run.Models[0].Ready = "self.("
	b = tt.Builder{}
	if _, err := check.SpecFor(nil, b.Add(run).Index()); err == nil {
		t.Fatal("SpecFor accepts a model ready that does not compile")
	}
}

func TestJudgeNotesAModelFaultThatHeldNoObject(t *testing.T) {
	model := trace.Caller("model:gadgets.toy")
	stuck := tt.ReadyCondition("a", 1, false)
	stuck["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["reason"] = "Stuck"
	held := map[string]any{"metadata": map[string]any{"finalizers": []any{"other", trace.SlowDeleteFinalizer}}}
	for name, c := range map[string]struct {
		what   string
		writer trace.Caller
		body   map[string]any
		noted  bool
	}{
		"nothing written":          {"stuck", "", nil, true},
		"a stuck status":           {"stuck", model, stuck, false},
		"another's stuck status":   {"stuck", ctl, stuck, true},
		"a not ready status":       {"stuck", model, tt.ReadyCondition("a", 1, false), true},
		"the slowDelete finalizer": {"slowDelete", model, held, false},
		"a stuck for slowDelete":   {"slowDelete", model, stuck, true},
	} {
		b := createStep()
		b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), tt.ModelFault(1, 0, false, c.what, string(model)))
		if c.writer != "" {
			b.At(2500*ms).Add(tt.Call(c.writer, "patch", gadgetA, 200, "9"), tt.Version(gadgetA, "ug", "9", c.body))
		}
		b.At(3 * s).Add(tt.ModelFault(1, 0, true, c.what, string(model)))
		notes := check.Judge(b.Index(), spec).Notes
		want := "Step 1, fault 0 (" + c.what + ") held no object."
		if got := slices.Contains(notes, want); got != c.noted {
			t.Errorf("%s: notes = %q, want %q: %v", name, notes, want, c.noted)
		}
	}
}

func TestJudgeNotesAHoldWatchOnlyWithoutAWatchOfItsKind(t *testing.T) {
	hold := trace.Ref{System: "kube", Resource: "secrets"}
	for name, c := range map[string]struct {
		watch trace.Ref
		noted bool
	}{
		"a watch of the kind":     {trace.Ref{System: "kube", Resource: "secrets", Namespace: "ns"}, false},
		"a watch of another kind": {trace.Ref{System: "kube", Resource: "configmaps"}, true},
	} {
		b := createStep()
		watch := tt.Call(ctl, "watch", c.watch, 200, "")
		watch.Call.Watch, watch.Call.Stream = true, 7
		b.At(1500 * ms).Add(watch)
		b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), tt.HoldFault(1, 0, false, hold))
		b.At(2200 * ms).Add(tt.HoldFault(1, 0, true, hold))
		notes := check.Judge(b.Index(), spec).Notes
		if got := slices.Contains(notes, "Step 1, fault 0 (holdWatch) held no watch."); got != c.noted {
			t.Errorf("%s: notes = %q, want the note: %v", name, notes, c.noted)
		}
		if slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "picked no request") }) {
			t.Errorf("%s: notes = %q, want no picked-no-request note", name, notes)
		}
	}
}
