//go:build envtest

package run_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// seededBugB11 selects the bug of DESIGN.md §9.1 that a refused create makes
// permanent. A later --bug wins over the one target.yaml declares (§11).
const seededBugB11 = "--bug=11"

// The acceptance of DESIGN.md §10 M6: a fault makes the toy fail an invariant
// it passes without the fault.
//
// The fault refuses one ConfigMap create, and B11 goes on believing in the
// child it never made. The toy falls quiet one child short of its spec, so the
// settle wait after the scale-up expires however wide the windows are: neither
// a backoff interval nor a window width enters into the outcome. The fault's
// own count ends it, so the run is judged from the refusal on (D36).
func TestAFaultMakesTheToyFailAnInvariantItOtherwisePasses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// One target for both runs, so that the fault is the only difference.
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, seededBugB11)
	testCluster := startCluster(t, toy.CRDs)
	dir := t.TempDir()
	faulted, err := run.ReadSequence("../../targets/toy-widget/sequences/b11-fault.json")
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}

	found, err := run.Run(ctx, toy, faulted, run.Options{
		Dir: filepath.Join(dir, "faulted"), Config: testCluster.Config(), Check: run.Engine{},
	})
	if err != nil {
		t.Fatalf("The faulted run failed: %v", err)
	}
	if found.Violation == nil {
		t.Fatal("The faulted run found nothing, and the create the fault refused leaves B11 a child short for good.")
	}
	// Pin the check. If the fault starts tripping something else, the test
	// would otherwise pass for a reason it does not describe.
	if found.Violation.ID != "G4" {
		t.Errorf("The faulted run reported %s, and this sequence is written to break G4: %s",
			found.Violation.ID, found.Violation.Evidence)
	}
	// The toy asked the API server for its one child once and never again,
	// which is what makes the state permanent rather than slow to recover.
	if asked, denied := configMapCreates(found.Recorded.Requests); asked != 1 || denied != 1 {
		t.Errorf("The toy made %d creates of configmaps, %d of them refused, want the one child asked for once and refused.",
			asked, denied)
	}
	// §10 M6 asks the report to name the evidence, and this is the run that
	// shows it: the create the fault refused is in what the violation carries.
	if !slices.ContainsFunc(found.Violation.Requests, refusedCreate) {
		t.Errorf("The violation carries %d requests, none of them the create the fault refused.", len(found.Violation.Requests))
	}
	if len(found.Violation.Versions) == 0 {
		t.Error("The violation carries no version of the CR, and its report has a table to quote them in.")
	}

	// The control is this sequence with the fault taken out, so nothing but the
	// fault can account for the difference.
	clean, err := run.Run(ctx, toy, without(faulted, run.OpFault), run.Options{
		Dir: filepath.Join(dir, "clean"), Config: testCluster.Config(), Check: run.Engine{},
	})
	if err != nil {
		t.Fatalf("The control run failed: %v", err)
	}
	if clean.Violation != nil {
		t.Errorf("The control reports %+v, so the %s the faulted run found is not the fault's.",
			clean.Violation, found.Violation.ID)
	}
}

// The toy with no bug retries a refused create, backing off as it goes, so it
// recovers once the fault stops, however the fault stopped. B11 never asks
// again. fault.json is docs/targets.md's example: the teardown clears its fault.
func TestAFaultLeavesTheTargetTimeToRecover(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	const example = "../../targets/toy-widget/sequences/fault.json"
	runUnder := func(t *testing.T, launchArgs []string, sequence run.Sequence) run.Result {
		t.Helper()
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, launchArgs...)
		result, err := run.Run(ctx, toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		return result
	}
	readExample := func(t *testing.T) run.Sequence {
		t.Helper()
		sequence, err := run.ReadSequence(example)
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}
		return sequence
	}

	t.Run("after the teardown clears the fault", func(t *testing.T) {
		result := runUnder(t, nil, readExample(t))

		if result.Violation != nil {
			t.Errorf("The run reported %v, and the toy with no bug recovers.", result.Violation)
		}
		if recovery := result.Timeline.Recovery; recovery == nil || !recovery.Converged {
			t.Errorf("The teardown recorded the recovery %+v, want a wait that converged.", recovery)
		}
	})

	t.Run("after the fault runs out inside a settle wait", func(t *testing.T) {
		// Ten refusals back the toy off for 2.56s before its next create,
		// which then needs T_stable of quiet: more than T_settle after the
		// update.
		sequence := readExample(t)
		sequence.Ops[1].Fault.Until.Count = 10

		result := runUnder(t, nil, sequence)

		if result.Violation != nil {
			t.Errorf("The run reported %v, and the toy with no bug recovers.", result.Violation)
		}
		settle := loadTarget(t, binary).Timeouts.Settle
		if wait := result.Timeline.Ops[2].Settled; wait == nil || wait.Window.End.Sub(wait.Window.Start) <= settle {
			t.Errorf("The update's wait was %+v, want one that ran past T_settle of %v.", wait, settle)
		}
	})

	t.Run("never, under a bug that never asks again", func(t *testing.T) {
		result := runUnder(t, []string{seededBugB11}, readExample(t))

		if result.Violation == nil || result.Violation.ID != "G4" ||
			!strings.Contains(result.Violation.Statement, "after the last fault stopped") {
			t.Errorf("The run reported %v, want the G4 of the wait after the last fault stopped.", result.Violation)
		}
	})
}

// The reference's example holds every op and fault field. A correct
// controller passes it, and each of its faults applies to a request. The
// faults leave no check unjudged but P1, which skips the checkpoints where a
// fault excuses the target, the one after op 7 among them: a fault drops the
// deletes op 7 asks for until op 8.
func TestTheReferenceSequencePassesTheToy(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	testCluster := startCluster(t, toy.CRDs)
	sequence, err := run.ReadSequence(repoRoot + "/docs/reference/sequence.json")
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}

	result, err := run.Run(t.Context(), toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %s: %s", result.Violation.ID, result.Violation.Statement)
	}
	for _, note := range result.Notes {
		if !strings.HasPrefix(note, "P1 is not evaluated at the checkpoint after op ") || !strings.Contains(note, faultExcused) {
			t.Errorf("The run left a check unjudged where no fault excused P1: %q", note)
		}
	}
	if want := "P1 is not evaluated at the checkpoint after op 7 (update): " + faultExcused; !slices.ContainsFunc(result.Notes, startsWith(want)) {
		t.Errorf("The run noted %q, want one beginning %q.", result.Notes, want)
	}
	if got := len(result.Timeline.Faults); got != 3 {
		t.Errorf("The run injected %d faults, want 3.", got)
	}
	for i, window := range result.Timeline.Faults {
		if window.Start.IsZero() {
			t.Errorf("The proxy applied fault %d to no request.", i)
		}
	}
}

// faultExcused is why a property skips a checkpoint where a fault excuses the
// target.
const faultExcused = "a fault was active there, or the target was still owed time to recover from one"

func startsWith(prefix string) func(string) bool {
	return func(note string) bool { return strings.HasPrefix(note, prefix) }
}

// keptCR fails the toy's patches of its Widget until after a recreate. The toy
// deletes the Widget's child, and the patch that clears its finalizer fails,
// so the Widget stays with status.ready 1.
const keptCR = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "patch", "resource": "widgets"}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "recreate", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 3, "t": "settle"}
  ]
}`

// lostChild refuses the toy's ConfigMap creates while reconciler-fuzzer deletes
// a child, so status.ready counts the child until the update after the fault.
const lostChild = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 3, "t": "update", "patch": {"spec": {"count": 3}}}
  ]
}`

// The correct toy passes where a fault keeps it from repairing what P1 reads,
// and the run notes where P1 was not evaluated.
func TestTheCorrectToyPassesAFaultThatKeepsWhatP1Reads(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	runToy := func(t *testing.T, sequence string, noted ...string) run.Result {
		t.Helper()
		result, err := run.Run(t.Context(), loadTarget(t, binary), readSequence(t, sequence),
			run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
		}
		for _, want := range noted {
			if !slices.ContainsFunc(result.Notes, startsWith(want)) {
				t.Errorf("The run noted %q, want one beginning %q.", result.Notes, want)
			}
		}
		return result
	}

	t.Run("a recreate whose old CR the fault keeps", func(t *testing.T) {
		t.Parallel()

		result := runToy(t, keptCR,
			"op 2 (recreate) stopped the run: the CR widget was still there",
			"P1 is not evaluated at the checkpoint after op 2 (recreate): "+faultExcused)

		if recovery := result.Timeline.Recovery; recovery == nil || !recovery.Converged {
			t.Errorf("The teardown recorded the recovery %+v, want a wait that converged.", recovery)
		}
	})

	t.Run("a wait that converged under the fault", func(t *testing.T) {
		t.Parallel()

		result := runToy(t, lostChild, "P1 is not evaluated at the checkpoint after op 2 (deleteManaged): "+faultExcused)

		if wait := result.Timeline.Ops[2].Settled; wait == nil || !wait.Converged {
			t.Errorf("The deleteManaged's wait was %+v, want one that converged.", wait)
		}
	})
}

// refusedAcrossARecreate refuses the toy's ConfigMap creates from before a
// scale-up until after a recreate.
const refusedAcrossARecreate = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"}, "action": {"error": 500}, "until": {"op": 4}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 2}}},
    {"i": 3, "t": "recreate", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 4, "t": "settle"}
  ]
}`

// A fault active during a recreate excuses a finalizer that never clears, and
// stops the run. The teardown clears the fault, and its recovery wait blames
// the finalizer.
func TestAFinalizerThatNeverClearsFailsG4OnceAFaultThatStoppedARecreateIsCleared(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	runUnder := func(t *testing.T, launchArgs ...string) run.Result {
		t.Helper()
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, launchArgs...)
		result, err := run.Run(t.Context(), toy, readSequence(t, refusedAcrossARecreate),
			run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})
		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		return result
	}

	t.Run("B13", func(t *testing.T) {
		t.Parallel()

		result := runUnder(t, "--bug=13")

		if v := result.Violation; v == nil || v.ID != "G4" || !strings.Contains(v.Statement, "after the last fault stopped") ||
			!strings.Contains(v.Statement, "held by the finalizers widget.reconciler-fuzzer/cleanup") {
			t.Errorf("The run reported %v, want the G4 of the wait after the last fault stopped, naming the finalizer.", v)
		}
	})

	t.Run("the correct toy", func(t *testing.T) {
		t.Parallel()

		if result := runUnder(t); result.Violation != nil {
			t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
		}
	})
}

// leaseFault fails most of the toy's lease updates until the teardown.
const leaseFault = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "update", "resource": "leases", "fraction": 0.8}, "action": {"error": 500}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 2}}},
    {"i": 3, "t": "update", "patch": {"spec": {"count": 3}}},
    {"i": 4, "t": "settle"},
    {"i": 5, "t": "settle"}
  ]
}`

// The toy with no bug exits whenever it loses its lease, and reconciler-fuzzer
// restarts it. Once the teardown clears the fault, it recovers.
func TestAToyThatLosesItsLeaseUnderAFaultPasses(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, "--lease=3s")
	testCluster := startCluster(t, toy.CRDs)
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := run.Run(ctx, toy, readSequence(t, leaseFault), run.Options{
		Dir: dir, Config: testCluster.Config(), Check: run.Engine{},
	})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %v, and the toy with no bug recovers.", result.Violation)
	}
	exits := len(result.Timeline.Exits)
	if exits == 0 {
		t.Error("The toy never exited, want it to lose its lease.")
	}
	// The toy's last line races the manager's own shutdown lines.
	logged, err := os.ReadFile(filepath.Join(dir, "target.log"))
	if lost := strings.Count(string(logged), "leader election lost"); err != nil || lost < exits {
		t.Errorf("The toy exited %d times and wrote that it lost its lease %d times (%v).", exits, lost, err)
	}
	if recovery := result.Timeline.Recovery; recovery == nil || !recovery.Converged {
		t.Errorf("The teardown recorded the recovery %+v, want a wait that converged.", recovery)
	}
}

// leaseSlow fails most of the toy's lease updates and holds its ConfigMap
// creates, then deletes a child op after op. A restarted toy can take longer
// than timeouts.settle to win its lease back, and until then it recreates
// nothing.
const leaseSlow = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "update", "resource": "leases", "fraction": 0.8}, "action": {"error": 500}}},
    {"i": 2, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"}, "action": {"delay": "1.5s"}}},
    {"i": 3, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 4, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 5, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 7, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 8, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0}
  ]
}`

// The toy with no bug passes where a wait ends before it has won its lease
// back, and the run notes the properties it did not judge there.
func TestAToyThatWinsItsLeaseBackLatePasses(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, "--lease=3s")
	testCluster := startCluster(t, toy.CRDs)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := run.Run(ctx, toy, readSequence(t, leaseSlow), run.Options{
		Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
	})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
	}
	if !slices.ContainsFunc(result.Notes, func(note string) bool {
		return strings.HasPrefix(note, "P1 is not evaluated at the checkpoint after op ") && strings.Contains(note, "may not yet have acted")
	}) {
		t.Errorf("The run noted %q, want P1 left unjudged where the toy was still starting.", result.Notes)
	}
}

// A toy with a field index watches Widgets before it leads. The toy with no
// bug passes, because reconciler-fuzzer counts it back only once it wins its
// lease.
func TestAToyThatWatchesBeforeItLeadsPasses(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, "--lease=3s", "--index")
	testCluster := startCluster(t, toy.CRDs)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	result, err := run.Run(ctx, toy, readSequence(t, leaseSlow), run.Options{
		Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
	})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
	}
	requests := result.Recorded.Requests
	widgets := slices.IndexFunc(requests, func(r proxy.Request) bool { return r.Resource == "widgets" })
	leases := slices.IndexFunc(requests, func(r proxy.Request) bool { return r.Resource == "leases" })
	if widgets < 0 || leases < 0 || widgets > leases {
		t.Errorf("The toy first requested Widgets at request %d and leases at request %d, want Widgets first.", widgets, leases)
	}
}

// A fault excuses every exit while it is active, and this one never stops. The
// crash loop still fails G4 once the teardown clears the fault.
func TestACrashLoopUnderAFaultThatNeverStopsFailsG4(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	toy.Launch.Args = append(toy.Launch.Args, "--bug=12")
	testCluster := startCluster(t, toy.CRDs)
	sequence, err := run.ReadSequence("../../targets/toy-widget/sequences/b12-fault.json")
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}
	// A wait that owed every exit would still be open here.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	result, err := run.Run(ctx, toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation == nil || result.Violation.ID != "G4" ||
		!strings.Contains(result.Violation.Statement, "after the last fault stopped") {
		t.Errorf("The run reported %v, want the G4 of the wait after the last fault stopped.", result.Violation)
	}
}

// heldCreate has the proxy hold the toy's ConfigMap creates for longer than
// T_stable, then deletes a child. Until the proxy releases the create of its
// replacement, status.ready counts a child that is not there.
const heldCreate = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps"}, "action": {"delay": "3s"}}},
    {"i": 2, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0}
  ]
}`

// heldReads has the proxy hold the toy's watches of ConfigMaps, which it
// makes as it restarts, and its lists of them, which it makes as it cleans up
// after a deleted Widget.
const heldReads = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "list", "resource": "configmaps"}, "action": {"delay": "1s"}}},
    {"i": 2, "t": "fault", "spec": {"match": {"verb": "watch", "resource": "configmaps"}, "action": {"delay": "1s"}}},
    {"i": 3, "t": "restart"},
    {"i": 4, "t": "settle"},
    {"i": 5, "t": "delete"}
  ]
}`

// The correct toy passes, and each wait converges, no sooner than T_stable
// after the proxy released every request it held, however long a held watch
// then streams.
func TestASettleWaitOutlastsTheRequestsTheProxyHolds(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	spendsItsCount := readSequence(t, heldCreate)
	spendsItsCount.Ops[1].Fault.Until.Count = 1
	pastSettle := readSequence(t, heldCreate)
	pastSettle.Ops[1].Fault.Action.Delay = run.Duration(6 * time.Second)
	for _, test := range []struct {
		name     string
		sequence run.Sequence
		delay    time.Duration
	}{
		{"a create held while a child is gone", readSequence(t, heldCreate), 3 * time.Second},
		{"a create that spends its fault's count", spendsItsCount, 3 * time.Second},
		{"a create held past timeouts.settle", pastSettle, 6 * time.Second},
		{"watches and lists", readSequence(t, heldReads), time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			toy := loadTarget(t, binary)

			result, err := run.Run(t.Context(), toy, test.sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if result.Violation != nil {
				t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
			}
			for i, window := range result.Timeline.Faults {
				if window.Start.IsZero() {
					t.Errorf("The proxy held no request with fault %d.", i)
				}
			}
			for _, op := range result.Timeline.Ops {
				if wait := op.Settled; wait != nil {
					requireQuietAfterHolds(t, op.Op.Index, *wait, result.Recorded.Requests, test.delay, toy.Timeouts.Stable)
				}
			}
		})
	}
}

// heldDeletes has the proxy hold each of the toy's ConfigMap requests for
// longer than timeouts.settle, then deletes the Widget. The toy deletes its
// children one held request after another, so the wait after the delete ends
// with one held.
const heldDeletes = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"resource": "configmaps"}, "action": {"delay": "6s"}}},
    {"i": 2, "t": "delete"}
  ]
}`

// heldChildDeletes has the proxy hold the toy's ConfigMap deletes for
// timeouts.settle, then deletes a Widget with three children. The wait after
// the delete ends as the proxy releases the third delete, before the toy
// removes the Widget's finalizer.
const heldChildDeletes = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "delete", "resource": "configmaps"}, "action": {"delay": "5s"}}},
    {"i": 2, "t": "delete"}
  ]
}`

// heldFinalizer has the proxy hold the toy's patch that clears its Widget's
// finalizer for longer than timeouts.delete, which a recreate waits for the
// Widget to go.
const heldFinalizer = `{
  "seed": 23,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 2}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "patch", "resource": "widgets"}, "action": {"delay": "12s"}, "until": {"count": 1}}},
    {"i": 2, "t": "recreate", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}}
  ]
}`

// The correct toy passes where a wait ends with a request held, or just
// released.
func TestTheCorrectToyPassesWhereAWaitEndsWithARequestHeld(t *testing.T) {
	t.Parallel()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)
	for _, test := range []struct {
		name, sequence string
		// noted is what the run notes, if anything.
		noted string
	}{
		{"a delete's wait", heldDeletes, "P1 is not evaluated at the checkpoint after op 2 (delete)"},
		{"a delete's wait that ends as the proxy releases a request", heldChildDeletes, "P1 is not evaluated at the checkpoint after op 2 (delete)"},
		{"a recreate's wait for its CR", heldFinalizer, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			result, err := run.Run(t.Context(), loadTarget(t, binary), readSequence(t, test.sequence),
				run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if result.Violation != nil {
				t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
			}
			if test.noted != "" && !slices.ContainsFunc(result.Notes, func(note string) bool { return strings.HasPrefix(note, test.noted) }) {
				t.Errorf("The run noted %q, want one beginning %q.", result.Notes, test.noted)
			}
		})
	}
}

// informerFault restarts the toy into a fault on ConfigMaps. Its ConfigMap
// informer backs off past the fault, while the waits converge on the Widget.
const informerFault = `{
  "seed": 20260924,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "restart"},
    {"i": 2, "t": "fault", "spec": {"match": {"resource": "configmaps"}, "action": {"error": 500}, "until": {"for": "4s"}}},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "settle"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0}
  ]
}`

// The wait after the deleteManaged lasts until the toy has had as long as the
// fault lasted, and T_settle more, to see the deletion.
func TestTheCorrectToyRecreatesAChildItsInformerMissedUnderAFault(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	testCluster := startCluster(t, toy.CRDs)

	result, err := run.Run(t.Context(), toy, readSequence(t, informerFault),
		run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	if result.Violation != nil {
		t.Errorf("The run reported %s at %v: %s", result.Violation.ID, result.Violation.At, result.Violation.Statement)
	}
	fault := result.Timeline.Faults[0]
	owed := fault.End.Add(fault.End.Sub(fault.Start) + toy.Timeouts.Settle)
	if wait := result.Timeline.Ops[6].Settled; wait == nil || wait.Window.End.Before(owed) {
		t.Errorf("The wait after the deleteManaged was %+v, want one that ended no sooner than %v.", wait, owed)
	}
	if slices.ContainsFunc(result.Notes, func(note string) bool { return strings.HasPrefix(note, "G7") }) {
		t.Errorf("The run noted %q, want G7 judged.", result.Notes)
	}
}

// requireQuietAfterHolds requires a wait that converged T_stable after the
// proxy released each request it held for delay that arrived before the end.
func requireQuietAfterHolds(t *testing.T, op int, wait run.Wait, log []proxy.Request, delay, stable time.Duration) {
	t.Helper()
	if !wait.Converged {
		t.Errorf("The wait after op %d expired at %v.", op, wait.Window.End)
	}
	for _, request := range log {
		released := request.Start.Add(delay)
		if strings.HasPrefix(request.Fault, "delay") && request.Start.Before(wait.Window.End) && wait.Window.End.Before(released.Add(stable)) {
			t.Errorf("The wait after op %d ended at %v, less than T_stable after the proxy released the %s %s it held at %v.",
				op, wait.Window.End, request.Verb, request.Path, released)
		}
	}
}

// The fault names the toy's CRD, which the API server serves as widgets.
func TestAFaultOnAKindNameEndsTheRun(t *testing.T) {
	t.Parallel()
	toy := loadTarget(t, buildToy(t))
	testCluster := startCluster(t, toy.CRDs)
	sequence := run.Sequence{Seed: 1, Target: toy.Name, Ops: []run.Op{
		{Index: 0, Type: run.OpCreate, Obj: toy.Sample.DeepCopy()},
		{Index: 1, Type: run.OpFault, Fault: &run.Fault{Match: run.Match{Resource: "Widget"}, Action: run.Action{Error: 500}}},
		{Index: 2, Type: run.OpSettle},
	}}

	_, err := run.Run(t.Context(), toy, sequence, run.Options{Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{}})

	if err == nil || !strings.Contains(err.Error(), "did you mean widgets?") {
		t.Errorf("The run returned %v, want it to name widgets.", err)
	}
}

// configMapCreates counts what the toy asked the API server to create and how
// many of those the proxy refused.
func configMapCreates(log []proxy.Request) (asked, denied int) {
	for _, request := range log {
		if !configMapCreate(request) {
			continue
		}
		asked++
		if refusedCreate(request) {
			denied++
		}
	}
	return asked, denied
}

func configMapCreate(request proxy.Request) bool {
	return request.Verb == "create" && request.Resource == "configmaps"
}

// refusedCreate is the create the fault answered with a 500.
func refusedCreate(request proxy.Request) bool {
	return configMapCreate(request) && request.Fault != ""
}

// without is the sequence with every op of that type removed, renumbered so it
// stays legal (DESIGN.md §7).
func without(s run.Sequence, remove run.OpType) run.Sequence {
	shorter := s
	shorter.Ops = nil
	for _, op := range s.Ops {
		if op.Type == remove {
			continue
		}
		op.Index = len(shorter.Ops)
		shorter.Ops = append(shorter.Ops, op)
	}
	return shorter
}
