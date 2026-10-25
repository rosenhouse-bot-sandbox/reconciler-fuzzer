package check

import (
	"cmp"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// named is an object that a CR names by s.References[ref].
type named struct {
	ref    int
	object trace.Ref
}

// names returns the objects that body, a version of the CR at cr, names. A
// namespaced To takes the namespace value, or else cr's namespace, so a
// cluster-scoped CR without a namespace value names no namespaced object.
func (s Spec) names(cr trace.Ref, body map[string]any) []named {
	var objects []named
	for i, r := range s.References {
		if !cr.OfKind(r.From) {
			continue
		}
		for _, n := range config.ReferencedNames(r.Name, r.Namespace, body) {
			object := r.To
			object.Name = n.Name
			if r.Namespaced {
				if object.Namespace = cmp.Or(n.Namespace, cr.Namespace); object.Namespace == "" {
					continue
				}
			}
			objects = append(objects, named{i, object})
		}
	}
	return objects
}

// sight is what the controller knows of a named object: what its last look
// showed, or, if it never looked, the object's state.
type sight struct {
	looked, missing, ready bool
	// unknown says an opaque response may have shown the object since.
	unknown bool
}

// holds reports whether the sight holds back a CR that names the object:
// it shows the object missing, with required, or seen not ready.
func (g sight) holds(required bool) bool {
	if g.missing {
		return required
	}
	return g.looked && !g.ready
}

func (g sight) describe() string {
	switch {
	case !g.looked:
		return "not seen"
	case g.missing:
		return "seen missing"
	}
	return "seen not ready"
}

// sight is the controller's sight of n at t.
func (s Spec) sight(ix *trace.Index, n named, t time.Duration) sight {
	look, looked := lastSeen(ix.Looks(trace.Controller, n.object), t)
	if !looked {
		g := s.world(ix, n, t)
		g.looked, g.unknown = false, ix.Opaque(trace.Controller, n.object, -1, t)
		return g
	}
	g := s.shown(ix, n, look)
	g.unknown = ix.Opaque(trace.Controller, n.object, look.At, t)
	return g
}

// shown is what look, one of the controller's looks at n, showed.
func (s Spec) shown(ix *trace.Index, n named, look trace.Event) sight {
	if look.Seen == nil || look.Seen.Deleted {
		return sight{looked: true, missing: true}
	}
	v, found := ix.VersionOf(look.Seen.Ref, look.Seen.RV)
	return sight{looked: true, ready: found && satisfies(s.References[n.ref].Ready, decode(v.Body))}
}

// world is what a look at n at t would show: the observer's latest version
// of it by then, missing when deleted or when its system's gone CEL accepts
// it.
func (s Spec) world(ix *trace.Index, n named, t time.Duration) sight {
	v, found := ix.LatestAt(n.object, t)
	if !found || v.Deleted {
		return sight{looked: true, missing: true}
	}
	body := decode(v.Body)
	if gone := s.Owners.Gone[n.object.System]; gone != nil && satisfies(gone, body) {
		return sight{looked: true, missing: true}
	}
	return sight{looked: true, ready: satisfies(s.References[n.ref].Ready, body)}
}

// heldBack reports whether a reference holds back the CR at cr, whose body
// is body, at t, as the controller last saw and as a look at t would see,
// or leaves it unknown.
func (s Spec) heldBack(ix *trace.Index, cr trace.Ref, body map[string]any, t time.Duration) bool {
	for _, n := range s.names(cr, body) {
		required := s.References[n.ref].Required
		if g := s.sight(ix, n, t); g.unknown || g.holds(required) && s.world(ix, n, t).holds(required) {
			return true
		}
	}
	return false
}
