package check

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Backoff fails a failing request that the controller repeats in a tight
// loop. DESIGN.md §12 defines it.
func Backoff(ix *trace.Index, s Spec) []Finding {
	runs := map[request][]time.Duration{}
	found := map[findingKey]*Finding{}
	var order []findingKey
	steps := ix.Steps()
	holds := holdWatches(ix)
	w := owner.New(ix)
	for _, e := range backoffEvents(ix, s) {
		if v := e.Version; v != nil {
			for r := range runs {
				if !ownedByAnother(w, r.object, v.UID, e.At) {
					delete(runs, r)
				}
			}
			continue
		}
		c, start := e.Call, e.At-e.Call.Latency
		object := c.Object()
		object.Name = cmp.Or(object.Name, c.RequestName)
		r := request{c.Ref.System, c.Verb, object, c.Subresource, c.Status}
		if succeeded(*c) {
			for key := range runs {
				if key.object == r.object && key.verb == r.verb && key.subresource == r.subresource {
					delete(runs, key)
				}
			}
			continue
		}
		if c.Status == 409 && holds(c.Ref, start) {
			continue
		}
		runs[r] = append(runs[r], start)
		n := max(1, crsLive(w, runs[r][0], start))
		if len(runs[r]) != tightRepeats*2*n+1 || !tight(runs[r][tightRepeats*n:]) {
			continue
		}
		key := findingKey{stepAt(steps, start), tightLoop, kind(c.Ref)}
		if found[key] == nil {
			found[key] = &Finding{Check: "Backoff", Step: key.step, At: e.At, Summary: tightLoop, Kind: key.kind}
			order = append(order, key)
		}
		f := found[key]
		what := describe(r.object)
		if r.subresource != "" {
			what += "/" + r.subresource
		}
		f.Details = append(f.Details, fmt.Sprintf("%s %s: %d, %d calls from %v to %v", c.Verb, what, c.Status, len(runs[r]), runs[r][0], start))
	}
	findings := make([]Finding, len(order))
	for n, key := range order {
		findings[n] = *found[key]
	}
	return findings
}

// crsLive counts the CRs live at some time in [from, to]. Each CR may back
// off on its own, so their calls of one request interleave.
func crsLive(w *owner.World, from, to time.Duration) int {
	n := 0
	for _, i := range w.All() {
		if w.Primary(i) && i.Created <= to && (i.Gone == 0 || i.Gone >= from) {
			n++
		}
	}
	return n
}

// ownedByAnother reports whether ref's instance at t has a rule-1 owning
// CR other than cr, so that cr's generation does not bear on it.
func ownedByAnother(w *owner.World, ref trace.Ref, cr string, t time.Duration) bool {
	i, found := w.Latest(ref, t)
	if !found || !i.Live(t) {
		return false
	}
	if w.Primary(i) {
		return i.UID != cr
	}
	o := w.Owner(i, t)
	return o.Rule == 1 && o.CRs[0] != cr
}

const (
	tightLoop = "retried a failing request in a tight loop"
	// tightRepeats is how many repeats per CR come before the judged gaps,
	// and how many gaps per CR are judged.
	tightRepeats = 5
	tightGap     = 100 * time.Millisecond
)

// request is what a run of failing calls repeats.
type request struct {
	system, verb string
	object       trace.Ref
	subresource  string
	status       int
}

func tight(starts []time.Duration) bool {
	for n := 1; n < len(starts); n++ {
		if starts[n]-starts[n-1] >= tightGap {
			return false
		}
	}
	return true
}

// backoffEvents returns, in time order, the controller's judged calls that
// are neither watches nor of unknown outcome, by request start, and the CR
// versions whose generation differs from their previous version's.
func backoffEvents(ix *trace.Index, s Spec) []trace.Event {
	var events []trace.Event
	generations := map[string]any{}
	for _, e := range ix.Events() {
		switch {
		case e.Call != nil:
			if c := e.Call; c.Caller == trace.Controller && judged(e) && !c.Watch && !c.Unknown() {
				events = append(events, e)
			}
		case e.Version != nil && e.Version.Ref.Group == s.PrimaryGroup && e.Version.Ref.Resource == s.PrimaryResource:
			metadata, _ := decode(e.Version.Body)["metadata"].(map[string]any)
			was, seen := generations[e.Version.UID]
			generations[e.Version.UID] = metadata["generation"]
			if seen && was != metadata["generation"] {
				events = append(events, e)
			}
		}
	}
	slices.SortStableFunc(events, func(a, b trace.Event) int { return cmp.Compare(backoffTime(a), backoffTime(b)) })
	return events
}

func backoffTime(e trace.Event) time.Duration {
	if e.Call != nil {
		return e.At - e.Call.Latency
	}
	return e.At
}

// holdWatches says whether a holdWatch fault on ref's kind is active at t.
func holdWatches(ix *trace.Index) func(ref trace.Ref, t time.Duration) bool {
	type span struct {
		hold       trace.Ref
		start, end time.Duration
	}
	var spans []span
	starts := map[[2]int]int{}
	for _, e := range ix.Events() {
		f := e.Fault
		if f == nil || f.Hold == nil {
			continue
		}
		key := [2]int{f.Step, f.Index}
		if !f.Ended {
			starts[key] = len(spans)
			spans = append(spans, span{*f.Hold, e.At, time.Duration(1<<63 - 1)})
		} else if n, found := starts[key]; found {
			spans[n].end = e.At
		}
	}
	return func(ref trace.Ref, t time.Duration) bool {
		return slices.ContainsFunc(spans, func(s span) bool {
			return s.hold.Group == ref.Group && s.hold.Resource == ref.Resource && s.start <= t && t <= s.end
		})
	}
}
