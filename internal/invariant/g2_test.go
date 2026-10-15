package invariant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

func TestG2PassesWhenNothingMovesInTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

// The window opens where the settle wait ended, which for a target that
// converges is well inside T_settle (DESIGN.md §6).
func TestG2FiresOnAResourceVersionThatMoves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		request(2900*time.Millisecond, widgetPatch()).
		record(3*time.Second, widget("11", spec(1), status(1, 1))).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if violation.ID != "G2" {
		t.Errorf("The violation is %q, want G2.", violation.ID)
	}
	if len(violation.Versions) != 1 || violation.Versions[0].ResourceVersion != "11" {
		t.Fatalf("The evidence holds %v, want the Widget at resourceVersion 11.", violation.Versions)
	}
	if want := "the target changed 1 object in the 2s (timeouts.stable) after op 0 (create) settled, " +
		"where a converged target changes nothing"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
}

func TestG2FiresOnAManagedObjectThatAppears(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, child("w-0", "11")).
		settled(2*time.Second, invariant.Converged).
		request(2900*time.Millisecond, createChild()).
		record(3*time.Second, child("w-0-xk92f", "20")).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Versions) != 1 || violation.Versions[0].Name != "w-0-xk92f" {
		t.Fatalf("The evidence holds %v, want the ConfigMap that appeared.", violation.Versions)
	}
}

func TestG2FiresOnAStatusWriteThatMovesNoResourceVersion(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, 500*time.Millisecond, 3, statusPatch()).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Requests) != 3 {
		t.Fatalf("The evidence holds %d requests, want the 3 status writes.", len(violation.Requests))
	}
	if !strings.Contains(violation.Statement, "status") {
		t.Errorf("The statement is %q, want it to name the status writes.", violation.Statement)
	}
}

// A timer that rewrites an unchanged status is what thresholds.quiet admits.
func TestG2PassesStatusWritesAtTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, time.Second, 2, statusPatch()).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	silent(t, invariant.NoChurn, in)
}

func TestG2FiresOnStatusWritesPastTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, 400*time.Millisecond, 4, statusPatch()).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	violation := fired(t, invariant.NoChurn, in)

	if want := "made 4 status writes"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say it %s.", violation.Statement, want)
	}
	if want := "thresholds.quiet allows 2"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to name the threshold: %q.", violation.Statement, want)
	}
	if want := at(3300 * time.Millisecond); !violation.At.Equal(want) {
		t.Errorf("The violation is at %v, want the write that went past the threshold, at %v.", violation.At, want)
	}
}

func TestG2ReadsANegativeQuietAsZero(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, statusPatch()).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = -1

	violation := fired(t, invariant.NoChurn, in)

	if want := "the target made 1 status write in the 2s (timeouts.stable) after op 0 (create) settled, " +
		"where thresholds.quiet allows 0"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
}

// A write that changed something is churn however many the target declares.
func TestG2IgnoresTheQuietThresholdForAResourceVersionThatMoves(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		request(2900*time.Millisecond, statusPatch()).
		record(3*time.Second, widget("11", spec(1), status(1, 1))).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 5

	violation := fired(t, invariant.NoChurn, in)

	if len(violation.Versions) != 1 || violation.Versions[0].ResourceVersion != "11" {
		t.Fatalf("The evidence holds %v, want the Widget at resourceVersion 11.", violation.Versions)
	}
}

func TestG2IgnoresChangesBeforeTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(0, 1))).
		record(1500*time.Millisecond, widget("11", spec(1), status(1, 1))).
		record(1800*time.Millisecond, child("w-0", "12")).
		request(1800*time.Millisecond, statusPatch()).
		settled(2*time.Second, invariant.Converged).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresAnObjectReconcilerFuzzerCreated(t *testing.T) {
	fixture := child("shared", "10", orphaned)
	in := newRun().
		fixture(fixture).
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		record(3*time.Second, fixture).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresTheChangesTheTeardownMade(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		checkpoint(2*time.Second, invariant.Converged).
		teardown(3*time.Second).
		remove(3500*time.Millisecond, child("w-0", "12")).
		request(3500*time.Millisecond, statusPatch()).
		through(14 * time.Second)

	silent(t, invariant.NoChurn, in)
}

// On kind, the garbage collector deletes a child seconds after its owner
// goes. The create that made the child explains nothing that late.
func TestG2IgnoresAChangeNoWriteOfTheTargetsExplains(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(100*time.Millisecond, createChild()).
		record(150*time.Millisecond, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		checkpoint(2*time.Second, invariant.Converged).
		op(invariant.OpDelete, 10*time.Second).
		remove(10100*time.Millisecond, widget("12", spec(1), status(1, 1))).
		settled(12100*time.Millisecond, invariant.Converged).
		remove(12500*time.Millisecond, child("w-0", "13")).
		through(20 * time.Second)

	silent(t, invariant.NoChurn, in)
}

func TestG2IgnoresAChangeTheTargetsWriteElsewhereCannotExplain(t *testing.T) {
	otherResource := write("patch", "w-0")
	otherResource.Resource = "secrets"
	otherGroup := write("patch", "w-0")
	otherGroup.Group = "example.com"
	otherNamespace := write("patch", "w-0")
	otherNamespace.Namespace = "reconciler-fuzzer-run-2"
	for name, elsewhere := range map[string]struct {
		when time.Duration
		req  proxy.Request
	}{
		"a read of the object":         {2500 * time.Millisecond, get("w-0")},
		"a write to another name":      {2500 * time.Millisecond, write("patch", "w-1")},
		"a write to the CR":            {2500 * time.Millisecond, widgetPatch()},
		"a write to another resource":  {2500 * time.Millisecond, otherResource},
		"a write to another group":     {2500 * time.Millisecond, otherGroup},
		"a write in another namespace": {2500 * time.Millisecond, otherNamespace},
		"a write after the change":     {3500 * time.Millisecond, write("update", "w-0")},
	} {
		t.Run(name, func(t *testing.T) {
			in := newRun().
				op(invariant.OpCreate, 0).
				record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
				settled(2*time.Second, invariant.Converged).
				request(elsewhere.when, elsewhere.req).
				remove(3*time.Second, child("w-0", "12")).
				through(14 * time.Second)

			silent(t, invariant.NoChurn, in)
		})
	}
}

// Removing the last finalizer of an object being deleted deletes it, so one
// write explains both versions.
func TestG2CountsEveryVersionAWriteOfTheTargetsLeadsTo(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11", finalizers(cleanup), deleting(500*time.Millisecond))).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, write("patch", "w-0")).
		record(3010*time.Millisecond, child("w-0", "12", deleting(500*time.Millisecond))).
		remove(3020*time.Millisecond, child("w-0", "13", deleting(500*time.Millisecond))).
		through(14 * time.Second)

	violation := fired(t, invariant.NoChurn, in)

	if got, want := quoted(violation), []string{"w-0@12", "w-0@13"}; !slices.Equal(got, want) {
		t.Errorf("The evidence holds %v, want %v.", got, want)
	}
}

func TestG2CountsAWriteToTheCollection(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), child("w-0", "11")).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, write("deletecollection", "")).
		remove(3050*time.Millisecond, child("w-0", "12")).
		through(14 * time.Second)

	fired(t, invariant.NoChurn, in)
}

// A target may write its CR at any version the CRD serves.
func TestG2CountsAWriteAtAnotherVersionOfTheKind(t *testing.T) {
	patched := widgetPatch()
	patched.Version = "v1beta1"
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1))).
		settled(2*time.Second, invariant.Converged).
		request(2900*time.Millisecond, patched).
		record(3*time.Second, widget("11", spec(1), status(1, 1))).
		through(14 * time.Second)

	fired(t, invariant.NoChurn, in)
}
