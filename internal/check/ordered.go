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
	steps := ix.Steps()
	details, at := map[int]map[string][]string{}, map[int]time.Duration{}
	for _, object := range w.All() {
		r, found := releaseOf(w, object)
		if seens := ix.Seen(trace.Controller, object.UID); !found || len(seens) == 0 || seens[0].At > r.start-s.Stable {
			continue
		}
		for _, cr := range crs {
			if !kept(ix.Seen(trace.Controller, cr.UID), claims[cr.UID], object.Ref, r, r.start-s.Stable) {
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

// A release is an instance's deleted version, at at with resourceVersion
// rv. The instance's deletion started at start: its first version with a
// deletionTimestamp, or else the release.
type release struct {
	at, start time.Duration
	rv        uint64
}

// releaseOf is i's release, if it has one.
func releaseOf(w *owner.World, i owner.Instance) (release, bool) {
	versions := w.Versions(i)
	last := versions[len(versions)-1]
	if !last.Version.Deleted {
		return release{}, false
	}
	r := release{at: last.At, start: last.At, rv: revision(last.Version.RV)}
	if n := slices.IndexFunc(versions, func(e trace.Event) bool { return deleting(e.Version.Body) }); n >= 0 {
		r.start = versions[n].At
	}
	return r, true
}

// A claim is a version of a CR, by resourceVersion, and the objects it names
// by block references. A deleted version names none.
type claim struct {
	rv      uint64
	objects []trace.Ref
}

// claims are the claims of versions, a CR instance's versions.
func (s Spec) claims(versions []trace.Event) []claim {
	claims := make([]claim, len(versions))
	for n, e := range versions {
		claims[n].rv = revision(e.Version.RV)
		if !e.Version.Deleted {
			claims[n].objects = s.blocked(e.Version.Ref, decode(e.Version.Body))
		}
	}
	return claims
}

// kept reports whether a CR, whose versions make claims in order, kept
// object until r: by bound, one of seens, the controller's seens of the CR,
// showed a version that names object, and each later version before r names
// it too.
func kept(seens []trace.Event, claims []claim, object trace.Ref, r release, bound time.Duration) bool {
	held := false
	for _, c := range claims {
		if c.rv >= r.rv {
			break
		}
		held = slices.Contains(c.objects, object) && (held || slices.ContainsFunc(seens, func(e trace.Event) bool {
			return e.At <= bound && revision(e.Seen.RV) == c.rv
		}))
	}
	return held
}

// revision is a resourceVersion as the integer etcd assigns, which orders
// versions of objects of different kinds. The observer's watches do not.
func revision(rv string) uint64 {
	n, _ := strconv.ParseUint(rv, 10, 64)
	return n
}
