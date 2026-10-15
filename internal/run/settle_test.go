package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

var testTimeouts = target.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, Delete: 10 * time.Second}

const testPoll = 100 * time.Millisecond

// clock drives a settle wait without sleeping: each poll advances it. A wait
// that polls on well beyond T_settle can no longer end, so the clock fails it
// rather than letting the test spin.
type clock struct {
	now   time.Time
	polls int
}

// maxPolls is four times what a wait that runs to T_settle takes.
var maxPolls = 4 * int(testTimeouts.Settle/testPoll)

func newClock() *clock { return &clock{now: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)} }

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.polls++; c.polls > maxPolls {
		return fmt.Errorf("the wait polled %d times without ending", c.polls)
	}
	c.now = c.now.Add(d)
	return nil
}

// neverStops is the Exited of a target the wait never sees stop.
var neverStops = make(chan struct{})

// waitOn runs a settle wait over state and returns whether it converged and
// how long it took on the clock.
func waitOn(t *testing.T, ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time), stopped <-chan struct{}) (bool, time.Duration, error) {
	t.Helper()
	return waitOwing(t, ctx, c, state, stopped, nil)
}

// waitOwing is waitOn for a target owed time to recover from faults.
func waitOwing(t *testing.T, ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time), stopped <-chan struct{}, owed func() time.Time) (bool, time.Duration, error) {
	t.Helper()
	return waitReading(ctx, c, func(since time.Time) (bool, time.Time, error) {
		ready, changed := state(since)
		return ready, changed, nil
	}, stopped, owed)
}

// waitReading is a settle wait over a reading of the run that can fail.
func waitReading(ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time, error), stopped <-chan struct{}, owed func() time.Time) (bool, time.Duration, error) {
	return waitHolding(ctx, c, state, holdsNothing, owed, stopped)
}

// waitHolding is a settle wait while the proxy holds requests as held says.
func waitHolding(ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time, error), held func(before time.Time) (bool, time.Time), owed func() time.Time, stopped <-chan struct{}) (bool, time.Duration, error) {
	return waitOnClock(ctx, c, settle{state: state, held: held, stopped: stopped, owed: owed})
}

// waitOnClock runs the wait on the clock, under testTimeouts.
func waitOnClock(ctx context.Context, c *clock, wait settle) (bool, time.Duration, error) {
	start := c.now
	wait.timeouts, wait.poll, wait.now, wait.sleep = testTimeouts, testPoll, func() time.Time { return c.now }, c.sleep
	converged, err := wait.wait(ctx)
	return converged, c.now.Sub(start), err
}

// waitFloored is a settle wait that may first converge floor into it.
func waitFloored(ctx context.Context, c *clock, state func(since time.Time) (bool, time.Time, error), floor time.Duration) (bool, time.Duration, error) {
	return waitOnClock(ctx, c, settle{state: state, held: holdsNothing, stopped: neverStops, floor: c.now.Add(floor)})
}

func holdsNothing(time.Time) (bool, time.Time) { return false, time.Time{} }

// holding is a proxy that holds each request from its arrival to its release,
// both offsets from the start of the wait.
type holding struct {
	c        *clock
	start    time.Time
	requests [][2]time.Duration
}

func (h holding) held(before time.Time) (held bool, released time.Time) {
	for _, request := range h.requests {
		arrived, release := h.start.Add(request[0]), h.start.Add(request[1])
		switch {
		case !arrived.Before(before) || h.c.now.Before(arrived):
		case h.c.now.Before(release):
			held = true
		case release.After(released):
			released = release
		}
	}
	return held, released
}

func readyAndQuiet(since time.Time) (bool, time.Time, error) { return true, since, nil }

func notReady(since time.Time) (bool, time.Time, error) { return false, since, nil }

// A ready that yields a non-bool is a configuration error the first time it
// does, not a CR that is never ready.
func TestSettleEndsWhereReadingTheRunFails(t *testing.T) {
	c := newClock()
	failed := errors.New("the expression did not yield a bool")
	polls := 0
	failsOnTheThirdPoll := func(since time.Time) (bool, time.Time, error) {
		if polls++; polls == 3 {
			return false, since, failed
		}
		return false, since, nil
	}

	converged, elapsed, err := waitReading(t.Context(), c, failsOnTheThirdPoll, neverStops, nil)

	if !errors.Is(err, failed) || converged {
		t.Fatalf("The wait returned (%t, %v), want the error reading the run.", converged, err)
	}
	if want := 2 * testPoll; elapsed != want {
		t.Errorf("The wait took %v, want the %v to the poll that failed.", elapsed, want)
	}
}

// A fault that stops 4s into the wait leaves the target owed until 9s, which
// the wait learns only then.
func TestSettleRunsAsLongAsRecoveryIsOwed(t *testing.T) {
	c := newClock()
	start := c.now
	notReady := func(since time.Time) (bool, time.Time) { return false, since }
	owed := func() time.Time {
		if c.now.Sub(start) < 4*time.Second {
			return time.Time{}
		}
		return start.Add(9 * time.Second)
	}

	converged, elapsed, err := waitOwing(t, t.Context(), c, notReady, neverStops, owed)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target is not ready.", converged, err)
	}
	if want := 9 * time.Second; elapsed != want {
		t.Errorf("The wait took %v, want the %v the target was owed.", elapsed, want)
	}
}

// The target may still be recovering from a fault, though ready held.
func TestSettleConvergesNoSoonerThanItsFloor(t *testing.T) {
	c := newClock()

	converged, elapsed, err := waitFloored(t.Context(), c, readyAndQuiet, 7*time.Second)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if want := 7 * time.Second; elapsed != want {
		t.Errorf("The wait took %v, want the %v to its floor.", elapsed, want)
	}
}

func TestSettleGivesUpTStablePastItsFloor(t *testing.T) {
	c := newClock()
	churning := func(time.Time) (bool, time.Time, error) { return true, c.now, nil }

	converged, elapsed, err := waitFloored(t.Context(), c, churning, 9*time.Second)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the run never holds still.", converged, err)
	}
	if want := 9*time.Second + testTimeouts.Stable; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_stable past its floor.", elapsed, want)
	}
}

func TestSettleIgnoresAFloorBehindIt(t *testing.T) {
	c := newClock()

	converged, elapsed, err := waitFloored(t.Context(), c, readyAndQuiet, -time.Second)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if elapsed != testTimeouts.Stable {
		t.Errorf("The wait took %v, want T_stable of %v.", elapsed, testTimeouts.Stable)
	}
}

func TestSettleGivesTSettleWhenLessIsOwed(t *testing.T) {
	c := newClock()
	owedEarly := func() time.Time { return c.now.Add(-time.Second) }
	notReady := func(since time.Time) (bool, time.Time) { return false, since }

	converged, elapsed, err := waitOwing(t, t.Context(), c, notReady, neverStops, owedEarly)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target is not ready.", converged, err)
	}
	if elapsed != testTimeouts.Settle {
		t.Errorf("The wait took %v, want T_settle of %v.", elapsed, testTimeouts.Settle)
	}
}

func TestSettleConvergesPastTSettleWhileRecoveryIsOwed(t *testing.T) {
	c := newClock()
	start := c.now
	readyAt7s := func(since time.Time) (bool, time.Time) { return c.now.Sub(start) >= 7*time.Second, since }
	owed := func() time.Time { return start.Add(9 * time.Second) }

	converged, elapsed, err := waitOwing(t, t.Context(), c, readyAt7s, neverStops, owed)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if want := 7 * time.Second; elapsed != want {
		t.Errorf("The wait took %v, want the %v the target took.", elapsed, want)
	}
}

func TestSettleConvergesOnceTheRunHoldsStill(t *testing.T) {
	c := newClock()
	quiet := func(since time.Time) (bool, time.Time) { return true, since }

	converged, elapsed, err := waitOn(t, t.Context(), c, quiet, neverStops)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if elapsed != testTimeouts.Stable {
		t.Errorf("The wait took %v, want T_stable of %v.", elapsed, testTimeouts.Stable)
	}
}

// A request the proxy holds is about to change what the checks read.
func TestSettleConvergesTStableAfterTheProxyReleasesWhatItHeld(t *testing.T) {
	c := newClock()
	proxy := holding{c: c, start: c.now, requests: [][2]time.Duration{{500 * time.Millisecond, 2500 * time.Millisecond}}}

	converged, elapsed, err := waitHolding(t.Context(), c, readyAndQuiet, proxy.held, nil, neverStops)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if want := 2500*time.Millisecond + testTimeouts.Stable; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_stable past the release.", elapsed, want)
	}
}

// A request held when the wait's time runs out gives the target T_settle past
// its release, as a change would.
func TestSettleGivesUpNoSoonerThanTSettlePastTheReleaseOfARequestItHeld(t *testing.T) {
	c := newClock()
	proxy := holding{c: c, start: c.now, requests: [][2]time.Duration{{4 * time.Second, 6 * time.Second}}}

	converged, elapsed, err := waitHolding(t.Context(), c, notReady, proxy.held, nil, neverStops)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target is not ready.", converged, err)
	}
	if want := 6*time.Second + testTimeouts.Settle; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_settle past the release.", elapsed, want)
	}
}

// A request that reaches the proxy once the wait's time has run out does not
// hold the wait open, so a proxy that never stops holding requests does not.
func TestSettleGivesUpWhileTheProxyKeepsHoldingRequests(t *testing.T) {
	c := newClock()
	proxy := holding{c: c, start: c.now}
	for arrived := time.Duration(0); arrived < time.Minute; arrived += 1500 * time.Millisecond {
		proxy.requests = append(proxy.requests, [2]time.Duration{arrived, arrived + 1500*time.Millisecond})
	}

	converged, elapsed, err := waitHolding(t.Context(), c, readyAndQuiet, proxy.held, nil, neverStops)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the proxy always holds a request.", converged, err)
	}
	// The last request to arrive within T_settle arrives at 4.5s.
	if want := 6*time.Second + testTimeouts.Settle; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_settle past the release of the last request that arrived within T_settle.", elapsed, want)
	}
}

func TestSettleWaitsForTStableAfterTheLastChange(t *testing.T) {
	c := newClock()
	const lastChange = time.Second
	changedOnce := func(since time.Time) (bool, time.Time) { return true, since.Add(lastChange) }

	converged, elapsed, err := waitOn(t, t.Context(), c, changedOnce, neverStops)

	if err != nil || !converged {
		t.Fatalf("The wait returned (%t, %v), want convergence.", converged, err)
	}
	if want := lastChange + testTimeouts.Stable; elapsed != want {
		t.Errorf("The wait took %v, want %v: T_stable is counted from the last change.", elapsed, want)
	}
}

func TestSettleGivesUpWhileTheRunKeepsChanging(t *testing.T) {
	c := newClock()
	churning := func(time.Time) (bool, time.Time) { return true, c.now }

	converged, elapsed, err := waitOn(t, t.Context(), c, churning, neverStops)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the run never holds still.", converged, err)
	}
	if elapsed != testTimeouts.Settle {
		t.Errorf("The wait took %v, want T_settle of %v.", elapsed, testTimeouts.Settle)
	}
}

func TestSettleGivesUpWhileTheTargetIsNotReady(t *testing.T) {
	c := newClock()
	notReady := func(since time.Time) (bool, time.Time) { return false, since }

	converged, elapsed, err := waitOn(t, t.Context(), c, notReady, neverStops)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target is not ready.", converged, err)
	}
	if elapsed != testTimeouts.Settle {
		t.Errorf("The wait took %v, want T_settle of %v.", elapsed, testTimeouts.Settle)
	}
}

func TestSettleWaitsForACRUnderDeletionToGo(t *testing.T) {
	cr := widget("widget")
	holds := func(*unstructured.Unstructured) (bool, error) { return true, nil }

	for _, test := range []struct {
		name    string
		deleted *metav1.Time
		want    bool
	}{
		{name: "a live CR", want: true},
		{name: "a CR under deletion", deleted: &metav1.Time{Time: time.Now()}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ready(holds, []observe.Version{{Object: cr, DeletionTimestamp: test.deleted}})

			if err != nil || got != test.want {
				t.Errorf("ready returned (%t, %v), want %t.", got, err, test.want)
			}
		})
	}
}

// A target that stopped will never converge, and the ops behind the wait would
// run against nothing, so the wait ends at the exit rather than at T_settle.
func TestSettleEndsWhereTheTargetStopped(t *testing.T) {
	c := newClock()
	stopped := make(chan struct{})
	polls := 0
	diesOnTheSecondPoll := func(since time.Time) (bool, time.Time) {
		if polls++; polls == 2 {
			close(stopped)
		}
		return false, since
	}

	converged, elapsed, err := waitOn(t, t.Context(), c, diesOnTheSecondPoll, stopped)

	if err != nil || converged {
		t.Fatalf("The wait returned (%t, %v), want no convergence: the target stopped.", converged, err)
	}
	if elapsed != testPoll {
		t.Errorf("The wait took %v, want the %v it took the target to stop.", elapsed, testPoll)
	}
}

// A target that stopped as its state came to rest did not converge: its exit
// came first.
func TestSettleDoesNotConvergeOnATargetThatStopped(t *testing.T) {
	c := newClock()
	start := c.now
	stopped := make(chan struct{})
	diesAsItSettles := func(since time.Time) (bool, time.Time) {
		if c.now.Sub(start) == testTimeouts.Stable {
			close(stopped)
		}
		return true, since
	}

	converged, _, err := waitOn(t, t.Context(), c, diesAsItSettles, stopped)

	if err != nil || converged {
		t.Errorf("The wait returned (%t, %v), want no convergence: the target stopped.", converged, err)
	}
}

func TestSettleEndsWithTheContext(t *testing.T) {
	c := newClock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	converged, _, err := waitOn(t, ctx, c, func(since time.Time) (bool, time.Time) { return false, since }, neverStops)

	if !errors.Is(err, context.Canceled) || converged {
		t.Errorf("The wait returned (%t, %v), want the context's error.", converged, err)
	}
}
