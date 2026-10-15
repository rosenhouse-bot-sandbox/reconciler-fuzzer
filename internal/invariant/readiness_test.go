package invariant_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// noSuchKey is the ready of a target that misspelled a field.
func noSuchKey(*unstructured.Unstructured) (bool, error) {
	return false, &target.EvalError{Predicate: "ready", Expr: "status.readyy == spec.count", Err: errors.New("no such key: readyy")}
}

// yieldsAnInt is the ready of a target that left out the comparison.
func yieldsAnInt(*unstructured.Unstructured) (bool, error) {
	return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
}

// unreadyCreate is a create whose CR the Observer recorded at 1s in the status
// given. G4 judges it at 5s.
func unreadyCreate(ready, observed int64) *run {
	return newRun().
		op(invariant.OpCreate, 0).
		running(100*time.Millisecond).
		record(time.Second, widget("10", spec(3), status(ready, observed)))
}

// expiredSettle is a settle op whose wait ran from 1s to 6s on a CR that never
// became ready. No spec changed, so G4 judges the expired wait alone.
func expiredSettle() *run {
	return newRun().
		running(100*time.Millisecond).
		record(500*time.Millisecond, widget("10", spec(3), status(0, 1))).
		op(invariant.OpSettle, time.Second).
		checkpoint(6*time.Second, invariant.Expired)
}

// expiredWait returns the G4 of the expired settle wait, beside which G4 may
// also judge the CR at a deadline.
func expiredWait(t *testing.T, in invariant.Input) invariant.Violation {
	t.Helper()
	result := evaluate(t, invariant.Convergence, in)
	for _, violation := range result.Violations {
		if strings.HasPrefix(violation.Statement, "the settle wait") {
			return violation
		}
	}
	t.Fatalf("G4 reported %v, want the expired settle wait.", statements(result))
	return invariant.Violation{}
}

func requireStatement(t *testing.T, violation invariant.Violation, want string) {
	t.Helper()
	if !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say %q.", violation.Statement, want)
	}
}

func requireEnding(t *testing.T, violation invariant.Violation, want string) {
	t.Helper()
	if !strings.HasSuffix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to end %q.", violation.Statement, want)
	}
}

func TestAnExpiredWaitSaysReadyNeverHeld(t *testing.T) {
	in := unreadyCreate(0, 1).checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "the settle wait after op 0 (create) expired with no fault active: in 5s (timeouts.settle is 5s), ready never held: it evaluated to false")
}

// A wait runs past its timeout while the target is owed time, so the statement
// gives the timeout beside how long the wait ran.
func TestAnExpiredWaitNamesItsTimeout(t *testing.T) {
	recreated := func() *run {
		return newRun().
			running(100*time.Millisecond).
			record(time.Second, widget("10", spec(3), finalizers("example.com/stuck"), status(3, 1))).
			op(invariant.OpRecreate, 2*time.Second).
			record(2050*time.Millisecond, widget("11", spec(3), finalizers("example.com/stuck"), deleting(2*time.Second), status(3, 1)))
	}
	for _, test := range []struct {
		name string
		run  *run
		want string
	}{
		{"a settle wait", unreadyCreate(0, 1).checkpoint(7*time.Second, invariant.Expired),
			"expired with no fault active: in 7s (timeouts.settle is 5s), ready never held"},
		{"a recreate's wait for its old CR", recreated().checkpoint(12*time.Second, invariant.Expired).stayed(),
			"expired with no fault active: in 10s (timeouts.delete is 10s), the CR w was still being deleted"},
		{"a recreate's settle wait", recreated().
			remove(3*time.Second, widget("12", spec(3), finalizers("example.com/stuck"), deleting(2*time.Second), status(3, 1))).
			record(3100*time.Millisecond, widget("13", uid("uid-w2"), spec(3))).
			checkpoint(8*time.Second, invariant.Expired).waitBegan(3100 * time.Millisecond),
			"expired with no fault active: in 4.9s (timeouts.settle is 5s), ready never held"},
		{"a delete's settle wait that ran past its CR's deadline", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			checkpoint(20150*time.Millisecond, invariant.Expired),
			"expired with no fault active: in 10.15s (timeouts.delete is 10s), the CR w was still being deleted"},
		{"a delete's settle wait that ended on its CR's deadline", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			checkpoint(20100*time.Millisecond, invariant.Expired),
			"expired with no fault active: in 10.1s (timeouts.delete is 10s), the CR w was still being deleted"},
		{"a delete's settle wait that ended before its CR's deadline", beingDeleted().
			checkpoint(18*time.Second, invariant.Expired),
			"expired with no fault active: in 8s (timeouts.settle is 5s)"},
		{"a delete's settle wait that ran on after its CR went", beingDeleted().
			remove(19*time.Second, deletedWidget("16")).
			checkpoint(24*time.Second, invariant.Expired),
			"expired with no fault active: in 14s (timeouts.settle is 5s)"},
		{"a delete's settle wait that ran on after its CR went late", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			remove(21*time.Second, deletedWidget("16")).
			checkpoint(22*time.Second, invariant.Expired),
			"expired with no fault active: in 12s (timeouts.settle is 5s)"},
		{"a delete's settle wait that ended as its CR went late", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			remove(20150*time.Millisecond, deletedWidget("16")).
			checkpoint(20150*time.Millisecond, invariant.Expired),
			"expired with no fault active: in 10.15s (timeouts.settle is 5s)"},
		{"a settle wait that began too late for a CR's deadline to hold it", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			op(invariant.OpSettle, 16*time.Second).
			checkpoint(21*time.Second, invariant.Expired),
			"expired with no fault active: in 5s (timeouts.settle is 5s), the CR w was still being deleted"},
		{"a settle wait whose timeout ended on a CR's deadline", beingDeleted().fault(11*time.Second, 11500*time.Millisecond).
			op(invariant.OpSettle, 15100*time.Millisecond).
			checkpoint(20100*time.Millisecond, invariant.Expired),
			"expired with no fault active: in 5s (timeouts.settle is 5s), the CR w was still being deleted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := expiredWait(t, test.run.through(25*time.Second))

			requireStatement(t, violation, test.want)
		})
	}
}

func TestAnExpiredWaitQuotesWhyReadyCouldNotBeEvaluated(t *testing.T) {
	r := unreadyCreate(0, 1)
	r.in.Target.Ready = noSuchKey
	in := r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, `in 5s (timeouts.settle is 5s), ready never held: evaluating ready "status.readyy == spec.count": no such key: readyy`)
}

func TestAnExpiredWaitSaysWhenReadyStoppedHolding(t *testing.T) {
	in := unreadyCreate(3, 1).
		record(2500*time.Millisecond, widget("11", spec(3), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "in 5s (timeouts.settle is 5s), ready held until 2.5s: it evaluated to false")
}

// A target that holds Ready while it keeps writing never gives the wait its
// quiet, and the changes are what the timeline quotes.
func TestAnExpiredWaitSaysWhatKeptTheNamespaceFromHoldingStill(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		running(100*time.Millisecond).
		record(500*time.Millisecond, widget("10", spec(1), status(1, 1))).
		record(time.Second, child("w-0", "11")).
		record(3500*time.Millisecond, widget("12", spec(1), status(1, 1))).
		record(4*time.Second, child("w-0", "13")).
		record(4500*time.Millisecond, child("w-0", "14")).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s (timeouts.settle is 5s), ready held from 500ms on, but the namespace never held still for 2s (timeouts.stable): "+
		"3 changes in the last 2s, the last to v1/ConfigMap w-0")
	if got := quoted(violation); strings.Join(got, ",") != "w@12,w-0@13,w-0@14" {
		t.Errorf("The timeline holds %v, want the 3 changes that broke the quiet.", got)
	}
	if violation.VersionsOf != "" {
		t.Errorf("The timeline says it is of %q, and it quotes two objects.", violation.VersionsOf)
	}
}

// A crash loop never converges. Its wait counts the exits since the target
// last converged, up to the wait's end, and quotes the last.
func TestAnExpiredWaitQuotesTheLastExitSinceTheTargetConverged(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		exit(2*time.Second, 2*time.Second).
		checkpoint(3*time.Second, invariant.Converged).
		op(invariant.OpUpdate, 4*time.Second).
		record(4*time.Second, widget("11", spec(2), generation(2), status(1, 1))).
		exit(5*time.Second, 5*time.Second).
		exit(6*time.Second, 16*time.Second).
		checkpoint(9*time.Second, invariant.Expired).
		exit(10*time.Second, 10*time.Second).
		through(12 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "; the target exited 2 times since it last converged, last with the exit at 6s")
}

// A wait does not converge while the target waits to restart, nor within
// stable of a restart, whatever the Observer saw.
func TestAnExpiredWaitSaysARestartKeptItFromConverging(t *testing.T) {
	for _, test := range []struct {
		name          string
		ready         int64
		exit, restart time.Duration
		want          string
	}{
		{"waiting to restart", 1, 5 * time.Second, 15 * time.Second,
			"in 5s (timeouts.settle is 5s), ready held from 0s on, but the target was waiting to restart; the target exited 1 time"},
		{"waiting to restart, where ready never held", 0, 5 * time.Second, 15 * time.Second,
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false, but the target was waiting to restart; the target exited 1 time"},
		{"restarted in the last stable", 1, 5 * time.Second, 8 * time.Second,
			"in 5s (timeouts.settle is 5s), ready held from 0s on, but the target restarted in the last 2s (timeouts.stable); the target exited 1 time"},
		{"restarted in the last stable, where ready never held", 0, 5 * time.Second, 8 * time.Second,
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false, but the target restarted in the last 2s (timeouts.stable); the target exited 1 time"},
		{"restarted before the last stable", 1, 5 * time.Second, 6 * time.Second,
			"in 5s (timeouts.settle is 5s), ready held from 0s on, and nothing changed in the last 2s (timeouts.stable); the target exited 1 time"},
		{"exited after the wait", 1, 10 * time.Second, 20 * time.Second,
			"in 5s (timeouts.settle is 5s), ready held from 0s on, and nothing changed in the last 2s (timeouts.stable)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := newRun().
				record(time.Second, widget("10", spec(1), status(test.ready, 1))).
				running(1100*time.Millisecond).
				op(invariant.OpSettle, 4*time.Second).
				exit(test.exit, test.restart).
				running(test.restart+500*time.Millisecond).
				checkpoint(9*time.Second, invariant.Expired).
				through(12 * time.Second)

			violation := expiredWait(t, in)

			requireStatement(t, violation, test.want)
		})
	}
}

// Only a request shows reconciler-fuzzer that the process it started last runs,
// so a wait does not converge before one, nor within stable of the first.
func TestAnExpiredWaitSaysTheTargetHadNotShownItRuns(t *testing.T) {
	readyCR := func() *run { return newRun().record(time.Second, widget("10", spec(1), status(1, 1))) }
	unreadySettle := func() *run {
		return newRun().record(time.Second, widget("10", spec(1), status(0, 1))).op(invariant.OpSettle, 3*time.Second)
	}
	restarted := func() *run {
		return readyCR().running(1100*time.Millisecond).op(invariant.OpRestart, 3*time.Second).op(invariant.OpSettle, 3*time.Second)
	}
	const sinceTheRestart = "in 5s (timeouts.settle is 5s), ready held from 0s on, but the target had requested no resource outside leader election since op 0 (restart)"
	const wonNoLease = "in 5s (timeouts.settle is 5s), ready held from 0s on, but the target had won no lease since op 0 (restart)"
	for _, test := range []struct {
		name string
		run  *run
		want string
	}{
		{"after a restart op", restarted(), sinceTheRestart},
		{"after a restart op, with leader election alone", restarted().requests(3100*time.Millisecond, time.Second, 4, lease("get")), wonNoLease},
		{"after a restart op, with a watch before it won its lease", restarted().
			running(3100*time.Millisecond).
			requests(3200*time.Millisecond, time.Second, 5, lease("get")).
			request(8100*time.Millisecond, lease("update")), wonNoLease},
		{"until the last stable, once it won its lease", restarted().
			running(3100*time.Millisecond).
			request(3200*time.Millisecond, lease("get")).
			request(6500*time.Millisecond, lease("update")), wonNoLease + " until the last 2s (timeouts.stable)"},
		{"after a restart op, with a request after the wait alone", restarted().running(9 * time.Second), sinceTheRestart},
		{"after a restart op at the instant of a supervised restart", restarted().exit(2*time.Second, 3*time.Second),
			sinceTheRestart + "; the target exited 1 time since it last converged, last with the exit at 2s"},
		{"after a supervised restart", readyCR().running(1100*time.Millisecond).op(invariant.OpSettle, 3*time.Second).exit(4*time.Second, 4*time.Second),
			"but the target had requested no resource outside leader election since the restart after its exit during op 0 (settle); " +
				"the target exited 1 time since it last converged, last with the exit at 4s"},
		{"since it first started", readyCR().op(invariant.OpSettle, 3*time.Second),
			"in 5s (timeouts.settle is 5s), ready held from 0s on, but the target had requested no resource outside leader election since it started"},
		{"since it first started, where ready never held", unreadySettle(),
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false, but the target had requested no resource outside leader election since it started"},
		{"since it first started until the last stable, where ready never held", unreadySettle().running(6500 * time.Millisecond),
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false, but the target had requested no resource outside leader election since it started until the last 2s (timeouts.stable)"},
		{"where ready never held", readyCR().running(1100*time.Millisecond).op(invariant.OpRestart, 3*time.Second).
			op(invariant.OpUpdate, 3*time.Second).record(3100*time.Millisecond, widget("11", spec(2), generation(2), status(1, 1))),
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false, but the target had requested no resource outside leader election since op 0 (restart)"},
		{"where ready stopped holding", restarted().record(4*time.Second, widget("11", spec(1), status(0, 1))),
			"in 5s (timeouts.settle is 5s), ready held until 1s: it evaluated to false, but the target had requested no resource outside leader election since op 0 (restart)"},
		{"where no CR was left", readyCR().running(1100*time.Millisecond).op(invariant.OpRestart, 3*time.Second).
			op(invariant.OpDelete, 3*time.Second).remove(3100*time.Millisecond, widget("11", spec(1), status(1, 1))),
			"in 5s (timeouts.settle is 5s), no CR was left to be ready, but the target had requested no resource outside leader election since op 0 (restart)"},
		{"until the last stable", restarted().running(6500 * time.Millisecond), sinceTheRestart + " until the last 2s (timeouts.stable)"},
		{"until the last stable began", restarted().running(6 * time.Second), "in 5s (timeouts.settle is 5s), ready held from 0s on, and nothing changed in the last 2s (timeouts.stable)"},
		// The Runner says why where the wait ends, before it can see later requests.
		{"with a lease got after the wait alone", restarted().running(6*time.Second).request(9*time.Second, lease("get")),
			"in 5s (timeouts.settle is 5s), ready held from 0s on, and nothing changed in the last 2s (timeouts.stable)"},
		{"not back, with a lease got after the wait", restarted().request(9*time.Second, lease("get")), sinceTheRestart},
		{"until the last stable, with a lease got after the wait", restarted().running(6500*time.Millisecond).request(9*time.Second, lease("get")),
			sinceTheRestart + " until the last 2s (timeouts.stable)"},
		// A recreate's wait lasts T_delete, which can be shorter than stable.
		{"in a wait shorter than stable", readyCR().running(1100*time.Millisecond).op(invariant.OpRestart, 7*time.Second).op(invariant.OpRecreate, 7*time.Second),
			"but the target had requested no resource outside leader election since op 0 (restart)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := test.run.checkpoint(8*time.Second, invariant.Expired).through(10 * time.Second)

			violation := expiredWait(t, in)

			requireEnding(t, violation, test.want)
		})
	}
}

// The op at the instant a wait ended came after the wait.
func TestAnExpiredWaitIgnoresARestartAtItsEnd(t *testing.T) {
	in := newRun().
		record(time.Second, widget("10", spec(1), status(1, 1))).
		running(1100*time.Millisecond).
		op(invariant.OpSettle, 3*time.Second).
		checkpoint(8*time.Second, invariant.Expired).
		op(invariant.OpRestart, 8*time.Second).
		through(10 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "and nothing changed in the last 2s (timeouts.stable)")
}

// Ready holds on the CR as it was when the wait began, which the Observer
// recorded before then.
func TestAnExpiredWaitReadsTheCRAsTheWaitFoundIt(t *testing.T) {
	in := newRun().
		record(time.Second, widget("10", spec(1), status(1, 1))).
		running(1100*time.Millisecond).
		op(invariant.OpSettle, 10*time.Second).
		record(14*time.Second, child("w-0", "11")).
		checkpoint(15*time.Second, invariant.Expired).
		through(18 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s (timeouts.settle is 5s), ready held from 0s on, but the namespace never held still for 2s (timeouts.stable): 1 change")
}

// A wait can begin before the Observer sees the op's write, and the CR it
// found then is the one the op replaced.
func TestAnExpiredWaitJudgesTheCRFromTheWriteOn(t *testing.T) {
	for name, r := range map[string]*run{
		"an update": newRun().
			running(100*time.Millisecond).
			record(time.Second, widget("10", spec(3), status(3, 1))).
			op(invariant.OpUpdate, 2*time.Second).
			record(2001*time.Millisecond, widget("11", spec(4), generation(2), status(3, 1))),
		"an update, the CR recorded as it was applied": newRun().
			running(100*time.Millisecond).
			record(2*time.Second, widget("10", spec(3), status(3, 1))).
			op(invariant.OpUpdate, 2*time.Second).
			record(2001*time.Millisecond, widget("11", spec(4), generation(2), status(3, 1))),
	} {
		t.Run(name, func(t *testing.T) {
			violation := expiredWait(t, r.checkpoint(7*time.Second, invariant.Expired).through(9*time.Second))

			requireEnding(t, violation, "in 5s (timeouts.settle is 5s), ready never held: it evaluated to false")
		})
	}
}

// A settle wait waits for a CR under deletion to go, whether or not Ready
// holds on it, so a wait that ends on one names what holds it.
func TestAnExpiredWaitSaysTheCRWasStillBeingDeleted(t *testing.T) {
	stuck := []option{spec(3), finalizers("example.com/stuck", "toy"), deleting(2 * time.Second)}
	deleted := func() *run {
		return newRun().
			running(100*time.Millisecond).
			record(time.Second, widget("10", spec(3), finalizers("example.com/stuck"), status(3, 1))).
			op(invariant.OpDelete, 2*time.Second)
	}
	const stillDeleting = "in 5s (timeouts.settle is 5s), the CR w was still being deleted, held by the finalizers example.com/stuck, toy"
	for _, c := range []struct {
		name string
		run  *run
		want string
	}{
		{"ready never held", deleted().record(2001*time.Millisecond, widget("11", append(stuck, generation(2), status(3, 1))...)), stillDeleting},
		{
			"ready held and stopped",
			deleted().
				record(2001*time.Millisecond, widget("11", append(stuck, status(3, 1))...)).
				record(3*time.Second, widget("12", append(stuck, status(2, 1))...)),
			stillDeleting,
		},
		{
			"ready held while a child churned",
			deleted().
				record(2001*time.Millisecond, widget("11", append(stuck, status(3, 1))...)).
				record(5500*time.Millisecond, child("w-0", "12")).
				record(6500*time.Millisecond, child("w-0", "13")),
			stillDeleting,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			violation := expiredWait(t, c.run.checkpoint(7*time.Second, invariant.Expired).through(9*time.Second))

			requireEnding(t, violation, c.want)
		})
	}
}

// The Observer can record the op's write before the wait begins or after, and
// the verdict does not depend on which.
func TestAnExpiredWaitJudgesTheWriteWhereverTheWaitBegan(t *testing.T) {
	for _, c := range []struct {
		name  string
		began time.Duration
		want  string
	}{
		{"the write recorded after the wait began", 2 * time.Second, "in 5s (timeouts.settle is 5s), ready held until 1s: it evaluated to false"},
		{"the write recorded before it", 2010 * time.Millisecond, "in 4.99s (timeouts.settle is 5s), ready held until 990ms: it evaluated to false"},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newRun().
				running(100*time.Millisecond).
				record(time.Second, widget("10", spec(3), status(3, 1))).
				op(invariant.OpUpdate, 2*time.Second).
				record(2005*time.Millisecond, widget("11", spec(3), labelled("x"), status(3, 1))).
				record(3*time.Second, widget("12", spec(3), labelled("x"), status(2, 1))).
				checkpoint(7*time.Second, invariant.Expired).
				waitBegan(c.began).
				through(9 * time.Second)

			violation := expiredWait(t, in)

			requireEnding(t, violation, c.want)
		})
	}
}

// Only a version of the CR the op wrote shows the write, whatever the other
// CRs' versions show.
func TestAnExpiredWaitReadsTheWriteOnTheOpsCR(t *testing.T) {
	for _, c := range []struct {
		name string
		run  *run
		want string
	}{
		{
			"another CR recorded before the op",
			newRun().
				running(100*time.Millisecond).
				record(500*time.Millisecond, object(widgetGVK, "v", "9", generation(1), spec(3), status(3, 1))).
				op(invariant.OpCreate, time.Second).
				record(1100*time.Millisecond, widget("10", spec(3), status(3, 1))).
				record(3*time.Second, widget("11", spec(3), status(2, 1))).
				checkpoint(6*time.Second, invariant.Expired).
				waitBegan(1200 * time.Millisecond),
			"in 4.8s (timeouts.settle is 5s), ready held until 1.8s: it evaluated to false",
		},
		{
			"another CR changed before the op's write was recorded",
			newRun().
				running(100*time.Millisecond).
				record(500*time.Millisecond, object(widgetGVK, "v", "9", generation(1), spec(3), status(3, 1))).
				record(time.Second, widget("10", spec(3), status(3, 1))).
				op(invariant.OpUpdate, 2*time.Second).
				record(2001*time.Millisecond, object(widgetGVK, "v", "11", generation(1), spec(3), labelled("x"), status(3, 1))).
				record(2003*time.Millisecond, widget("12", spec(4), generation(2), status(3, 1))).
				checkpoint(7*time.Second, invariant.Expired),
			"in 5s (timeouts.settle is 5s), ready never held: it evaluated to false",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			violation := expiredWait(t, c.run.through(9*time.Second))

			requireEnding(t, violation, c.want)
		})
	}
}

// A restart writes no CR, so the CR the wait found is the target's.
func TestAnExpiredWaitSaysReadyStoppedHoldingAfterARestart(t *testing.T) {
	in := newRun().
		record(time.Second, widget("10", spec(3), status(3, 1))).
		op(invariant.OpRestart, 2*time.Second).
		running(2100*time.Millisecond).
		record(3*time.Second, widget("11", spec(3), status(0, 1))).
		checkpoint(7*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "in 5s (timeouts.settle is 5s), ready held until 1s: it evaluated to false")
}

// The wait holds Ready only where it holds on every CR, as the wait reads it.
func TestAnExpiredWaitHoldsReadyOnlyOnEveryCR(t *testing.T) {
	in := newRun().
		running(100*time.Millisecond).
		record(500*time.Millisecond, object(widgetGVK, "v", "9", spec(3), status(0, 1))).
		op(invariant.OpCreate, time.Second).
		record(1100*time.Millisecond, widget("10", spec(3), status(3, 1))).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "in 5s (timeouts.settle is 5s), ready never held: it evaluated to false")
}

func TestAnExpiredWaitQuotesTheCRReadyFailedOn(t *testing.T) {
	in := newRun().
		running(100*time.Millisecond).
		record(500*time.Millisecond, object(widgetGVK, "v", "9", generation(1), spec(3), status(3, 1))).
		op(invariant.OpCreate, time.Second).
		record(1100*time.Millisecond, widget("10", spec(3), status(0, 1))).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireEnding(t, violation, "in 5s (timeouts.settle is 5s), ready never held: it evaluated to false")
	if ready := violation.Ready; ready == nil || ready.CR != "w" || ready.Status["ready"] != int64(0) {
		t.Errorf("The violation quotes %+v, want the status of w, where ready failed.", ready)
	}
	if want := "toy.reconciler-fuzzer/v1/Widget w"; violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
}

func TestAnExpiredWaitQuotesTheFirstCRWhereReadyHeld(t *testing.T) {
	in := newRun().
		record(500*time.Millisecond, object(widgetGVK, "v", "9", generation(1), spec(3), status(3, 1))).
		op(invariant.OpCreate, time.Second).
		record(1100*time.Millisecond, widget("10", spec(3), status(3, 1))).
		running(1150*time.Millisecond).
		record(5*time.Second, child("w-0", "11")).
		checkpoint(6*time.Second, invariant.Expired).
		waitBegan(1200 * time.Millisecond).
		through(9 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "in 4.8s (timeouts.settle is 5s), ready held from 0s on, but the namespace never held still")
	if ready := violation.Ready; ready == nil || ready.CR != "v" {
		t.Errorf("The violation quotes the ready of %+v, want v's, the first CR.", ready)
	}
}

func TestAnExpiredWaitSaysWhereNothingChanged(t *testing.T) {
	in := unreadyCreate(3, 1).checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s (timeouts.settle is 5s), ready held from 1s on, and nothing changed in the last 2s (timeouts.stable)")
	if got := quoted(violation); strings.Join(got, ",") != "w@10" || violation.Ready == nil || violation.Ready.CR != widgetName {
		t.Errorf("The violation quotes the timeline %v and the ready of %+v, want the CR's.", got, violation.Ready)
	}
}

func TestAnExpiredWaitSaysNoCRWasLeft(t *testing.T) {
	in := newRun().
		record(0, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		running(500*time.Millisecond).
		op(invariant.OpDelete, time.Second).
		remove(1100*time.Millisecond, widget("12", spec(1), status(1, 1))).
		record(5*time.Second, child("w-1", "13")).
		checkpoint(6*time.Second, invariant.Expired).
		through(9 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "in 5s (timeouts.settle is 5s), no CR was left to be ready, but the namespace never held still for 2s (timeouts.stable): 1 change")
	if got := quoted(violation); strings.Join(got, ",") != "w-1@13" {
		t.Errorf("The timeline holds %v, want the change that broke the quiet.", got)
	}
}

func TestAnExpiredWaitQuotesNoRequestMadeAfterIt(t *testing.T) {
	in := expiredSettle().
		request(3*time.Second, get("w-0")).
		request(6*time.Second, get("w-1")).
		request(7*time.Second, get("w-2")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if n := len(violation.Requests); n != 3 || violation.Requests[n-1].Name != "w-1" || violation.RequestsTotal != 3 {
		t.Errorf("The violation quotes %v of %d requests, want the three made by its end.", violation.Requests, violation.RequestsTotal)
	}
}

// A ready that yields a non-bool is a configuration error wherever it is first
// evaluated.
func TestAReadyThatYieldsNoBoolIsAConfigurationError(t *testing.T) {
	for name, r := range map[string]*run{
		"at a deadline":      unreadyCreate(0, 1),
		"at an expired wait": expiredSettle(),
	} {
		t.Run(name, func(t *testing.T) {
			r.in.Target.Ready = yieldsAnInt

			result, err := invariant.Convergence(r.through(8 * time.Second))

			if !errors.Is(err, target.ErrNotBool) {
				t.Errorf("G4 returned %v and the error %v, want the error of a ready that yields no bool.", statements(result), err)
			}
		})
	}
}

// A readiness verdict quotes the predicate and the status it read.
func TestAReadinessVerdictQuotesTheReadyPredicate(t *testing.T) {
	for name, r := range map[string]*run{
		"at a deadline":      unreadyCreate(0, 1),
		"at an expired wait": expiredSettle(),
	} {
		t.Run(name, func(t *testing.T) {
			r.in.Target.Ready = noSuchKey
			r.in.Target.ReadyExpr = "status.readyy == spec.count"

			violation := fired(t, invariant.Convergence, r.through(8*time.Second))

			ready := violation.Ready
			if ready == nil {
				t.Fatal("The violation quotes no ready predicate.")
			}
			if ready.Expr != "status.readyy == spec.count" {
				t.Errorf("The violation quotes the expression %q.", ready.Expr)
			}
			if ready.CR != widgetName {
				t.Errorf("The violation names the CR %q, want %q.", ready.CR, widgetName)
			}
			if !strings.Contains(ready.Error, "no such key: readyy") {
				t.Errorf("The violation quotes the error %q, want the one evaluating it.", ready.Error)
			}
			if got := ready.Status["ready"]; got != int64(0) {
				t.Errorf("The violation quotes status.ready %v, want the CR's 0.", got)
			}
		})
	}
}

// An error loop under backoff can stay under G6's threshold, and the verdict
// it causes names the request instead.
func TestAVerdictNamesAFailingRequestTheTargetRepeated(t *testing.T) {
	for _, c := range []struct {
		name  string
		check invariant.Check
		run   *run
	}{
		{"G4 at a deadline", invariant.Convergence, unreadyCreate(0, 1)},
		{"G4 at an expired wait", invariant.Convergence, expiredSettle()},
		{"G1", invariant.BoundedReconciliation, newRun().op(invariant.OpCreate, 0).settled(time.Second, invariant.Converged)},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := c.run.
				request(1100*time.Millisecond, failedGet("w-0", http.StatusInternalServerError)).
				request(1200*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
				request(1300*time.Millisecond, failedGet("w-0", http.StatusBadRequest)).
				through(8 * time.Second)

			violation := fired(t, c.check, in)

			requireStatement(t, violation, "; the target repeated the failing request get configmaps/w-0 2 times, the last answered 400")
		})
	}
}

func TestAVerdictNamesTheSubresource(t *testing.T) {
	in := unreadyCreate(0, 1).
		requests(1100*time.Millisecond, 100*time.Millisecond, 2, failedStatusUpdate(http.StatusForbidden)).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "; the target repeated the failing request update toy.reconciler-fuzzer/widgets/w/status 2 times, the last answered 403")
}

func TestAVerdictCountsTheMainResourceAndASubresourceApart(t *testing.T) {
	onWidget := failedStatusUpdate(http.StatusForbidden)
	onWidget.Subresource = ""
	in := unreadyCreate(0, 1).
		request(1100*time.Millisecond, onWidget).
		request(1200*time.Millisecond, failedStatusUpdate(http.StatusForbidden)).
		request(1300*time.Millisecond, failedStatusUpdate(http.StatusForbidden)).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "; the target repeated the failing request update toy.reconciler-fuzzer/widgets/w/status 2 times")
}

func TestAVerdictNamesTheRequestThatFirstFailedMostOften(t *testing.T) {
	in := unreadyCreate(0, 1).
		request(1100*time.Millisecond, failedGet("w-0", http.StatusNotFound)).
		request(1200*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		request(1300*time.Millisecond, failedGet("w-0", http.StatusNotFound)).
		request(1400*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "; the target repeated the failing request get configmaps/w-0 2 times")
}

func TestAVerdictCountsARequestInEachNamespaceApart(t *testing.T) {
	elsewhere := failedGet("w-0", http.StatusNotFound)
	elsewhere.Namespace = "reconciler-fuzzer-run-0"
	in := unreadyCreate(0, 1).
		request(1100*time.Millisecond, failedGet("w-0", http.StatusNotFound)).
		request(1200*time.Millisecond, elsewhere).
		request(1300*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		request(1400*time.Millisecond, failedGet("w-1", http.StatusNotFound)).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	requireStatement(t, violation, "; the target repeated the failing request get configmaps/w-1 2 times")
}

func TestAVerdictNamesNoRequestTheTargetDidNotRepeat(t *testing.T) {
	refused := failedGet("w-0", http.StatusInternalServerError)
	refused.Fault = "error 500"
	for _, c := range []struct {
		name string
		run  *run
	}{
		{"one failure", unreadyCreate(0, 1).request(1100*time.Millisecond, failedGet("w-0", http.StatusNotFound))},
		{"successes", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, get("w-0"))},
		{"lost races", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, conflicted("update", "w-0"))},
		{"what a fault refused", unreadyCreate(0, 1).requests(time.Second, 100*time.Millisecond, 3, refused)},
		{"failures before the window", newRun().
			requests(0, 100*time.Millisecond, 3, failedGet("w-0", http.StatusNotFound)).
			op(invariant.OpCreate, time.Second).
			record(1100*time.Millisecond, widget("10", spec(3), status(0, 1)))},
		{"failures after the window", unreadyCreate(0, 1).requests(5100*time.Millisecond, 100*time.Millisecond, 3, failedGet("w-0", http.StatusNotFound))},
	} {
		t.Run(c.name, func(t *testing.T) {
			violation := fired(t, invariant.Convergence, c.run.through(9*time.Second))

			if strings.Contains(violation.Statement, "repeated") {
				t.Errorf("The statement is %q, and the target repeated no failing request.", violation.Statement)
			}
		})
	}
}
