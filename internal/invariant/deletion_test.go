package invariant_test

import (
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

// beingDeleted is a converged Widget that reconciler-fuzzer deletes at 10s,
// recorded under deletion at 10.1s. Its deletion deadline is 20.1s.
func beingDeleted() *run {
	return converged().
		running(5*time.Second).
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, deletedWidget("15", finalizers(cleanup)))
}

func TestG4LeavesToG3AWaitThatEndedOnACRPastItsDeletionDeadline(t *testing.T) {
	for name, in := range map[string]invariant.Input{
		"the CR is still there": beingDeleted().
			checkpoint(20150*time.Millisecond, invariant.Expired).
			through(25 * time.Second),
		"the wait ended on the deadline": beingDeleted().
			checkpoint(20100*time.Millisecond, invariant.Expired).
			through(25 * time.Second),
		"the CR went after its deadline": beingDeleted().
			remove(21*time.Second, deletedWidget("16")).
			checkpoint(22100*time.Millisecond, invariant.Expired).
			through(25 * time.Second),
		"a later wait on a CR still there": beingDeleted().
			checkpoint(20150*time.Millisecond, invariant.Expired).
			op(invariant.OpSettle, 25*time.Second).
			checkpoint(30*time.Second, invariant.Expired).
			through(32 * time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			silent(t, invariant.Convergence, in)
		})
	}
}

func TestG4JudgesAWaitG3DoesNot(t *testing.T) {
	for _, test := range []struct {
		name string
		in   invariant.Input
		want string
	}{
		{
			name: "the CR went in time, and a child kept changing",
			in: beingDeleted().
				remove(12*time.Second, deletedWidget("16")).
				record(15500*time.Millisecond, child("w-2", "17")).
				record(16500*time.Millisecond, child("w-2", "18")).
				checkpoint(17*time.Second, invariant.Expired).
				through(19 * time.Second),
			want: "no CR was left to be ready, but the namespace never held still",
		},
		{
			name: "the CR went in time, and a child kept changing past its deadline",
			in: beingDeleted().
				remove(17*time.Second, deletedWidget("16")).
				record(20500*time.Millisecond, child("w-2", "17")).
				record(21500*time.Millisecond, child("w-2", "18")).
				checkpoint(22*time.Second, invariant.Expired).
				through(24 * time.Second),
			want: "no CR was left to be ready, but the namespace never held still",
		},
		{
			name: "no finalizer held the CR at its deadline",
			in: beingDeleted().
				record(12*time.Second, deletedWidget("16")).
				checkpoint(20150*time.Millisecond, invariant.Expired).
				through(25 * time.Second),
			want: "the CR w was still being deleted",
		},
		{
			name: "the CR went late, but before the wait began",
			in: beingDeleted().
				checkpoint(20150*time.Millisecond, invariant.Expired).
				remove(23*time.Second, deletedWidget("16")).
				op(invariant.OpSettle, 25*time.Second).
				record(28*time.Second, child("w-2", "17")).
				record(29*time.Second, child("w-2", "18")).
				checkpoint(30*time.Second, invariant.Expired).
				through(32 * time.Second),
			want: "no CR was left to be ready, but the namespace never held still",
		},
		{
			name: "a fault reached into the deletion",
			in: beingDeleted().
				fault(11*time.Second, 11500*time.Millisecond).
				checkpoint(20150*time.Millisecond, invariant.Expired).
				through(25 * time.Second),
			want: "the CR w was still being deleted, held by the finalizers " + cleanup,
		},
		{
			name: "a fault reached into the deletion before a later wait began",
			in: beingDeleted().
				fault(11*time.Second, 11500*time.Millisecond).
				op(invariant.OpSettle, 15*time.Second).
				checkpoint(20150*time.Millisecond, invariant.Expired).
				through(25 * time.Second),
			want: "the CR w was still being deleted, held by the finalizers " + cleanup,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			violation := fired(t, invariant.Convergence, test.in)

			requireStatement(t, violation, test.want)
		})
	}
}

// A CR created under the deleted one's name is not the one G3 judges.
func TestG4JudgesAWaitThatEndedOnACRRecreatedUnderTheSameName(t *testing.T) {
	in := beingDeleted().
		remove(12*time.Second, deletedWidget("16")).
		op(invariant.OpCreate, 13*time.Second).
		record(13100*time.Millisecond, widget("20", uid("uid-w-again"), spec(2))).
		op(invariant.OpSettle, 16*time.Second).
		checkpoint(21*time.Second, invariant.Expired).
		through(23 * time.Second)

	violation := expiredWait(t, in)

	requireStatement(t, violation, "ready never held")
}

func TestWaitOwedCoversEachDeletion(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where nothing is owed.
		want time.Duration
	}{
		{name: "no deletion", run: converged(), at: 12 * time.Second},
		{name: "a deletion recorded after the instant asked about", run: beingDeleted(), at: 10 * time.Second},
		{name: "a CR still being deleted", run: beingDeleted(), at: 12 * time.Second, want: 20100 * time.Millisecond},
		{name: "a CR that went before its deadline", run: beingDeleted().remove(13*time.Second, deletedWidget("16")),
			at: 14 * time.Second, want: 18 * time.Second},
		{name: "a CR that went at its deadline", run: beingDeleted().remove(20100*time.Millisecond, deletedWidget("16")),
			at: 25 * time.Second, want: 25100 * time.Millisecond},
		{name: "a CR that went after the instant asked about", run: beingDeleted().remove(13*time.Second, deletedWidget("16")),
			at: 12 * time.Second, want: 20100 * time.Millisecond},
		{name: "a CR that went at the instant asked about", run: beingDeleted().remove(13*time.Second, deletedWidget("16")),
			at: 13 * time.Second, want: 18 * time.Second},
		{name: "a CR that went after its deadline", run: beingDeleted().remove(21*time.Second, deletedWidget("16")),
			at: 25 * time.Second, want: 20100 * time.Millisecond},
		{name: "a CR deleted twice", run: beingDeleted().remove(13*time.Second, deletedWidget("16")).
			op(invariant.OpCreate, 15*time.Second).
			record(15100*time.Millisecond, widget("20", uid("uid-w-again"), spec(2), finalizers(cleanup))).
			op(invariant.OpDelete, 16*time.Second).
			record(16100*time.Millisecond, widget("21", uid("uid-w-again"), spec(2), finalizers(cleanup), deleting(16*time.Second))),
			at: 17 * time.Second, want: 26100 * time.Millisecond},
		{name: "a CR still being deleted, and a later one that went", run: beingDeleted().
			record(11*time.Second, object(widgetGVK, "b", "30", finalizers(cleanup))).
			record(12*time.Second, object(widgetGVK, "b", "31", finalizers(cleanup), deleting(12*time.Second))).
			remove(12500*time.Millisecond, object(widgetGVK, "b", "32")),
			at: 13 * time.Second, want: 20100 * time.Millisecond},
		{name: "a fault owed less than the deletion", run: beingDeleted().fault(11*time.Second, 11500*time.Millisecond),
			at: 12 * time.Second, want: 20100 * time.Millisecond},
		{name: "a fault owed more than the deletion", run: beingDeleted().fault(11*time.Second, 19*time.Second),
			at: 19500 * time.Millisecond, want: 32 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := test.run.through(40 * time.Second)

			got := in.WaitOwed(at(test.at))

			if test.want == 0 && !got.IsZero() {
				t.Errorf("The wait is owed until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The wait is owed until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}
