package invariant_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

func TestG4PassesWhenTheCRIsReadyWithinTheSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(3*time.Second, invariant.Converged).
		through(8 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4FiresWhenTheReadyPredicateStillFails(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ID != "G4" {
		t.Errorf("The violation is %q, want G4.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "op 0") {
		t.Errorf("The statement is %q, want it to name the op that changed the spec.", violation.Statement)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the CR's timeline.", violation.Versions)
	}
}

func TestG4FiresOnASettleWaitThatExpiredWithNoFault(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "expired") {
		t.Errorf("The statement is %q, want it to name the expired settle wait.", violation.Statement)
	}
}

func TestG4IgnoresASettleWaitAFaultReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 6*time.Second).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4WaitsForTheWholeSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(4 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4IgnoresAWindowALaterSpecChangeCutShort(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		op(invariant.OpUpdate, 3*time.Second).
		record(3100*time.Millisecond, widget("11", spec(5), generation(2), status(0, 1))).
		record(7*time.Second, widget("12", spec(5), generation(2), status(5, 2))).
		through(9 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4RequiresConvergenceAfterAFixtureChanges(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*run) *run
		want  string
	}{
		{"an update", func(r *run) *run { return r.op(invariant.OpUpdateFixture, 10*time.Second) }, "op 1 (updateFixture)"},
		{"a restore", func(r *run) *run { return r.restoring(invariant.OpSettle, 10*time.Second) },
			"op 1 (settle), where reconciler-fuzzer restored a fixture"},
	} {
		t.Run(change.name, func(t *testing.T) {
			in := change.apply(newRun().
				op(invariant.OpCreate, 0).
				record(time.Second, widget("10", spec(1), status(1, 1)))).
				record(11*time.Second, widget("11", spec(1), status(0, 1))).
				through(16 * time.Second)

			violation := fired(t, invariant.Convergence, in)

			if want := "the CR w was not ready 5s after " + change.want; violation.Statement != want {
				t.Errorf("The statement is %q, want %q.", violation.Statement, want)
			}
		})
	}
}

// A dependency that is gone may leave the CR rightly not ready, so a change to
// a fixture hands the window to the next.
func TestG4IgnoresAWindowAFixtureOpCutShort(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*run) *run
	}{
		{"a delete never restored", func(r *run) *run { return r.op(invariant.OpDeleteFixture, time.Second) }},
		{"an update", func(r *run) *run { return r.op(invariant.OpUpdateFixture, time.Second) }},
		{"a restore", func(r *run) *run { return r.restoring(invariant.OpSettle, time.Second) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			in := change.apply(newRun().
				op(invariant.OpCreate, 0).
				record(500*time.Millisecond, widget("10", spec(1), status(0, 1)))).
				record(5500*time.Millisecond, widget("11", spec(1), status(1, 1))).
				through(8 * time.Second)

			silent(t, invariant.Convergence, in)
		})
	}
}

// A fault that lasted 3s leaves the target 3s and T_settle to recover.
func TestG4RequiresConvergenceAfterAFaultStops(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(13 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "8s after the fault stopped") {
		t.Errorf("The statement is %q, want it to name the fault and the 8s the target had to recover.", violation.Statement)
	}
}

// A target backs off while its requests fail, so it may wait about as long as
// it has been failing before it tries again.
func TestG4GivesTheTargetAsLongAsTheFaultLastedToRecover(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		record(11*time.Second, widget("11", spec(3), status(3, 1))).
		through(13 * time.Second)

	silent(t, invariant.Convergence, in)
}

// A target that converged has recovered from the faults before it, so they no
// longer lengthen the time it has after a later one.
func TestG4MeasuresAFaultFromTheLastConvergence(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(5*time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(7*time.Second, invariant.Converged).
		fault(20*time.Second, 21*time.Second).
		record(21500*time.Millisecond, widget("11", spec(2), status(1, 1))).
		through(28 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "6s after the fault stopped") {
		t.Errorf("The statement is %q, want the 1s the second fault lasted and T_settle.", violation.Statement)
	}
}

// A settle wait that converged shows the target recovered, so a spec change
// made while it was recovering is judged no later than that.
func TestG4StopsGivingTheTargetTimeOnceItConverged(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(500*time.Millisecond, 10*time.Second).
		record(time.Second, widget("10", spec(2), status(0, 1))).
		op(invariant.OpUpdate, 11*time.Second).
		record(11500*time.Millisecond, widget("11", spec(3), generation(2), status(3, 2))).
		checkpoint(12*time.Second, invariant.Converged).
		op(invariant.OpRestart, 20*time.Second).
		record(20100*time.Millisecond, widget("12", spec(3), generation(2), status(1, 2))).
		record(26*time.Second, widget("13", spec(3), generation(2), status(3, 2))).
		checkpoint(27*time.Second, invariant.Converged).
		through(28 * time.Second)

	silent(t, invariant.Convergence, in)
}

// A settle wait that expired shows no recovery, so it leaves the target the
// time the fault left it.
func TestG4GivesTheTargetItsTimePastAnExpiredWait(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		checkpoint(4500*time.Millisecond, invariant.Expired).
		record(11*time.Second, widget("11", spec(3), status(3, 1))).
		through(13 * time.Second)

	silent(t, invariant.Convergence, in)
}

// A target that converged only after the time the fault left it is late.
func TestG4GivesNoMoreTimeForAConvergenceThatCameLate(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		record(12500*time.Millisecond, widget("11", spec(3), status(3, 1))).
		checkpoint(14500*time.Millisecond, invariant.Converged).
		through(15 * time.Second)

	fired(t, invariant.Convergence, in)
}

func TestG4SaysHowLongItGaveTheTargetToTheMillisecond(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(time.Second, 4*time.Second+400*time.Microsecond).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(13 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "not ready 8s after the fault stopped") {
		t.Errorf("The statement is %q, want the 8s it gave the target, rounded.", violation.Statement)
	}
}

// The target still owes nothing for a spec change made while it was
// recovering from a fault, until it has had the time the fault leaves it.
func TestG4GivesASpecChangeAfterAFaultTheTimeTheFaultLeaves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(500*time.Millisecond, 4*time.Second).
		record(time.Second, widget("10", spec(2), status(0, 1))).
		op(invariant.OpUpdate, 5*time.Second).
		record(5100*time.Millisecond, widget("11", spec(3), generation(2), status(0, 1))).
		record(11*time.Second, widget("12", spec(3), generation(2), status(3, 2))).
		through(13 * time.Second)

	silent(t, invariant.Convergence, in)
}

// reconciler-fuzzer chose to restart the target, so G4 gives it T_settle past
// its return to converge on a spec change made around the restart.
func TestG4GivesARestartedTargetTSettlePastItsReturn(t *testing.T) {
	converged := func() *run {
		return newRun().
			op(invariant.OpCreate, 0).
			record(500*time.Millisecond, widget("10", spec(2), status(2, 1))).
			running(600*time.Millisecond).
			checkpoint(3*time.Second, invariant.Converged)
	}
	readyAt := func(r *run, back, ready time.Duration) invariant.Input {
		return r.
			record(4200*time.Millisecond, widget("11", spec(3), generation(2), status(2, 1))).
			running(back).
			record(ready, widget("12", spec(3), generation(2), status(3, 2))).
			checkpoint(ready+stableWindow, invariant.Converged).
			through(ready + 3*time.Second)
	}
	restartThenUpdate := func() *run {
		return converged().op(invariant.OpRestart, 4*time.Second).op(invariant.OpUpdate, 4100*time.Millisecond)
	}
	updateThenRestart := func() *run {
		return converged().op(invariant.OpUpdate, 4*time.Second).op(invariant.OpRestart, 4100*time.Millisecond)
	}

	restartAfterConverging := newRun().
		op(invariant.OpCreate, 0).
		record(500*time.Millisecond, widget("10", spec(2), status(2, 1))).
		running(600*time.Millisecond).
		checkpoint(2500*time.Millisecond, invariant.Converged).
		op(invariant.OpRestart, 4*time.Second).
		running(4500*time.Millisecond).
		checkpoint(6500*time.Millisecond, invariant.Converged).
		record(8*time.Second, widget("11", spec(2), status(1, 1))).
		record(11*time.Second, widget("12", spec(2), status(2, 1))).
		through(14 * time.Second)

	for name, in := range map[string]invariant.Input{
		"a restart before the change":                readyAt(restartThenUpdate(), 8900*time.Millisecond, 10*time.Second),
		"a restart after the change":                 readyAt(updateThenRestart(), 8900*time.Millisecond, 10*time.Second),
		"a restart after the target converged on it": restartAfterConverging,
	} {
		t.Run(name, func(t *testing.T) { silent(t, invariant.Convergence, in) })
	}
	t.Run("a target back only T_settle after the restart", func(t *testing.T) {
		violation := fired(t, invariant.Convergence, readyAt(restartThenUpdate(), 9*time.Second, 10*time.Second))

		if want := "not ready 5s after op 2 (update)"; !strings.Contains(violation.Statement, want) {
			t.Errorf("The statement is %q, want it to say %q.", violation.Statement, want)
		}
	})
}

func TestG4IgnoresADeadlineWhoseCRIsNotBackYet(t *testing.T) {
	in := newRun().
		record(0, widget("10", spec(2), status(2, 1))).
		op(invariant.OpRecreate, 10*time.Second).
		remove(11*time.Second, widget("11", spec(2), status(2, 1))).
		record(16*time.Second, widget("20", spec(2), status(2, 1), uid("uid-w2"))).
		through(22 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4QuotesTheVersionsNearestTheViolation(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(3), status(0, 1)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if len(violation.Versions) != 20 {
		t.Fatalf("The evidence holds %d versions, want the 20 a report carries.", len(violation.Versions))
	}
	if last := violation.Versions[19]; last.ResourceVersion != "34" {
		t.Errorf("The evidence ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
}

func TestG4NamesTheOpByItsIndexRatherThanItsPosition(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Ops:         []invariant.Op{{Index: 5, Type: invariant.OpUpdate, Time: at(0)}},
		Checkpoints: []invariant.Checkpoint{{Op: 5, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}

	violation := fired(t, invariant.Convergence, in)

	if !strings.Contains(violation.Statement, "op 5") {
		t.Errorf("The statement is %q, want it to name op 5.", violation.Statement)
	}
}

// A fault that lasted 0.6s excuses an expired wait until 5.6s after it stopped,
// wherever the wait began.
func TestG4ExcusesAnExpiredWaitUntilTheTargetHadTimeToRecover(t *testing.T) {
	for _, expiry := range []struct {
		at      time.Duration
		excused bool
	}{
		{at: 15600 * time.Millisecond, excused: true},
		{at: 15800 * time.Millisecond, excused: false},
	} {
		t.Run(expiry.at.String(), func(t *testing.T) {
			in := invariant.Input{
				Target:      toyTarget(),
				Ops:         []invariant.Op{{Index: 1, Type: invariant.OpUpdate, Time: at(10 * time.Second)}},
				Checkpoints: []invariant.Checkpoint{{Op: 1, Time: at(expiry.at), Settle: invariant.Expired}},
				Faults:      []invariant.FaultWindow{{Start: at(9500 * time.Millisecond), End: at(10100 * time.Millisecond)}},
				End:         at(expiry.at),
			}

			if expiry.excused {
				silent(t, invariant.Convergence, in)
			} else {
				fired(t, invariant.Convergence, in)
			}
		})
	}
}

// A target that exits on a fault's error waits out reconciler-fuzzer's backoff,
// which is not the target's to answer for. An exit no fault excused is.
func TestG4JudgesNoDeadlineAnExitAFaultExcusedFellBefore(t *testing.T) {
	for _, test := range []struct {
		name    string
		run     *run
		excused bool
	}{
		{"an exit on the fault's error", newRun().fault(10100*time.Millisecond, 10100*time.Millisecond), true},
		{"an exit with no fault", newRun(), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := test.run.
				op(invariant.OpCreate, 0).
				record(time.Second, widget("10", spec(1), status(1, 1))).
				checkpoint(3*time.Second, invariant.Converged).
				op(invariant.OpUpdate, 10*time.Second).
				record(10*time.Second, widget("11", spec(2), generation(2), status(1, 1))).
				exit(10200*time.Millisecond, 20200*time.Millisecond).
				record(21*time.Second, widget("12", spec(2), generation(2), status(2, 2))).
				checkpoint(23*time.Second, invariant.Converged).
				through(25 * time.Second)

			if test.excused {
				silent(t, invariant.Convergence, in)
			} else {
				fired(t, invariant.Convergence, in)
			}
		})
	}
}

// An excused exit hands only the window it falls in to the recovery.
func TestG4JudgesTheWindowsAnExcusedExitFallsOutside(t *testing.T) {
	for _, test := range []struct {
		name, fired string
		in          invariant.Input
	}{
		{"a window before the exit", "op 0", newRun().
			op(invariant.OpCreate, 0).
			record(time.Second, widget("10", spec(2), status(0, 1))).
			fault(20*time.Second, 20*time.Second).
			exit(20100*time.Millisecond, 30100*time.Millisecond).
			through(40 * time.Second)},
		{"a window after the target recovered", "op 1", newRun().
			op(invariant.OpCreate, 0).
			fault(time.Second, time.Second).
			exit(1100*time.Millisecond, 2*time.Second).
			record(3*time.Second, widget("10", spec(1), status(1, 1))).
			checkpoint(5*time.Second, invariant.Converged).
			op(invariant.OpUpdate, 10*time.Second).
			record(10*time.Second, widget("11", spec(2), generation(2), status(1, 1))).
			through(20 * time.Second)},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := fired(t, invariant.Convergence, test.in)

			if !strings.Contains(violation.Statement, test.fired) {
				t.Errorf("The statement is %q, want the window after %s.", violation.Statement, test.fired)
			}
		})
	}
}

// The teardown gives the target a settle wait of its own once the last fault
// stops, and an expired one is named for that.
func TestG4NamesTheWaitAfterTheLastFaultStopped(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(3*time.Second, invariant.Converged).
		fault(4*time.Second, 5*time.Second).
		through(12 * time.Second)
	in.Checkpoints = append(in.Checkpoints, invariant.Checkpoint{Op: invariant.Recovery, Time: at(11 * time.Second), Settle: invariant.Expired})

	violation := fired(t, invariant.Convergence, in)

	if want := "the settle wait after the last fault stopped expired"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
}

// The Observer runs one informer per kind, so the history can arrive out of
// order. The checks read it as a timeline.
func TestG4ReadsAHistoryTheObserverRecordedOutOfOrder(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(6*time.Second, child("w-0", "11")).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	fired(t, invariant.Convergence, in)
}

// The teardown's own delete bumps metadata.generation while
// status.observedGeneration lags, so the CR reconciler-fuzzer is deleting reads
// as not ready (DESIGN.md §5.5, step 4).
func TestG4IgnoresAWindowTheTeardownReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		teardown(4*time.Second).
		record(4100*time.Millisecond, widget("11", spec(2), status(2, 1), generation(2),
			finalizers(cleanup), deleting(4*time.Second))).
		through(6 * time.Second)

	silent(t, invariant.Convergence, in)
}

func TestG4FiresOnAWindowThatClosedBeforeTheTeardown(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		teardown(6 * time.Second).
		through(8 * time.Second)

	fired(t, invariant.Convergence, in)
}

// TestG4IgnoresACRUnderDeletion pins §6: a CR whose deletion a mid-sequence op
// requested need not be ready, because the finalizer it is waiting on is G3's
// business. Its generation moves while observedGeneration lags, so without the
// rule the delete itself reads as a convergence failure.
func TestG4IgnoresACRUnderDeletion(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		op(invariant.OpDelete, 2*time.Second).
		record(2100*time.Millisecond, widget("11", spec(2), status(2, 1), generation(2),
			finalizers(cleanup), deleting(2*time.Second))).
		teardown(20 * time.Second).
		through(22 * time.Second)

	silent(t, invariant.Convergence, in)
}

// G4 counts what the target managed at the verdict (#13).
func TestG4SaysHowManyObjectsTheTargetManaged(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil {
		t.Fatalf("The violation counts no managed objects, want the count at the verdict.")
	}
	if *violation.ManagedTotal != 0 {
		t.Errorf("The violation says the target managed %d objects, want 0.", *violation.ManagedTotal)
	}
}

// The count is the target's holding at the deadline: one child was gone by
// then and another arrived after it.
func TestG4CountsTheManagedObjectsLiveAtTheDeadline(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(3), status(0, 1)), child("w-0", "11"), child("w-1", "12")).
		remove(2*time.Second, child("w-1", "13")).
		record(6*time.Second, child("w-2", "14")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 1 {
		t.Fatalf("The violation counts %v managed objects, want the 1 live at the deadline.", managed(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child live at the deadline.", got)
	}
}

func TestG4CountsTheManagedObjectsAtAnExpiredWait(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11")).
		checkpoint(5*time.Second, invariant.Expired).
		record(6*time.Second, child("w-1", "12")).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 1 {
		t.Fatalf("The violation counts %v managed objects, want the 1 the wait expired with.", managed(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the wait expired with.", got)
	}
}

// A child that exists and is wrong is what the verdict is about, so G4 quotes
// the state of the objects the target managed beside the CR's timeline (#24).
func TestG4QuotesTheStateOfEveryManagedObject(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(0, 1)), child("w-0", "11", data("wrong"))).
		record(2*time.Second, widget("12", spec(2), status(0, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the managed object the verdict is about.", got)
	}
	if got := versionsOf(violation.Versions, widgetGVK); len(got) != 2 {
		t.Errorf("The timeline holds %v, want the CR's history.", quoted(violation))
	}
	if len(versionsOf(violation.Versions, configMapGVK)) > 0 {
		t.Errorf("The timeline holds %v, want the CR alone: the children are a table of their own.", quoted(violation))
	}
}

// The timeline and the state each have a bound of their own, so neither
// crowds the other out (#24, D39).
func TestG4BoundsTheTimelineAndTheStateApart(t *testing.T) {
	for _, c := range []struct {
		crVersions, children  int
		wantCRs, wantChildren int
	}{
		{crVersions: 41, children: 35, wantCRs: 20, wantChildren: 20},
		{crVersions: 40, children: 5, wantCRs: 20, wantChildren: 5},
		{crVersions: 5, children: 30, wantCRs: 5, wantChildren: 20},
		{crVersions: 3, children: 2, wantCRs: 3, wantChildren: 2},
	} {
		t.Run(fmt.Sprintf("%d versions of the CR and %d children", c.crVersions, c.children), func(t *testing.T) {
			r := newRun().op(invariant.OpCreate, 0)
			for i := range c.crVersions {
				r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(30), status(0, 1)))
			}
			for i := range c.children {
				r.record(3*time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
			}
			in := r.through(8 * time.Second)

			violation := fired(t, invariant.Convergence, in)

			crs := violation.Versions
			if len(crs) != c.wantCRs {
				t.Errorf("The timeline holds %d versions of the CR, want %d.", len(crs), c.wantCRs)
			}
			if violation.VersionsTotal != c.crVersions {
				t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, c.crVersions)
			}
			if latest := strconv.Itoa(9 + c.crVersions); len(crs) == 0 || crs[len(crs)-1].ResourceVersion != latest {
				t.Errorf("The timeline holds %v, want it to end at the CR's latest, %s.", quoted(violation), latest)
			}
			if got := state(violation); len(got) != c.wantChildren {
				t.Errorf("The state holds %d children, want %d.", len(got), c.wantChildren)
			}
			if violation.ManagedTotal == nil || *violation.ManagedTotal != c.children {
				t.Errorf("The violation says the target managed %v objects, want %d.", managed(violation), c.children)
			}
		})
	}
}

// A child that did not change inside the wait is still the state the wait
// expired on (#20).
func TestG4QuotesAChildThatNeverChangedDuringAnExpiredWait(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		checkpoint(2*time.Second, invariant.Converged).
		op(invariant.OpUpdate, 10*time.Second).
		record(11*time.Second, widget("12", spec(2), generation(2), status(2, 2))).
		checkpoint(15*time.Second, invariant.Expired).
		through(18 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the wait expired with.", got)
	}
}

// A target manages several kinds, and the bound takes them one kind at a time
// so that none is lost whole (#20, D39).
func TestG4QuotesEveryManagedKindItCan(t *testing.T) {
	r := newRunManaging(configMapGVK, secretGVK).op(invariant.OpCreate, 0)
	for i := range 40 {
		r.record(time.Duration(i)*10*time.Millisecond, widget(strconv.Itoa(10+i), spec(30), status(0, 1)))
	}
	for i := range 25 {
		r.record(time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	for i := range 5 {
		r.record(time.Second, secret("s-"+strconv.Itoa(i), strconv.Itoa(200+i)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	for _, gvk := range []schema.GroupVersionKind{configMapGVK, secretGVK} {
		if got := versionsOf(violation.Managed, gvk); len(got) == 0 {
			t.Errorf("The state holds no %s, and the target managed some: %v", kindOf(gvk), state(violation))
		}
	}
}

// The children quoted are the ones nearest the verdict, because a target that
// manages more than the bound has to lose some (#20, D39).
func TestG4QuotesTheChildrenNearestTheVerdict(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	r.record(time.Second, widget("10", spec(30), status(0, 1)))
	for i := range 30 {
		r.record(time.Duration(100+i)*10*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	children := state(violation)
	if len(children) == 0 {
		t.Fatalf("The state holds no children.")
	}
	if children[0] != "w-29" {
		t.Errorf("The state opens at %s, want w-29, the child nearest the verdict.", children[0])
	}
}

// An expired wait quotes the CR's history nearest it, as every readiness
// verdict does, and the children as a state beside it (#24).
func TestG4QuotesTheCRsHistoryAtAnExpiredWait(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0)
	for i := range 25 {
		r.record(time.Duration(1000+i)*time.Millisecond, widget(strconv.Itoa(10+i), spec(2), status(2, 1)))
	}
	for i := range 3 {
		r.record(time.Duration(2000+i)*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if got := versionsOf(violation.Versions, configMapGVK); len(got) > 0 {
		t.Errorf("The timeline holds %v, want the CR's history alone.", quoted(violation))
	}
	if len(violation.Versions) != invariant.MaxEvidence {
		t.Fatalf("The timeline holds %d versions, want the bound of %d.", len(violation.Versions), invariant.MaxEvidence)
	}
	if want := 25; violation.VersionsTotal != want {
		t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, want)
	}
	if last := violation.Versions[invariant.MaxEvidence-1]; last.ResourceVersion != "34" {
		t.Errorf("The timeline ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if got := state(violation); len(got) != 3 || got[0] != "w-2" {
		t.Errorf("The state holds %v, want the 3 children, the newest first.", got)
	}
}

// A report of what the run looked like at the verdict cannot quote what came
// after it (#20).
func TestG4QuotesNoVersionRecordedAfterTheVerdict(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(0, 1))).
		record(7*time.Second, widget("11", spec(2), status(2, 1))).
		through(9 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	for _, v := range violation.Versions {
		if v.Time.After(violation.At) {
			t.Errorf("The evidence quotes %s at %s, after the verdict at %s.", v.Name, v.Time, violation.At)
		}
	}
}

func TestG4JudgesEveryCR(t *testing.T) {
	for _, c := range []struct {
		name     string
		first    *unstructured.Unstructured
		notReady []string
	}{
		{"beside a ready one", widget("10", spec(1), status(1, 1)), []string{"w2"}},
		{"beside one under deletion", widget("10", spec(1), finalizers(cleanup), deleting(500*time.Millisecond)), []string{"w2"}},
		{"beside another not ready", widget("10", spec(1)), []string{"w", "w2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := newRun().withSecondWidget().
				op(invariant.OpCreate, 0).
				record(100*time.Millisecond, c.first).
				opOn(invariant.OpCreate, time.Second, secondName).
				record(1100*time.Millisecond, secondWidget("20", spec(2))).
				through(7 * time.Second)

			result := evaluate(t, invariant.Convergence, in)

			var notReady []string
			for _, statement := range statements(result) {
				name, _, _ := strings.Cut(strings.TrimPrefix(statement, "the CR "), " was not ready")
				notReady = append(notReady, name)
			}
			if !slices.Equal(notReady, c.notReady) {
				t.Errorf("G4 reported %q, want the CRs %v not ready.", statements(result), c.notReady)
			}
		})
	}
}

// Another CR's objects may keep a CR from ready, so G4 quotes every CR's.
func TestG4QuotesTheObjectsOfEveryCR(t *testing.T) {
	in := newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		opOn(invariant.OpCreate, time.Second, secondName).
		record(1100*time.Millisecond, secondWidget("20", spec(2))).
		through(7 * time.Second)

	violation := fired(t, invariant.Convergence, in)

	if !slices.Contains(state(violation), "w-0") {
		t.Errorf("G4 quotes %v, want the child of w, which may be what keeps w2 from ready.", state(violation))
	}
}
