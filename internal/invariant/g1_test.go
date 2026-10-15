package invariant_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

// G1 ignores a watch whether it hung or failed; G6 counts the one that failed.
// Leader election reads its lease as well as writing it, and renews a lease
// candidate. G1 excludes all of these.
func TestG1PassesWhenOnlyWatchesAndLeaseTrafficRemain(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, watch()).
		request(3200*time.Millisecond, failedWatch(429)).
		request(3500*time.Millisecond, lease("update")).
		request(3700*time.Millisecond, lease("get")).
		request(3900*time.Millisecond, leaseCandidate("update")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1CountsAResourceNamedLeasesInAnotherGroup(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, leasesElsewhere()).
		through(14 * time.Second)

	fired(t, invariant.BoundedReconciliation, in)
}

// A health probe, and the discovery reads behind a RESTMapper refresh, name no
// resource, so none of them is reconciliation (DESIGN.md §6).
func TestG1IgnoresRequestsThatNameNoResource(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, nonResource("/livez/ping")).
		request(3200*time.Millisecond, nonResource("/apis")).
		request(3400*time.Millisecond, nonResource("/apis/toy.reconciler-fuzzer/v1")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// The window opens where the settle wait ended, which for a target that
// converges is well inside T_settle (DESIGN.md §6).
func TestG1FiresOnARequestInTheQuietWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if violation.ID != "G1" {
		t.Errorf("The violation is %q, want G1.", violation.ID)
	}
	if len(violation.Requests) != 1 || violation.Requests[0].Name != "w-0" {
		t.Fatalf("The evidence holds %v, want the get of w-0.", violation.Requests)
	}
	if want := "the target made 1 API request in the 2s (timeouts.stable) after op 0 (create) settled, " +
		"where thresholds.quiet allows 0"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
}

// A target that resyncs on a timer declares how many requests one window may
// hold.
func TestG1PassesAtTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, time.Second, 2, get("w-0")).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1FiresPastTheQuietThreshold(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, 400*time.Millisecond, 4, get("w-0")).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	violation := fired(t, invariant.BoundedReconciliation, in)

	if want := "made 4 API requests"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to say it %s.", violation.Statement, want)
	}
	if want := "thresholds.quiet allows 2"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to name the threshold: %q.", violation.Statement, want)
	}
	if want := at(3300 * time.Millisecond); !violation.At.Equal(want) {
		t.Errorf("The violation is at %v, want the request that went past the threshold, at %v.", violation.At, want)
	}
}

// Only the loader refuses a negative quiet, so a target built in code can
// carry one.
func TestG1ReadsANegativeQuietAsZero(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = -1

	violation := fired(t, invariant.BoundedReconciliation, in)

	if want := "thresholds.quiet allows 0"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to name the threshold: %q.", violation.Statement, want)
	}
}

// The threshold bounds one window, not the run.
func TestG1AppliesTheQuietThresholdToEachWindowApart(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		requests(2500*time.Millisecond, time.Second, 2, get("w-0")).
		op(invariant.OpUpdate, 5*time.Second).
		settled(6*time.Second, invariant.Converged).
		requests(6500*time.Millisecond, time.Second, 2, get("w-0")).
		through(14 * time.Second)
	in.Target.Thresholds.Quiet = 2

	silent(t, invariant.BoundedReconciliation, in)
}

// A settle wait that expires ends T_settle after the op, and the window
// follows it there.
func TestG1FiresOnARequestAfterAnExpiredSettle(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(settleTimeout, invariant.Expired).
		request(settleTimeout+time.Second, get("w-0")).
		through(14 * time.Second)

	fired(t, invariant.BoundedReconciliation, in)
}

// A target still working towards convergence is not yet held to §6's quiet.
func TestG1IgnoresTrafficBeforeTheSettleWaitEnded(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(0, 400*time.Millisecond, 5, get("w-0")).
		settled(2*time.Second, invariant.Converged).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// T_settle after the op is not itself a window: a run whose settle wait has
// not ended has nothing to judge.
func TestG1JudgesNothingUntilASettleWaitEnds(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(settleTimeout+time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowTheRunDidNotReach(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(3500 * time.Millisecond)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowAnotherOpCutShort(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		op(invariant.OpUpdate, 3*time.Second).
		request(3500*time.Millisecond, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// A fault anywhere between the op and the window's close makes the reaction
// the fault's, not the target's.
func TestG1IgnoresAWindowAFaultPreceded(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		fault(1*time.Second, 1500*time.Millisecond).
		settled(2*time.Second, invariant.Converged).
		request(3*time.Second, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1CountsEveryNonWatchRequestInTheWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		settled(2*time.Second, invariant.Converged).
		requests(2100*time.Millisecond, 150*time.Millisecond, 10, get("w-0")).
		through(14 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if !strings.Contains(violation.Statement, "10") {
		t.Errorf("The statement is %q, want it to count the 10 requests.", violation.Statement)
	}
}

// The T_stable the teardown waits before it deletes is a quiet window of its
// own (DESIGN.md §5.5 step 4), so a sequence whose last op never settles is
// still judged.
func TestG1JudgesTheTeardownWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpRestart, 0).
		quiet(5*time.Second).
		request(6*time.Second, get("w-0")).
		through(14 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if want := "the target made 1 API request in the 2s (timeouts.stable) the teardown waited before deleting, " +
		"where thresholds.quiet allows 0"; violation.Statement != want {
		t.Errorf("The statement is %q, want %q.", violation.Statement, want)
	}
}

// The teardown clears every fault before it opens its window, so a fault that
// ended there did not reach into it; one that outlived the clearing did.
func TestG1JudgesTheTeardownWindowByWhenTheFaultsStopped(t *testing.T) {
	for _, fault := range []struct {
		stopped string
		to      time.Duration
		judged  bool
	}{
		{stopped: "as the window opened", to: 5 * time.Second, judged: true},
		{stopped: "inside the window", to: 5500 * time.Millisecond, judged: false},
	} {
		t.Run(fault.stopped, func(t *testing.T) {
			in := newRun().
				op(invariant.OpRestart, 0).
				fault(time.Second, fault.to).
				quiet(5*time.Second).
				request(6*time.Second, get("w-0")).
				through(14 * time.Second)

			if fault.judged {
				fired(t, invariant.BoundedReconciliation, in)
				return
			}
			silent(t, invariant.BoundedReconciliation, in)
		})
	}
}

func TestG1IgnoresTrafficAfterTheTeardownWindowClosed(t *testing.T) {
	in := newRun().
		op(invariant.OpRestart, 0).
		quiet(5*time.Second).
		request(7500*time.Millisecond, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

func TestG1IgnoresAWindowTheTeardownReachedInto(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		checkpoint(2*time.Second, invariant.Converged).
		teardown(3*time.Second).
		request(3500*time.Millisecond, get("w-0")).
		through(14 * time.Second)

	silent(t, invariant.BoundedReconciliation, in)
}

// A check bounds what it quotes, so it says how much it chose from or a report
// cannot say what the bound left out (#22).
func TestG1SaysHowManyRequestsItChoseFrom(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		record(time.Second, widget("10", spec(2), status(2, 1))).
		checkpoint(2*time.Second, invariant.Converged).
		requests(2100*time.Millisecond, 10*time.Millisecond, 25, get("w-0")).
		through(8 * time.Second)

	violation := fired(t, invariant.BoundedReconciliation, in)

	if len(violation.Requests) != invariant.MaxEvidence {
		t.Errorf("G1 quotes %d requests, want the bound of %d.", len(violation.Requests), invariant.MaxEvidence)
	}
	if violation.RequestsTotal != 25 {
		t.Errorf("G1 says it chose from %d requests, want the 25 in the window.", violation.RequestsTotal)
	}
}
