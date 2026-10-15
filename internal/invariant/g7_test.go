package invariant_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// childDeleted is the toy converged with two children, and reconciler-fuzzer
// deleting w-0 behind its back at 10s.
func childDeleted() *run {
	return converged().
		deletedManaged(10*time.Second, "w-0").
		remove(10100*time.Millisecond, child("w-0", "15"))
}

func TestG7PassesWhenTheTargetRecreatesTheObject(t *testing.T) {
	in := childDeleted().
		record(10200*time.Millisecond, child("w-0", "16", uid("uid-w-0-again"))).
		checkpoint(12200*time.Millisecond, invariant.Converged).
		through(12200 * time.Millisecond)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want a verdict.", notes)
	}
}

func TestG7FiresOnAnObjectThatNeverCameBack(t *testing.T) {
	in := childDeleted().
		checkpoint(12100*time.Millisecond, invariant.Converged).
		waitBegan(10100 * time.Millisecond).
		through(12100 * time.Millisecond)

	violation := fired(t, invariant.SelfHealing, in)

	if violation.ID != "G7" {
		t.Errorf("The violation is %q, want G7.", violation.ID)
	}
	for _, want := range []string{
		"v1/ConfigMap w-0", "op 1 (deleteManaged)", "never came back within the 2s the run waited",
		"does not list v1/ConfigMap under notRecreated",
	} {
		if !strings.Contains(violation.Statement, want) {
			t.Errorf("The statement is %q, want it to say %q.", violation.Statement, want)
		}
	}
	if got := state(violation); managed(violation) != "1" || !slices.Equal(got, []string{"w-1"}) {
		t.Errorf("The violation quotes %s managed objects %v, want w-1 alone.", managed(violation), got)
	}
	if !violation.At.Equal(at(12100 * time.Millisecond)) {
		t.Errorf("G7 judged at %v, want where the wait after the op ended.", violation.At)
	}
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if n := len(violation.Versions); n == 0 || !violation.Versions[n-1].Deleted {
		t.Errorf("The timeline holds %v, want it to end where reconciler-fuzzer deleted w-0.", quoted(violation))
	}
}

// The wait is where the target had to have acted, not the end of the run.
func TestG7FiresOnAnObjectRecreatedOnlyAfterTheWaitEnded(t *testing.T) {
	in := childDeleted().
		checkpoint(12100*time.Millisecond, invariant.Converged).
		record(13*time.Second, child("w-0", "16", uid("uid-w-0-again"))).
		through(20 * time.Second)

	violation := fired(t, invariant.SelfHealing, in)

	if n := len(violation.Versions); n == 0 || !violation.Versions[n-1].Deleted {
		t.Errorf("The timeline holds %v, want it to end at the verdict, where w-0 was deleted.", quoted(violation))
	}
}

func TestG7ExemptsOnlyTheKindsTheTargetDoesNotRecreate(t *testing.T) {
	both := func() *run {
		r := newRunManaging(configMapGVK, secretGVK)
		r.in.Target.NotRecreated = []schema.GroupVersionKind{secretGVK}
		return r.
			op(invariant.OpCreate, 0).
			record(100*time.Millisecond, widget("11", spec(1), status(1, 1))).
			record(200*time.Millisecond, child("w-0", "12"), secret("w-0", "13")).
			checkpoint(2200*time.Millisecond, invariant.Converged)
	}
	t.Run("the kind listed", func(t *testing.T) {
		in := both().
			deletedManagedOf(10*time.Second, secretGVK, "w-0").
			remove(10100*time.Millisecond, secret("w-0", "14")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
			t.Errorf("G7 noted %v, want the target's declaration to settle it.", notes)
		}
	})
	t.Run("a kind not listed", func(t *testing.T) {
		in := both().
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "14")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		fired(t, invariant.SelfHealing, in)
	})
	t.Run("a kind of the same name in another group", func(t *testing.T) {
		r := childDeleted()
		r.in.Target.NotRecreated = []schema.GroupVersionKind{{Group: "example.com", Version: "v1", Kind: "ConfigMap"}}
		in := r.
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)

		fired(t, invariant.SelfHealing, in)
	})
}

// A wait that expired is still where the target had to have acted.
func TestG7FiresWhereTheWaitExpired(t *testing.T) {
	in := childDeleted().
		checkpoint(15*time.Second, invariant.Expired).
		through(15 * time.Second)

	fired(t, invariant.SelfHealing, in)
}

// An object that is back satisfies G7 whatever else happened.
func TestG7PassesAnObjectThatCameBack(t *testing.T) {
	for _, c := range []struct {
		name string
		in   invariant.Input
	}{
		{"after a fault", childDeleted().
			fault(10050*time.Millisecond, 11*time.Second).
			record(11500*time.Millisecond, child("w-0", "16", uid("uid-w-0-again"))).
			checkpoint(13500*time.Millisecond, invariant.Converged).
			through(13500 * time.Millisecond)},
		{"after an update", converged().
			op(invariant.OpUpdate, 10*time.Second).
			record(10050*time.Millisecond, widget("20", spec(2), generation(2), status(2, 1), finalizers(cleanup))).
			deletedManaged(10100*time.Millisecond, "w-0").
			remove(10150*time.Millisecond, child("w-0", "21")).
			record(10200*time.Millisecond, child("w-0", "22", uid("uid-w-0-again"))).
			record(10300*time.Millisecond, widget("23", spec(2), generation(2), status(2, 2), finalizers(cleanup))).
			checkpoint(12300*time.Millisecond, invariant.Converged).
			through(12300 * time.Millisecond)},
		{"after a restart", converged().
			op(invariant.OpRestart, 9*time.Second).
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			record(11*time.Second, child("w-0", "16", uid("uid-w-0-again"))).
			checkpoint(13*time.Second, invariant.Converged).
			through(13 * time.Second)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if notes := silent(t, invariant.SelfHealing, c.in).Notes; len(notes) > 0 {
				t.Errorf("G7 noted %v, want the object's return to settle it.", notes)
			}
		})
	}
}

// reconciler-fuzzer cannot tell when a restarted target is back. One still
// starting, or waiting out its predecessor's lease, recreates nothing.
func TestG7NotesAnObjectDeletedBeforeARestartedTargetWasBack(t *testing.T) {
	const wantNote = "G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between op 1 (restart) and it"
	const wonNoLease = "G7 is not evaluated for op 2 (deleteManaged): the target had won no lease between op 1 (restart) and it"
	deletedAfter := func(r *run) invariant.Input {
		return r.
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)
	}
	restarted := func() *run { return converged().op(invariant.OpRestart, 9*time.Second) }
	for _, c := range []struct {
		name string
		in   invariant.Input
		want string
	}{
		{"with no request", deletedAfter(restarted()), wantNote},
		{"with lease requests alone", deletedAfter(restarted().requests(9100*time.Millisecond, 500*time.Millisecond, 4, lease("get"))), wonNoLease},
		{"with lease candidate requests alone", deletedAfter(restarted().
			request(9050*time.Millisecond, leaseCandidate("get")).
			request(9100*time.Millisecond, leaseCandidate("list")).
			request(9200*time.Millisecond, leaseCandidate("create"))), wantNote},
		{"with a watch before it won its lease", deletedAfter(restarted().
			request(9100*time.Millisecond, watch()).
			request(9200*time.Millisecond, lease("get")).
			request(10500*time.Millisecond, lease("update"))), wonNoLease},
		{"with discovery reads alone", deletedAfter(restarted().request(9100*time.Millisecond, nonResource("/api"))), wantNote},
		{"with a request before the restart alone", deletedAfter(restarted().request(8500*time.Millisecond, get("w-1"))), wantNote},
		{"with a request in the wait alone", deletedAfter(restarted().request(10500*time.Millisecond, watch())), wantNote},
		{"with a request at the op's instant alone", deletedAfter(restarted().request(10*time.Second, watch())), wantNote},
		{"with an exit in the wait too", deletedAfter(restarted().exit(10500*time.Millisecond, 10500*time.Millisecond)), wantNote},
		{"with a request before the last restart alone", deletedAfter(converged().
			op(invariant.OpRestart, 8*time.Second).
			request(8500*time.Millisecond, get("w-1")).
			op(invariant.OpRestart, 9*time.Second)),
			"G7 is not evaluated for op 3 (deleteManaged): the target had requested no resource outside leader election between op 2 (restart) and it"},
		{"with a supervised restart", deletedAfter(converged().exit(9*time.Second, 9*time.Second)),
			"G7 is not evaluated for op 1 (deleteManaged): the target had requested no resource outside leader election between the restart after its exit during op 0 (create) and it"},
		{"with a supervised restart during a later op", deletedAfter(converged().
			exit(8900*time.Millisecond, 9100*time.Millisecond).
			op(invariant.OpSettle, 9*time.Second)),
			"G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between the restart after its exit during op 0 (create) and it"},
		{"with a supervised restart after an exit as a later op ran", deletedAfter(converged().
			op(invariant.OpSettle, 9*time.Second).
			exit(9*time.Second, 9100*time.Millisecond)),
			"G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between the restart after its exit during op 1 (settle) and it"},
		{"with a supervised restart after a restart op", deletedAfter(converged().
			op(invariant.OpRestart, 8*time.Second).
			request(8500*time.Millisecond, get("w-1")).
			exit(8800*time.Millisecond, 9*time.Second)),
			"G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between the restart after its exit during op 1 (restart) and it"},
		{"with a restart op after a supervised restart", deletedAfter(converged().
			exit(7*time.Second, 7*time.Second).
			request(7500*time.Millisecond, get("w-1")).
			op(invariant.OpRestart, 9*time.Second)),
			"G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between op 1 (restart) and it"},
		// The converged wait ends what the fault owed.
		{"with a supervised restart after an exit a fault excused", deletedAfter(converged().
			fault(3*time.Second, 4*time.Second).
			exit(4500*time.Millisecond, 4500*time.Millisecond).
			op(invariant.OpSettle, 5*time.Second).
			checkpoint(6600*time.Millisecond, invariant.Converged)),
			"G7 is not evaluated for op 2 (deleteManaged): the target had requested no resource outside leader election between the restart after its exit during op 0 (create) and it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, c.want)
		})
	}
}

func TestG7JudgesAnObjectDeletedOnceARestartedTargetWasBack(t *testing.T) {
	for _, restart := range []struct {
		name      string
		restarted func() *run
	}{
		{"after a restart op", func() *run { return converged().op(invariant.OpRestart, 9*time.Second) }},
		{"after a supervised restart", func() *run { return converged().exit(9*time.Second, 9*time.Second) }},
	} {
		for _, c := range []struct {
			name     string
			requests []proxy.Request
		}{
			{"with a watch", []proxy.Request{watch()}},
			{"with a get the API server refused", []proxy.Request{failedGet("w-0", http.StatusNotFound)}},
			{"with a resource named leases in another group", []proxy.Request{leasesElsewhere()}},
			{"with a lease it won", []proxy.Request{lease("get"), lease("update")}},
		} {
			t.Run(restart.name+" "+c.name, func(t *testing.T) {
				r := restart.restarted()
				for i, request := range c.requests {
					r.request(9500*time.Millisecond+time.Duration(i)*100*time.Millisecond, request)
				}
				in := r.
					deletedManaged(10*time.Second, "w-0").
					remove(10100*time.Millisecond, child("w-0", "15")).
					checkpoint(12100*time.Millisecond, invariant.Converged).
					through(12100 * time.Millisecond)

				fired(t, invariant.SelfHealing, in)
			})
		}
	}
}

// A target that exits, or waits to restart, around the op may not be running
// to recreate the object.
func TestG7NotesAnObjectDeletedWhileTheTargetWasStopped(t *testing.T) {
	const wantNote = "G7 is not evaluated for op 1 (deleteManaged): the target exited, or waited to restart, during it or the wait after it"
	deleted := func(r *run) *run {
		return r.
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15"))
	}
	for _, c := range []struct {
		name string
		in   invariant.Input
	}{
		{"waiting to restart when the op ran", deleted(converged().exit(9*time.Second, 10050*time.Millisecond)).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)},
		{"restarted in the wait", deleted(converged()).
			exit(10500*time.Millisecond, 10500*time.Millisecond).
			checkpoint(12600*time.Millisecond, invariant.Converged).
			through(12600 * time.Millisecond)},
		{"waiting to restart where the wait ended", deleted(converged()).
			exit(10500*time.Millisecond, 20500*time.Millisecond).
			checkpoint(15*time.Second, invariant.Expired).
			through(15 * time.Second)},
		{"restarted as the op ran", deleted(converged().exit(9*time.Second, 10*time.Second)).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)},
		{"exited as the wait ended", deleted(converged()).
			exit(12100*time.Millisecond, 12100*time.Millisecond).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond)},
		{"restarted during the op, before its wait began", deleted(converged()).
			exit(10050*time.Millisecond, 10050*time.Millisecond).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			waitBegan(10100 * time.Millisecond).
			through(12100 * time.Millisecond)},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, wantNote)
		})
	}
}

func TestG7JudgesAnObjectDeletedBeforeTheTargetExited(t *testing.T) {
	in := childDeleted().
		checkpoint(12100*time.Millisecond, invariant.Converged).
		exit(12200*time.Millisecond, 12200*time.Millisecond).
		through(12200 * time.Millisecond)

	fired(t, invariant.SelfHealing, in)
}

// An op that deleted nothing names no object, so G7 has nothing to judge.
func TestG7IgnoresAnOpThatDeletedNothing(t *testing.T) {
	in := converged().
		op(invariant.OpDeleteManaged, 10*time.Second).
		checkpoint(12*time.Second, invariant.Converged).
		through(12 * time.Second)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want nothing: the op deleted nothing.", notes)
	}
}

// A run that stopped inside the wait never reached the instant G7 judges.
func TestG7IgnoresAnOpWhoseWaitNeverEnded(t *testing.T) {
	in := childDeleted().through(12 * time.Second)

	silent(t, invariant.SelfHealing, in)
}

// A fault that ends while reconciler-fuzzer deletes the object can still hide
// the deletion from the target, and so can one the target is still recovering
// from.
func TestG7NotesAnObjectAFaultMayHaveKeptAway(t *testing.T) {
	const wantNote = "G7 is not evaluated for op 1 (deleteManaged): a fault was active during it or the wait after it, " +
		"or the target was still owed time to recover from one where the wait ended"
	for _, c := range []struct {
		name string
		in   invariant.Input
		want string
	}{
		{"in the wait", childDeleted().
			fault(10050*time.Millisecond, 11*time.Second).
			checkpoint(16*time.Second, invariant.Expired).
			through(16 * time.Second), wantNote},
		{"during the op alone", childDeleted().
			fault(9*time.Second, 10050*time.Millisecond).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			waitBegan(10100 * time.Millisecond).
			through(12100 * time.Millisecond), wantNote},
		// The Runner stops a fault that runs until an op just before it
		// applies the op. The target is owed until 20.98s.
		{"that stopped as the op began", childDeleted().
			fault(4*time.Second, 9990*time.Millisecond).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond), wantNote},
		{"that began as the wait ended", childDeleted().
			fault(12050*time.Millisecond, 13*time.Second).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond), wantNote},
		{"after a restart the target had not answered", converged().
			op(invariant.OpRestart, 9*time.Second).
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			fault(10050*time.Millisecond, 11*time.Second).
			checkpoint(16*time.Second, invariant.Expired).
			through(16 * time.Second),
			"G7 is not evaluated for op 2 (deleteManaged): a fault was active"},
		// The target is owed until 18s.
		{"that stopped before a restart the target had not answered", converged().
			fault(4*time.Second, 8500*time.Millisecond).
			op(invariant.OpRestart, 9*time.Second).
			deletedManaged(10*time.Second, "w-0").
			remove(10100*time.Millisecond, child("w-0", "15")).
			checkpoint(12100*time.Millisecond, invariant.Converged).
			through(12100 * time.Millisecond),
			"G7 is not evaluated for op 2 (deleteManaged): a fault was active"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, c.want)
		})
	}
}

// A fault from 4s that stopped as the op began leaves the target owed until
// 20.98s.
func TestG7JudgesOnceTheTargetHadRecoveredFromAFault(t *testing.T) {
	waitEndingAt := func(end time.Duration) invariant.Input {
		return childDeleted().
			fault(4*time.Second, 9990*time.Millisecond).
			checkpoint(end, invariant.Expired).
			through(end)
	}
	t.Run("at 20.98s", func(t *testing.T) {
		fired(t, invariant.SelfHealing, waitEndingAt(20980*time.Millisecond))
	})
	t.Run("just before", func(t *testing.T) {
		noted(t, invariant.SelfHealing, waitEndingAt(20979*time.Millisecond), "the target was still owed time to recover")
	})
}

// A target can converge while an informer of its still backs off. The fault
// from 4s to 8s leaves the target owed until 17s, though the run converged
// since.
func TestG7WaitsOutAFaultTheRunConvergedSince(t *testing.T) {
	waitEndingAt := func(end time.Duration) invariant.Input {
		return converged().
			fault(4*time.Second, 8*time.Second).
			op(invariant.OpSettle, 8500*time.Millisecond).
			checkpoint(10500*time.Millisecond, invariant.Converged).
			deletedManaged(11*time.Second, "w-0").
			remove(11100*time.Millisecond, child("w-0", "15")).
			checkpoint(end, invariant.Converged).
			through(end)
	}
	t.Run("at 13.1s", func(t *testing.T) {
		noted(t, invariant.SelfHealing, waitEndingAt(13100*time.Millisecond), "the target was still owed time to recover")
	})
	t.Run("just before 17s", func(t *testing.T) {
		noted(t, invariant.SelfHealing, waitEndingAt(16999*time.Millisecond), "the target was still owed time to recover")
	})
	t.Run("at 17s", func(t *testing.T) {
		fired(t, invariant.SelfHealing, waitEndingAt(17*time.Second))
	})
}

// A deleted CR holds the settle wait open, until 10.3s here, but owes the
// target no time to recover.
func TestG7FiresSoonAfterARecreate(t *testing.T) {
	in := converged().
		op(invariant.OpRecreate, 5*time.Second).
		record(5100*time.Millisecond, widget("15", spec(2), status(2, 1), deleting(5*time.Second), finalizers(cleanup))).
		remove(5200*time.Millisecond, child("w-0", "16"), child("w-1", "17")).
		remove(5300*time.Millisecond, widget("18", spec(2), status(2, 1), deleting(5*time.Second))).
		record(5400*time.Millisecond, widget("20", spec(2), uid("uid-w-again"), finalizers(cleanup))).
		record(5500*time.Millisecond, child("w-0", "21", uid("uid-w-0-2"), ownedByRecreated),
			child("w-1", "22", uid("uid-w-1-2"), ownedByRecreated)).
		record(5600*time.Millisecond, widget("23", spec(2), status(2, 1), uid("uid-w-again"), finalizers(cleanup))).
		checkpoint(7600*time.Millisecond, invariant.Converged).
		deletedManaged(8*time.Second, "w-0").
		remove(8100*time.Millisecond, child("w-0", "24", uid("uid-w-0-2"), ownedByRecreated)).
		checkpoint(10100*time.Millisecond, invariant.Converged).
		through(10100 * time.Millisecond)

	fired(t, invariant.SelfHealing, in)
}

// The update scales the toy down to one child before it settles, so the
// target meant to delete w-1 itself. Whether the target wanted w-1 back comes
// before whether it could recreate it.
func TestG7NotesAnObjectDeletedBeforeTheRunConverged(t *testing.T) {
	updated := func() *run {
		return converged().
			op(invariant.OpUpdate, 10*time.Second).
			record(10050*time.Millisecond, widget("20", spec(1), generation(2), status(2, 1), finalizers(cleanup)))
	}
	deletedAfter := func(r *run) invariant.Input {
		return r.
			deletedManaged(10100*time.Millisecond, "w-1").
			remove(10150*time.Millisecond, child("w-1", "21")).
			record(10200*time.Millisecond, widget("22", spec(1), generation(2), status(1, 2), finalizers(cleanup))).
			checkpoint(12200*time.Millisecond, invariant.Converged).
			through(12200 * time.Millisecond)
	}
	const wantNote = "G7 is not evaluated for op 2 (deleteManaged): the run had not converged since op 1 (update)"
	for _, c := range []struct {
		name string
		in   invariant.Input
		want string
	}{
		{"alone", deletedAfter(updated()), wantNote},
		{"with a fault in the wait", deletedAfter(updated().fault(10150*time.Millisecond, 11*time.Second)), wantNote},
		{"with a restart before the op", deletedAfter(updated().op(invariant.OpRestart, 10060*time.Millisecond)),
			"G7 is not evaluated for op 3 (deleteManaged): the run had not converged since op 1 (update)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			noted(t, invariant.SelfHealing, c.in, c.want)
		})
	}
}

// reconciler-fuzzer deleted the CR without waiting, and the Observer saw it go
// only after the op had deleted the object.
func TestG7ReadsTheCRWhereTheWaitEnds(t *testing.T) {
	in := converged().
		record(300*time.Millisecond, child("kept", "13", orphaned)).
		op(invariant.OpDelete, 5*time.Second).
		deletedManaged(5050*time.Millisecond, "kept").
		record(5100*time.Millisecond, widget("15", spec(2), status(2, 1), deleting(5*time.Second), finalizers(cleanup))).
		remove(5150*time.Millisecond, child("kept", "16", orphaned)).
		checkpoint(7150*time.Millisecond, invariant.Converged).
		through(7150 * time.Millisecond)

	if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
		t.Errorf("G7 noted %v, want nothing: the CR was going when the wait ended.", notes)
	}
}

// A target owes nothing to a CR that reconciler-fuzzer deleted.
func TestG7IgnoresAnObjectDeletedWhileTheCRWasGoing(t *testing.T) {
	for _, c := range []struct {
		name string
		cr   func(*run) *run
	}{
		{"gone", func(r *run) *run { return r.remove(5500*time.Millisecond, deletedWidget("15")) }},
		{"under deletion", func(r *run) *run { return r.record(5500*time.Millisecond, deletedWidget("15", finalizers(cleanup))) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := converged().
				record(300*time.Millisecond, child("kept", "13", orphaned)).
				op(invariant.OpDelete, 5*time.Second)
			in := c.cr(r).
				checkpoint(7500*time.Millisecond, invariant.Converged).
				deletedManaged(10*time.Second, "kept").
				remove(10100*time.Millisecond, child("kept", "16", orphaned)).
				checkpoint(12100*time.Millisecond, invariant.Converged).
				through(12100 * time.Millisecond)

			if notes := silent(t, invariant.SelfHealing, in).Notes; len(notes) > 0 {
				t.Errorf("G7 noted %v, want nothing: there was no CR to restore the object for.", notes)
			}
		})
	}
}

// twoConverged is two Widgets with a child each, converged at 2.4s, and an
// object that names neither.
func twoConverged() *run {
	return newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		opOn(invariant.OpCreate, 50*time.Millisecond, secondName).
		record(100*time.Millisecond, widget("11", spec(1), status(1, 1), finalizers(cleanup)),
			secondWidget("21", spec(1), status(1, 1), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12"), secondChild("w2-0", "22"), child("kept", "13", orphaned)).
		checkpoint(2400*time.Millisecond, invariant.Converged)
}

// oneDeleting has reconciler-fuzzer delete the Widget named at 5s, which its
// finalizer then holds.
func oneDeleting(name string) func(*run) *run {
	return func(r *run) *run {
		deleting := deleting(5 * time.Second)
		if name == secondName {
			return r.opOn(invariant.OpDelete, 5*time.Second, secondName).
				record(5100*time.Millisecond, secondWidget("23", spec(1), status(1, 1), finalizers(cleanup), deleting))
		}
		return r.op(invariant.OpDelete, 5*time.Second).
			record(5100*time.Millisecond, widget("14", spec(1), status(1, 1), finalizers(cleanup), deleting))
	}
}

func TestG7AsksOnlyTheCRsAnObjectNamesForItBack(t *testing.T) {
	for _, c := range []struct {
		name     string
		deleting string
		taken    string
		fires    bool
	}{
		{"w2's child while w2 goes", secondName, "w2-0", false},
		{"w2's child while w goes", widgetName, "w2-0", true},
		{"an object that names no CR while w goes", widgetName, "kept", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := oneDeleting(c.deleting)(twoConverged()).
				checkpoint(7500*time.Millisecond, invariant.Converged).
				deletedManaged(10*time.Second, c.taken).
				remove(10100*time.Millisecond, object(configMapGVK, c.taken, "30")).
				checkpoint(12100*time.Millisecond, invariant.Converged).
				through(12100 * time.Millisecond)

			result := evaluate(t, invariant.SelfHealing, in)

			if fired := len(result.Violations) > 0; fired != c.fires {
				t.Errorf("G7 reported %v, want a violation: %t.", statements(result), c.fires)
			}
			if len(result.Notes) > 0 {
				t.Errorf("G7 noted %v, want a verdict.", result.Notes)
			}
		})
	}
}
