package check

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Backoff fails a failing or no-op request that the controller repeats in a
// tight loop. DESIGN.md §12 defines it.
func Backoff(ix *trace.Index, s Spec) []Finding {
	runs := map[request]*run{}
	found := map[findingKey]*Finding{}
	named := map[namedRequest]bool{}
	var order []findingKey
	steps := ix.Steps()
	holds := holdWatches(ix, s.Stable)
	w := owner.New(ix, s.Owners)
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
		// A create's object is the name it requests.
		object.Name = cmp.Or(object.Name, c.RequestName)
		r := request{c.Ref.System, c.Verb, object, c.Subresource, c.Status}
		futile := !succeeded(*c) || trace.Mutating(c.Verb) && !ix.Changed(e)
		if succeeded(*c) {
			// A success ends each run of its verb, object and subresource
			// that it does not join.
			for key := range runs {
				if key.object == r.object && key.verb == r.verb && key.subresource == r.subresource && (key != r || !futile) {
					delete(runs, key)
				}
			}
		}
		if !futile || c.Status == 409 && holds(c.Ref, start) {
			continue
		}
		rn := runs[r]
		if rn == nil || start-rn.last >= tightGap {
			rn = &run{first: start}
			runs[r] = rn
		}
		rn.last = start
		rn.calls++
		if rn.calls <= tightCalls*max(1, crsLive(w, rn.first, start)) {
			continue
		}
		summary := tightLoop
		if succeeded(*c) {
			summary = noOpLoop
		}
		key := findingKey{stepAt(steps, start), summary, kind(c.Ref)}
		if named[namedRequest{key, r}] {
			continue
		}
		named[namedRequest{key, r}] = true
		if found[key] == nil {
			found[key] = &Finding{Check: "Backoff", Step: key.step, At: e.At, Summary: summary, Kind: key.kind}
			order = append(order, key)
		}
		f := found[key]
		what := describe(r.object)
		if r.subresource != "" {
			what += "/" + r.subresource
		}
		f.Details = append(f.Details, fmt.Sprintf("%s %s: %d, %d calls from %v to %v", c.Verb, what, c.Status, rn.calls, rn.first, start))
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

// ownedByAnother reports whether ref's instance at t has a rule 1 to 3 owning
// CR other than cr, or else is a CR other than cr, so that cr's generation
// does not bear on it.
func ownedByAnother(w *owner.World, ref trace.Ref, cr string, t time.Duration) bool {
	i, found := w.Latest(ref, t)
	if !found || !i.Live(t) || i.UID == cr {
		return false
	}
	if owner, named := w.Owner(i, t).Named(); named {
		return owner != cr
	}
	return w.Primary(i)
}

const (
	tightLoop = "retried a failing request in a tight loop"
	noOpLoop  = "repeated a no-op write in a tight loop"
	// A run fails with more than tightCalls calls per CR live.
	tightCalls = 10
	tightGap   = 100 * time.Millisecond
)

// request is what a run of futile calls repeats.
type request struct {
	system, verb string
	object       trace.Ref
	subresource  string
	status       int
}

// run counts a request's futile calls, each under tightGap after the last.
type run struct {
	first, last time.Duration
	calls       int
}

// namedRequest is a request that a finding names.
type namedRequest struct {
	findingKey
	request
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
		case e.Version != nil && s.listed(e.Version.Ref):
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

// holdWatches says whether a holdWatch fault on ref's kind is active at t,
// or ended less than stable/4 before it, while the controller's cache
// catches up.
func holdWatches(ix *trace.Index, stable time.Duration) func(ref trace.Ref, t time.Duration) bool {
	return func(ref trace.Ref, t time.Duration) bool {
		return slices.ContainsFunc(ix.Holds(), func(h trace.Hold) bool {
			return h.Kind.Group == ref.Group && h.Kind.Resource == ref.Resource && h.Start <= t && t <= h.End+stable/4
		})
	}
}
