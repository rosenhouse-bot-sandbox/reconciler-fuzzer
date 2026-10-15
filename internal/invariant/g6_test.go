package invariant_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// loop repeats a request every 500ms from 1s into the run, which keeps every
// repetition inside one T_settle window.
func loop(count int, req proxy.Request) *run {
	return newRun().op(invariant.OpCreate, 0).requests(time.Second, 500*time.Millisecond, count, req)
}

func TestG6PassesAtTheThreshold(t *testing.T) {
	in := loop(errLoop, failedGet("w-0", 404)).through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6FiresPastTheThreshold(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 404)).through(8 * time.Second)

	violation := fired(t, invariant.NoErrorLoop, in)

	if violation.ID != "G6" {
		t.Errorf("The violation is %q, want G6.", violation.ID)
	}
	if !strings.Contains(violation.Statement, "get") || !strings.Contains(violation.Statement, "w-0") {
		t.Errorf("The statement is %q, want it to name the request the target repeated.", violation.Statement)
	}
	if want := "6 times within 5s (timeouts.settle), where thresholds.errloop allows 5"; !strings.HasSuffix(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to end %q.", violation.Statement, want)
	}
	if len(violation.Requests) != errLoop+1 {
		t.Fatalf("The evidence holds %d requests, want all %d of the loop.", len(violation.Requests), errLoop+1)
	}
}

func TestG6NamesTheSubresource(t *testing.T) {
	in := loop(errLoop+1, failedStatusUpdate(403)).through(8 * time.Second)

	violation := fired(t, invariant.NoErrorLoop, in)

	if want := "the failing request update toy.reconciler-fuzzer/widgets/w/status 6 times"; !strings.Contains(violation.Statement, want) {
		t.Errorf("The statement is %q, want it to contain %q.", violation.Statement, want)
	}
}

// A loop every T_settle/N_errloop puts a failure at each end of T_settle, and
// both count.
func TestG6CountsFailuresAtBothEndsOfTheWindow(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(time.Second, settleTimeout/errLoop, errLoop+1, failedGet("w-0", 404)).
		through(8 * time.Second)

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6CountsServerErrorsToo(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 500)).through(8 * time.Second)

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresRequestsThatSucceed(t *testing.T) {
	in := loop(errLoop+1, get("w-0")).through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

// cert-manager runs six controllers that write one Certificate's status, so
// they conflict and re-read. That is the API server's optimistic-concurrency
// contract, not an error loop (DESIGN.md §6).
func TestG6IgnoresAConflictOnAnUpdateOrAPatch(t *testing.T) {
	for _, verb := range []string{"update", "patch"} {
		t.Run(verb, func(t *testing.T) {
			in := loop(errLoop+1, conflicted(verb, "w-0")).through(8 * time.Second)

			silent(t, invariant.NoErrorLoop, in)
		})
	}
}

// Only an update and a patch lose that race. A 409 on a create is
// AlreadyExists, which says the object is there: re-creating it is the error
// loop G6 exists to catch.
func TestG6CountsAConflictOnAnyOtherVerb(t *testing.T) {
	for _, verb := range []string{"create", "delete", "get"} {
		t.Run(verb, func(t *testing.T) {
			in := loop(errLoop+1, conflicted(verb, "w-0")).through(8 * time.Second)

			fired(t, invariant.NoErrorLoop, in)
		})
	}
}

// G1 ignores a watch because a watch that hangs is the target waiting. A watch
// that fails returns at once, and repeating it is a loop, so G6 counts it.
func TestG6CountsAFailingWatch(t *testing.T) {
	in := loop(errLoop+1, failedWatch(429)).through(8 * time.Second)

	fired(t, invariant.NoErrorLoop, in)
}

func TestG6CountsOneRequestAtATime(t *testing.T) {
	// An earlier run's namespace, which the target can still reconcile.
	elsewhere := failedGet("w-0", 404)
	elsewhere.Namespace = "reconciler-fuzzer-run-0"
	for _, differs := range []struct {
		field string
		other proxy.Request
	}{
		{"name", failedGet("w-1", 404)},
		{"verb", failedDelete("w-0", 404)},
		{"resource", failedWidgetGet(404)},
		{"namespace", elsewhere},
	} {
		t.Run(differs.field, func(t *testing.T) {
			in := loop(errLoop, failedGet("w-0", 404)).
				requests(1200*time.Millisecond, 500*time.Millisecond, errLoop, differs.other).
				through(8 * time.Second)

			silent(t, invariant.NoErrorLoop, in)
		})
	}
}

func TestG6CountsTheMainResourceAndASubresourceApart(t *testing.T) {
	onWidget := failedStatusUpdate(403)
	onWidget.Subresource = ""
	in := loop(errLoop, onWidget).
		requests(1200*time.Millisecond, 500*time.Millisecond, errLoop, failedStatusUpdate(403)).
		through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

// A run keeps its first violation, so G6 reports the loop that began first.
func TestG6OrdersLoopsByWhenTheyBegan(t *testing.T) {
	elsewhere := failedGet("w-0", 404)
	elsewhere.Namespace = "reconciler-fuzzer-run-0"
	for _, test := range []struct {
		name          string
		first, second proxy.Request
		later         time.Duration
		want          []string
	}{
		{"one after the other", failedGet("w-1", 404), failedGet("w-0", 404), 200 * time.Millisecond,
			[]string{namespace + "/w-1", namespace + "/w-0"}},
		{"together, by request", failedGet("w-1", 404), failedGet("w-0", 404), 0,
			[]string{namespace + "/w-0", namespace + "/w-1"}},
		{"together, by namespace", failedGet("w-0", 404), elsewhere, 0,
			[]string{"reconciler-fuzzer-run-0/w-0", namespace + "/w-0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The first loop also ends last.
			in := loop(errLoop+3, test.first).
				requests(time.Second+test.later, 500*time.Millisecond, errLoop+1, test.second).
				through(8 * time.Second)

			// Go ranges over a map in a random order, so one evaluation can
			// pass by chance.
			for range 100 {
				var loops []string
				for _, violation := range evaluate(t, invariant.NoErrorLoop, in).Violations {
					loops = append(loops, violation.Requests[0].Namespace+"/"+violation.Requests[0].Name)
				}
				if !slices.Equal(loops, test.want) {
					t.Fatalf("G6 reported the loops %v, want %v.", loops, test.want)
				}
			}
		})
	}
}

func TestG6IgnoresFailuresSpreadPastTheSettleTimeout(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(time.Second, time.Second+100*time.Millisecond, errLoop+1, failedGet("w-0", 404)).
		through(12 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresFailuresAFaultCaused(t *testing.T) {
	in := loop(errLoop+1, failedGet("w-0", 500)).
		fault(0, 6*time.Second).
		through(8 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresFailuresAnOpSplits(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		requests(time.Second, 200*time.Millisecond, errLoop, failedGet("w-0", 404)).
		op(invariant.OpUpdate, 2*time.Second).
		requests(2200*time.Millisecond, 200*time.Millisecond, errLoop, failedGet("w-0", 404)).
		through(12 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}

func TestG6IgnoresFailuresAFixtureOpSplits(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*run) *run
	}{
		{"an update", func(r *run) *run { return r.op(invariant.OpUpdateFixture, 2*time.Second) }},
		{"a delete", func(r *run) *run { return r.op(invariant.OpDeleteFixture, 2*time.Second) }},
		{"a restore", func(r *run) *run { return r.restoring(invariant.OpSettle, 2*time.Second) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			in := change.apply(newRun().
				op(invariant.OpCreate, 0).
				requests(time.Second, 200*time.Millisecond, errLoop, failedGet("w-0", 404))).
				requests(2200*time.Millisecond, 200*time.Millisecond, errLoop, failedGet("w-0", 404)).
				through(12 * time.Second)

			silent(t, invariant.NoErrorLoop, in)
		})
	}
}

func TestG6TakesTheThresholdSection6DefaultsWhenTheTargetDeclaresNone(t *testing.T) {
	in := loop(10, failedGet("w-0", 404)).through(40 * time.Second)
	in.Target.Thresholds = target.Thresholds{}
	in.Target.Timeouts = target.Timeouts{}

	silent(t, invariant.NoErrorLoop, in)

	in = loop(11, failedGet("w-0", 404)).through(40 * time.Second)
	in.Target.Thresholds = target.Thresholds{}
	in.Target.Timeouts = target.Timeouts{}

	fired(t, invariant.NoErrorLoop, in)
}

// controller-runtime's default rate limiter doubles a failing item's delay
// from 5ms, so the densest 30s holds 13 identical failures however long the
// loop runs, and G6 sees them at a threshold of 12 or less, such as the
// default.
func TestG6SeesControllerRuntimesDefaultBackoff(t *testing.T) {
	limiter := workqueue.DefaultTypedControllerRateLimiter[string]()
	backingOff := newRun().op(invariant.OpCreate, 0)
	for at := time.Second; at < 2*time.Minute; at += limiter.When("w-0") {
		backingOff.request(at, failedGet("w-0", 404))
	}
	in := backingOff.through(3 * time.Minute)
	in.Target.Timeouts = target.Timeouts{}

	in.Target.Thresholds = target.Thresholds{}
	fired(t, invariant.NoErrorLoop, in)

	in.Target.Thresholds = target.Thresholds{ErrLoop: 13}
	silent(t, invariant.NoErrorLoop, in)

	in.Target.Thresholds = target.Thresholds{ErrLoop: 12}
	if violation := fired(t, invariant.NoErrorLoop, in); len(violation.Requests) != 13 {
		t.Errorf("G6 quotes %d failures, want the 13 in the densest 30s.", len(violation.Requests))
	}
}

func TestG6ReadsALogTheProxyStampedOutOfOrder(t *testing.T) {
	in := newRun().
		op(invariant.OpCreate, 0).
		request(30*time.Second, failedGet("w-0", 404)).
		requests(time.Second, time.Second, errLoop, failedGet("w-0", 404)).
		through(40 * time.Second)

	silent(t, invariant.NoErrorLoop, in)
}
