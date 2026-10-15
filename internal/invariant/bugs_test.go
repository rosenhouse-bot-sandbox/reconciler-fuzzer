package invariant_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

// Each input below is the run the toy target produces under one seeded bug of
// DESIGN.md §9.1, observed as far as the run took it. `catalog` is what §9.1
// promises the bug trips; `fires` is everything the engine reports, which is a
// superset wherever the bug breaks another check too.
func TestTheSeededBugsTripTheChecksTheCatalogNames(t *testing.T) {
	for _, seeded := range []struct {
		bug     string
		catalog []string
		fires   []string
		in      invariant.Input
	}{
		{bug: "B0", in: noBug()},
		{bug: "B1", catalog: []string{"P1"}, fires: []string{"G1", "G2", "P1"}, in: b1()},
		{bug: "B2", catalog: []string{"G1", "G2"}, fires: []string{"G1", "G2", "G4"}, in: b2()},
		{bug: "B3", catalog: []string{"G3"}, fires: []string{"G3"}, in: b3()},
		{bug: "B4", catalog: []string{"G4"}, fires: []string{"G4"}, in: b4()},
		{bug: "B5", catalog: []string{"G1", "G6"}, fires: []string{"G1", "G4", "G6"}, in: b5()},
		{bug: "B6", catalog: []string{"G1", "G2"}, fires: []string{"G1", "G2", "G4"}, in: b6()},
		{bug: "B7", catalog: []string{"G4"}, fires: []string{"G4"}, in: b7()},
		{bug: "B8", catalog: []string{"G5", "G7", "P1"}, fires: []string{"G5", "G7", "P1"}, in: b8()},
		{bug: "B9", catalog: []string{"G3"}, fires: []string{"G3", "G4"}, in: b9()},
		{bug: "B10", catalog: []string{"G4"}, fires: []string{"G4", "P1"}, in: b10()},
		{bug: "B13", catalog: []string{"G3"}, fires: []string{"G3"}, in: b13()},
		{bug: "B15", catalog: []string{"G4"}, fires: []string{"G4"}, in: b15()},
	} {
		t.Run(seeded.bug, func(t *testing.T) {
			results, err := invariant.Evaluate(seeded.in)
			if err != nil {
				t.Fatalf("Evaluating %s returned an error: %v", seeded.bug, err)
			}
			fired := firingIDs(results)
			for _, promised := range seeded.catalog {
				if !slices.Contains(fired, promised) {
					t.Errorf("%s tripped %v, want §9.1's %s among them.", seeded.bug, fired, promised)
				}
			}
			if !slices.Equal(fired, seeded.fires) {
				t.Fatalf("%s tripped %v, want %v.", seeded.bug, fired, seeded.fires)
			}
		})
	}
}

func firingIDs(results []invariant.Result) []string {
	var ids []string
	for _, result := range results {
		if len(result.Violations) > 0 {
			ids = append(ids, result.ID)
		}
	}
	return ids
}

// converged is the toy's happy path: a Widget of two children, ready at 0.4s.
func converged() *run {
	return newRun().
		op(invariant.OpCreate, 0).
		running(50*time.Millisecond).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12")).
		record(300*time.Millisecond, child("w-1", "13")).
		record(400*time.Millisecond, widget("14", spec(2), status(2, 1), finalizers(cleanup))).
		checkpoint(2400*time.Millisecond, invariant.Converged)
}

// noBug deletes the converged Widget and comes away clean.
func noBug() invariant.Input {
	return converged().
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, deletedWidget("15", finalizers(cleanup))).
		remove(10500*time.Millisecond, child("w-0", "16"), child("w-1", "17")).
		remove(10600*time.Millisecond, deletedWidget("18")).
		checkpoint(12600*time.Millisecond, invariant.Converged).
		checkpoint(22*time.Second, invariant.NoSettle).
		through(22 * time.Second)
}

// deletedWidget is the CR reconciler-fuzzer deleted at 10s.
func deletedWidget(resourceVersion string, opts ...option) *unstructured.Unstructured {
	return widget(resourceVersion, append([]option{spec(2), status(2, 1), deleting(10 * time.Second)}, opts...)...)
}

// b1 reports both children ready before it creates them, and holds that
// status across the checkpoint. §9.1 times the hold so that the children then
// land inside the quiet window.
func b1() invariant.Input {
	return newRun().
		op(invariant.OpCreate, 0).
		running(50*time.Millisecond).
		record(100*time.Millisecond, widget("11", spec(2), status(2, 1), finalizers(cleanup))).
		settled(2200*time.Millisecond, invariant.Converged).
		request(3100*time.Millisecond, createChild()).
		record(3150*time.Millisecond, child("w-0", "12")).
		request(3200*time.Millisecond, createChild()).
		record(3250*time.Millisecond, child("w-1", "13")).
		through(14 * time.Second)
}

// b2 adds a generated child on every reconcile, so the namespace never goes
// still.
func b2() invariant.Input {
	r := newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup)))
	for i := range 8 {
		created := time.Duration(i)*time.Second + 200*time.Millisecond
		name := fmt.Sprintf("w-0-%d", i)
		r.record(created, child(name, strconv.Itoa(20+i))).
			request(created, createChild()).
			record(created+100*time.Millisecond, widget(strconv.Itoa(40+i), spec(2), status(int64(i+1), 1), finalizers(cleanup))).
			request(created+100*time.Millisecond, statusPatch())
	}
	return r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)
}

// b3 orphans w-0 and counts children by name, so only the deletion shows it.
func b3() invariant.Input {
	return newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12", orphaned)).
		record(300*time.Millisecond, child("w-1", "13")).
		record(400*time.Millisecond, widget("14", spec(2), status(2, 1), finalizers(cleanup))).
		checkpoint(2400*time.Millisecond, invariant.Converged).
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, deletedWidget("15", finalizers(cleanup))).
		remove(10300*time.Millisecond, child("w-1", "16")).
		remove(10500*time.Millisecond, deletedWidget("17")).
		checkpoint(12500*time.Millisecond, invariant.Converged).
		checkpoint(22*time.Second, invariant.NoSettle).
		through(22 * time.Second)
}

// b4 reads the count from status.ready, so it never creates a child.
func b4() invariant.Input {
	return newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(3), finalizers(cleanup))).
		record(300*time.Millisecond, widget("12", spec(3), status(0, 1), finalizers(cleanup))).
		checkpoint(5*time.Second, invariant.Expired).
		through(5 * time.Second)
}

// b5 treats NotFound on the child Get as an error and requeues forever.
func b5() invariant.Input {
	r := newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup)))
	for _, failed := range []time.Duration{200, 400, 800, 1600, 2400, 3200, 4000, 4800, 5600, 6400, 7200} {
		r.request(failed*time.Millisecond, failedGet("w-0", 404))
	}
	return r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)
}

// b6 writes a fresh status.lastSyncTime on every reconcile, which re-triggers
// it.
func b6() invariant.Input {
	r := newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12")).
		record(300*time.Millisecond, child("w-1", "13"))
	for i := range 15 {
		written := time.Duration(i)*500*time.Millisecond + 400*time.Millisecond
		r.record(written, widget(strconv.Itoa(20+i), spec(2), status(2, 1), finalizers(cleanup))).
			request(written, statusPatch())
	}
	return r.checkpoint(5*time.Second, invariant.Expired).through(8 * time.Second)
}

// b7 leaves the surplus children behind when the count drops to one.
func b7() invariant.Input {
	return newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(3), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12"), child("w-1", "13"), child("w-2", "14")).
		record(500*time.Millisecond, widget("15", spec(3), status(3, 1), finalizers(cleanup))).
		checkpoint(2500*time.Millisecond, invariant.Converged).
		op(invariant.OpUpdate, 10*time.Second).
		record(10050*time.Millisecond, widget("20", spec(1), generation(2), status(3, 1), finalizers(cleanup))).
		record(10300*time.Millisecond, widget("21", spec(1), generation(2), status(3, 2), finalizers(cleanup))).
		checkpoint(15*time.Second, invariant.Expired).
		through(15 * time.Second)
}

// b8 does not watch its children, so a deleted one comes back only with the
// restart.
func b8() invariant.Input {
	return converged().
		deletedManaged(10*time.Second, "w-0").
		remove(10100*time.Millisecond, child("w-0", "15")).
		checkpoint(12200*time.Millisecond, invariant.Converged).
		op(invariant.OpRestart, 16*time.Second).
		running(16200*time.Millisecond).
		record(16300*time.Millisecond, child("w-0", "30", uid("uid-w-0-again"))).
		checkpoint(18500*time.Millisecond, invariant.Converged).
		through(20 * time.Second)
}

// b9 releases the Widget before deleting its children and owns none of them,
// so no path cleans up.
func b9() invariant.Input {
	return newRun().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("11", spec(2), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12", orphaned), child("w-1", "13", orphaned)).
		record(400*time.Millisecond, widget("14", spec(2), status(0, 1), finalizers(cleanup))).
		checkpoint(5*time.Second, invariant.Expired).
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, widget("15", spec(2), status(0, 1), finalizers(cleanup), deleting(10*time.Second))).
		record(10200*time.Millisecond, widget("16", spec(2), status(0, 1), deleting(10*time.Second))).
		remove(10300*time.Millisecond, widget("17", spec(2), status(0, 1), deleting(10*time.Second))).
		checkpoint(12300*time.Millisecond, invariant.Converged).
		checkpoint(22*time.Second, invariant.NoSettle).
		through(22 * time.Second)
}

// b13 never runs its cleanup. The delete's settle wait gives the Widget until
// its deletion deadline, and expires with the Widget still there.
func b13() invariant.Input {
	return converged().
		op(invariant.OpDelete, 10*time.Second).
		record(10100*time.Millisecond, deletedWidget("15", finalizers(cleanup))).
		checkpoint(20150*time.Millisecond, invariant.Expired).
		checkpoint(35*time.Second, invariant.NoSettle).
		through(35 * time.Second)
}

// b15 names its children after the kind, so the second Widget finds its child
// owned by the first and never converges.
func b15() invariant.Input {
	return converged().withSecondWidget().
		opOn(invariant.OpCreate, 5*time.Second, secondName).
		record(5100*time.Millisecond, secondWidget("21", spec(2), finalizers(cleanup))).
		checkpoint(10100*time.Millisecond, invariant.Expired).
		through(10100 * time.Millisecond)
}

// restartedAndScaledDown is b10.json up to its scale-down: three children, a
// restart at 10s and an update of spec.count to 1 before anything settles.
func restartedAndScaledDown() *run {
	return newRun().
		op(invariant.OpCreate, 0).
		running(50*time.Millisecond).
		record(100*time.Millisecond, widget("11", spec(3), finalizers(cleanup))).
		record(200*time.Millisecond, child("w-0", "12"), child("w-1", "13"), child("w-2", "14")).
		record(500*time.Millisecond, widget("15", spec(3), status(3, 1), finalizers(cleanup))).
		checkpoint(2500*time.Millisecond, invariant.Converged).
		op(invariant.OpRestart, 10*time.Second).
		op(invariant.OpUpdate, 10100*time.Millisecond).
		record(10150*time.Millisecond, widget("20", spec(1), generation(2), status(3, 1), finalizers(cleanup))).
		running(10200*time.Millisecond).
		remove(10300*time.Millisecond, child("w-1", "21"), child("w-2", "22"))
}

// b10 writes the status from a flag the restart lost, so the scale-down
// leaves it stale.
func b10() invariant.Input {
	return restartedAndScaledDown().
		checkpoint(15100*time.Millisecond, invariant.Expired).
		through(15100 * time.Millisecond)
}

// The correct toy passes b10.json. Its update runs before the state after the
// restart settles, so G5 cannot tell which of the two changed the Widget.
func TestTheCorrectToyPassesTheSequenceOfB10(t *testing.T) {
	in := restartedAndScaledDown().
		record(10400*time.Millisecond, widget("23", spec(1), generation(2), status(1, 2), finalizers(cleanup))).
		checkpoint(12400*time.Millisecond, invariant.Converged).
		through(15 * time.Second)

	results, err := invariant.Evaluate(in)
	if err != nil {
		t.Fatalf("The checks failed to evaluate: %v", err)
	}
	if fired := firingIDs(results); len(fired) > 0 {
		t.Fatalf("The correct toy tripped %v, want nothing.", fired)
	}
	var notes []string
	for _, result := range results {
		notes = append(notes, result.Notes...)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "G5") || !strings.Contains(notes[0], "for op 1 (restart): op 2 (update) ran") {
		t.Fatalf("The checks noted %v, want one G5 note saying op 2 (update) ran across op 1 (restart).", notes)
	}
}

// Every violation the checks raise says how much evidence it chose from, or a
// report of it cannot say what the bound left out (#22).
func TestEveryViolationSaysHowMuchEvidenceItChoseFrom(t *testing.T) {
	for _, seeded := range []struct {
		bug string
		in  invariant.Input
	}{
		{"B1", b1()}, {"B2", b2()}, {"B3", b3()}, {"B4", b4()}, {"B5", b5()},
		{"B6", b6()}, {"B7", b7()}, {"B8", b8()}, {"B9", b9()}, {"B10", b10()},
		{"B13", b13()}, {"B15", b15()},
	} {
		t.Run(seeded.bug, func(t *testing.T) {
			results, err := invariant.Evaluate(seeded.in)
			if err != nil {
				t.Fatalf("The checks failed to evaluate: %v", err)
			}
			for _, result := range results {
				for _, v := range result.Violations {
					if len(v.Requests) > v.RequestsTotal {
						t.Errorf("%s quotes %d requests and says it chose from %d.", v.ID, len(v.Requests), v.RequestsTotal)
					}
					if len(v.Versions) > v.VersionsTotal {
						t.Errorf("%s quotes %d versions and says it chose from %d.", v.ID, len(v.Versions), v.VersionsTotal)
					}
				}
			}
		})
	}
}
