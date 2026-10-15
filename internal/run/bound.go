package run

import (
	"math"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// stopBudget is what stopping a run takes: stopping the target and deleting
// the namespace.
const stopBudget = launch.StopWithin + namespaceDeletionBudget

// Bound is the longest the Runner's waits can make the runs of the sequences
// take, one after another. A bound too long for a Duration is the longest
// Duration.
func Bound(t *target.Target, sequences ...Sequence) time.Duration {
	// Faults double the bound, so it is added up in float64, which does not
	// overflow.
	var total float64
	for _, s := range sequences {
		total += bound(t.Timeouts, s)
	}
	if total >= math.MaxInt64 {
		return math.MaxInt64
	}
	return time.Duration(total)
}

func bound(timeouts target.Timeouts, s Sequence) float64 {
	settle, deletion := float64(timeouts.Settle), float64(timeouts.Delete)
	waits := float64(defaultNamespaceDefaultsWithin)
	faults, stops, exits, untriggered := 0, 0, 0, false
	// delay is the longest a fault so far holds a request. A request held as
	// a wait's time runs out holds the wait open for hold more, a recreate's
	// wait for its CR too.
	var delay, hold float64
	for _, op := range s.Ops {
		if faults > 0 {
			// The op may first wait for the target to restart. The target is
			// then owed timeouts.settle past a return that can come
			// timeouts.settle after the op, timeouts.settle more than a
			// settle wait.
			waits += float64(launch.MaxBackoff) + settle
		}
		switch op.Type {
		case OpDelete, OpDeleteFixture:
			waits += deletion
		case OpRecreate:
			// Its wait for the CR to go lasts while the run is owed time,
			// which can run timeouts.settle past what the ops before it were
			// given.
			waits += max(deletion, settle) + hold
		case OpRestart:
			// The target is owed timeouts.settle past its return, which can
			// come timeouts.settle after the restart.
			waits += float64(launch.RestartWithin) + settle
		case OpFault:
			faults++
			if op.Fault.Action.Delay > 0 {
				delay = max(delay, float64(op.Fault.Action.Delay))
				hold = delay + settle
			}
			if op.Fault.Until == (Trigger{}) {
				untriggered = true
			} else {
				stops++
			}
		}
		if faults > 0 {
			exits++
		}
		if op.Settles() {
			waits += settle + hold
		}
	}
	// Faults with no trigger stop together, at the teardown.
	if untriggered {
		stops++
	}
	// From the first fault on, each op allows an exit, owed timeouts.settle
	// past a return that can come timeouts.settle after a restart that can
	// take MaxBackoff. Faults that stop are owed as long as they lasted and
	// timeouts.settle, a hold past that, and another exit. A fault lasts until
	// the proxy releases what it held, up to the delay past its trigger.
	exit := float64(launch.MaxBackoff) + 2*settle
	waits += float64(exits) * exit
	for range stops {
		waits = 2*(waits+delay) + settle + hold + exit
	}
	teardown := float64(timeouts.Stable) + deletion + float64(teardownMargin)
	return waits + teardown + float64(stopBudget)
}
