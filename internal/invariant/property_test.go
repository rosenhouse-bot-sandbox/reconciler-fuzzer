package invariant_test

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func property(when target.PropertyWhen, eval target.PropertyFunc) target.Property {
	return target.Property{
		ID:          "P1",
		Description: "status.ready never exceeds the number of ConfigMaps present.",
		Eval:        eval,
		When:        when,
	}
}

// claimed is a run whose CR reports two children it does not have from 1s to
// 3s, which only an event-by-event check sees.
func claimed(when target.PropertyWhen) invariant.Input {
	in := newRun().
		op(invariant.OpCreate, 0).
		running(500*time.Millisecond).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		record(3*time.Second, child("w-0", "11"), child("w-1", "12")).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(when, readyCountsChildren)}
	return in
}

func TestPropertyPassesAtACheckpointTheTargetHealedBefore(t *testing.T) {
	in := claimed(target.Checkpoint)

	silent(t, invariant.Property(in.Target.Properties[0]), in)
}

func TestPropertyFiresOnAnIntermediateStateWhenItIsEvaluatedAlways(t *testing.T) {
	in := claimed(target.Always)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if violation.ID != "P1" {
		t.Errorf("The violation is %q, want P1.", violation.ID)
	}
	if !violation.At.Equal(at(time.Second)) {
		t.Errorf("The violation is timestamped %v, want the event at 1s.", violation.At)
	}
	if !strings.Contains(violation.Statement, "never exceeds") {
		t.Errorf("The statement is %q, want it to carry the property's description.", violation.Statement)
	}
	if len(violation.Versions) != 1 || violation.Versions[0].GVK != widgetGVK {
		t.Fatalf("The evidence holds %v, want the state the property read.", violation.Versions)
	}
}

func TestPropertyFiresAtACheckpointThatStillBreaksIt(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		running(1100*time.Millisecond).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, readyCountsChildren)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if !violation.At.Equal(at(4 * time.Second)) {
		t.Errorf("The violation is timestamped %v, want the checkpoint at 4s.", violation.At)
	}
}

// A request the proxy held where the wait ended was about to change what the
// property reads.
func TestPropertyIsNotEvaluatedWhereTheProxyHeldARequest(t *testing.T) {
	for _, when := range []target.PropertyWhen{target.Checkpoint, target.End} {
		t.Run(string(when), func(t *testing.T) {
			in := newRun().
				op(invariant.OpDelete, 0).
				record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11")).
				running(1100*time.Millisecond).
				checkpoint(4*time.Second, invariant.Expired).held().
				through(8 * time.Second)
			in.Target.Properties = []target.Property{property(when, readyCountsChildren)}

			noted(t, invariant.Property(in.Target.Properties[0]), in,
				"P1 is not evaluated at the checkpoint after op 0 (delete): the proxy held a request of the target's there, or released one in the last 2s (timeouts.stable)")
		})
	}
}

// overcounted is a running target whose CR counts two children it does not
// have.
func overcounted(when target.PropertyWhen) *run {
	r := newRun().
		op(invariant.OpCreate, 0).
		running(100*time.Millisecond).
		record(time.Second, widget("10", spec(2), status(2, 1)))
	r.in.Target.Properties = []target.Property{property(when, readyCountsChildren)}
	return r
}

// A fault excuses the target where G4 excuses it, so it may not yet have
// repaired what the property reads.
func TestPropertyIsNotEvaluatedWhereAFaultExcusesTheTarget(t *testing.T) {
	for _, when := range []target.PropertyWhen{target.Checkpoint, target.End} {
		for name, r := range map[string]*run{
			"active where the wait expired":   overcounted(when).activeFault(500*time.Millisecond).checkpoint(4*time.Second, invariant.Expired),
			"active where the wait converged": overcounted(when).activeFault(500*time.Millisecond).checkpoint(4*time.Second, invariant.Converged),
			"stopped with time still owed":    overcounted(when).fault(500*time.Millisecond, 3*time.Second).checkpoint(4*time.Second, invariant.Expired),
		} {
			t.Run(string(when)+", "+name, func(t *testing.T) {
				in := r.through(8 * time.Second)

				noted(t, invariant.Property(in.Target.Properties[0]), in,
					"P1 is not evaluated at the checkpoint after op 0 (create): a fault was active there, or the target was still owed time to recover from one, so it may not yet have repaired what P1 reads")
			})
		}
	}
}

// The fault's 0.5s gives the target 0.5s and timeouts.settle past its end.
func TestPropertyFiresWhereTheTargetRecoveredFromAFault(t *testing.T) {
	for name, checkpoint := range map[string]struct {
		at     time.Duration
		result invariant.SettleResult
	}{
		"a wait that converged":             {4 * time.Second, invariant.Converged},
		"a wait that expired past its owed": {6500 * time.Millisecond, invariant.Expired},
	} {
		t.Run(name, func(t *testing.T) {
			in := overcounted(target.Checkpoint).
				fault(500*time.Millisecond, time.Second).
				checkpoint(checkpoint.at, checkpoint.result).
				through(8 * time.Second)

			violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

			if !violation.At.Equal(at(checkpoint.at)) {
				t.Errorf("The violation is timestamped %v, want the checkpoint at %v.", violation.At, checkpoint.at)
			}
		})
	}
}

// stale is a run whose CR counts a child reconciler-fuzzer deleted, which a
// target still starting has not yet seen go.
func stale(when target.PropertyWhen) *run {
	r := newRun().
		op(invariant.OpCreate, 0).
		running(0).
		record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11"), child("w-1", "12")).
		checkpoint(2*time.Second, invariant.Converged).
		deletedManaged(3*time.Second, "w-1").
		remove(3100*time.Millisecond, child("w-1", "13"))
	r.in.Target.Properties = []target.Property{property(when, readyCountsChildren)}
	return r
}

// A target still starting may not yet have acted on what the property reads.
func TestPropertyIsNotEvaluatedWhereTheTargetWasStillStarting(t *testing.T) {
	for _, when := range []target.PropertyWhen{target.Checkpoint, target.End} {
		for _, test := range []struct {
			name string
			run  *run
			want string
		}{
			{"waiting to restart", stale(when).exit(4*time.Second, 9*time.Second),
				"the target was waiting to restart"},
			{"waiting to restart under a fault", stale(when).activeFault(3500*time.Millisecond).exit(4*time.Second, 9*time.Second),
				"the target was waiting to restart"},
			{"exited at the checkpoint", stale(when).exit(8*time.Second, 9*time.Second),
				"the target was waiting to restart"},
			{"restarted in the last stable", stale(when).exit(4*time.Second, 6100*time.Millisecond).running(6200 * time.Millisecond),
				"the target restarted in the last 2s (timeouts.stable)"},
			{"restarted at the checkpoint", stale(when).exit(4*time.Second, 8*time.Second),
				"the target restarted in the last 2s (timeouts.stable)"},
			{"restarted stable before the checkpoint", stale(when).exit(4*time.Second, 6*time.Second),
				"the target had requested no resource outside leader election since the restart after its exit during op 1 (deleteManaged)"},
			{"after a supervised restart, with leader election alone", stale(when).exit(4*time.Second, 5*time.Second).
				requests(5100*time.Millisecond, time.Second, 3, lease("get")),
				"the target had won no lease since the restart after its exit during op 1 (deleteManaged)"},
			{"after a supervised restart, with a watch before it won its lease", stale(when).exit(4*time.Second, 5*time.Second).
				running(5100*time.Millisecond).
				requests(5200*time.Millisecond, time.Second, 3, lease("get")).
				request(8100*time.Millisecond, lease("update")),
				"the target had won no lease since the restart after its exit during op 1 (deleteManaged)"},
			{"won its lease in the last stable", stale(when).exit(4*time.Second, 5*time.Second).
				running(5100*time.Millisecond).
				request(5200*time.Millisecond, lease("get")).
				request(7900*time.Millisecond, lease("update")),
				"the target had won no lease since the restart after its exit during op 1 (deleteManaged) until the last 2s (timeouts.stable)"},
			{"after a supervised restart, with a request after the checkpoint alone", stale(when).exit(4*time.Second, 5*time.Second).running(8100 * time.Millisecond),
				"the target had requested no resource outside leader election since the restart after its exit during op 1 (deleteManaged)"},
			{"after a restart op", stale(when).op(invariant.OpRestart, 4*time.Second).op(invariant.OpSettle, 4*time.Second),
				"the target had requested no resource outside leader election since op 2 (restart)"},
			{"back in the last stable", stale(when).exit(4*time.Second, 5*time.Second).running(7900 * time.Millisecond),
				"the target had requested no resource outside leader election since the restart after its exit during op 1 (deleteManaged) until the last 2s (timeouts.stable)"},
			{"back at the checkpoint", stale(when).exit(4*time.Second, 5*time.Second).running(8 * time.Second),
				"the target had requested no resource outside leader election since the restart after its exit during op 1 (deleteManaged) until the last 2s (timeouts.stable)"},
		} {
			t.Run(string(when)+", "+test.name, func(t *testing.T) {
				in := test.run.checkpoint(8*time.Second, invariant.Expired).through(10 * time.Second)

				noted(t, invariant.Property(in.Target.Properties[0]), in,
					"P1 is not evaluated at the checkpoint after op "+strconv.Itoa(len(in.Ops)-1))
				noted(t, invariant.Property(in.Target.Properties[0]), in,
					": "+test.want+", so it may not yet have acted on what P1 reads")
			})
		}
		t.Run(string(when)+", at the teardown", func(t *testing.T) {
			in := stale(when).exit(7*time.Second, 9*time.Second).teardownCheckpoint(8 * time.Second).through(10 * time.Second)

			noted(t, invariant.Property(in.Target.Properties[0]), in,
				"P1 is not evaluated at the checkpoint after teardown: the target was waiting to restart, so it may not yet have acted on what P1 reads")
		})
	}
}

// A target that has never shown it runs is still starting too.
func TestPropertyIsNotEvaluatedWhereTheTargetHadNotShownItRuns(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		requests(100*time.Millisecond, time.Second, 4, lease("get")).
		checkpoint(5*time.Second, invariant.Expired).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, readyCountsChildren)}

	noted(t, invariant.Property(in.Target.Properties[0]), in,
		"P1 is not evaluated at the checkpoint after op 0 (create): the target had won no lease since it started")
}

// The property judges a target back for T_stable by the checkpoint, as a wait
// that converges does.
func TestPropertyFiresWhereTheTargetWasBack(t *testing.T) {
	for name, r := range map[string]*run{
		"back for stable": stale(target.Checkpoint).exit(4*time.Second, 5*time.Second).running(6 * time.Second),
		"back after a restart before the op": stale(target.Checkpoint).exit(2500*time.Millisecond, 2600*time.Millisecond).
			running(2700 * time.Millisecond),
		"exited after the checkpoint": stale(target.Checkpoint).exit(8100*time.Millisecond, 9*time.Second),
		"back once it won its lease": stale(target.Checkpoint).exit(4*time.Second, 5*time.Second).
			running(5100*time.Millisecond).
			request(5200*time.Millisecond, lease("get")).
			request(6*time.Second, lease("update")),
	} {
		t.Run(name, func(t *testing.T) {
			in := r.checkpoint(8*time.Second, invariant.Expired).through(10 * time.Second)

			violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

			if !violation.At.Equal(at(8 * time.Second)) {
				t.Errorf("The violation is timestamped %v, want the checkpoint at 8s.", violation.At)
			}
		})
	}
}

// A wait that converged saw the target back for T_stable. A restart op can
// replace a target waiting out its backoff, so the restart its exit scheduled
// never comes.
func TestPropertyFiresWhereAWaitConverged(t *testing.T) {
	in := stale(target.Checkpoint).
		exit(3500*time.Millisecond, 20*time.Second).
		op(invariant.OpRestart, 4*time.Second).
		running(4200*time.Millisecond).
		checkpoint(8*time.Second, invariant.Converged).
		through(10 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if !violation.At.Equal(at(8 * time.Second)) {
		t.Errorf("The violation is timestamped %v, want the checkpoint at 8s.", violation.At)
	}
}

// One checkpoint where the target was still starting leaves the property to
// the others.
func TestPropertyFiresAtACheckpointAfterOneWhereTheTargetWasStillStarting(t *testing.T) {
	in := stale(target.Checkpoint).
		exit(4*time.Second, 5*time.Second).
		checkpoint(8*time.Second, invariant.Expired).
		running(9*time.Second).
		op(invariant.OpSettle, 10*time.Second).
		checkpoint(12*time.Second, invariant.Expired).
		through(14 * time.Second)

	result := evaluate(t, invariant.Property(in.Target.Properties[0]), in)

	if len(result.Violations) != 1 || !result.Violations[0].At.Equal(at(12*time.Second)) || len(result.Notes) != 1 {
		t.Errorf("P1 reported %v and noted %q, want the checkpoint at 12s and a note on the one at 8s.", statements(result), result.Notes)
	}
}

// The property leaves to G4 a target that does not come back.
func TestATargetThatDoesNotComeBackStillFailsG4(t *testing.T) {
	in := stale(target.Checkpoint).
		op(invariant.OpRestart, 4*time.Second).
		op(invariant.OpSettle, 4*time.Second).
		checkpoint(9*time.Second, invariant.Expired).
		through(10 * time.Second)

	results, err := invariant.Evaluate(in)

	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	if fired := firingIDs(results); !slices.Equal(fired, []string{"G4"}) {
		t.Errorf("Evaluate reported %v, want G4 alone.", fired)
	}
	const want = "P1 is not evaluated at the checkpoint after op 3 (settle)"
	if !slices.ContainsFunc(results, func(r invariant.Result) bool {
		return r.ID == "P1" && len(r.Notes) == 1 && strings.HasPrefix(r.Notes[0], want)
	}) {
		t.Errorf("The checks found %+v, want P1 to note one checkpoint, beginning %q.", results, want)
	}
}

// A property evaluated on every event reads no checkpoint, so it judges a
// target still starting.
func TestPropertyEvaluatedAlwaysIgnoresWhereTheTargetWasStillStarting(t *testing.T) {
	in := stale(target.Always).
		exit(4*time.Second, 9*time.Second).
		checkpoint(8*time.Second, invariant.Expired).
		through(10 * time.Second)

	result := evaluate(t, invariant.Property(in.Target.Properties[0]), in)

	if len(result.Violations) != 1 || !result.Violations[0].At.Equal(at(3100*time.Millisecond)) || len(result.Notes) > 0 {
		t.Errorf("P1 reported %v and noted %q, want the event at 3.1s alone.", statements(result), result.Notes)
	}
}

// A fault's transient states are what a property evaluated on every event
// exists to catch.
func TestPropertyEvaluatedAlwaysReadsEventsUnderAFault(t *testing.T) {
	in := claimed(target.Always)
	in.Faults = []invariant.FaultWindow{{Start: at(500 * time.Millisecond)}}

	result := evaluate(t, invariant.Property(in.Target.Properties[0]), in)

	if len(result.Violations) != 1 || !result.Violations[0].At.Equal(at(time.Second)) || len(result.Notes) > 0 {
		t.Errorf("P1 reported %v and noted %q, want the event at 1s alone.", statements(result), result.Notes)
	}
}

// A property evaluated on every event reads no checkpoint.
func TestPropertyEvaluatedAlwaysIgnoresWhereTheProxyHeldARequest(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		record(3*time.Second, child("w-0", "11"), child("w-1", "12")).
		checkpoint(4*time.Second, invariant.Expired).held().
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Always, readyCountsChildren)}

	result := evaluate(t, invariant.Property(in.Target.Properties[0]), in)

	if len(result.Violations) != 1 || !result.Violations[0].At.Equal(at(time.Second)) || len(result.Notes) > 0 {
		t.Errorf("P1 reported %v and noted %q, want the event at 1s alone.", statements(result), result.Notes)
	}
}

func TestPropertyEvaluatedAtTheEndOfARunWithNoCheckpointJudgesNothing(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.End, readyCountsChildren)}

	if result := silent(t, invariant.Property(in.Target.Properties[0]), in); len(result.Notes) > 0 {
		t.Errorf("P1 noted %q, want nothing.", result.Notes)
	}
}

// One held request leaves the property to the other checkpoints.
func TestPropertyFiresAtACheckpointAfterOneWhereTheProxyHeldARequest(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		running(1100*time.Millisecond).
		checkpoint(2*time.Second, invariant.Expired).held().
		op(invariant.OpUpdate, 3*time.Second).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, readyCountsChildren)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if !violation.At.Equal(at(4 * time.Second)) {
		t.Errorf("The violation is timestamped %v, want the checkpoint at 4s.", violation.At)
	}
}

func TestPropertyEvaluatedAtTheEndReadsTheLastCheckpointOnly(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		running(1100*time.Millisecond).
		checkpoint(4*time.Second, invariant.Converged).
		record(5*time.Second, child("w-0", "11"), child("w-1", "12")).
		checkpoint(6*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.End, readyCountsChildren)}

	if result := silent(t, invariant.Property(in.Target.Properties[0]), in); len(result.Notes) > 0 {
		t.Errorf("P1 noted %q, want the last checkpoint judged.", result.Notes)
	}
}

func TestPropertyReadsTheManagedObjectsOnly(t *testing.T) {
	fixture := child("shared", "9", orphaned)
	var seen []string
	in := newRun().
		fixture(fixture).
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(1), status(1, 1)), fixture, child("w-0", "11")).
		running(1100*time.Millisecond).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint,
		func(_ *unstructured.Unstructured, managed []*unstructured.Unstructured) (bool, error) {
			for _, object := range managed {
				seen = append(seen, object.GetName())
			}
			return true, nil
		})}

	silent(t, invariant.Property(in.Target.Properties[0]), in)

	if len(seen) != 1 || seen[0] != "w-0" {
		t.Fatalf("The property read %v, want the managed ConfigMap alone.", seen)
	}
}

func TestPropertyReturnsAnEvaluationErrorAsAConfigurationError(t *testing.T) {
	in := claimed(target.Checkpoint)
	broken := errors.New("no such key: spec")
	in.Target.Properties = []target.Property{property(target.Checkpoint,
		func(*unstructured.Unstructured, []*unstructured.Unstructured) (bool, error) { return false, broken })}

	result, err := invariant.Property(in.Target.Properties[0])(in)

	if !errors.Is(err, broken) {
		t.Fatalf("Property returned %v, want the evaluation error.", err)
	}
	if len(result.Violations) > 0 {
		t.Errorf("Property reported %v, want no finding for a configuration error.", statements(result))
	}
}

func TestEvaluateSurvivesARunWithNoHistory(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Ops:         []invariant.Op{{Index: 0, Type: invariant.OpCreate, Time: at(0)}},
		Checkpoints: []invariant.Checkpoint{{Op: 0, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}

	results, err := invariant.Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate returned an error: %v", err)
	}
	if fired := firingIDs(results); len(fired) != 1 || fired[0] != "G4" {
		t.Fatalf("Evaluate reported %v, want the expired settle wait alone.", fired)
	}
}

// The teardown deletes the children before the CR's finalizer clears, so a
// property evaluated on every event sees a CR that outlived them
// (DESIGN.md §5.5, step 4).
func TestPropertyEvaluatedAlwaysIgnoresTheTeardownsOwnEvents(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1)), child("w-0", "11"), child("w-1", "12")).
		teardown(4*time.Second).
		remove(4100*time.Millisecond, child("w-0", "13"), child("w-1", "14")).
		through(6 * time.Second)
	in.Target.Properties = []target.Property{property(target.Always, readyCountsChildren)}

	silent(t, invariant.Property(in.Target.Properties[0]), in)
}

func TestPropertyEvaluatedAlwaysFiresOnAnEventTheTeardownCameAfter(t *testing.T) {
	in := claimed(target.Always)
	in.Teardown = at(6 * time.Second)

	fired(t, invariant.Property(in.Target.Properties[0]), in)
}

// A property about the children says how many there were and quotes their
// state beside the CR's timeline, one from each kind in turn and newest first
// (#24).
func TestAPropertyQuotesTheStateItSawBesideTheTimeline(t *testing.T) {
	r := newRunManaging(configMapGVK, secretGVK).op(invariant.OpCreate, 0).running(500 * time.Millisecond)
	for i := range 25 {
		r.record(time.Duration(1000+i)*time.Millisecond, secret("s-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	for i := range 3 {
		r.record(time.Duration(2000+i)*time.Millisecond, child("w-"+strconv.Itoa(i), strconv.Itoa(200+i)))
	}
	in := r.record(3*time.Second, widget("300", spec(40), status(40, 1))).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if violation.ManagedTotal == nil || *violation.ManagedTotal != 28 {
		t.Errorf("The violation counts %v managed objects, want the 28 the property saw.", managed(violation))
	}
	if len(violation.Managed) != invariant.MaxEvidence {
		t.Fatalf("The state holds %d objects, want the bound of %d.", len(violation.Managed), invariant.MaxEvidence)
	}
	if got := versionsOf(violation.Managed, configMapGVK); len(got) != 3 {
		t.Errorf("The state holds %d of the 3 ConfigMaps: %v", len(got), state(violation))
	}
	if first := state(violation)[0]; first != "w-2" {
		t.Errorf("The state opens at %s, want w-2, the object recorded last.", first)
	}
	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q.", violation.VersionsOf, want)
	}
	if got := versionsOf(violation.Versions, widgetGVK); len(got) != 1 {
		t.Errorf("The timeline holds %v, want the CR's history.", quoted(violation))
	}
}

// A property's evidence is bounded like any other, so it says how much it
// chose from (#22, #24).
func TestAPropertySaysHowMuchEvidenceItChoseFrom(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0).running(500 * time.Millisecond)
	for i := range 25 {
		r.record(time.Second, child("w-"+strconv.Itoa(i), strconv.Itoa(100+i)))
	}
	in := r.record(2*time.Second, widget("14", spec(30), status(30, 1))).
		checkpoint(5*time.Second, invariant.Converged).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Managed) != invariant.MaxEvidence {
		t.Errorf("The property quotes %d managed objects, want the bound of %d.", len(violation.Managed), invariant.MaxEvidence)
	}
	if violation.ManagedTotal == nil || *violation.ManagedTotal != 25 {
		t.Errorf("The property says the target managed %v objects, want the 25 it saw.", managed(violation))
	}
	if len(violation.Versions) != 1 || violation.VersionsTotal != 1 {
		t.Errorf("The timeline holds %v of %d, want the CR's one version.", quoted(violation), violation.VersionsTotal)
	}
}

// A property's timeline is the CR versions nearest the violation, as every
// timeline is (#24, D35).
func TestAPropertyQuotesTheCRVersionsNearestTheViolation(t *testing.T) {
	r := newRun().op(invariant.OpCreate, 0).running(50 * time.Millisecond)
	for i := range 25 {
		r.record(time.Duration(i)*100*time.Millisecond, widget(strconv.Itoa(10+i), spec(2), status(2, 1)))
	}
	in := r.checkpoint(5*time.Second, invariant.Converged).through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) != invariant.MaxEvidence {
		t.Fatalf("The timeline holds %d versions, want the bound of %d.", len(violation.Versions), invariant.MaxEvidence)
	}
	if last := violation.Versions[invariant.MaxEvidence-1]; last.ResourceVersion != "34" {
		t.Errorf("The timeline ends at resourceVersion %s, want the CR's latest, 34.", last.ResourceVersion)
	}
	if want := 25; violation.VersionsTotal != want {
		t.Errorf("The timeline says it chose from %d versions, want the %d the CR has.", violation.VersionsTotal, want)
	}
}

// A report of what the run looked like where the property failed cannot quote
// what came after it (#24).
func TestAPropertyQuotesNoVersionRecordedAfterTheViolation(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		running(500*time.Millisecond).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(3*time.Second, invariant.Converged).
		record(4*time.Second, widget("11", spec(2), status(2, 1))).
		through(8 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	for _, v := range violation.Versions {
		if v.Time.After(violation.At) {
			t.Errorf("The timeline quotes %s at %s, after the violation at %s.", v.Name, v.Time, violation.At)
		}
	}
	if violation.VersionsTotal != 1 {
		t.Errorf("The timeline says it chose from %d versions, want the 1 recorded by then.", violation.VersionsTotal)
	}
}

// A run the Observer never recorded has no CR and no timeline, and the
// property that failed is still reported (#24).
func TestAPropertyFiresOnARunWithNoHistory(t *testing.T) {
	in := invariant.Input{
		Target:      toyTarget(),
		Requests:    []proxy.Request{{Start: at(time.Second), Verb: "list", Version: "v1", Resource: "configmaps", Status: 200}},
		Checkpoints: []invariant.Checkpoint{{Op: 0, Time: at(5 * time.Second), Settle: invariant.Expired}},
		End:         at(5 * time.Second),
	}
	in.Target.Properties = []target.Property{property(target.Checkpoint, crExists)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) > 0 {
		t.Errorf("The timeline holds %v, want none: the run recorded nothing.", quoted(violation))
	}
}

// A property whose CR is gone has no timeline to quote, and the state is still
// the finding (#24).
func TestAPropertyThatFoundNoCRQuotesTheStateAlone(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, child("w-0", "11")).
		running(1100*time.Millisecond).
		checkpoint(4*time.Second, invariant.Converged).
		through(8 * time.Second)
	in.Target.Properties = []target.Property{property(target.Checkpoint, crExists)}

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if len(violation.Versions) > 0 {
		t.Errorf("The timeline holds %v, want none: the run has no CR.", quoted(violation))
	}
	if got := state(violation); len(got) != 1 || got[0] != "w-0" {
		t.Errorf("The state holds %v, want the child the property read.", got)
	}
	if want := "the property did not hold where no CR existed: "; !strings.HasPrefix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to begin %q.", violation.Statement, want)
	}
}

func TestAPropertyHoldsForEveryCR(t *testing.T) {
	in := newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("10", spec(1), status(1, 1)), secondWidget("20", spec(3), status(3, 1))).
		running(0).
		record(200*time.Millisecond, child("w-0", "11"), secondChild("w2-0", "21")).
		checkpoint(2*time.Second, invariant.Converged).
		through(2 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if want := timelineOf(widgetGVK, secondName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q, the CR the property failed on.", violation.VersionsOf, want)
	}
	if want := "the property did not hold on the CR w2: "; !strings.HasPrefix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to begin %q.", violation.Statement, want)
	}
}

// A CR's property reads the objects that name it and those that name no CR.
func TestAPropertyReadsTheObjectsOfItsCR(t *testing.T) {
	in := newRun().withSecondWidget().
		op(invariant.OpCreate, 0).
		record(100*time.Millisecond, widget("10", spec(2), status(2, 1)), secondWidget("20", spec(2), status(2, 1))).
		running(0).
		record(200*time.Millisecond, child("w-0", "11"), secondChild("w2-0", "21"), secondChild("w2-1", "22")).
		checkpoint(2*time.Second, invariant.Converged).
		through(2 * time.Second)

	violation := fired(t, invariant.Property(in.Target.Properties[0]), in)

	if want := timelineOf(widgetGVK, widgetName); violation.VersionsOf != want {
		t.Errorf("The timeline is of %q, want %q, whose one child the property counted.", violation.VersionsOf, want)
	}
	if got := state(violation); !slices.Equal(got, []string{"w-0"}) {
		t.Errorf("The violation quotes %v, want w's own child.", got)
	}
}

// crExists is a property of the CR itself, which a run that has none breaks.
func crExists(cr *unstructured.Unstructured, _ []*unstructured.Unstructured) (bool, error) {
	return cr != nil, nil
}
