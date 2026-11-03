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
	// retained maps each object that the controller released to whether its
	// deletion policy keeps its resources.
	retained := map[string]bool{}
	details, at := map[int]map[string][]string{}, map[int]time.Duration{}
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		released, ok := releasedBy(ix, e, s)
		if !ok {
			continue
		}
		if retained[released.UID] = retains(s, released.Ref, released.Body); retained[released.UID] {
			continue
		}
		for _, i := range w.Instances(e.At) {
			if !controllerResource(i) {
				continue
			}
			o, owned := ownedBy(w, s, i, released.UID, retained, e.At)
			if !owned || w.Collected(i, released.UID, e.At) || beingDeleted(ix, w, deletes, i, e.At) {
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

// ownedBy returns the owner that describes i, and whether rules 1 to 3 make
// i a resource of the CR with uid, which the controller released at t. Rule
// 3 may find several CRs that name i. Then i is that CR's once each of them
// is released or gone, unless one of them retains i.
func ownedBy(w *owner.World, s Spec, i owner.Instance, uid string, retained map[string]bool, t time.Duration) (owner.Owner, bool) {
	if o := w.Owner(i, t); o.Rule != 3 {
		cr, named := o.Named()
		return o, named && cr == uid
	}
	naming := w.Naming(i, t)
	return owner.Owner{Rule: 3, CRs: []string{uid}}, slices.ContainsFunc(naming, func(cr owner.Instance) bool { return cr.UID == uid }) &&
		!slices.ContainsFunc(naming, func(cr owner.Instance) bool {
			if kept, released := retained[cr.UID]; released {
				return kept
			}
			return cr.Live(t) || retains(s, cr.Ref, w.Body(cr, t))
		})
}

// releasedBy returns the version of the object that e, a controller write,
// released: the write left the object, which was being deleted, none of the
// finalizers that the controller may own. Only a CR owns resources, so
// another object's release finds none.
func releasedBy(ix *trace.Index, e trace.Event, s Spec) (trace.Version, bool) {
	if !judgedWrite(*e.Call, s) {
		return trace.Version{}, false
	}
	base := replaced(ix, e)
	own := slices.DeleteFunc(finalizers(base), func(f string) bool {
		_, foreign := adder(ix, base, f)
		return foreign
	})
	kept := slices.ContainsFunc(own, func(f string) bool { return slices.Contains(*e.Call.ResultFinalizers, f) })
	return base, deleting(base.Body) && len(own) > 0 && !kept
}

// beingDeleted reports whether, by t, i or the kube namespace that holds it
// had a deletionTimestamp or a delete of it had answered. The namespace
// controller deletes what a deleting namespace holds. A delete sent before
// an instance appeared may have hit an earlier instance of its ref, so it
// does not count. A kube delete counts only if the trace shows i deleting
// or gone, since a dry run or a deletecollection's selector may spare it.
func beingDeleted(ix *trace.Index, w *owner.World, deletes []trace.Event, i owner.Instance, t time.Duration) bool {
	start, marked := deletionStart(w.Versions(i))
	if marked && start <= t {
		return true
	}
	if (i.Ref.System != "kube" || marked || i.Gone != 0) && slices.ContainsFunc(deletes, func(d trace.Event) bool {
		return d.At <= t && d.Call.Object().Covers(i.Ref) && d.At-d.Call.Latency >= appeared(ix, i)
	}) {
		return true
	}
	if i.Ref.System != "kube" {
		return false
	}
	namespace, found := w.Latest(trace.Ref{System: "kube", Resource: "namespaces", Name: i.Ref.Namespace}, t)
	return found && beingDeleted(ix, w, deletes, namespace, t)
}

// deleteCalls are the delete and deletecollection calls of every caller,
// with no subresource, that the server answered with a 2xx or a 404.
func deleteCalls(ix *trace.Index) []trace.Event {
	var deletes []trace.Event
	for _, e := range ix.Events() {
		if c := e.Call; c != nil && (c.Verb == "delete" || c.Verb == "deletecollection") && c.Subresource == "" && (succeeded(*c) || c.Status == 404) {
			deletes = append(deletes, e)
		}
	}
	return deletes
}
