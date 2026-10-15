package invariant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

const cleanup = "widget.reconciler-fuzzer/cleanup"

// deletedRun is a run whose CR reconciler-fuzzer deletes at 10s, with the
// children the options build.
func deletedRun() *run {
	return newRun().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second)))
}

func TestG3PassesWhenTheChildrenAndTheFinalizerGo(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3FiresOnAnOrphanTheCollectorCannotReach(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if violation.ID != "G3" {
		t.Errorf("The violation is %q, want G3.", violation.ID)
	}
	if want := "the v1/ConfigMap w-0 was still there 10s (timeouts.delete) after w, the last CR it may belong to, was deleted, " +
		"orphaned: it carries no ownerReference to the CR"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
	if want := timelineOf(configMapGVK, "w-0"); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the orphan's timeline.", violation.Versions)
	}
}

// A run reports its first violation, and KindName orders the kinds, so
// apps/v1/Deployment comes before v1/ConfigMap.
func TestG3FiresOnTheLeftoversInTheOrderOfTheirKindsNames(t *testing.T) {
	deploymentGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	in := newRunManaging(configMapGVK, deploymentGVK).
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		record(time.Second, object(configMapGVK, "c", "11"), object(deploymentGVK, "d", "12")).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("13", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	got := statements(evaluate(t, invariant.CleanDeletion, in))

	if len(got) != 2 || !strings.HasPrefix(got[0], "the apps/v1/Deployment d ") || !strings.HasPrefix(got[1], "the v1/ConfigMap c ") {
		t.Errorf("G3 reported %q, want the Deployment d and then the ConfigMap c.", got)
	}
}

// A count of zero is a finding, so a check that never asked what the target
// managed leaves none (#24).
func TestG3QuotesNoStateOfItsOwn(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if violation.ManagedTotal != nil {
		t.Errorf("G3 counted %v managed objects, want none: it judges one deletion, not the state.", managed(violation))
	}
	if len(violation.Managed) > 0 {
		t.Errorf("G3 quotes the state %v, want none.", state(violation))
	}
}

func TestG3FiresOnAChildTheTargetKeptOwning(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the child that remained.", violation.Statement)
	}
}

func TestG3FiresOnAFinalizerThatNeverClears(t *testing.T) {
	in := deletedRun().through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if want := "the CR w still carried the finalizers [" + cleanup + "] 10s (timeouts.delete) after its deletion"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if len(violation.Versions) == 0 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the CR's timeline.", violation.Versions)
	}
}

func TestG3WaitsForTheWholeDeleteTimeout(t *testing.T) {
	in := deletedRun().through(19 * time.Second)

	noted(t, invariant.CleanDeletion, in,
		"G3 is not evaluated for the deletion of w by op 0 (delete): the run ended less than 10s (timeouts.delete) after it")
}

// A namespace that came clean settles the deletion before T_delete is up, so
// a run that stops observing there is judged rather than left unjudged
// (DESIGN.md §6).
func TestG3PassesOnANamespaceThatCameCleanBeforeTheDeadline(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		cleaned(12 * time.Second).
		through(12 * time.Second)

	if notes := silent(t, invariant.CleanDeletion, in).Notes; len(notes) > 0 {
		t.Errorf("G3 noted %v, want the clean namespace to decide the deletion.", notes)
	}
}

// The namespace came clean long after this deletion's deadline, which the run
// observed, so the leftovers still count.
func TestG3FiresOnLeftoversTheNamespaceOnlyLostAfterTheDeadline(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		remove(25*time.Second, child("w-0", "13")).
		cleaned(25 * time.Second).
		through(25 * time.Second)

	fired(t, invariant.CleanDeletion, in)
}

func TestG3NotesADeletionAFaultReachedInto(t *testing.T) {
	in := deletedRun().
		fault(11*time.Second, 12*time.Second).
		through(21 * time.Second)

	noted(t, invariant.CleanDeletion, in, "G3 is not evaluated for the deletion of w by op 0 (delete): a fault was active before its deadline")
}

// A CR can be deleted more than once in a run, so each note names what
// deleted it.
func TestG3NamesWhatDeletedTheCRInEachNote(t *testing.T) {
	in := newRun().
		withSecondWidget().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpRecreate, 10*time.Second).
		record(10*time.Second, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(11*time.Second, widget("12", spec(1), status(1, 1), deleting(10*time.Second))).
		record(11*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		opOn(invariant.OpDelete, 19500*time.Millisecond, secondName).
		op(invariant.OpDelete, 20*time.Second).
		record(21*time.Second, widget("21", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup), deleting(21*time.Second))).
		remove(22*time.Second, widget("22", spec(1), status(1, 1), uid("uid-w2"), deleting(21*time.Second))).
		op(invariant.OpCreate, 23*time.Second).
		record(23*time.Second, widget("30", spec(1), status(1, 1), uid("uid-w3"), finalizers(cleanup))).
		fault(10500*time.Millisecond, 50*time.Second).
		teardown(30*time.Second).
		record(30*time.Second, widget("31", spec(1), status(1, 1), uid("uid-w3"), finalizers(cleanup), deleting(30*time.Second))).
		through(45 * time.Second)

	result := silent(t, invariant.CleanDeletion, in)

	want := []string{
		"G3 is not evaluated for the deletion of w by op 0 (recreate): a fault was active before its deadline",
		"G3 is not evaluated for the deletion of w by op 2 (delete): a fault was active before its deadline",
		"G3 is not evaluated for the deletion of w by the teardown: a fault was active before its deadline",
	}
	if !slices.Equal(result.Notes, want) {
		t.Errorf("G3 noted %q, want %q.", result.Notes, want)
	}
}

// An op names only the CR it deleted: not a later one of the same name that
// something else deleted, nor one already being deleted when the op began. A
// later teardown names neither.
func TestG3NamesNoOpForADeletionNoOpMade(t *testing.T) {
	running := newRun().
		op(invariant.OpCreate, 0).
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(11*time.Second, widget("12", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpCreate, 12*time.Second).
		record(12*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		op(invariant.OpUpdate, 15*time.Second).
		record(15*time.Second, widget("21", spec(2), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		record(20*time.Second, widget("22", spec(2), status(1, 1), uid("uid-w2"), finalizers(cleanup), deleting(20*time.Second))).
		op(invariant.OpRecreate, 25*time.Second).
		fault(10500*time.Millisecond, 30*time.Second).
		through(35 * time.Second)
	tornDown := running
	tornDown.Teardown = at(30 * time.Second)

	for _, in := range []invariant.Input{running, tornDown} {
		result := silent(t, invariant.CleanDeletion, in)

		want := []string{
			"G3 is not evaluated for the deletion of w by op 1 (delete): a fault was active before its deadline",
			"G3 is not evaluated for the deletion of w: a fault was active before its deadline",
		}
		if !slices.Equal(result.Notes, want) {
			t.Errorf("With the teardown at %v, G3 noted %q, want %q.", in.Teardown, result.Notes, want)
		}
	}
}

// The Observer can record a deletion after the next op begins, so that op
// finds the CR the op before it deleted.
func TestG3NamesTheOpThatDeletedTheCRNotALaterOne(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpDelete, 10*time.Second).
		op(invariant.OpRecreate, 10006*time.Millisecond).
		record(10020*time.Millisecond, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		fault(10500*time.Millisecond, 40*time.Second).
		through(35 * time.Second)

	noted(t, invariant.CleanDeletion, in, "G3 is not evaluated for the deletion of w by op 1 (delete): a fault was active before its deadline")
}

// The teardown can begin before the Observer records the last op's deletion.
func TestG3NamesTheOpNotTheTeardownThatFollowedIt(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup))).
		op(invariant.OpDelete, 10*time.Second).
		teardown(10005*time.Millisecond).
		record(10020*time.Millisecond, widget("11", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		through(15 * time.Second)

	noted(t, invariant.CleanDeletion, in,
		"G3 is not evaluated for the deletion of w by op 1 (delete): the run ended less than 10s (timeouts.delete) after it")
}

func TestG3IgnoresACRTheRunRecreated(t *testing.T) {
	in := deletedRun().
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		through(30 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3IgnoresTheObjectsOfACRTheRunRecreated(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		record(13500*time.Millisecond, child("w-0", "21", uid("uid-w-0-again"))).
		through(30 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3StillFiresOnAnOrphanTheRunRecreatedTheCRPast(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		record(13*time.Second, widget("20", spec(1), status(1, 1), uid("uid-w2"), finalizers(cleanup))).
		through(30 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the orphan the first CR left.", violation.Statement)
	}
}

// A deleteManaged op takes the object out of the target's hands, so whether
// the target would have cleaned it is nobody's to say (DESIGN.md §5.4, D38).
func TestG3DoesNotCreditACleanupReconcilerFuzzerPerformed(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		deletedManaged(13*time.Second, "w-0").
		remove(13*time.Second, child("w-0", "15", orphaned)).
		cleaned(13 * time.Second).
		through(21 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Violations) != 0 {
		t.Errorf("G3 reported %+v, and the target still had until its deadline.", result.Violations)
	}
	note := strings.Join(result.Notes, "\n")
	want := "for the deletion of w by op 0 (delete): op 1 (deleteManaged) deleted v1/ConfigMap w-0 within 10s (timeouts.delete) of it"
	if !strings.Contains(note, want) {
		t.Errorf("G3 noted %q, want it to contain %q.", note, want)
	}
}

// The window is the deletion's own. An op before the CR went, or after its
// deadline, says nothing about whether the target cleaned up (D38).
func TestG3NotesOnlyWhatReconcilerFuzzerTookInsideTheWindow(t *testing.T) {
	for _, taken := range []struct {
		name  string
		when  time.Duration
		notes int
	}{
		{"before the CR was deleted", 5 * time.Second, 0},
		{"at the instant the CR went", 10 * time.Second, 0},
		{"at the deadline", 20 * time.Second, 1},
		{"after the deadline", 21 * time.Second, 0},
	} {
		t.Run(taken.name, func(t *testing.T) {
			in := deletedRun().
				record(time.Second, child("w-0", "12", orphaned)).
				deletedManaged(taken.when, "w-0").
				record(taken.when+time.Second, child("w-0", "13", orphaned)).
				remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
				through(22 * time.Second)

			result := evaluate(t, invariant.CleanDeletion, in)

			if len(result.Notes) != taken.notes {
				t.Errorf("G3 noted %v, want %d: the op fell %s.", result.Notes, taken.notes, taken.name)
			}
			if len(result.Violations) != 1 {
				t.Errorf("G3 reported %d violations, want the orphan still there at the deadline.", len(result.Violations))
			}
		})
	}
}

// An object the run recreated carries the same name and a new UID.
// reconciler-fuzzer taking that one says nothing about the CR that went before
// it (D38), and this is the shape generation reaches: it draws deleteManaged
// only while a CR is live, which a recreate makes true again.
func TestG3DoesNotNoteAnObjectTheRunRecreated(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(11*time.Second, child("w-0", "13", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		record(13*time.Second, child("w-0", "15", orphaned, uid("uid-w-0-again"))).
		deletedManaged(14*time.Second, "w-0").
		remove(14*time.Second, child("w-0", "16", orphaned, uid("uid-w-0-again"))).
		cleaned(15 * time.Second).
		through(21 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Notes) != 0 {
		t.Errorf("G3 noted %v, and reconciler-fuzzer took the object that came after this CR.", result.Notes)
	}
	if len(result.Violations) != 0 {
		t.Errorf("G3 reported %v, and the CR's own child went on time.", result.Violations)
	}
}

// G3 notes only the objects the CR itself had when it went.
func TestG3NotesOnlyTheObjectsTheCRHad(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		op(invariant.OpRecreate, 13*time.Second).
		deletedManaged(14*time.Second, "w-9").
		through(22 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if len(result.Notes) != 0 {
		t.Errorf("G3 noted %v, and the CR never had the object reconciler-fuzzer took.", result.Notes)
	}
}

// twoWidgetsDeleted is a run of two Widgets with a child each, whose first
// reconciler-fuzzer deletes at 10s.
func twoWidgetsDeleted() *run {
	return newRun().withSecondWidget().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup)),
			secondWidget("20", spec(1), status(1, 1), finalizers(cleanup))).
		record(time.Second, child("w-0", "11"), secondChild("w2-0", "21")).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("12", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second)))
}

func TestG3LeavesTheChildrenOfAnotherCRToIt(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

func TestG3FiresOnTheLeftoversOfTheCRThatWent(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if want := "the v1/ConfigMap w-0 was still there 10s (timeouts.delete) after the CR w was deleted"; !strings.HasPrefix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to begin %q.", violation.Statement, want)
	}
}

func TestG3LeavesAChildAnotherCRStillOwnsToIt(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("shared", "15", ownedByBoth)).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

// An object that names no CR may be any CR's, so the last of them answers for
// it.
func TestG3JudgesAnObjectThatNamesNoCRWhereNoOtherCRRemains(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("kept", "15", orphaned)).
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		opOn(invariant.OpDelete, 30*time.Second, secondName).
		record(30*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(30*time.Second))).
		remove(31*time.Second, secondChild("w2-0", "23")).
		remove(32*time.Second, secondWidget("24", spec(1), status(1, 1), deleting(30*time.Second))).
		through(41 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "kept") || !violation.At.Equal(at(40*time.Second)) {
		t.Errorf("G3 reported %q at %v, want the object w2's deletion left, at its deadline.", violation.Statement, violation.At)
	}
}

// Each deleted CR answers for its own leftovers, even where both went.
func TestG3HoldsEachDeletedCRToItsOwnLeftovers(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		opOn(invariant.OpDelete, 10*time.Second, secondName).
		record(10*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(12*time.Second, secondWidget("24", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if !strings.Contains(violation.Statement, "w2-0") {
		t.Errorf("The statement is %q, want it to name w2's child.", violation.Statement)
	}
}

// The finalizer that holds a CR does not make its children another CR's.
func TestG3FiresOnTheChildrenOfACRItsFinalizerHolds(t *testing.T) {
	in := deletedRun().
		record(time.Second, child("w-0", "12")).
		through(21 * time.Second)

	if result := evaluate(t, invariant.CleanDeletion, in); len(result.Violations) != 2 {
		t.Errorf("G3 reported %v, want the finalizer and the child w-0.", statements(result))
	}
}

// An owner of another kind is not a CR, so its object names none.
func TestG3JudgesAnObjectWhoseOwnersAreNoCR(t *testing.T) {
	for _, c := range []struct {
		name  string
		owner option
	}{
		{"a Pod", ownedByAPod},
		{"a Widget of another group", ownedByAWidgetElsewhere},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := deletedRun().
				record(time.Second, child("w-0", "12", c.owner)).
				remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
				through(21 * time.Second)

			violation := fired(t, invariant.CleanDeletion, in)

			if !strings.Contains(violation.Statement, "w-0") {
				t.Errorf("The statement is %q, want it to name w-0.", violation.Statement)
			}
		})
	}
}

// Of the CRs an object may be, the last to go answers for it at its own
// deadline.
func TestG3LeavesAnObjectToTheLastOfItsCRsToGo(t *testing.T) {
	for _, c := range []struct {
		name  string
		owner option
	}{
		{"an object that names no CR", orphaned},
		{"an object that names both", ownedByBoth},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := twoWidgetsDeleted().
				record(time.Second, child("kept", "15", c.owner)).
				remove(11*time.Second, child("w-0", "13")).
				remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
				opOn(invariant.OpDelete, 19*time.Second, secondName).
				record(19*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(19*time.Second))).
				remove(19500*time.Millisecond, secondChild("w2-0", "23"),
					secondWidget("24", spec(1), status(1, 1), deleting(19*time.Second))).
				remove(21*time.Second, child("kept", "16", c.owner)).
				through(30 * time.Second)

			silent(t, invariant.CleanDeletion, in)
		})
	}
}

// The teardown deletes the CRs one after another.
func TestG3HoldsTheLastCRDeletedToAnObjectThatNamesNone(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("kept", "15", orphaned)).
		opOn(invariant.OpDelete, 10100*time.Millisecond, secondName).
		record(10100*time.Millisecond, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(11*time.Second, child("w-0", "13"), secondChild("w2-0", "23")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second)),
			secondWidget("24", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	violation := fired(t, invariant.CleanDeletion, in)

	if want := "the v1/ConfigMap kept was still there 10s (timeouts.delete) after w2, the last CR it may belong to, was deleted"; !strings.HasPrefix(violation.Statement, want) ||
		!violation.At.Equal(at(20100*time.Millisecond)) {
		t.Errorf("G3 reported %q at %v, want it to begin %q, at w2's deadline.", violation.Statement, violation.At, want)
	}
}

func TestG3JudgesAnObjectThatNamesNoCRWhereItsCRsWentAtOnce(t *testing.T) {
	in := twoWidgetsDeleted().
		record(time.Second, child("kept", "15", orphaned)).
		opOn(invariant.OpDelete, 10*time.Second, secondName).
		record(10*time.Second, secondWidget("22", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		remove(11*time.Second, child("w-0", "13"), secondChild("w2-0", "23")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second)),
			secondWidget("24", spec(1), status(1, 1), deleting(10*time.Second))).
		through(21 * time.Second)

	result := evaluate(t, invariant.CleanDeletion, in)

	if !slices.ContainsFunc(statements(result), func(s string) bool { return strings.Contains(s, "kept") }) {
		t.Errorf("G3 reported %v, want the object kept.", statements(result))
	}
}

// A CR that came after the deleted one and took the object on keeps it.
func TestG3LeavesAnObjectToACRThatAdoptedIt(t *testing.T) {
	in := deletedRun().withSecondWidget().
		record(time.Second, child("w-0", "12")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		opOn(invariant.OpCreate, 13*time.Second, secondName).
		record(13*time.Second, secondWidget("20", spec(1), status(1, 1))).
		record(14*time.Second, child("w-0", "15", ownedByBoth)).
		through(21 * time.Second)

	silent(t, invariant.CleanDeletion, in)
}

// w answers for an object that names no CR, since w2 goes first, and
// reconciler-fuzzer took the object inside w's window.
func TestG3NotesAnObjectReconcilerFuzzerTookFromTheLastOfItsCRs(t *testing.T) {
	in := newRun().withSecondWidget().
		record(0, widget("10", spec(1), status(1, 1), finalizers(cleanup)),
			secondWidget("20", spec(1), status(1, 1), finalizers(cleanup))).
		record(time.Second, child("kept", "11", orphaned)).
		opOn(invariant.OpDelete, 9*time.Second, secondName).
		record(9*time.Second, secondWidget("21", spec(1), status(1, 1), finalizers(cleanup), deleting(9*time.Second))).
		op(invariant.OpDelete, 10*time.Second).
		record(10*time.Second, widget("12", spec(1), status(1, 1), finalizers(cleanup), deleting(10*time.Second))).
		deletedManaged(11*time.Second, "kept").
		remove(11*time.Second, child("kept", "13", orphaned)).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second)),
			secondWidget("22", spec(1), status(1, 1), deleting(9*time.Second))).
		through(21 * time.Second)

	result := silent(t, invariant.CleanDeletion, in)

	want := "for the deletion of w by op 1 (delete): op 2 (deleteManaged) deleted v1/ConfigMap kept"
	if len(result.Notes) != 1 || !strings.Contains(result.Notes[0], want) {
		t.Errorf("G3 noted %v, want one note saying %q.", result.Notes, want)
	}
}

func TestG3NotesNothingReconcilerFuzzerTookFromAnotherCR(t *testing.T) {
	in := twoWidgetsDeleted().
		remove(11*time.Second, child("w-0", "13")).
		remove(12*time.Second, widget("14", spec(1), status(1, 1), deleting(10*time.Second))).
		deletedManaged(13*time.Second, "w2-0").
		remove(13*time.Second, secondChild("w2-0", "25")).
		record(14*time.Second, secondChild("w2-0", "26", uid("uid-w2-0-again"))).
		through(21 * time.Second)

	if notes := silent(t, invariant.CleanDeletion, in).Notes; len(notes) > 0 {
		t.Errorf("G3 noted %v, and reconciler-fuzzer took w2's child, not w's.", notes)
	}
}
