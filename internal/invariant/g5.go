package invariant

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// RestartStable is G5: restarting the target does not change converged state.
// It compares the last converged snapshot before each Restart with the first
// converged one after it, keyed by kind and name. A Restart missing either
// snapshot, or with a fault between them, is not evaluated, and the result
// says so. G5 leaves out what a change of reconciler-fuzzer's between them may
// have changed, and says so too.
func RestartStable(in Input) (Result, error) {
	out := Result{ID: "G5"}
	for _, op := range in.Ops {
		if op.Type != OpRestart {
			continue
		}
		before, after, missing := in.convergedAround(op)
		if missing != "" {
			out.note("for %s: it has no converged snapshot %s it", describe(op), missing)
			continue
		}
		changes := in.changesBetween(before.Time, after.Time)
		reached := in.reached(changes)
		if len(changes) > 0 && !in.spares(reached, before, after) {
			out.note("for %s: %s ran between the converged states before and after it, so G5 cannot tell what the restart changed; a settle op on each side of a restart lets G5 judge it",
				describe(op), describeAll(changes))
			continue
		}
		if in.faulted(before.Time, after.Time) {
			out.note("for %s: a fault was active between the converged states before and after it, so G5 cannot tell what the restart changed",
				describe(op))
			continue
		}
		if len(changes) > 0 {
			out.note("for %s on what %s may have changed between the converged states before and after it",
				describe(op), describeAll(changes))
		}
		out.compare(in, op, before, after, reached)
	}
	return out, nil
}

func describeAll(ops []Op) string {
	described := make([]string, len(ops))
	for i, op := range ops {
		described[i] = describe(op)
	}
	last := len(described) - 1
	if last == 0 {
		return described[0]
	}
	return strings.Join(described[:last], ", ") + " and " + described[last]
}

// changedBetween returns the first op that changed a CR, a managed object or a
// fixture in [from, to).
func (in Input) changedBetween(from, to time.Time) (Op, bool) {
	if changes := in.changesBetween(from, to); len(changes) > 0 {
		return changes[0], true
	}
	return Op{}, false
}

// changesBetween are the ops that changed a CR, a managed object or a fixture
// in [from, to).
func (in Input) changesBetween(from, to time.Time) []Op {
	var changes []Op
	for _, op := range in.Ops {
		if op.changesRun() && !op.Time.Before(from) && op.Time.Before(to) {
			changes = append(changes, op)
		}
	}
	return changes
}

// spares reports whether either converged state holds an object the reach
// left alone.
func (in Input) spares(reached reach, before, after Checkpoint) bool {
	for _, s := range in.statesAt([]time.Time{before.Time, after.Time}) {
		if slices.ContainsFunc(s.live, func(v observe.Version) bool { return !reached.touches(in, v) }) {
			return true
		}
	}
	return false
}

// convergedAround returns the checkpoints of the settle waits that converged
// on either side of the op, or which side is missing.
func (in Input) convergedAround(op Op) (before, after Checkpoint, missing string) {
	var last, first *Checkpoint
	for i, checkpoint := range in.Checkpoints {
		switch {
		case checkpoint.Settle != Converged:
		case checkpoint.Time.Before(op.Time):
			last = &in.Checkpoints[i]
		case first == nil && checkpoint.Time.After(op.Time):
			first = &in.Checkpoints[i]
		}
	}
	switch {
	case last == nil:
		return before, after, "before"
	case first == nil:
		return before, after, "after"
	}
	return *last, *first, ""
}

// changedObject is one object that differs across a Restart, and the version
// whose history a report quotes.
type changedObject struct {
	version     observe.Version
	how         string
	differences []Difference
}

// compare reports every object that the Restart added, dropped or changed, in
// one violation. It leaves out what reconciler-fuzzer's changes reached.
func (out *Result) compare(in Input, op Op, before, after Checkpoint, reached reach) {
	states := in.statesAt([]time.Time{before.Time, after.Time})
	differ := in.differ(states[0], states[1], out)
	indexed := func(s state) map[objectKey]observe.Version {
		objects := map[objectKey]observe.Version{}
		for _, v := range s.live {
			if !reached.touches(in, v) {
				objects[objectKey{kind: observe.KindName(v.GVK), name: v.Name}] = v
			}
		}
		return objects
	}
	was, is := indexed(states[0]), indexed(states[1])
	var changed []changedObject
	for _, key := range union(was, is) {
		old, existed := was[key]
		current, exists := is[key]
		object := Difference{Object: key.kind + " " + key.name, ResourceVersions: [2]string{old.ResourceVersion, current.ResourceVersion}}
		switch {
		case !exists:
			changed = append(changed, changedObject{old, "is gone after", []Difference{whole(object, "(present)", "(absent)")}})
		case !existed:
			changed = append(changed, changedObject{current, "appeared only after", []Difference{whole(object, "(absent)", "(present)")}})
		default:
			if differences := differ(object, old, current); len(differences) > 0 {
				changed = append(changed, changedObject{current, "changed across", differences})
			}
		}
	}
	if len(changed) == 0 {
		return
	}
	first := changed[0].version
	statement := fmt.Sprintf("the %s %s %s the Restart at %s", observe.KindName(first.GVK), first.Name, changed[0].how, describe(op))
	if len(changed) > 1 {
		statement = fmt.Sprintf("%d objects differ across the Restart at %s, the first the %s %s, which %s it",
			len(changed), describe(op), observe.KindName(first.GVK), first.Name, changed[0].how)
	}
	out.violate(Violation{
		Statement: statement,
		At:        after.Time,
		Compared:  fmt.Sprintf("the state converged after %s and the one after %s", in.describeOp(before.Op), in.describeOp(after.Op)),
	}.quotingVersions(RecentHistory(first.Key, upTo(in.History.History(first.Key), after.Time))).
		quotingDifferences(spread(changed)))
}

type objectKey struct{ kind, name string }

// spread bounds the differences at MaxEvidence, taking one from each object in
// turn, so that the bound drops those of the objects with the most rather than
// every object after the first.
func spread(changed []changedObject) Excerpt[Difference] {
	total := 0
	for _, c := range changed {
		total += len(c.differences)
	}
	kept := make([]int, len(changed))
	for n := 0; n < min(total, MaxEvidence); {
		for i, c := range changed {
			if kept[i] < len(c.differences) && n < MaxEvidence {
				kept[i]++
				n++
			}
		}
	}
	var quoted []Difference
	for i, c := range changed {
		quoted = append(quoted, c.differences[:kept[i]]...)
	}
	return Excerpt[Difference]{Quoted: quoted, Total: total}
}

func union(was, is map[objectKey]observe.Version) []objectKey {
	keys := make([]objectKey, 0, len(was)+len(is))
	for key := range was {
		keys = append(keys, key)
	}
	for key := range is {
		if _, both := was[key]; !both {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b objectKey) int {
		return cmp.Or(strings.Compare(a.kind, b.kind), strings.Compare(a.name, b.name))
	})
	return keys
}
