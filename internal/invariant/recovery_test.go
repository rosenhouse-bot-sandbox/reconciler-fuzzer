package invariant_test

import (
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

func TestOwedIsAsLongAfterTheFaultsAsTheyLastedAndTSettleMore(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where the target owes nothing.
		want time.Duration
	}{
		{name: "no fault", run: newRun(), at: 10 * time.Second},
		{name: "a fault still active", run: newRun().fault(time.Second, 20*time.Second), at: 10 * time.Second},
		{name: "a fault that stopped", run: newRun().fault(time.Second, 4*time.Second), at: 5 * time.Second,
			want: 12 * time.Second},
		{name: "a fault that had not stopped by then", run: newRun().fault(time.Second, 4*time.Second), at: 3 * time.Second},
		{name: "a fault that stopped while another stayed active",
			run: newRun().fault(time.Second, 20*time.Second).fault(8*time.Second, 9*time.Second),
			at:  10 * time.Second, want: 15 * time.Second},
		{name: "overlapping faults",
			run: newRun().fault(2*time.Second, 4*time.Second).fault(time.Second, 3*time.Second).
				fault(3*time.Second, 4500*time.Millisecond).fault(2500*time.Millisecond, 3500*time.Millisecond),
			at: 5 * time.Second, want: 13 * time.Second},
		{name: "faults either side of a convergence",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged).fault(8*time.Second, 9*time.Second),
			at: 10 * time.Second, want: 15 * time.Second},
		{name: "a fault the target converged after",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 10 * time.Second},
		{name: "a fault the target converged during",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 8*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 10 * time.Second, want: 15 * time.Second},
		{name: "a convergence at the instant asked about",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 6 * time.Second},
		{name: "a convergence after the instant asked about",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 4*time.Second).
				checkpoint(6*time.Second, invariant.Converged),
			at: 5 * time.Second, want: 12 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			owes(t, test.run.through(30*time.Second), test.at, test.want)
		})
	}
}

// An exit a fault excused owes the target T_settle past its return from the
// restart, where it returned within T_settle. An exit that only another exit
// could excuse owes nothing.
func TestOwedRunsPastTheReturnFromAnExitAFaultExcused(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where the target owes nothing.
		want time.Duration
	}{
		{name: "an exit while a fault was active", run: newRun().fault(time.Second, 4*time.Second).exit(3*time.Second, 13*time.Second),
			at: 14 * time.Second, want: 18 * time.Second},
		{name: "an exit the target came back from", run: newRun().fault(time.Second, 4*time.Second).exit(3*time.Second, 13*time.Second).
			running(16500 * time.Millisecond),
			at: 17 * time.Second, want: 21500 * time.Millisecond},
		{name: "an exit the target came back from only T_settle later", run: newRun().fault(time.Second, 4*time.Second).
			exit(3*time.Second, 13*time.Second).running(18 * time.Second),
			at: 19 * time.Second, want: 18 * time.Second},
		{name: "an exit while recovery was owed", run: newRun().fault(time.Second, time.Second).exit(1100*time.Millisecond, 11100*time.Millisecond),
			at: 12 * time.Second, want: 16100 * time.Millisecond},
		{name: "an exit after the recovery", run: newRun().fault(time.Second, 4*time.Second).exit(20*time.Second, 30*time.Second),
			at: 31 * time.Second, want: 12 * time.Second},
		{name: "an exit with no fault", run: newRun().exit(3*time.Second, 4*time.Second), at: 5 * time.Second},
		{name: "an exit after the instant asked about", run: newRun().fault(time.Second, time.Second).exit(1100*time.Millisecond, 11100*time.Millisecond),
			at: 1050 * time.Millisecond, want: 6 * time.Second},
		{name: "an exit the target converged after",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, time.Second).exit(1100*time.Millisecond, 2*time.Second).
				checkpoint(5*time.Second, invariant.Converged),
			at: 6 * time.Second},
		{name: "an exit during an excused one's recovery",
			run: newRun().fault(time.Second, time.Second).exit(1100*time.Millisecond, 11100*time.Millisecond).exit(12*time.Second, 22*time.Second),
			at:  23 * time.Second, want: 16100 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			owes(t, test.run.through(30*time.Second), test.at, test.want)
		})
	}
}

// A crash loop under a fault that never stops would otherwise be owed time for
// good. Once no fault is active, every exit a fault excused is owed.
func TestWhileAFaultIsActiveOnlyTheFirstExitDuringEachOpIsOwed(t *testing.T) {
	crashLoop := func(faultEnd time.Duration) *run {
		return newRun().fault(time.Second, faultEnd).op(invariant.OpUpdate, 2*time.Second).exit(3*time.Second, 3*time.Second).
			running(3500*time.Millisecond).exit(4*time.Second, 14*time.Second).exit(14500*time.Millisecond, 34500*time.Millisecond)
	}
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where the target owes nothing.
		want time.Duration
	}{
		{name: "a crash loop under an active fault", run: crashLoop(time.Minute), at: 20 * time.Second, want: 8500 * time.Millisecond},
		{name: "a crash loop once the fault stopped", run: crashLoop(16 * time.Second), at: 20 * time.Second, want: 39500 * time.Millisecond},
		{name: "the first exits during each of two ops",
			run: newRun().fault(time.Second, time.Minute).op(invariant.OpUpdate, 2*time.Second).exit(3*time.Second, 3*time.Second).
				exit(4*time.Second, 14*time.Second).op(invariant.OpSettle, 14*time.Second).exit(15*time.Second, 35*time.Second).
				exit(36*time.Second, 56*time.Second),
			at: 30 * time.Second, want: 40 * time.Second},
		{name: "an exit no fault excused before one a fault did",
			run: newRun().op(invariant.OpUpdate, 0).fault(time.Second, time.Minute).exit(500*time.Millisecond, 500*time.Millisecond).
				exit(2*time.Second, 12*time.Second).exit(12500*time.Millisecond, 32500*time.Millisecond),
			at: 20 * time.Second, want: 17 * time.Second},
		{name: "an exit after the target converged during the same op",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, time.Minute).exit(3*time.Second, 3*time.Second).
				checkpoint(10*time.Second, invariant.Converged).exit(12*time.Second, 22*time.Second),
			at: 20 * time.Second},
		{name: "an exit at the instant of an op",
			run: newRun().fault(time.Second, time.Minute).op(invariant.OpUpdate, 2*time.Second).exit(3*time.Second, 3*time.Second).
				op(invariant.OpSettle, 4*time.Second).exit(4*time.Second, 14*time.Second),
			at: 10 * time.Second, want: 19 * time.Second},
		{name: "an exit after one at the instant of an op",
			run: newRun().fault(time.Second, time.Minute).op(invariant.OpUpdate, 2*time.Second).op(invariant.OpSettle, 4*time.Second).
				exit(4*time.Second, 4*time.Second).running(4500*time.Millisecond).exit(5*time.Second, 15*time.Second),
			at: 10 * time.Second, want: 9500 * time.Millisecond},
		{name: "the first exit after a fault that stopped, while another is active",
			run: newRun().fault(time.Second, 2*time.Second).exit(4*time.Second, 14*time.Second).fault(6*time.Second, time.Minute),
			at:  10 * time.Second, want: 19 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			owes(t, test.run.through(time.Minute), test.at, test.want)
		})
	}
}

// An op that lands before the target has had its time after an exit owes it
// that time, though another exit came first during the op before.
func TestWhileAFaultIsActiveAnOpOwesAnExitItLandsSoonAfter(t *testing.T) {
	// The target restarts from its second exit at 14s.
	exitsTwice := func() *run {
		return newRun().fault(time.Second, time.Minute).op(invariant.OpUpdate, 2*time.Second).exit(3*time.Second, 3*time.Second).
			running(3500*time.Millisecond).exit(4*time.Second, 14*time.Second)
	}
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		want time.Duration
	}{
		{name: "an op at the restart", run: exitsTwice().op(invariant.OpSettle, 14*time.Second).running(17 * time.Second),
			at: 20 * time.Second, want: 22 * time.Second},
		{name: "an op before the target's return", run: exitsTwice().op(invariant.OpSettle, 16*time.Second).running(17 * time.Second),
			at: 20 * time.Second, want: 22 * time.Second},
		{name: "an op as the target's time ends", run: exitsTwice().running(15*time.Second).op(invariant.OpSettle, 20*time.Second),
			at: 25 * time.Second, want: 8500 * time.Millisecond},
		{name: "an exit during that op",
			run: exitsTwice().op(invariant.OpSettle, 14*time.Second).running(15*time.Second).exit(16*time.Second, 26*time.Second),
			at:  20 * time.Second, want: 31 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			owes(t, test.run.through(time.Minute), test.at, test.want)
		})
	}
}

// No op lands while the target waits out a restart a fault excused.
func TestPendingRestartIsARestartAFaultExcused(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where nothing is pending.
		want time.Duration
	}{
		{name: "a restart after the fault stopped", run: newRun().fault(time.Second, 10*time.Second).exit(8*time.Second, 30*time.Second),
			at: 12 * time.Second, want: 30 * time.Second},
		{name: "a restart while the fault is active", run: newRun().fault(time.Second, 10*time.Second).exit(8*time.Second, 30*time.Second),
			at: 9 * time.Second, want: 30 * time.Second},
		{name: "a restart that came", run: newRun().fault(time.Second, 10*time.Second).exit(8*time.Second, 11*time.Second),
			at: 12 * time.Second},
		{name: "a restart at the instant asked about", run: newRun().fault(time.Second, 10*time.Second).exit(8*time.Second, 12*time.Second),
			at: 12 * time.Second},
		{name: "an exit at the instant asked about", run: newRun().fault(time.Second, 20*time.Second).exit(12*time.Second, 30*time.Second),
			at: 12 * time.Second, want: 30 * time.Second},
		{name: "a restart after an exit no fault excused", run: newRun().exit(8*time.Second, 30*time.Second),
			at: 12 * time.Second},
		{name: "a restart after an exit while recovery was owed", run: newRun().fault(time.Second, 2*time.Second).exit(7*time.Second, 30*time.Second),
			at: 12 * time.Second, want: 30 * time.Second},
		{name: "a restart after an exit once recovery was not owed", run: newRun().fault(time.Second, 2*time.Second).exit(9*time.Second, 30*time.Second),
			at: 12 * time.Second},
		{name: "an exit after the instant asked about", run: newRun().fault(time.Second, 10*time.Second).exit(13*time.Second, 30*time.Second),
			at: 12 * time.Second},
		{name: "a restart after an exit only another exit excused",
			run: newRun().fault(time.Second, 2*time.Second).exit(7*time.Second, 7*time.Second).running(7500*time.Millisecond).
				exit(10*time.Second, 30*time.Second),
			at: 12 * time.Second},
		{name: "a restart still pending where the target converged",
			run: newRun().op(invariant.OpCreate, 0).fault(time.Second, 20*time.Second).exit(8*time.Second, 30*time.Second).
				checkpoint(9*time.Second, invariant.Converged),
			at: 12 * time.Second, want: 30 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.run.through(time.Minute).PendingRestart(at(test.at))
			if test.want == 0 && !got.IsZero() {
				t.Errorf("The target waits to restart until %v, want nothing pending.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The target waits to restart until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}

// A target can converge while an informer of its still backs off, so an object
// deleted after a fault is owed time past each fault that stopped, as long as
// the fault lasted and T_settle more.
func TestRecreateOwedGivesEachFaultThatStoppedItsOwnTime(t *testing.T) {
	convergedAt := func(r *run) *run { return r.checkpoint(10500*time.Millisecond, invariant.Converged) }
	for _, test := range []struct {
		name string
		run  *run
		// want is zero where the target owes nothing.
		want time.Duration
	}{
		{name: "a fault the run converged after", run: convergedAt(newRun().op(invariant.OpCreate, 0).fault(4*time.Second, 8*time.Second)),
			want: 17 * time.Second},
		{name: "faults the run converged after",
			run:  convergedAt(newRun().op(invariant.OpCreate, 0).fault(4*time.Second, 8*time.Second).fault(9*time.Second, 9500*time.Millisecond)),
			want: 17 * time.Second},
		{name: "faults the run did not converge after", run: newRun().fault(time.Second, 2*time.Second).fault(9*time.Second, 10*time.Second),
			want: 24 * time.Second},
		{name: "a fault still active", run: convergedAt(newRun().op(invariant.OpCreate, 0).activeFault(4 * time.Second))},
		{name: "a fault that stopped after the instant asked about", run: newRun().fault(4*time.Second, 12*time.Second)},
		{name: "no fault", run: convergedAt(newRun().op(invariant.OpCreate, 0))},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.run.through(30 * time.Second).RecreateOwed(at(11 * time.Second))
			if test.want == 0 && !got.IsZero() {
				t.Errorf("The target owes a recreate until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The target owes a recreate until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}

// owes fails the test unless the target owes recovery until want, where a
// zero want owes nothing.
func owes(t *testing.T, in invariant.Input, asked, want time.Duration) {
	t.Helper()
	got := in.Owed(at(asked))
	if want == 0 && !got.IsZero() {
		t.Errorf("The target owes recovery until %v, want nothing.", got.Sub(epoch))
	}
	if want != 0 && !got.Equal(at(want)) {
		t.Errorf("The target owes recovery until %v, want %v.", got.Sub(epoch), want)
	}
}

func TestRecoveringIsWhileAFaultIsActiveOrOwed(t *testing.T) {
	in := newRun().fault(time.Second, 4*time.Second).through(30 * time.Second)

	for _, test := range []struct {
		at   time.Duration
		want bool
	}{
		{at: 500 * time.Millisecond, want: false},
		{at: 2 * time.Second, want: true},
		{at: 11900 * time.Millisecond, want: true},
		{at: 12 * time.Second, want: false},
	} {
		if got := in.Recovering(at(test.at)); got != test.want {
			t.Errorf("Recovering at %v is %t, want %t.", test.at, got, test.want)
		}
	}
}
