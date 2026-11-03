package check

import (
	"maps"
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// releasedFirst is the summary of a Teardown finding.
const releasedFirst = "released a CR before deleting its resources"

// Teardown fails a controller write that releases a deleting CR while a
// resource of the CR that gc does not delete is not yet being deleted
// (DESIGN.md §12).
func Teardown(ix *trace.Index, s Spec) []Finding {
	w := owner.New(ix, s.Owners)
	steps, deletes := ix.Steps(), deleteCalls(ix)
	details, at := map[int]map[string][]string{}, map[int]time.Duration{}
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		cr, released := releasedBy(ix, e, s)
		if !released {
			continue
		}
		for _, i := range w.Instances(e.At) {
			o := w.Owner(i, e.At)
			if uid, _ := o.Named(); uid != cr || !controllerResource(i) || w.Collected(i, cr, e.At) || beingDeleted(ix, w, deletes, i, e.At) {
				continue
			}
			step := stepAt(steps, e.At-e.Call.Latency)
			if details[step] == nil {
				details[step], at[step] = map[string][]string{}, e.At
			}
			k := kind(i.Ref)
			details[step][k] = append(details[step][k], describeOwned(w, i, o))
		}
	}
	var findings []Finding
	for _, step := range slices.Sorted(maps.Keys(details)) {
		findings = append(findings, byKind("Teardown", step, at[step], releasedFirst, details[step])...)
	}
	return findings
}

// releasedBy returns the uid of the object that e, a controller write,
// released: the write left the object, which was being deleted, none of the
// finalizers that the controller may own.
func releasedBy(ix *trace.Index, e trace.Event, s Spec) (string, bool) {
	if !judgedWrite(*e.Call, s) {
		return "", false
	}
	base := replaced(ix, e)
	own := slices.DeleteFunc(finalizers(base), func(f string) bool {
		_, foreign := adder(ix, base, f)
		return foreign
	})
	kept := slices.ContainsFunc(own, func(f string) bool { return slices.Contains(*e.Call.ResultFinalizers, f) })
	return base.UID, deleting(base.Body) && len(own) > 0 && !kept
}

// beingDeleted reports whether, by t, i had a deletionTimestamp or a delete
// of it had answered. A delete sent before i appeared may have hit an
// earlier instance of i's ref, so it does not count.
func beingDeleted(ix *trace.Index, w *owner.World, deletes []trace.Event, i owner.Instance, t time.Duration) bool {
	if start, found := deletionStart(w.Versions(i)); found && start <= t {
		return true
	}
	return slices.ContainsFunc(deletes, func(d trace.Event) bool {
		return d.At <= t && d.Call.Object().Covers(i.Ref) && d.At-d.Call.Latency >= appeared(ix, i)
	})
}

// deleteCalls are the delete and deletecollection calls of every caller
// that the server answered with a 2xx or a 404.
func deleteCalls(ix *trace.Index) []trace.Event {
	var deletes []trace.Event
	for _, e := range ix.Events() {
		if c := e.Call; c != nil && (c.Verb == "delete" || c.Verb == "deletecollection") && (succeeded(*c) || c.Status == 404) {
			deletes = append(deletes, e)
		}
	}
	return deletes
}
