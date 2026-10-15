package invariant_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
)

func TestBackIsTheFirstRequestThatShowsTheTargetRunning(t *testing.T) {
	for _, test := range []struct {
		name string
		run  *run
		want time.Duration
	}{
		{"a watch", newRun().request(1500*time.Millisecond, watch()), 1500 * time.Millisecond},
		{"a get the API server refused", newRun().request(1500*time.Millisecond, failedGet("w-0", http.StatusNotFound)), 1500 * time.Millisecond},
		{"the earliest, though logged last", newRun().
			request(1700*time.Millisecond, watch()).
			request(1600*time.Millisecond, get("w-0")), 1600 * time.Millisecond},
		{"one after discovery", newRun().
			request(1300*time.Millisecond, nonResource("/api")).
			request(1400*time.Millisecond, watch()), 1400 * time.Millisecond},
		{"one after requests up to since", newRun().
			request(500*time.Millisecond, get("w-0")).
			request(time.Second, get("w-0")).
			request(1400*time.Millisecond, watch()), 1400 * time.Millisecond},
		{"one after it listed and watched leases", newRun().
			request(1100*time.Millisecond, lease("list")).
			request(1200*time.Millisecond, lease("watch")).
			request(1400*time.Millisecond, watch()), 1400 * time.Millisecond},
		{"the lease it created, after a watch as it waited to lead", newRun().
			request(1100*time.Millisecond, watch()).
			request(1200*time.Millisecond, leaseAnswered("get", http.StatusNotFound)).
			request(1300*time.Millisecond, leaseAnswered("create", http.StatusCreated)), 1300 * time.Millisecond},
		{"the lease it won after its get, not its predecessor's renewal", newRun().
			request(500*time.Millisecond, lease("get")).
			request(600*time.Millisecond, lease("update")).
			request(1050*time.Millisecond, lease("update")).
			request(1200*time.Millisecond, lease("get")).
			request(1600*time.Millisecond, lease("update")), 1600 * time.Millisecond},
		{"the lease it patched, after a patch the API server refused", newRun().
			request(1200*time.Millisecond, lease("get")).
			request(1300*time.Millisecond, leaseAnswered("patch", http.StatusConflict)).
			request(1400*time.Millisecond, lease("patch")).
			request(1500*time.Millisecond, watch()), 1400 * time.Millisecond},
		{"the lease it updated, after updates the API server refused", newRun().
			request(1100*time.Millisecond, watch()).
			request(1200*time.Millisecond, lease("get")).
			request(1300*time.Millisecond, leaseAnswered("update", http.StatusInternalServerError)).
			request(1400*time.Millisecond, leaseAnswered("update", http.StatusConflict)).
			request(1600*time.Millisecond, lease("update")), 1600 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			back, found := invariant.Back(test.run.in.Requests, at(time.Second))

			if !found || !back.Equal(at(test.want)) {
				t.Errorf("Back returned (%v, %t), want %v.", back, found, at(test.want))
			}
		})
	}
}

// reconciler-fuzzer chose to restart the target, so a wait gives it T_settle
// past its return, where it returned within T_settle.
func TestWaitOwedRunsPastTheReturnFromARestartOp(t *testing.T) {
	restarted := func() *run { return newRun().running(time.Second).op(invariant.OpRestart, 3*time.Second) }
	for _, test := range []struct {
		name string
		run  *run
		at   time.Duration
		// want is zero where nothing is owed.
		want time.Duration
	}{
		{name: "back", run: restarted().running(6500 * time.Millisecond), at: 7 * time.Second, want: 11500 * time.Millisecond},
		{name: "not back", run: restarted(), at: 7 * time.Second, want: 8 * time.Second},
		{name: "with leader election alone", run: restarted().requests(3100*time.Millisecond, time.Second, 4, lease("get")),
			at: 7 * time.Second, want: 8 * time.Second},
		{name: "back once it won its lease", run: restarted().running(3100*time.Millisecond).
			request(3200*time.Millisecond, lease("get")).
			request(6500*time.Millisecond, lease("update")),
			at: 7 * time.Second, want: 11500 * time.Millisecond},
		{name: "back only T_settle after it", run: restarted().running(8 * time.Second), at: 9 * time.Second, want: 8 * time.Second},
		{name: "a restart at the instant asked about", run: restarted().running(3500 * time.Millisecond), at: 3 * time.Second},
		{name: "a restart and a later op", run: restarted().running(6500*time.Millisecond).op(invariant.OpUpdate, 7*time.Second),
			at: 8 * time.Second, want: 11500 * time.Millisecond},
		{name: "two restarts", run: restarted().running(3500*time.Millisecond).op(invariant.OpRestart, 5*time.Second).running(9 * time.Second),
			at: 10 * time.Second, want: 14 * time.Second},
		{name: "an exit no fault excused", run: newRun().running(time.Second).exit(3*time.Second, 3*time.Second).running(3500 * time.Millisecond),
			at: 4 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.run.through(20 * time.Second).WaitOwed(at(test.at))

			if test.want == 0 && !got.IsZero() {
				t.Errorf("The wait is owed until %v, want nothing.", got.Sub(epoch))
			}
			if test.want != 0 && !got.Equal(at(test.want)) {
				t.Errorf("The wait is owed until %v, want %v.", got.Sub(epoch), test.want)
			}
		})
	}
}

func TestBackFindsNothingUntilTheTargetShowsItRuns(t *testing.T) {
	leasesElsewhereCreated := leasesElsewhere()
	leasesElsewhereCreated.Verb, leasesElsewhereCreated.Status = "create", http.StatusCreated
	for name, r := range map[string]*run{
		"no request": newRun(),
		"leader election alone": newRun().
			request(1100*time.Millisecond, lease("get")).
			request(1200*time.Millisecond, leaseCandidate("create")).
			request(1300*time.Millisecond, leaseCandidate("list")),
		"discovery alone":      newRun().request(1100*time.Millisecond, nonResource("/apis")),
		"requests up to since": newRun().request(500*time.Millisecond, watch()).request(time.Second, watch()),
		"a lease won up to since": newRun().
			request(time.Second, lease("update")).
			request(1100*time.Millisecond, lease("get")),
		"a renewal by its predecessor alone": newRun().
			request(500*time.Millisecond, lease("get")).
			request(600*time.Millisecond, lease("update")).
			request(1050*time.Millisecond, lease("update")),
		"a renewal by its predecessor before it got its lease": newRun().
			request(500*time.Millisecond, lease("get")).
			request(600*time.Millisecond, lease("update")).
			request(1050*time.Millisecond, lease("update")).
			request(1100*time.Millisecond, watch()).
			request(1200*time.Millisecond, lease("get")),
		"a watch, by a target that won a lease before since": newRun().
			request(500*time.Millisecond, lease("get")).
			request(600*time.Millisecond, lease("update")).
			request(1400*time.Millisecond, watch()),
		"a watch as it waited to lead": newRun().
			request(1100*time.Millisecond, watch()).
			request(1200*time.Millisecond, lease("get")),
		"lease writes the API server refused": newRun().
			request(1100*time.Millisecond, watch()).
			request(1150*time.Millisecond, lease("get")).
			request(1200*time.Millisecond, leaseAnswered("create", http.StatusConflict)).
			request(1300*time.Millisecond, leaseAnswered("update", http.StatusInternalServerError)),
		"a lease update the API server has not answered": newRun().
			request(1100*time.Millisecond, lease("get")).
			request(1200*time.Millisecond, leaseAnswered("update", 0)),
		"leases created in another group as it waited to lead": newRun().
			request(1100*time.Millisecond, lease("get")).
			request(1200*time.Millisecond, leasesElsewhereCreated),
	} {
		t.Run(name, func(t *testing.T) {
			if back, found := invariant.Back(r.in.Requests, at(time.Second)); found {
				t.Errorf("Back found the target back at %v, want nothing.", back)
			}
		})
	}
}
