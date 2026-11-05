package check_test

import (
	"encoding/json"
	"fmt"
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
func cleanRun() *tt.Builder { return restartRun(tt.Exit("signal: terminated", true, "bye")) }

// restartRun is cleanRun with exit as the end of the restart's stop.
func restartRun(exit trace.Event) *tt.Builder {
	b := createStep()
	b.At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed)).
		At(5500 * ms).Add(tt.Phase(1, trace.FaultPeriod)).
		At(5600 * ms).Add(exit).
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

// Under B16, launch.rbac lacks widgets/status, so the widget never gets a
// status. The trace predates recorded 403 messages, so it has no RBAC note.
func TestRecordedB16RunDidNotSettle(t *testing.T) {
	r := judgeRecorded(t, "testdata/b16.jsonl")
	if f := only(t, r.Findings); f.Summary != "did not settle (clause 1)" || f.Step != 0 ||
		!slices.ContainsFunc(f.Details, func(d string) bool {
			return strings.HasSuffix(d, "/widget is not ready; it has generation 1 and no status")
		}) {
		t.Fatalf("finding = %+v", f)
	}
	want := []string{"primary.ready held for no CR version; the last of widgets.toy.reconciler-fuzzer rf-d1ebbd7f/widget has generation 1 and no status"}
	if !slices.Equal(r.Notes, want) {
		t.Errorf("notes = %q, want %q", r.Notes, want)
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

func TestJudgeNotesAFailedActionAndWhetherItEndedThePlan(t *testing.T) {
	b := cleanRun()
	unmatched := tt.Action(1, "damage", widgetA)
	unmatched.Action.Error = "no instance matches"
	taken := tt.Action(1, "create", widgetA)
	taken.Action.Index, taken.Action.Error, taken.Action.EndedPlan = 2, "the slot's name is still taken after the create waited 1s", true
	b.At(9 * s).Add(unmatched).At(10 * s).Add(taken)
	r := check.Judge(b.Index(), spec)
	want := []string{"Step 1, action 0 (damage) failed at 9s: no instance matches", "Step 1, action 2 (create) failed at 10s: the slot's name is still taken after the create waited 1s"}
	if !slices.Equal(r.Notes, want) || !slices.Equal(r.EndedPlan, want[1:]) {
		t.Fatalf("notes = %q, ended plan = %q, want %q and its last", r.Notes, r.EndedPlan, want)
	}
}

// A ready that no CR ever meets more likely names the wrong field or value
// than finds a bug, so the report says so. The note quotes the last version
// that is neither deleted nor being deleted, as clause 1 does.
func TestJudgeNotesAReadyNoCRVersionMet(t *testing.T) {
	unready := func() *tt.Builder {
		var b tt.Builder
		b.Add(tt.Run("toy", "widgets"), tt.Start(1)).
			At(0).Add(tt.Phase(0, trace.FaultPeriod)).
			At(100*ms).Add(tt.Call(trace.Harness, "create", widgetA, 201, "1"), tt.Version(widgetA, "ua", "1", ready(false))).
			At(1*s).Add(tt.EndFaults(0, 10*s, false)).
			At(1500*ms).Add(tt.Call(ctl, "update", widgetA, 200, "2"), tt.Version(widgetA, "ua", "2", ready("Ready")))
		return &b
	}
	timedOut := unready()
	timedOut.At(10 * s).Add(tt.Phase(0, trace.TimedOut))
	finalized := unready()
	finalizing := objectBody("a", true, "toy/cleanup")
	finalizing["status"] = map[string]any{"phase": "Deleting"}
	finalized.At(10*s).Add(tt.Phase(0, trace.TimedOut)).
		At(12*s).Add(tt.Call(trace.Harness, "delete", widgetA, 200, "3"), tt.Version(widgetA, "ua", "3", finalizing))
	deleted := unready()
	deleted.At(10*s).Add(tt.Phase(0, trace.TimedOut)).
		At(12*s).Add(tt.Call(trace.Harness, "delete", widgetA, 200, "3"), tt.Deleted(widgetA, "ua", "3", ready("Gone")))
	// The CR appears after the step settled, so the step did not stay
	// settled.
	var late tt.Builder
	late.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(0).Add(tt.Phase(0, trace.FaultPeriod)).
		At(500*ms).Add(tt.Call(ctl, "list", widgetA, 200, "")).
		At(1*s).Add(tt.EndFaults(0, 10*s, false)).
		At(3*s).Add(tt.Phase(0, trace.Settled)).
		At(4*s).Add(tt.Call(trace.Harness, "create", widgetA, 201, "1"), tt.Version(widgetA, "ua", "1", ready(false))).
		At(5 * s).Add(tt.Phase(0, trace.Confirmed))
	for _, c := range []struct {
		name    string
		b       *tt.Builder
		summary string
		status  string
	}{
		{"timed out", timedOut, "did not settle (clause 1)", `{"ready":"Ready"}`},
		{"finalized after the deadline", finalized, "did not settle (clause 1)", `{"ready":"Ready"}`},
		{"deleted after the deadline", deleted, "did not settle (clause 1)", `{"ready":"Ready"}`},
		{"created after the settle", &late, "did not stay settled (clause 1)", `{"ready":false}`},
	} {
		r := check.Judge(c.b.Index(), spec)
		if !slices.ContainsFunc(r.Findings, func(f check.Finding) bool { return f.Summary == c.summary }) {
			t.Errorf("%s: findings = %+v, want %q", c.name, r.Findings, c.summary)
		}
		want := []string{"primary.ready held for no CR version; the last of widgets.toy ns/a has status " + c.status}
		if notes := neverReadyNotes(r); !slices.Equal(notes, want) {
			t.Errorf("%s: notes = %q, want %q", c.name, notes, want)
		}
	}
}

func neverReadyNotes(r check.Report) []string {
	return slices.DeleteFunc(r.Notes, func(n string) bool { return !strings.Contains(n, "held for no CR version") })
}

// The note explains an unmet clause 1, so a run without one gets none, nor
// does one in which a CR was ready once.
func TestNoNeverReadyNoteOnceReadyHeldOrWithoutAnUnreadyCR(t *testing.T) {
	once := createStep()
	once.At(3500*ms).Add(tt.Phase(0, trace.Settled)).
		At(5500*ms).Add(tt.Phase(0, trace.Confirmed)).
		At(5500*ms).Add(tt.Phase(1, trace.FaultPeriod)).
		At(5600*ms).Add(tt.EndFaults(1, 20*s, false)).
		At(6*s).Add(tt.Call(ctl, "update", widgetA, 200, "3"), tt.Version(widgetA, "ua", "3", ready(false))).
		At(20 * s).Add(tt.Phase(1, trace.TimedOut))
	var gone tt.Builder
	gone.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(0).Add(tt.Phase(0, trace.FaultPeriod)).
		At(100 * ms).Add(tt.Version(widgetA, "ua", "1", ready(false))).
		At(200 * ms).Add(tt.Call(ctl, "get", widgetA, 200, "")).
		At(500 * ms).Add(tt.Deleted(widgetA, "ua", "2", ready(false))).
		At(1 * s).Add(tt.EndFaults(0, 10*s, false)).
		At(3500 * ms).Add(tt.Phase(0, trace.Settled)).
		At(5500 * ms).Add(tt.Phase(0, trace.Confirmed))
	var held tt.Builder
	held.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(0).Add(tt.Phase(0, trace.FaultPeriod)).
		At(100*ms).Add(tt.Call(trace.Harness, "create", widgetA, 201, "1"), tt.Version(widgetA, "ua", "1", ready(false))).
		At(1*s).Add(tt.EndFaults(0, 10*s, false)).
		At(2*s).Add(tt.Call(trace.Harness, "delete", widgetA, 200, "2"), tt.Version(widgetA, "ua", "2", objectBody("a", true, "toy/cleanup"))).
		At(10 * s).Add(tt.Phase(0, trace.TimedOut))
	for _, c := range []struct {
		name    string
		b       *tt.Builder
		summary string
	}{
		{"ready once", once, "did not stay settled (clause 1)"},
		{"deleted unready", &gone, ""},
		{"held in deletion", &held, "did not settle (clause 2)"},
	} {
		r := check.Judge(c.b.Index(), spec)
		if c.summary != "" && !slices.ContainsFunc(r.Findings, func(f check.Finding) bool { return f.Summary == c.summary }) {
			t.Errorf("%s: findings = %+v, want %q", c.name, r.Findings, c.summary)
		}
		if notes := neverReadyNotes(r); len(notes) != 0 {
			t.Errorf("%s: notes = %q, want no never-ready note", c.name, notes)
		}
	}
}

// forbiddenBy returns a call that the API server answered with 403 and
// message.
func forbiddenBy(caller trace.Caller, verb string, ref trace.Ref, message string) trace.Event {
	e := tt.Call(caller, verb, ref, 403, "")
	e.Call.Forbidden = message
	return e
}

func rbacNotes(r check.Report) []string {
	return slices.DeleteFunc(r.Notes, func(n string) bool { return !strings.HasPrefix(n, "kube: launch.rbac") })
}

func TestJudgeNotesARequestLaunchRBACForbids(t *testing.T) {
	b := cleanRun()
	gadgets := tt.Ref("toy/gadgets", "ns", "")
	for _, at := range []time.Duration{2 * s, 4 * s} {
		watch := forbiddenBy(ctl, "watch", gadgets, `gadgets.toy is forbidden: User "u" cannot watch resource "gadgets" in API group "toy" in the namespace "ns"`)
		watch.Call.Watch = true
		b.At(at).Add(watch)
	}
	want := []string{"kube: launch.rbac forbids the controller's watch on gadgets.toy in namespace ns, 2 times"}
	if notes := rbacNotes(check.Judge(b.Index(), spec)); !slices.Equal(notes, want) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
}

// Only RBAC's 403s to the controller count: the authorizer's, in its words,
// where a server-side apply needs create, and a refused grant of
// permissions the controller lacks. An injected fault, a namespace being
// deleted, a quota or a webhook give other messages.
func TestJudgeNotesOnlyRBACDenialsOfTheController(t *testing.T) {
	const user = `User "system:serviceaccount:ns:ctl"`
	const grant = ` is forbidden: user "system:serviceaccount:ns:ctl" (groups=["system:serviceaccounts" "system:authenticated"]) is attempting to grant RBAC permissions not currently held:` + "\n" + `{APIGroups:["toy"], Resources:["widgets"], Verbs:["get"]}`
	b := cleanRun()
	status := forbiddenBy(ctl, "update", widgetA, `widgets.toy "a" is forbidden: `+user+` cannot update resource "widgets/status" in API group "toy" in the namespace "ns"`)
	status.Call.Subresource, status.Call.Injected = "status", "delay(1s)"
	injected := forbiddenBy(ctl, "get", widgetA, "reconciler-fuzzer fault: get /apis/toy/v1/namespaces/ns/widgets/a")
	injected.Call.Injected = "error(403)"
	b.At(2*s).Add(injected, status,
		forbiddenBy(trace.Harness, "get", cm, `configmaps "c" is forbidden: User "harness" cannot get resource "configmaps" in API group "" in the namespace "ns"`),
		forbiddenBy(ctl, "create", cm, `configmaps "c" is forbidden: unable to create new content in namespace ns because it is being terminated`),
		forbiddenBy(ctl, "create", cm, `configmaps "c" is forbidden: exceeded quota: q, requested: count/configmaps=1, used: count/configmaps=1, limited: count/configmaps=1`),
		forbiddenBy(ctl, "create", cm, `admission webhook "deny.example.com" denied the request: no`),
		forbiddenBy(ctl, "patch", cm, `configmaps "c" is forbidden: `+user+` cannot create resource "configmaps" in API group "" in the namespace "ns"`),
		forbiddenBy(ctl, "list", tt.Ref("toy/widgets", "", ""), `widgets.toy is forbidden: `+user+` cannot list resource "widgets" in API group "toy" at the cluster scope: RBAC: clusterrole.rbac.authorization.k8s.io "ctl" not found`),
		forbiddenBy(ctl, "get", trace.Ref{System: "kube"}, `forbidden: `+user+` cannot get path "/livez"`),
		forbiddenBy(ctl, "update", widgetA, `widgets.toy "a" is forbidden: `+user+` cannot update resource "widgets/status" in API group "toy" in the namespace "ns"`),
		forbiddenBy(ctl, "create", tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "r"), `clusterroles.rbac.authorization.k8s.io "r"`+grant),
		forbiddenBy(ctl, "patch", tt.Ref("rbac.authorization.k8s.io/rolebindings", "ns", "rb"), `rolebindings.rbac.authorization.k8s.io "rb"`+grant),
		forbiddenBy(trace.Harness, "create", tt.Ref("rbac.authorization.k8s.io/roles", "ns", "r"), `roles.rbac.authorization.k8s.io "r"`+grant))
	want := []string{
		"kube: launch.rbac forbids the controller's update on the status subresource of widgets.toy in namespace ns, 2 times",
		"kube: launch.rbac forbids the controller's create on configmaps in namespace ns, 1 time",
		"kube: launch.rbac forbids the controller's list on widgets.toy at the cluster scope, 1 time",
		"kube: launch.rbac forbids the controller's get on path /livez, 1 time",
		"kube: launch.rbac forbids the controller's create on clusterroles.rbac.authorization.k8s.io at the cluster scope, which grants permissions the controller lacks, 1 time",
		"kube: launch.rbac forbids the controller's patch on rolebindings.rbac.authorization.k8s.io in namespace ns, which grants permissions the controller lacks, 1 time",
	}
	if notes := rbacNotes(check.Judge(b.Index(), spec)); !slices.Equal(notes, want) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
}

func TestJudgeNotesAStopThatSentSIGKILL(t *testing.T) {
	const sentSIGKILL = "the controller did not exit within launch.gracePeriod of SIGTERM, so the harness sent SIGKILL."
	killed := trace.Event{Proc: &trace.Proc{Event: trace.Exit, Exit: "signal: killed", HarnessCaused: true, GracePeriodPassed: true}}
	kill := trace.Event{Proc: &trace.Proc{Event: trace.Exit, Exit: "signal: killed", HarnessCaused: true}}
	restart := restartRun(killed).Events()
	// endsInFaults ends at the restart's exit, before step 1's faults end.
	endsInFaults := restart[:slices.IndexFunc(restart, func(e trace.Event) bool { return e.Proc != nil && e.Proc.Event == trace.Exit })+1]
	// cut ends in step 1's settle wait, as after a harness error, with the
	// run's stop.
	cut := createStep()
	cut.At(3500*ms).Add(tt.Phase(0, trace.Settled)).
		At(5500*ms).Add(tt.Phase(0, trace.Confirmed), tt.Phase(1, trace.FaultPeriod)).
		At(5600*ms).Add(killed).
		At(5700*ms).Add(tt.Start(2), tt.EndFaults(1, 20*s, false)).
		At(7 * s).Add(killed)
	run := tt.Run("toy", "widgets")
	run.Run.Primaries[0].Kind = widgets
	var early tt.Builder
	early.Add(run, tt.Start(1), killed)
	for name, c := range map[string]struct {
		events []trace.Event
		want   []string
	}{
		"a stop before step 0":                  {early.Events(), []string{"Before step 0, at 0s: " + sentSIGKILL}},
		"a restart's stop":                      {restart, []string{"Step 1, at 5.6s: " + sentSIGKILL}},
		"a restart's stop that ends the trace":  {endsInFaults, []string{"Step 1, at 5.6s: " + sentSIGKILL}},
		"the run's stop":                        {cleanRun().At(10 * s).Add(killed).Events(), []string{"After the last step, at 10s: " + sentSIGKILL}},
		"the run's stop after a timed-out step": {churn(false).At(11 * s).Add(killed).Events(), []string{"After the last step, at 11s: " + sentSIGKILL}},
		"both stops of a run cut short": {cut.Events(), []string{
			"Step 1, at 5.6s: " + sentSIGKILL, "After the last step, at 7s: " + sentSIGKILL,
			"Step 1 is not judged: the trace ends at 7s, before its deadline 20s.",
		}},
		"a kill": {cleanRun().At(10 * s).Add(kill).Events(), nil},
	} {
		if notes := check.Judge(trace.NewIndex(c.events), spec).Notes; !slices.Equal(notes, c.want) {
			t.Errorf("%s: notes = %q, want %q", name, notes, c.want)
		}
	}
}

// A step without a period ends its faults as its last action returns, so
// a fault meant for the controller's next request never acts. A period
// does not help while a kill or a stop keeps the controller down. Nor does
// it help a hold of a kind that the controller does not watch later in the
// hold's step.
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
	watchOpens := []trace.Event{watch}
	watchOpensAndCloses := []trace.Event{watch, {WatchEnd: &trace.WatchEnd{Caller: ctl, System: "kube", Stream: 7}}}
	nextStep := []trace.Event{tt.FaultPeriod(2, 0)}
	watchOpensInNextStep := []trace.Event{watch, tt.EndFaults(2, 10*s, false)}
	for name, c := range map[string]struct {
		phase                        trace.Event
		fault                        func(ended bool) trace.Event
		before, during, after, later []trace.Event
		lasted                       time.Duration
		want                         string
	}{
		"a request fault":                               {phase: tt.FaultPeriod(1, 0), fault: request, want: "Step 1, fault 1 (" + what + ") picked no request: it lasted 0s" + hint},
		"a longer request fault":                        {phase: tt.FaultPeriod(1, 0), fault: request, lasted: 1234567 * time.Microsecond, want: "Step 1, fault 1 (" + what + ") picked no request: it lasted 1.235s" + hint},
		"a model fault":                                 {phase: tt.FaultPeriod(1, 0), fault: model, lasted: s, want: "Step 1, fault 1 (stuck) held no object: it lasted 1s" + hint},
		"a holdWatch":                                   {phase: tt.FaultPeriod(1, 0), fault: hold, after: watchOpens, lasted: s, want: "Step 1, fault 1 (holdWatch) held no watch: it lasted 1s" + hint},
		"a holdWatch of a kind never watched":           {phase: tt.FaultPeriod(1, 0), fault: hold, lasted: s, want: "Step 1, fault 1 (holdWatch) held no watch."},
		"a holdWatch of a kind watched only before it":  {phase: tt.FaultPeriod(1, 0), fault: hold, before: watchOpensAndCloses, lasted: s, want: "Step 1, fault 1 (holdWatch) held no watch."},
		"a holdWatch of a kind watched in a later step": {phase: tt.FaultPeriod(1, 0), fault: hold, after: nextStep, later: watchOpensInNextStep, lasted: s, want: "Step 1, fault 1 (holdWatch) held no watch."},
		"a request fault of a step with period":         {phase: tt.FaultPeriod(1, s), fault: request, lasted: s, want: "Step 1, fault 1 (" + what + ") picked no request."},
		"a model fault of a step with period":           {phase: tt.FaultPeriod(1, s), fault: model, lasted: s, want: "Step 1, fault 1 (stuck) held no object."},
		"a holdWatch of a step with period":             {phase: tt.FaultPeriod(1, s), fault: hold, after: watchOpens, lasted: s, want: "Step 1, fault 1 (holdWatch) held no watch."},
		"a trace that lacks the period":                 {phase: tt.Phase(1, trace.FaultPeriod), fault: request, want: "Step 1, fault 1 (" + what + ") picked no request."},
		"a request fault that ends after a kill":        {phase: tt.FaultPeriod(1, 0), fault: request, during: kill, lasted: s, want: "Step 1, fault 1 (" + what + ") picked no request."},
		"a model fault that ends after a stop":          {phase: tt.FaultPeriod(1, 0), fault: model, during: stop, lasted: s, want: "Step 1, fault 1 (stuck) held no object."},
	} {
		b := createStep()
		b.At(1600*ms).Add(c.before...).At(2*s).Add(c.phase, c.fault(false)).At(2*s + c.lasted/2).Add(c.during...).At(2*s + c.lasted).Add(c.fault(true)).
			At(3*s + c.lasted).Add(c.after...).At(4*s + c.lasted).Add(c.later...)
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
	upgrade := trace.Event{Action: &trace.Action{Step: 1, Index: 2, Do: "restart", Upgrade: true}}
	kill := trace.Event{Action: &trace.Action{Step: 1, Index: 3, Do: "kill"}}
	failed := trace.Event{Action: &trace.Action{Step: 1, Index: 4, Do: "changeFixture", Error: "no instance matches"}}
	const stuck = "stuck on group=toy resource=gadgets"
	held := tt.HoldFault(1, 1, false, trace.Ref{System: "kube", Group: "toy", Resource: "widgets"})
	unrecordedPeriod := trace.Event{Phase: &trace.Phase{Step: 0, Name: trace.FaultPeriod}}
	ix := b.Add(run, unrecordedPeriod, tt.Fault(0, 0, false, "error(409)")).
		At(1*s).Add(tt.FaultPeriod(1, 2*s), tt.Fault(1, 0, false, stuck), held, tt.SlotAction(1, "create", "a", widgetA), tt.SlotAction(1, "damage", "a", cm)).
		At(2*s).Add(upgrade, kill, failed, tt.Fault(1, 0, true, stuck)).
		At(3*s).Add(tt.FaultPeriod(2, 0), tt.Fault(2, 0, false, "error(500)"), tt.FaultPeriod(3, 2*s)).
		Index()
	const replay = "reconciler-fuzzer run --config c.yaml --plan out/plan.json --out out/replay"
	if got := check.Markdown(ix, check.Report{}, replay); got != "No findings.\n" {
		t.Fatalf("Markdown of nothing = %q", got)
	}
	got := check.Markdown(ix, check.Report{
		Findings: []check.Finding{
			{Check: "Converge", Step: 1, At: 1500 * ms, Labels: []string{"backoff-bound"}, Summary: "did not settle", Details: []string{"ns/a is not ready"}},
			{Check: "Converge", Step: 2, At: 16907430335, Summary: "did not settle"},
			{Check: "Crash", Step: 3, Summary: "the controller exited 1 time"},
			{Check: "Crash", Step: -1, Summary: "the controller exited 2 times"},
			{Check: "Crash", Step: 0, Summary: "the controller exited 3 times"},
		},
		Notes: []string{"Step 0 is not judged."},
	}, replay)
	for _, want := range []string{"# Findings",
		"## Converge: did not settle\n\nStep 1, at 1.5s.\nActions: create slot a, damage configmaps ns/c, restart (upgrade), kill, changeFixture (failed).\n" +
			"Faults: stuck on group=toy resource=gadgets, holdWatch, period 2s.\nLabels: backoff-bound.\n",
		"ns/a is not ready", "Step 2 (delete all), at 16.907s.\nFaults: error(500).\n\n## Crash: the controller exited 1 time\n\nStep 3 (restart), at 0s.\n\n## Crash",
		"## Crash: the controller exited 2 times\n\nBefore step 0, at 0s.\n\n## Crash: the controller exited 3 times\n\nStep 0, at 0s.\nFaults: error(409).\n\n# Notes\n\n- Step 0 is not judged.\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("Markdown lacks %q:\n%s", want, got)
		}
	}
	const failures = "[docs/failures.md](https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/blob/main/docs/failures.md) says what each finding means.\n"
	if want := "\n# Next steps\n\nReplay the plan:\n\n```\n" + replay + "\n```\n\n" + failures; !strings.HasSuffix(got, want) {
		t.Errorf("Markdown does not end with %q:\n%s", want, got)
	}
	unreplayed := check.Markdown(ix, check.Report{Findings: []check.Finding{{Check: "Crash", Step: 3}}}, "")
	if want := "\n# Next steps\n\n" + failures; !strings.HasSuffix(unreplayed, want) || strings.Contains(unreplayed, "Replay") {
		t.Errorf("Markdown without a replay = %q, want it to end with %q", unreplayed, want)
	}
	if got := check.Markdown(ix, check.Report{Notes: []string{"n"}}, replay); got != "No findings.\n\n# Notes\n\n- n\n" {
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

// thingCR is a CR of testdata/numbers as a trace holds it, so that its
// numbers decode to float64s.
func thingCR(t *testing.T) map[string]any {
	t.Helper()
	var cr map[string]any
	if err := json.Unmarshal([]byte(`{"metadata": {"name": "abc"}, "spec": {"size": 2, "ratio": 1}}`), &cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

func TestSpecForComputesWithTheTypesTheCRDSchemaGives(t *testing.T) {
	c, err := config.Load("testdata/numbers/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	things := trace.Kind{Group: "toy", Version: "v1", Kind: "Thing", Resource: "things"}
	run := tt.Run("toy", "things")
	run.Run.Primaries[0].Kind = things
	run.Run.References = []trace.Reference{{From: things, To: things, Name: "spec.peer", Ready: "true"}}
	var b tt.Builder
	spec, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	p := spec.Primaries[0]
	for name, expr := range map[string]func(map[string]any) (bool, error){"ready": p.Ready, "retain": p.Retain, "halted": p.Halted} {
		if ok, err := expr(thingCR(t)); !ok || err != nil {
			t.Errorf("%s = %v, %v; want true", name, ok, err)
		}
	}
	if name, err := spec.Owners.Owns[0].Name(thingCR(t)); name != "ab1.5" || err != nil {
		t.Errorf("owns name = %q, %v; want ab1.5", name, err)
	}
	if namespace, err := spec.Owners.Owns[0].Namespace(thingCR(t)); namespace != "ns-1.5" || err != nil {
		t.Errorf("owns namespace = %q, %v; want ns-1.5", namespace, err)
	}
	if ready, err := spec.References[0].Ready(thingCR(t)); !ready || err != nil {
		t.Errorf("reference ready = %v, %v; want true", ready, err)
	}
}

// Without a config, the run event's schema types the CR. An older trace
// holds no schema, so each whole number in its CRs is an int.
func TestSpecForWithoutAConfigTypesTheCRByTheRunEventsSchema(t *testing.T) {
	c, err := config.Load("testdata/numbers/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for ready, recorded := range map[string]map[string]any{
		"self.spec.ratio + 0.5 == 1.5": c.Schema(schema.GroupVersionKind{Group: "toy", Version: "v1", Kind: "Thing"}),
		"self.spec.ratio + 1 == 2":     nil,
	} {
		run := tt.Run("toy", "things")
		run.Run.Primaries[0].Ready, run.Run.Primaries[0].Schema = ready, recorded
		run.Run.Stable, run.Run.Reads, run.Run.Writes = 4*s, 3, 5
		var b tt.Builder
		spec, err := check.SpecFor(nil, b.Add(run).Index())
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := spec.Primaries[0].Ready(thingCR(t)); !ok || err != nil {
			t.Errorf("ready %s: Ready = %v, %v; want true", ready, ok, err)
		}
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

// A role that fails on every resource of a kind the controller created gets
// a note, which names the first error. A Secret whose role works does not
// excuse the ConfigMaps. The harness's objects and ignored kinds, such as
// Events, do not count.
func TestJudgeNotesARoleThatFailedOnEveryResourceOfAKind(t *testing.T) {
	const noLabels, noRole, hasRole = 0, 1, 2
	body := func(name string, labels int) map[string]any {
		metadata := map[string]any{"name": name}
		switch labels {
		case noRole:
			metadata["labels"] = map[string]any{"other": name}
		case hasRole:
			metadata["labels"] = map[string]any{"role": name}
		}
		return map[string]any{"metadata": metadata}
	}
	// The first ConfigMap that fails has no labels at all.
	const note = `kube/configmaps: the role failed on every resource of this kind the controller created: evaluating "self.metadata.labels['role']": no such key: labels`
	for name, c := range map[string]struct {
		configMaps []int
		notes      []string
	}{
		"fails on every configmap": {[]int{noLabels, noRole}, []string{note}},
		"works on one configmap":   {[]int{noLabels, hasRole}, nil},
	} {
		r := newRun(1)
		r.Spec.Systems = []trace.System{{Name: "kube", Role: "self.metadata.labels['role']"}}
		r.Begin()
		r.Create(trace.Harness, widgetA, body("a", noLabels))
		r.Act("create", "a", &widgetA)
		r.Create(ctl, tt.Ref("events", "ns", "a.1"), body("a.1", noLabels))
		r.Create(ctl, tt.Ref("secrets", "ns", "a-tls"), tt.OwnedBy(body("a-tls", hasRole), r.UID(widgetA), true))
		for n, labels := range c.configMaps {
			child := fmt.Sprintf("a-%d", n)
			r.Create(ctl, tt.Ref("configmaps", "ns", child), tt.OwnedBy(body(child, labels), r.UID(widgetA), true))
		}
		r.Ready(widgetA).EndFaults().Settle()
		ix := r.Index()
		spec, err := check.SpecFor(nil, ix)
		if err != nil {
			t.Fatal(err)
		}
		if notes := check.Judge(ix, spec).Notes; !slices.Equal(notes, c.notes) {
			t.Errorf("%s: notes = %q, want %q", name, notes, c.notes)
		}
	}
}
