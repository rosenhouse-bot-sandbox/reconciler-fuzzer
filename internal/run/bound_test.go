package run

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// waitingHarness runs the Runner on a clock that each wait moves by the
// longest the live harness can take over it.
type waitingHarness struct {
	*fakeHarness
	timeouts target.Timeouts
	began    time.Time
	at       time.Time
	// runsOut names, in turn, the wait at whose end the next fault's trigger
	// runs out, and exitsLate the wait the target exits in just before its
	// end.
	runsOut   []int
	ranOut    int
	exitsLate []int
	// returnsLate has the target show it runs only just before T_settle past
	// each restart, at back.
	returnsLate bool
	back        time.Time
	// goesLate has deleting, the CR reconciler-fuzzer deleted last, go just before
	// the wait for it ends.
	goesLate bool
	deleting *unstructured.Unstructured
	// delay has the proxy hold a request for that long from just before each
	// wait's time runs out, once a fault op has run, and one from just before
	// each fault retires.
	delay time.Duration
}

func newWaitingHarness(timeouts target.Timeouts) *waitingHarness {
	// Far from the wall clock, so that a stamp taken on it shows.
	began := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	w := &waitingHarness{fakeHarness: newFakeHarness(), timeouts: timeouts, began: began, at: began}
	w.clock = func() time.Time { return w.at }
	w.faulting = true
	return w
}

// waited counts the start's wait for a kubeconfig cluster's namespace
// defaults too.
func (w *waitingHarness) waited() time.Duration {
	return defaultNamespaceDefaultsWithin + w.at.Sub(w.began)
}

// settle waits T_settle, or until what the run owes, which can move during the
// wait, or T_stable past its floor. A target that exits in the wait exits a
// moment into it.
func (w *waitingHarness) settle(ctx context.Context, owed func() time.Time, floor time.Time) (bool, error) {
	began := w.at
	w.at = w.at.Add(time.Millisecond)
	converged, err := w.fakeHarness.settle(ctx, owed, floor)
	for {
		deadline := began.Add(w.timeouts.Settle)
		if owes := owed(); owes.After(deadline) {
			deadline = owes
		}
		if floored := floor.Add(w.timeouts.Stable); floored.After(deadline) {
			deadline = floored
		}
		if w.delay > 0 && w.added > 0 {
			deadline = deadline.Add(w.delay + w.timeouts.Settle)
		}
		if w.returnBefore(deadline) {
			continue
		}
		if deadline.After(w.at) && len(w.exitsLate) > 0 && w.exitsLate[0] == w.waits {
			w.exitsLate = w.exitsLate[1:]
			w.at = deadline.Add(-time.Millisecond)
			w.exit()
			if exits := w.exits(); len(exits) > 0 {
				w.returnLate(exits[len(exits)-1].Restart)
			}
			continue
		}
		if deadline.After(w.at) {
			w.at = deadline
			continue
		}
		if len(w.runsOut) == 0 || w.runsOut[0] != w.waits {
			return converged, err
		}
		w.runsOut = w.runsOut[1:]
		w.remove(proxy.FaultID(w.ranOut))
		w.ranOut++
	}
}

// held has the proxy hold, once a fault op has run, a request that arrives
// just before before.
func (w *waitingHarness) held(before time.Time) (bool, time.Time) {
	arrived := before.Add(-time.Millisecond)
	if w.delay == 0 || w.added == 0 || w.at.Before(arrived) {
		return false, time.Time{}
	}
	if released := arrived.Add(w.delay); !w.at.Before(released) {
		return false, released
	}
	return true, time.Time{}
}

// faultWindow keeps each fault open until it releases the request it held as
// it retired.
func (w *waitingHarness) faultWindow(id proxy.FaultID) proxy.FaultWindow {
	window := w.fakeHarness.faultWindow(id)
	if w.delay > 0 && !window.Retired.IsZero() {
		window.Retired = window.Retired.Add(w.delay)
		if w.at.Before(window.Retired) {
			window.Retired = time.Time{}
		}
	}
	return window
}

// returnLate has a target that returns late show it runs just before T_settle
// past the restart.
func (w *waitingHarness) returnLate(restart time.Time) {
	if w.returnsLate {
		w.back = restart.Add(w.timeouts.Settle - time.Millisecond)
	}
}

// returnBefore logs the late target's return if it comes before deadline, and
// reports whether it did. The clock stays where a fixed wait took it past the
// return.
func (w *waitingHarness) returnBefore(deadline time.Time) bool {
	if w.back.IsZero() || !deadline.After(w.back) {
		return false
	}
	w.logged = append(w.logged, proxy.Request{Start: w.back, Verb: "list", Resource: "widgets"})
	if w.back.After(w.at) {
		w.at = w.back
	}
	w.back = time.Time{}
	return true
}

func (w *waitingHarness) sleep(ctx context.Context, d time.Duration) error {
	w.at = w.at.Add(d)
	return w.fakeHarness.sleep(ctx, d)
}

// deleteCR leaves the CR under deletion, as a finalizer would hold it.
func (w *waitingHarness) deleteCR(ctx context.Context, name string) error {
	w.deleting = widget(name)
	w.deleting.SetNamespace(fakeNamespace)
	w.deleting.SetResourceVersion(w.at.Format(time.RFC3339Nano))
	w.deleting.SetUID(types.UID(w.deleting.GetResourceVersion()))
	w.deleting.SetFinalizers([]string{"toy.reconciler-fuzzer/cleanup"})
	w.deleting.SetDeletionTimestamp(&metav1.Time{Time: w.at})
	w.store.Record(widgetKind, w.deleting, w.at)
	return w.fakeHarness.deleteCR(ctx, name)
}

// awaitCRGone waits until what until returns, which can move during the wait.
// A CR that goes late goes just before then.
func (w *waitingHarness) awaitCRGone(ctx context.Context, name string, until func() time.Time) (bool, error) {
	end := until
	if w.goesLate {
		end = func() time.Time { return until().Add(-time.Millisecond) }
	}
	for deadline := end(); deadline.After(w.at); deadline = end() {
		if !w.returnBefore(deadline) {
			w.at = deadline
		}
	}
	if w.goesLate {
		w.store.RecordDeletion(widgetKind, w.deleting, w.at)
	}
	return w.fakeHarness.awaitCRGone(ctx, name, until)
}

func (w *waitingHarness) awaitClean(ctx context.Context, within time.Duration) (bool, error) {
	w.at = w.at.Add(within)
	return w.fakeHarness.awaitClean(ctx, within)
}

func (w *waitingHarness) restart(ctx context.Context) error {
	w.returnLate(w.at)
	w.at = w.at.Add(launch.RestartWithin)
	return w.fakeHarness.restart(ctx)
}

func (w *waitingHarness) deleteFixture(ctx context.Context, gvk schema.GroupVersionKind, name string) error {
	w.at = w.at.Add(w.timeouts.Delete)
	return w.fakeHarness.deleteFixture(ctx, gvk, name)
}

func (w *waitingHarness) stop(ctx context.Context) error {
	w.at = w.at.Add(launch.StopWithin + namespaceDeletionBudget)
	return w.fakeHarness.stop(ctx)
}

func withTimeouts(timeouts target.Timeouts) *target.Target {
	t := *toyTarget
	t.Timeouts = timeouts
	return &t
}

var (
	createOp = Op{Type: OpCreate, Obj: widget("widget")}
	updateOp = Op{Type: OpUpdate, Patch: map[string]any{"spec": map[string]any{"count": float64(5)}}}
	settleOp = Op{Type: OpSettle}
	faultOp  = Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}}}
	// countedOp's fault stops once it has faulted three requests.
	countedOp       = Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Count: 3}}}
	deleteManagedOp = Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}
)

func faultUntil(op int) Op {
	return Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{Op: &op}}}
}

// heldFor is how long delayed holds a request.
const heldFor = 15 * time.Minute

// delayed is a fault that holds requests for heldFor.
func delayed(until Trigger) Op {
	return Op{Type: OpFault, Fault: &Fault{Action: Action{Delay: Duration(heldFor)}, Until: until}}
}

func TestBoundCoversTheRunnersWaits(t *testing.T) {
	// Long beside the harness's own margins, so that a wait Bound leaves out
	// shows.
	long := target.Timeouts{Settle: 20 * time.Minute, Stable: 10 * time.Minute, Delete: 40 * time.Minute}
	// Where T_settle outlasts T_delete and the reap, a recreate's wait for its
	// CR can outlast T_delete.
	settlesLonger := target.Timeouts{Settle: 20 * time.Minute, Stable: 10 * time.Minute, Delete: time.Minute}
	recreateOp := Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}
	for _, test := range []struct {
		name     string
		timeouts target.Timeouts
		ops      []Op
		// settles are the outcomes of the settle waits. The target restarts
		// after the longest backoff.
		settles     []bool
		exitsLate   []int
		runsOut     []int
		returnsLate bool
		goesLate    bool
		delay       time.Duration
		// waits is what the Runner's waits take, which shows the case holds
		// what it names.
		waits time.Duration
	}{
		{name: "a create", timeouts: long, ops: []Op{createOp}},
		{name: "an update", timeouts: long, ops: []Op{createOp, updateOp}},
		{name: "a delete", timeouts: long, ops: []Op{createOp, {Type: OpDelete}}},
		{name: "a recreate", timeouts: long, ops: []Op{createOp, {Type: OpRecreate, Obj: widget("widget")}}},
		{name: "a recreate that does not settle", timeouts: long,
			ops: []Op{createOp, {Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, settleOp}},
		{name: "a restart", timeouts: long, ops: []Op{createOp, {Type: OpRestart}, settleOp}},
		{name: "a restart the target returns from late", timeouts: long,
			ops: []Op{createOp, {Type: OpRestart}, settleOp}, returnsLate: true},
		{name: "a deleteManaged", timeouts: long, ops: []Op{createOp, deleteManagedOp}},
		{name: "a deleteManaged after a fault stopped", timeouts: long,
			ops: []Op{createOp, faultUntil(3), updateOp, deleteManagedOp}, waits: 2*time.Hour + 21*time.Minute + 11*time.Second},
		{name: "a deleteManaged after faults that stopped one after another", timeouts: long,
			ops:     []Op{createOp, faultUntil(3), updateOp, faultUntil(5), updateOp, deleteManagedOp},
			settles: []bool{true, false, false}, waits: 3*time.Hour + 41*time.Minute + 11*time.Second},
		{name: "a fault the teardown stops", timeouts: long,
			ops: []Op{createOp, faultOp, updateOp, updateOp, updateOp}, settles: []bool{true, false, false, false},
			waits: 3*time.Hour + 31*time.Minute + 11*time.Second},
		{name: "faults that stop one after another", timeouts: long,
			ops:     []Op{createOp, faultUntil(4), faultOp, updateOp, updateOp, updateOp},
			settles: []bool{true, false, false, false}, waits: 4*time.Hour + 11*time.Minute + 11*time.Second},
		{name: "faults whose triggers run out as a wait would end", timeouts: long,
			ops:     []Op{createOp, countedOp, countedOp, countedOp, countedOp, updateOp},
			settles: []bool{true, false}, runsOut: []int{2, 2, 2, 2}, waits: 11*time.Hour + 31*time.Minute + 11*time.Second},
		{name: "faults the teardown stops together", timeouts: long,
			ops: []Op{createOp, faultOp, faultOp, updateOp}, settles: []bool{true, false},
			waits: 2*time.Hour + 11*time.Minute + 11*time.Second},
		{name: "exits before and after a fault stops", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, updateOp}, settles: []bool{true, false}, exitsLate: []int{2, 3},
			waits: 20*time.Minute + 50*time.Second},
		{name: "exits before and after faults stop together", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, faultOp, updateOp}, settles: []bool{true, false}, exitsLate: []int{2, 2, 3},
			waits: 20*time.Minute + 50*time.Second},
		{name: "exits after faults that began apart", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, updateOp, faultOp, updateOp}, settles: []bool{true, false, false}, exitsLate: []int{2, 3, 4},
			waits: 32*time.Minute + 50*time.Second},
		{name: "a crash loop under a fault the teardown stops", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, updateOp, settleOp}, settles: []bool{true, false, false}, exitsLate: []int{2, 2, 2, 3, 3, 4, 4},
			waits: 42*time.Minute + 50*time.Second},
		// Six ops outlast the slack Bound has elsewhere.
		{name: "exits before each op under a fault the teardown stops", timeouts: long,
			ops:       slices.Concat([]Op{createOp, faultOp}, slices.Repeat([]Op{updateOp}, 6)),
			settles:   slices.Concat([]bool{true}, slices.Repeat([]bool{false}, 6)),
			exitsLate: []int{2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7}, returnsLate: true,
			waits: 18*time.Hour + 41*time.Minute + 10*time.Second},
		{name: "exits before ops that do not settle under a fault the teardown stops", timeouts: target.DefaultTimeouts,
			ops:     []Op{createOp, faultOp, updateOp, {Type: OpUpdate, Patch: updateOp.Patch, NoSettle: true}, recreateOp, settleOp},
			settles: []bool{true, false, false}, exitsLate: []int{2, 2}, returnsLate: true,
			waits: 29*time.Minute + 20*time.Second},
		{name: "a crash loop under a fault that stops at an op", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultUntil(3), updateOp, settleOp}, settles: []bool{true, false, false}, exitsLate: []int{2, 2, 2, 3, 3},
			waits: 20*time.Minute + 50*time.Second},
		{name: "an exit after a fault stopped", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultUntil(3), settleOp, updateOp}, settles: []bool{true, false, false}, exitsLate: []int{3},
			waits: 9*time.Minute + 50*time.Second},
		{name: "a delete that does not settle", timeouts: long,
			ops: []Op{createOp, {Type: OpDelete, NoSettle: true}, settleOp}},
		{name: "a deleteFixture", timeouts: long,
			ops: []Op{createOp, {Type: OpDeleteFixture, Kind: "v1/Secret", Name: "token", Until: &Until{Op: 2}}, settleOp}},
		{name: "exits the target returns from late", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, faultOp, updateOp}, settles: []bool{true, false}, exitsLate: []int{2, 3}, returnsLate: true,
			waits: 22*time.Minute + 20*time.Second},
		{name: "a recreate right after a restart the target returns from late", timeouts: settlesLonger,
			ops: []Op{createOp, {Type: OpRestart}, recreateOp}, returnsLate: true, waits: time.Hour + 12*time.Minute + 10*time.Second},
		{name: "a recreate right after a CR went", timeouts: settlesLonger,
			ops: []Op{createOp, recreateOp, recreateOp}, goesLate: true, waits: 53*time.Minute + 10*time.Second},
		{name: "a delay the teardown stops", timeouts: long,
			ops: []Op{createOp, delayed(Trigger{}), updateOp, updateOp}, settles: []bool{true, false, false}, delay: heldFor,
			waits: 6*time.Hour + 16*time.Minute + 11*time.Second},
		{name: "delays whose triggers run out as a wait would end", timeouts: long,
			ops:     []Op{createOp, delayed(Trigger{Count: 3}), delayed(Trigger{Count: 3}), updateOp},
			settles: []bool{true, false}, runsOut: []int{2, 2}, delay: heldFor, waits: 4*time.Hour + 26*time.Minute + 11*time.Second},
		{name: "a recreate under a delay", timeouts: settlesLonger,
			ops: []Op{createOp, delayed(Trigger{}), recreateOp, settleOp}, settles: []bool{true, false, false}, delay: heldFor,
			waits: 4*time.Hour + 59*time.Minute + 10*time.Second},
		{name: "exits under a delay", timeouts: target.DefaultTimeouts,
			ops: []Op{createOp, delayed(Trigger{}), updateOp}, settles: []bool{true, false}, exitsLate: []int{2, 3}, delay: heldFor,
			waits: 2*time.Hour + 2*time.Minute + 50*time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newWaitingHarness(test.timeouts)
			h.settles, h.exitsLate, h.restartsIn, h.runsOut = test.settles, test.exitsLate, launch.MaxBackoff, test.runsOut
			h.returnsLate, h.goesLate, h.delay = test.returnsLate, test.goesLate, test.delay
			exercised, sequence := withTimeouts(test.timeouts), sequenceOf(test.ops...)
			exercised.Fixtures = withSecret().Fixtures

			if _, err := runSequence(t.Context(), exercised, sequence, Options{Check: &fakeChecker{}}, h); err != nil {
				t.Fatalf("The run failed: %v", err)
			}

			if len(h.runsOut)+len(h.exitsLate) > 0 {
				t.Fatalf("%d faults never ran out and %d exits never came.", len(h.runsOut), len(h.exitsLate))
			}
			if waited := h.waited().Truncate(time.Second); test.waits > 0 && waited != test.waits {
				t.Fatalf("The Runner's waits took %v, want %v.", waited, test.waits)
			}
			if bound := Bound(exercised, sequence); bound < h.waited() {
				t.Errorf("Bound is %v, and the Runner's waits can take %v.", bound, h.waited())
			}
		})
	}
}

// deadlineHarness records until when the teardown's steps may run.
type deadlineHarness struct {
	*fakeHarness
	stepsUntil time.Time
}

func (d *deadlineHarness) deleteCR(ctx context.Context, name string) error {
	d.stepsUntil, _ = ctx.Deadline()
	return d.fakeHarness.deleteCR(ctx, name)
}

// The teardown's steps may run until T_stable, T_delete and 30s past where its
// quiet window opens, which is what Bound gives them.
func TestTheTeardownsStepsRunOnABudget(t *testing.T) {
	h := &deadlineHarness{fakeHarness: newFakeHarness()}

	result, err := runSequence(t.Context(), toyTarget, sequenceOf(createOp), Options{Check: &fakeChecker{}}, h)

	if err != nil {
		t.Fatalf("The run failed: %v", err)
	}
	budget := testTimeouts.Stable + testTimeouts.Delete + 30*time.Second
	if steps := h.stepsUntil.Sub(result.Timeline.Quiet.Start); steps > budget || steps < budget-time.Second {
		t.Errorf("The teardown's steps had %v, want %v.", steps, budget)
	}
}

func TestBoundAddsWhatEachOpCanWait(t *testing.T) {
	timeouts := target.DefaultTimeouts
	defaults := withTimeouts(timeouts)
	alone := Bound(defaults, sequenceOf(createOp))
	for _, test := range []struct {
		name string
		op   Op
		adds time.Duration
	}{
		{"an update", updateOp, timeouts.Settle},
		{"an update that does not settle", Op{Type: OpUpdate, Patch: updateOp.Patch, NoSettle: true}, 0},
		{"a delete", Op{Type: OpDelete}, timeouts.Delete + timeouts.Settle},
		{"a delete that does not settle", Op{Type: OpDelete, NoSettle: true}, timeouts.Delete},
		{"a recreate", Op{Type: OpRecreate, Obj: widget("widget")}, timeouts.Delete + timeouts.Settle},
		{"a recreate that does not settle", Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, timeouts.Delete},
		{"a restart", Op{Type: OpRestart}, launch.RestartWithin + timeouts.Settle},
		{"a deleteManaged", Op{Type: OpDeleteManaged, Kind: "v1/ConfigMap", Nth: nth(0)}, timeouts.Settle},
		{"a settle", settleOp, timeouts.Settle},
		{"an updateFixture", Op{Type: OpUpdateFixture, Kind: "v1/Secret", Name: "token", Patch: map[string]any{"data": nil}}, timeouts.Settle},
		{"a deleteFixture", Op{Type: OpDeleteFixture, Kind: "v1/Secret", Name: "token", Until: &Until{Op: 2}}, timeouts.Delete},
	} {
		t.Run(test.name, func(t *testing.T) {
			if adds := Bound(defaults, sequenceOf(createOp, test.op)) - alone; adds != test.adds {
				t.Errorf("Bound adds %v for %s, want %v.", adds, test.name, test.adds)
			}
		})
	}
}

func TestBoundGivesARecreateTSettleWhereThatIsLonger(t *testing.T) {
	settlesLonger := withTimeouts(target.Timeouts{Settle: 2 * time.Minute, Stable: 10 * time.Second, Delete: time.Minute})
	recreate := Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}

	if adds := Bound(settlesLonger, sequenceOf(createOp, recreate)) - Bound(settlesLonger, sequenceOf(createOp)); adds != 2*time.Minute {
		t.Errorf("Bound adds %v for a recreate, want 2m0s.", adds)
	}
}

// At the default timeouts, a create alone can take 30s to start, 30s to settle,
// and 10s of quiet, 60s for the CR to go and 30s more in the teardown. Stopping
// the target takes up to 10s and deleting the namespace 30s.
func TestBoundOfACreateAlone(t *testing.T) {
	if bound, want := Bound(withTimeouts(target.DefaultTimeouts), sequenceOf(createOp)), 200*time.Second; bound != want {
		t.Errorf("Bound is %v, want %v.", bound, want)
	}
}

// A target that exits while a fault excuses it restarts after a backoff of up
// to 5m, and is owed T_settle past its return, which can come T_settle after
// the restart. Once faults stop, they are owed as long as they lasted and
// T_settle. So from the first fault on each op allows an exit. Each op after it
// may first wait out a backoff, and then owe T_settle past a return that can
// come T_settle after the op, T_settle more than a settle wait. Each time faults
// stop, the time before the teardown doubles and another exit is allowed.
// Faults with no trigger stop together at the teardown.
func TestBoundDoublesTheRunEachTimeFaultsStop(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	const settle, teardown = 30 * time.Second, 140 * time.Second
	exit := launch.MaxBackoff + 2*settle
	stop := func(before time.Duration) time.Duration { return 2*before + settle + exit }
	const guard = launch.MaxBackoff + settle
	// The start, the create and the update take 90s.
	const run = 90 * time.Second
	timed := Op{Type: OpFault, Fault: &Fault{Action: Action{Error: 500}, Until: Trigger{For: Duration(time.Second)}}}
	noSettle := func(op Op) Op {
		op.NoSettle = true
		return op
	}
	for _, test := range []struct {
		name string
		ops  []Op
		want time.Duration
	}{
		{"a fault", []Op{createOp, faultOp, updateOp}, stop(run+2*exit+guard) + teardown},
		{"faults that stop together", []Op{createOp, faultOp, faultOp, updateOp}, stop(run+3*exit+2*guard) + teardown},
		{"a fault that stops at an op", []Op{createOp, faultUntil(2), updateOp}, stop(run+2*exit+guard) + teardown},
		{"faults that stop apart", []Op{createOp, faultUntil(3), countedOp, timed, updateOp},
			stop(stop(stop(run+4*exit+3*guard))) + teardown},
		{"faults that stop apart and together", []Op{createOp, faultUntil(3), faultOp, faultOp, updateOp},
			stop(stop(run+4*exit+3*guard)) + teardown},
		// The recreate waits T_delete of 60s for its CR to go.
		{"ops that do not settle after a fault", []Op{createOp, faultOp, noSettle(updateOp), noSettle(Op{Type: OpRecreate, Obj: widget("widget")}), updateOp},
			stop(run+time.Minute+4*exit+3*guard) + teardown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if bound := Bound(defaults, sequenceOf(test.ops...)); bound != test.want {
				t.Errorf("Bound is %v, want %v.", bound, test.want)
			}
		})
	}
}

// A request the proxy holds when a wait's time runs out holds the wait open
// for up to the delay and T_settle more, which can come after what the faults
// owe. A fault lasts until the proxy releases what it held, which can be the
// delay past its trigger.
func TestBoundAllowsForTheRequestsAFaultHolds(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	const settle, deletion, teardown, delay = 30 * time.Second, time.Minute, 140 * time.Second, 2 * time.Minute
	const hold = delay + settle
	exit := launch.MaxBackoff + 2*settle
	stop := func(before time.Duration) time.Duration { return 2*(before+delay) + settle + hold + exit }
	const guard = launch.MaxBackoff + settle
	const run = 90 * time.Second
	held := Op{Type: OpFault, Fault: &Fault{Action: Action{Delay: Duration(delay)}}}
	shorter := Op{Type: OpFault, Fault: &Fault{Action: Action{Delay: Duration(time.Second)}}}
	for _, test := range []struct {
		name string
		ops  []Op
		want time.Duration
	}{
		{"a delay", []Op{createOp, held, updateOp}, stop(run+2*exit+guard+hold) + teardown},
		{"the longest of two delays", []Op{createOp, shorter, held, shorter, updateOp},
			stop(run+4*exit+3*guard+hold) + teardown},
		{"a wait before the delay", []Op{createOp, updateOp, held, settleOp},
			stop(run+settle+2*exit+guard+hold) + teardown},
		{"a recreate's wait for its CR", []Op{createOp, held, {Type: OpRecreate, Obj: widget("widget"), NoSettle: true}, settleOp},
			stop(run+deletion+3*exit+2*guard+2*hold) + teardown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if bound := Bound(defaults, sequenceOf(test.ops...)); bound != test.want {
				t.Errorf("Bound is %v, want %v.", bound, test.want)
			}
		})
	}
}

func TestBoundOfSeveralRunsIsTheirSum(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	one, other := sequenceOf(createOp), sequenceOf(createOp, updateOp)

	if bound, want := Bound(defaults, one, other), Bound(defaults, one)+Bound(defaults, other); bound != want {
		t.Errorf("Bound of both runs is %v, want %v.", bound, want)
	}
}

func TestBoundCountsASumPastHalfTheLongestDuration(t *testing.T) {
	// Each run takes 3×2^60ns, with 102s of margins beside its settle wait.
	long := withTimeouts(target.Timeouts{Settle: 3<<60 - 102*time.Second, Stable: time.Second, Delete: time.Second})

	if bound, want := Bound(long, sequenceOf(createOp), sequenceOf(createOp)), time.Duration(3<<61); bound != want {
		t.Errorf("Bound is %v, want %v.", bound, want)
	}
}

func TestBoundSaturatesRatherThanOverflow(t *testing.T) {
	defaults := withTimeouts(target.DefaultTimeouts)
	ops := []Op{createOp}
	for range 64 {
		ops = append(ops, countedOp)
	}
	manyFaults := sequenceOf(append(ops, updateOp)...)
	const longest = math.MaxInt64
	settles := withTimeouts(target.Timeouts{Settle: longest, Stable: time.Second, Delete: time.Second})
	deletes := withTimeouts(target.Timeouts{Settle: 2 * time.Second, Stable: time.Second, Delete: longest / 3})
	teardowns := withTimeouts(target.Timeouts{Settle: 2 * time.Second, Stable: longest, Delete: longest})
	// A run takes over half the longest duration.
	long := withTimeouts(target.Timeouts{Settle: longest / 4, Stable: time.Second, Delete: longest / 4})
	recreate := Op{Type: OpRecreate, Obj: widget("widget"), NoSettle: true}
	for _, test := range []struct {
		name  string
		bound time.Duration
	}{
		{"many faults", Bound(defaults, manyFaults)},
		{"long settle waits", Bound(settles, sequenceOf(createOp, updateOp))},
		{"long deletions", Bound(deletes, sequenceOf(slices.Concat([]Op{createOp}, slices.Repeat([]Op{recreate}, 7))...))},
		{"a long teardown", Bound(teardowns, sequenceOf(createOp))},
		{"two long runs", Bound(long, sequenceOf(createOp), sequenceOf(createOp))},
		// Each run takes 2^62ns, with 102s of margins beside its settle wait.
		{"two runs that just overflow", Bound(withTimeouts(target.Timeouts{Settle: 1<<62 - 102*time.Second, Stable: time.Second, Delete: time.Second}),
			sequenceOf(createOp), sequenceOf(createOp))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.bound != math.MaxInt64 {
				t.Errorf("Bound is %v, want the longest duration.", test.bound)
			}
		})
	}
}
