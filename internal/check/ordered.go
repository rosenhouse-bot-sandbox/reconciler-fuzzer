package check

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// released is the summary of an Ordered finding.
const released = "released an object that another names"

// Ordered fails the to object of a block reference that is released while
// a from CR that the controller saw names it (DESIGN.md §12).
func Ordered(ix *trace.Index, s Spec) []Finding {
	if !slices.ContainsFunc(s.References, func(r Reference) bool { return r.Block }) {
		return nil
	}
	w := owner.New(ix, s.Owners)
	var crs []owner.Instance
	claims := map[string][]claim{}
	for _, i := range w.All() {
		if w.Primary(i) {
			crs = append(crs, i)
			claims[i.UID] = s.claims(w.Versions(i))
		}
	}
	steps, due := ix.Steps(), s.holdDue(ix)
	details, at := map[int]map[string][]string{}, map[int]time.Duration{}
	for _, object := range w.All() {
		r, found := releaseOf(w, object)
		if !found {
			continue
		}
		inTime := func(e trace.Event) bool { return due(e.At) <= r.start }
		if !slices.ContainsFunc(ix.Seen(trace.Controller, object.UID), inTime) {
			continue
		}
		for _, cr := range crs {
			if !kept(ix.Seen(trace.Controller, cr.UID), claims[cr.UID], object.Ref, r, inTime) || breaksCycle(crs, claims, r.rv, object.Ref, cr.Ref) {
				continue
			}
			step := stepAt(steps, r.at)
			if details[step] == nil {
				details[step], at[step] = map[string][]string{}, r.at
			}
			at[step] = min(at[step], r.at)
			k := kind(object.Ref)
			details[step][k] = append(details[step][k], describe(object.Ref)+": "+crName(w, cr.UID)+" names it")
		}
	}
	var findings []Finding
	for _, step := range slices.Sorted(maps.Keys(details)) {
		findings = append(findings, byKind("Ordered", step, at[step], released, details[step])...)
	}
	return findings
}

// holdDue returns when a controller that saw an object, or a CR that names
// it, at sight must hold the object. The hold is due stable later, stalls
// aside. If a step's faults stalled the controller after sight and began by
// then, the hold is due no sooner than the step's excuse start, since the
// controller may wait in retry backoff until then.
func (s Spec) holdDue(ix *trace.Index) func(sight time.Duration) time.Duration {
	steps, faulted := ix.Steps(), faultStalls(ix)
	all := stalls(ix, faulted)
	return func(sight time.Duration) time.Duration {
		due := until(sight, s.Stable, all)
		for _, step := range steps {
			if f, found := faulted[step.Step]; found && f.end > sight && f.start <= due {
				due = max(due, s.excuseStart(step))
			}
		}
		return due
	}
}

// A release is an instance's deleted version, at at with resourceVersion
// rv. The instance's deletion started at start: its first version with a
// deletionTimestamp, or else the release.
type release struct {
	at, start time.Duration
	rv        uint64
}

// releaseOf is i's release, if it has one. A delete that the observer
// recorded from a relist carries the resourceVersion of the version before
// it, so the trace does not order it, and releaseOf reports none.
func releaseOf(w *owner.World, i owner.Instance) (release, bool) {
	versions := w.Versions(i)
	last := versions[len(versions)-1]
	if !last.Version.Deleted {
		return release{}, false
	}
	r := release{at: last.At, start: last.At, rv: revision(last.Version.RV)}
	if len(versions) > 1 && r.rv <= revision(versions[len(versions)-2].Version.RV) {
		return release{}, false
	}
	if start, found := deletionStart(versions); found {
		r.start = start
	}
	return r, true
}

// A claim is a version of a CR, by resourceVersion, the objects it names by
// block references, and whether the CR is being deleted. A deleted version
// names none.
type claim struct {
	rv       uint64
	objects  []trace.Ref
	deleting bool
}

// claims are the claims of versions, a CR instance's versions.
func (s Spec) claims(versions []trace.Event) []claim {
	claims := make([]claim, len(versions))
	for n, e := range versions {
		claims[n].rv = revision(e.Version.RV)
		if !e.Version.Deleted {
			claims[n].objects = s.blocked(e.Version.Ref, decode(e.Version.Body))
			claims[n].deleting = deleting(e.Version.Body)
		}
	}
	return claims
}

// kept reports whether a CR, whose versions make claims in order, kept
// object until r: one of seens, the controller's seens of the CR, that is in
// time showed a version that names object, and each later version before r
// names it too. If the controller saw a version before r that the observer
// missed, the trace may lack a move off object, and kept reports false.
func kept(seens []trace.Event, claims []claim, object trace.Ref, r release, inTime func(trace.Event) bool) bool {
	if slices.ContainsFunc(seens, func(e trace.Event) bool {
		rv := revision(e.Seen.RV)
		return rv < r.rv && !slices.ContainsFunc(claims, func(c claim) bool { return c.rv == rv })
	}) {
		return false
	}
	held := false
	for _, c := range claims {
		if c.rv >= r.rv {
			break
		}
		held = slices.Contains(c.objects, object) && (held || slices.ContainsFunc(seens, func(e trace.Event) bool {
			return revision(e.Seen.RV) == c.rv && inTime(e)
		}))
	}
	return held
}

// breaksCycle reports whether, by the claims of crs before rv, releasing
// object breaks a cycle of block names among CRs being deleted that cr is on.
// Clause 2 needs such a cycle broken.
func breaksCycle(crs []owner.Instance, claims map[string][]claim, rv uint64, object, cr trace.Ref) bool {
	blocked, deletingCR := map[trace.Ref][]trace.Ref{}, map[trace.Ref]bool{}
	for _, i := range crs {
		for _, c := range claims[i.UID] {
			if c.rv >= rv {
				break
			}
			blocked[i.Ref], deletingCR[i.Ref] = c.objects, c.deleting
		}
	}
	return reaches(blocked, deletingCR, object, cr)
}

// revision is a resourceVersion as the integer etcd assigns, which orders
// versions of objects of different kinds. The observer's watches do not.
func revision(rv string) uint64 {
	n, _ := strconv.ParseUint(rv, 10, 64)
	return n
}
