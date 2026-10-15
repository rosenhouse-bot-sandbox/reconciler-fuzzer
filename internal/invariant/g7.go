package invariant

import (
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// SelfHealing is G7: an object a DeleteManaged op deleted exists again, by
// kind and name, where the settle wait after the op ends. A kind the target
// lists as not recreated is exempt.
func SelfHealing(in Input) (Result, error) {
	out := Result{ID: "G7"}
	for _, checkpoint := range in.Checkpoints {
		op, _ := in.op(checkpoint.Op) // Teardown and Recovery follow no op.
		deleted := op.Deleted
		if deleted == (observe.Key{}) || slices.Contains(in.Target.NotRecreated, deleted.GVK) {
			continue
		}
		end := checkpoint.Time
		seen := in.stateAt(end)
		if _, back := seen.version(deleted); back {
			continue
		}
		if took, _ := in.versionAt(deleted, op.Time); !in.askedFor(took, seen) {
			continue // No CR asks for the object back.
		}
		object := observe.KindName(deleted.GVK) + " " + deleted.Name
		if changed, found := in.changedBetween(in.lastConverged(op.Time), op.Time); found {
			out.note("for %s: the run had not converged since %s, so the target may not have wanted the %s back",
				describe(op), describe(changed), object)
			continue
		}
		if in.faulted(op.Time, end) || end.Before(in.RecreateOwed(op.Time)) {
			out.note("for %s: a fault was active during it or the wait after it, or the target was still owed time to recover from one where the wait ended, so it may have been unable to recreate the %s",
				describe(op), object)
			continue
		}
		if start, starting := in.stillStarting(op); starting {
			out.note("for %s: the target had %s between %s and it, so it may not yet have been running to recreate the %s",
				describe(op), notBack(in.Requests), start, object)
			continue
		}
		if in.stopped(op.Time, end) {
			out.note("for %s: the target exited, or waited to restart, during it or the wait after it, so it may not have been running to recreate the %s",
				describe(op), object)
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the %s that %s deleted never came back within the %s the run waited after it, and the target does not list %s under notRecreated",
				object, describe(op), end.Sub(checkpoint.Began).Round(time.Millisecond), observe.KindName(deleted.GVK)),
			At: end,
		}.quotingVersions(RecentHistory(deleted, upTo(in.History.History(deleted), end))).
			quotingManaged(Sample(seen.managed(in))))
	}
	return out, nil
}

// stillStarting names the target's last restart before the op if the target
// was not back by the op.
func (in Input) stillStarting(op Op) (string, bool) {
	start, named := in.lastRestart(op.Time)
	if named == "" {
		return "", false
	}
	back, found := Back(in.Requests, start)
	return named, !found || !back.Before(op.Time)
}

// stopped reports whether the target exited, or waited to restart, at some
// point in [from, to].
func (in Input) stopped(from, to time.Time) bool {
	return slices.ContainsFunc(in.Exits, func(exit Exit) bool {
		return !exit.At.After(to) && !exit.Restart.Before(from)
	})
}
