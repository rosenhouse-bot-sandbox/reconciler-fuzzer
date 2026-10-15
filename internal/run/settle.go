package run

import (
	"context"
	"errors"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// settlePoll is how often a settle wait reads the Observer.
const settlePoll = 50 * time.Millisecond

// Settle waits for the target's reaction: the Ready predicate holds, the
// target has shown it runs, and neither the CR nor a managed object has
// changed, nor the target restarted or come back, nor the proxy held one of
// its requests, for timeouts.stable. It reports whether it converged within
// timeouts.settle, or by what owed returns if that is later: the target may
// still be recovering from a fault or deleting a CR. A request the proxy held
// by then extends the wait to timeouts.settle past its release. A nil owed
// owes nothing. The caller judges a wait that expires.
func (h *Harness) Settle(ctx context.Context, owed func() time.Time) (bool, error) {
	return h.settleFrom(ctx, owed, time.Time{})
}

// settleFrom is Settle, converging no sooner than floor and giving up no
// sooner than timeouts.stable past it.
func (h *Harness) settleFrom(ctx context.Context, owed func() time.Time, floor time.Time) (bool, error) {
	return settle{
		timeouts: h.target.Timeouts,
		poll:     settlePoll,
		now:      time.Now,
		sleep:    sleep,
		state:    h.state,
		held:     h.Proxy.Held,
		stopped:  h.Launcher.Exited(),
		owed:     owed,
		floor:    floor,
	}.wait(ctx)
}

// settle is one settle wait. Its clock and its reading of the run are injected,
// so the decision is testable without a cluster.
type settle struct {
	timeouts target.Timeouts
	poll     time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	// state reports whether the target is ready, and when the run last
	// changed, which is never before since. Its error ends the wait.
	state func(since time.Time) (ready bool, changed time.Time, err error)
	// held reports, of the target's requests that reached the proxy before
	// then, whether the proxy holds one and when it last released one.
	held func(before time.Time) (holding bool, released time.Time)
	// stopped is closed once the target's process has stopped.
	stopped <-chan struct{}
	// owed is when the wait may give up, which can move while the wait runs.
	// Nil owes nothing.
	owed func() time.Time
	// floor is when the wait may first converge. The wait gives up no sooner
	// than timeouts.stable past it.
	floor time.Time
}

func (s settle) wait(ctx context.Context) (bool, error) {
	start := s.now()
	for {
		ready, changed, err := s.state(start)
		if err != nil {
			return false, err
		}
		now := s.now()
		// A target that stopped will never converge, so the wait ends there.
		if closed(s.stopped) {
			return false, nil
		}
		// A held request is about to change what the checks read.
		if holding, released := s.held(now); holding {
			changed = now
		} else if released.After(changed) {
			changed = released
		}
		if ready && !now.Before(changed.Add(s.timeouts.Stable)) && !now.Before(s.floor) {
			return true, nil
		}
		deadline := start.Add(s.timeouts.Settle)
		if floored := s.floor.Add(s.timeouts.Stable); floored.After(deadline) {
			deadline = floored
		}
		if s.owed != nil {
			if owed := s.owed(); owed.After(deadline) {
				deadline = owed
			}
		}
		if !now.Before(heldOpen(s.held, deadline, now, s.timeouts.Settle)) {
			return false, nil
		}
		if err := s.sleep(ctx, s.poll); err != nil {
			return false, err
		}
	}
}

// heldOpen is when a wait whose time runs out at deadline may end: settle
// past the release of each request that reached the proxy before deadline.
// While the proxy still holds one, its release is yet to come, so heldOpen
// answers settle from now. Each hold ends within its delay.
func heldOpen(held func(before time.Time) (bool, time.Time), deadline, now time.Time, settle time.Duration) time.Time {
	holding, released := held(deadline)
	if holding {
		released = now
	}
	if end := released.Add(settle); end.After(deadline) {
		return end
	}
	return deadline
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// state reads the launcher, the proxy and the Observer. Readiness is read
// first, so that a change arriving during the read counts against stability
// rather than for it. The op that opened the wait changed the CR, which is why
// stability runs from since. A target waiting to restart, or not back since
// it started, is down. Its start and its return count as changes.
func (h *Harness) state(since time.Time) (bool, time.Time, error) {
	target := h.Launcher.Status()
	back, running := invariant.Back(h.Proxy.Log(), target.Started)
	ready, err := ready(h.target.Ready, h.Observer.Current(h.target.Primary))
	changed := since
	if window := h.Observer.Window(since, time.Now()); len(window) > 0 {
		changed = window[len(window)-1].Time
	}
	for _, change := range []time.Time{target.Started, back} {
		if change.After(changed) {
			changed = change
		}
	}
	return ready && running && !target.Restarting, changed, err
}

// ready reports whether the predicate holds on every primary CR observed. An
// evaluation error means "not ready", and a result that is not a bool is a
// configuration error. A CR under deletion is not ready until it is gone, and
// a run whose CR is gone has nothing left to be ready.
func ready(predicate target.ReadyFunc, observed []observe.Version) (bool, error) {
	for _, cr := range observed {
		ready, err := predicate(cr.Object)
		if errors.Is(err, target.ErrNotBool) {
			return false, err
		}
		if err != nil || !ready || cr.DeletionTimestamp != nil {
			return false, nil
		}
	}
	return true, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
