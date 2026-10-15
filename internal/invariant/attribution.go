package invariant

import (
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// namedCRs are the primary CRs among the object's owners, by UID.
func (in Input) namedCRs(v observe.Version) []types.UID {
	var named []types.UID
	for _, ref := range v.OwnerReferences {
		if schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).GroupKind() == in.Target.Primary.GroupKind() {
			named = append(named, ref.UID)
		}
	}
	return named
}

// objectsOf are the objects that name the CR, and those that name no CR.
func (in Input) objectsOf(cr types.UID, objects []observe.Version) []observe.Version {
	return slices.DeleteFunc(slices.Clone(objects), func(v observe.Version) bool {
		named := in.namedCRs(v)
		return len(named) > 0 && !slices.Contains(named, cr)
	})
}

// crs are the primary CRs a state holds.
func (s state) crs(gvk schema.GroupVersionKind) []observe.Version {
	var crs []observe.Version
	for _, v := range s.live {
		if v.GVK == gvk {
			crs = append(crs, v)
		}
	}
	return crs
}

func (s state) holds(uid types.UID) bool {
	return slices.ContainsFunc(s.live, func(v observe.Version) bool { return v.UID == uid })
}

// leftBy reports whether an object still there at the deleted CR's deadline
// is that CR's to answer for. An object belongs to the CRs it names, and one
// that names none to every CR there when the deleted one went. The last of
// them to go answers for it: another that is still there at the deadline, or
// whose own deadline is later, keeps it.
func (in Input) leftBy(deleted deletion, v observe.Version, then, now state) bool {
	named := in.namedCRs(v)
	if len(named) > 0 && !slices.Contains(named, deleted.uid) {
		return false
	}
	later := map[types.UID]bool{}
	for _, other := range in.crDeletions() {
		later[other.uid] = other.deadline.After(deleted.deadline)
	}
	crs := slices.Concat(then.crs(in.Target.Primary), now.crs(in.Target.Primary))
	return !slices.ContainsFunc(crs, func(cr observe.Version) bool {
		claims := slices.Contains(named, cr.UID) || len(named) == 0 && then.holds(cr.UID)
		keeps := now.holds(cr.UID) || later[cr.UID]
		return cr.UID != deleted.uid && claims && keeps
	})
}

// askedFor reports whether a CR the state holds, and is not deleting, asks
// for the object: one the object names, or any where it names none.
func (in Input) askedFor(v observe.Version, s state) bool {
	named := in.namedCRs(v)
	return slices.ContainsFunc(s.crs(in.Target.Primary), func(cr observe.Version) bool {
		return cr.DeletionTimestamp == nil && (len(named) == 0 || slices.Contains(named, cr.UID))
	})
}

// reach is what reconciler-fuzzer's changes may have changed: the CRs they
// acted on and what those CRs own, or everything.
type reach struct {
	any, all bool
	crs      map[types.UID]bool
}

// reached is the reach of the ops. A deleted object reaches the CRs it names.
// An object that names no CR may be any CR's, so deleting one reaches
// everything. Any CR may read a fixture, so a change to one reaches everything
// too.
func (in Input) reached(ops []Op) reach {
	r := reach{any: len(ops) > 0, crs: map[types.UID]bool{}}
	for _, op := range ops {
		r.all = r.all || op.Type.onFixture() || op.Restored
		for _, v := range in.History.History(op.CR) {
			r.crs[v.UID] = true
		}
		if op.Deleted == (observe.Key{}) {
			continue
		}
		took, _ := in.versionAt(op.Deleted, op.Time)
		named := in.namedCRs(took)
		r.all = r.all || len(named) == 0
		for _, uid := range named {
			r.crs[uid] = true
		}
	}
	return r
}

// touches reports whether the reach may have changed the object. An object
// that names no CR may be a changed CR's.
func (r reach) touches(in Input, v observe.Version) bool {
	switch {
	case !r.any:
		return false
	case r.all || r.crs[v.UID]:
		return true
	case v.GVK == in.Target.Primary:
		return false
	}
	named := in.namedCRs(v)
	return len(named) == 0 || slices.ContainsFunc(named, func(uid types.UID) bool { return r.crs[uid] })
}
