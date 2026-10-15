package invariant

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// MaxEvidence caps how much of a request log or a version history one
// violation carries into the report.
const MaxEvidence = 20

// quiet is a stretch in which the run must have gone still: the
// timeouts.stable that follows an op's settle wait, and the timeouts.stable
// the teardown waits before it deletes. G1 and G2 both check it.
type quiet struct {
	what       string
	start, end time.Time
}

func (w quiet) String() string { return w.what }

// quietWindows returns every window the run observed to its end under an
// unchanged spec and no fault: one per op that settled, and the teardown's.
func (in Input) quietWindows() []quiet {
	var windows []quiet
	stable := in.timeouts().Stable
	for i, op := range in.Ops {
		settled, ok := in.settleEnd(op.Index)
		if !ok {
			continue
		}
		w := quiet{
			what:  fmt.Sprintf("the %s (timeouts.stable) after op %d (%s) settled", stable, op.Index, op.Type),
			start: settled,
			end:   settled.Add(stable),
		}
		if !in.observed(w.end) || in.faulted(op.Time, w.end) || in.tornDown(w.end) {
			continue
		}
		if i+1 < len(in.Ops) && in.Ops[i+1].Time.Before(w.end) {
			continue // Another op reopened the window, so it is that op's.
		}
		windows = append(windows, w)
	}
	if w, ok := in.teardownWindow(); ok {
		windows = append(windows, w)
	}
	return windows
}

// teardownWindow is the timeouts.stable the teardown waits before it deletes
// anything. It is a quiet window of its own, because a run whose last op never
// settles has no other. The teardown clears every fault before the window
// opens, so a fault it cleared did not reach into it.
func (in Input) teardownWindow() (quiet, bool) {
	w := quiet{
		what:  fmt.Sprintf("the %s (timeouts.stable) the teardown waited before deleting", in.timeouts().Stable),
		start: in.Quiet,
		end:   in.Teardown,
	}
	if w.start.IsZero() || w.end.IsZero() || !in.observed(w.end) || in.faulted(w.start, w.end) {
		return quiet{}, false
	}
	return w, true
}

// settleEnd is when the op's settle wait ended, on convergence or where it
// gave up, which is where the op's quiet window opens. An op the Runner did
// not settle after, or whose wait the run did not reach the end of, has no
// window.
func (in Input) settleEnd(op int) (time.Time, bool) {
	for _, checkpoint := range in.Checkpoints {
		if checkpoint.Op == op && checkpoint.Settle != NoSettle {
			return checkpoint.Time, true
		}
	}
	return time.Time{}, false
}

// tornDown reports whether reconciler-fuzzer had started emptying the namespace
// by t, so that what happened then is no longer the target's doing.
func (in Input) tornDown(t time.Time) bool {
	return !in.Teardown.IsZero() && t.After(in.Teardown)
}

// Excerpt is bounded evidence and how many entries it was chosen from. The
// count lets a report say it quoted a part. Of names the object a timeline is
// the history of, and is empty for evidence drawn from several.
type Excerpt[T any] struct {
	Quoted []T
	Total  int
	Of     string
}

// Recent caps a timeline of evidence at its MaxEvidence latest entries. The
// Runner bounds the evidence of a violation it raises itself the same way.
func Recent[T any](evidence []T) Excerpt[T] {
	if len(evidence) > MaxEvidence {
		return Excerpt[T]{Quoted: evidence[len(evidence)-MaxEvidence:], Total: len(evidence)}
	}
	return Excerpt[T]{Quoted: evidence, Total: len(evidence)}
}

// RecentHistory caps one object's history and names it, so that a report can
// say whose versions it quotes without any site spelling the subject out.
func RecentHistory(key observe.Key, history []observe.Version) Excerpt[observe.Version] {
	quoted := Recent(history)
	quoted.Of = observe.KindName(key.GVK) + " " + key.Name
	return quoted
}

// Sample caps the state of the objects the target managed at a verdict, within
// a bound of its own so that it does not crowd out the timeline beside it. It
// takes one object from each kind in turn, newest first, so that a bound too
// small for them drops the oldest of each kind rather than every object of the
// kinds that sort last.
func Sample(managed []observe.Version) Excerpt[observe.Version] {
	kinds := map[schema.GroupVersionKind][]observe.Version{}
	for _, v := range managed {
		kinds[v.GVK] = append(kinds[v.GVK], v)
	}
	order := slices.SortedFunc(maps.Keys(kinds), func(a, b schema.GroupVersionKind) int {
		return strings.Compare(observe.KindName(a), observe.KindName(b))
	})
	for _, gvk := range order {
		slices.SortStableFunc(kinds[gvk], func(a, b observe.Version) int { return b.Time.Compare(a.Time) })
	}
	var quoted []observe.Version
	for i := 0; len(quoted) < len(managed); i++ {
		for _, gvk := range order {
			if i < len(kinds[gvk]) {
				quoted = append(quoted, kinds[gvk][i])
			}
		}
	}
	return Excerpt[observe.Version]{Quoted: quoted[:min(len(quoted), MaxEvidence)], Total: len(managed)}
}
