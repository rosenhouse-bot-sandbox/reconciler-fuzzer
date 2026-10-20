package check_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace/tracetest"
)

// newRun starts a run whose plan has planSteps steps.
func newRun(planSteps int) *tt.Script { return tt.NewScript(widgetA, planSteps, 1*s) }

// withChild has the harness create the CR in slot, and the controller give
// it the child configmap <slot>-0 and make it ready. owned sets the child's
// ownerReference.
func withChild(r *tt.Script, slot string, owned bool) trace.Ref {
	cr, child := tt.Ref("toy/widgets", "ns", slot), tt.Ref("configmaps", "ns", slot+"-0")
	uid := r.CreateCR(slot, cr)
	body := tt.Object(child.Name)
	if owned {
		body = tt.OwnedBy(body, uid, true)
	}
	r.Create(ctl, child, body)
	r.Ready(cr)
	return child
}

// collect has the gc model delete ref.
func collect(r *tt.Script, ref trace.Ref) *tt.Script {
	r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
	r.Delete(trace.GC, ref)
	r.At(r.Now, trace.Event{Work: &trace.Work{System: "gc", Pending: false}})
	return r
}

func judgeWith(t *testing.T, r *tt.Script, c func(*trace.Index, check.Spec) []check.Finding) []check.Finding {
	t.Helper()
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	return c(ix, spec)
}

func requireNone(t *testing.T, findings []check.Finding) {
	t.Helper()
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

func requireFinding(t *testing.T, findings []check.Finding, check string, step int, summary, kind string, details ...string) {
	t.Helper()
	f := only(t, findings)
	if f.Check != check || f.Step != step || f.Summary != summary || f.Kind != kind {
		t.Fatalf("finding = %+v, want %s step %d %q of %s", f, check, step, summary, kind)
	}
	for _, want := range details {
		if !slices.ContainsFunc(f.Details, func(d string) bool { return strings.Contains(d, want) }) {
			t.Errorf("details = %q, want one that says %q", f.Details, want)
		}
	}
}

// createThenDelete settles a step that creates slot a with its child, then
// one that deletes slot a.
func createThenDelete(owned bool, gc bool) *tt.Script {
	r := newRun(2)
	r.Begin()
	child := withChild(r, "a", owned)
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults()
	if gc {
		collect(r, child)
	}
	return r.Settle()
}

func TestCleanFailsAChildLeftByADeletedCR(t *testing.T) {
	for _, owned := range []bool{true, false} {
		findings := judgeWith(t, createThenDelete(owned, false), check.Clean)
		rule := map[bool]string{true: "rule 1", false: "rule 4"}[owned]
		requireFinding(t, findings, "Clean", 1, "left resources behind", "kube/configmaps", "configmaps ns/a-0", rule, "slot a")
	}
}

func TestCleanPassesAChildTheGCCollected(t *testing.T) {
	requireNone(t, judgeWith(t, createThenDelete(true, true), check.Clean))
}

func TestCleanJudgesOnlySettledSteps(t *testing.T) {
	r := newRun(2)
	r.Begin()
	withChild(r, "a", true)
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().At(r.Now, tt.Phase(1, trace.Settled))
	r.Tick(500*ms).Create(ctl, cm, tt.Object("c"))
	r.Tick(500*ms).At(r.Now, tt.Phase(1, trace.Confirmed))
	requireNone(t, judgeWith(t, r, check.Clean))
}

func TestCleanJudgesOnlyStepsThatDelete(t *testing.T) {
	r := createThenDelete(true, false)
	r.Begin().Restart().EndFaults().Settle()
	findings := judgeWith(t, r, check.Clean)
	if len(findings) != 1 || findings[0].Step != 1 {
		t.Fatalf("findings = %+v, want one in step 1", findings)
	}
}

func TestCleanJudgesARule4ChildOnlyOnceEveryCRInItsSetIsGone(t *testing.T) {
	r := newRun(3)
	r.Begin()
	r.CreateCR("a", widgetA)
	r.CreateCR("b", widgetB)
	r.Create(ctl, cm, tt.Object("c"))
	r.Ready(widgetA).Ready(widgetB).EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
	r.Begin().DeleteCR("b", widgetB).EndFaults().Settle()
	requireFinding(t, judgeWith(t, r, check.Clean), "Clean", 2, "left resources behind", "kube/configmaps", "rule 4, slots a, b")
}

func TestCleanAllowsWhatRetainKeeps(t *testing.T) {
	r := newRun(2)
	r.Spec.Retain = `self.metadata.name == "a"`
	r.Begin()
	withChild(r, "a", true)
	withChild(r, "b", true)
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).DeleteCR("b", widgetB).EndFaults().Settle()
	findings := judgeWith(t, r, check.Clean)
	requireFinding(t, findings, "Clean", 1, "left resources behind", "kube/configmaps", "configmaps ns/b-0")
	if strings.Contains(strings.Join(findings[0].Details, "\n"), "a-0") {
		t.Fatalf("details = %q", findings[0].Details)
	}
}

// lastStep settles a create step, a delete-all step whose wait times out,
// and a restart step: the run's last.
func lastStep(t *testing.T, between func(r *tt.Script)) []check.Finding {
	r := newRun(1)
	r.Begin()
	r.Create(ctl, lease, tt.Object("l"))
	r.Create(ctl, tt.Ref("secrets", "ns", "webhook-x1"), tt.Generated("webhook-", "webhook-x1"))
	withChild(r, "a", true)
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().TimeOut()
	r.Begin().Restart()
	between(r)
	r.EndFaults().Settle()
	return judgeWith(t, r, check.Clean)
}

func TestCleanFailsEverySurvivorAtTheLastStep(t *testing.T) {
	requireFinding(t, lastStep(t, func(*tt.Script) {}), "Clean", 2, "left resources behind", "kube/configmaps", "configmaps ns/a-0 (rule 1, slot a)")
}

func TestCleanJudgesSharedResourcesOnlyForGrowth(t *testing.T) {
	findings := lastStep(t, func(r *tt.Script) {
		r.Create(ctl, tt.Ref("secrets", "ns", "webhook-x2"), tt.Generated("webhook-", "webhook-x2"))
	})
	if len(findings) != 2 || findings[1].Kind != "kube/secrets" ||
		!strings.Contains(strings.Join(findings[1].Details, "\n"), "secrets ns/webhook-x2 (shared)") {
		t.Fatalf("findings = %+v, want configmaps and the second webhook secret", findings)
	}
}

func TestCleanFailsAResourceMadeWhileNoCRLives(t *testing.T) {
	r := createThenDelete(true, true)
	r.Spec.PlanSteps = 1
	r.Begin().Restart()
	r.Create(ctl, tt.Ref("configmaps", "ns", "a-0"), tt.Object("a-0"))
	r.EndFaults().Settle()
	requireFinding(t, judgeWith(t, r, check.Clean), "Clean", 2, "left resources behind", "kube/configmaps", "configmaps ns/a-0 (shared)")
}

func TestCleanReportsALeakOnlyAtTheFirstStepThatFindsIt(t *testing.T) {
	r := createThenDelete(true, false)
	r.Spec.PlanSteps = 1
	r.Begin().Restart().EndFaults().Settle()
	requireFinding(t, judgeWith(t, r, check.Clean), "Clean", 1, "left resources behind", "kube/configmaps", "configmaps ns/a-0")
}

func TestCleanIgnoresWhatTheControllerDidNotCreate(t *testing.T) {
	r := newRun(1)
	r.Begin()
	child := withChild(r, "a", true)
	r.EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults()
	collect(r, child).Settle()
	r.Begin().Restart()
	r.Create(trace.Harness, tt.Ref("configmaps", "ns", "fixture"), tt.Object("fixture"))
	r.At(r.Now, tt.Version(tt.Ref("configmaps", "ns", "kube-root-ca.crt"), "u-root", "r1", tt.Object("kube-root-ca.crt")))
	r.Create(ctl, tt.Ref("toy/widgets", "ns", "spawned"), tt.CR("spawned", true))
	r.EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Clean))
}

func TestCleanGivesOneFindingPerKind(t *testing.T) {
	r := newRun(2)
	r.Begin()
	uid := r.CreateCR("a", widgetA)
	request := tt.Ref("cert-manager.io/certificaterequests", "ns", "a-1")
	r.Create(ctl, request, tt.OwnedBy(tt.Object("a-1"), uid, true))
	r.Create(ctl, tt.Ref("configmaps", "ns", "a-0"), tt.Object("a-0"))
	r.Create(ctl, tt.Ref("configmaps", "other", "a-0"), tt.Object("a-0"))
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
	findings := judgeWith(t, r, check.Clean)
	if len(findings) != 2 || findings[0].Kind != "kube/cert-manager.io/certificaterequests" || findings[1].Kind != "kube/configmaps" ||
		len(findings[1].Details) != 2 {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestCleanIgnoresIgnoredKinds(t *testing.T) {
	r := newRun(1)
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Create(ctl, tt.Ref("events", "ns", "a.1"), tt.Object("a.1"))
	r.Create(ctl, lease, tt.Object("l"))
	r.Ready(widgetA).EndFaults().Settle()
	r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
	r.Begin().Restart().EndFaults().Settle()
	requireNone(t, judgeWith(t, r, check.Clean))
}
