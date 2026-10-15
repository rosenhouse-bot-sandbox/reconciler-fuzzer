package invariant

import (
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// CleanDeletion is G3: after the CR is deleted with no fault active, every
// object the target manages for it is deleted and the CR's finalizers are
// cleared within timeouts.delete. The garbage collector deletes the children
// the CR owns, so what remains is an orphan or an uncleared finalizer.
func CleanDeletion(in Input) (Result, error) {
	out := Result{ID: "G3"}
	for _, deleted := range in.crDeletions() {
		deadline := deleted.deadline
		out.noteWhatReconcilerFuzzerTook(in, deleted, deadline)
		switch {
		case in.cleanedBy(deadline): // The namespace emptied, so nothing was left.
		case in.faulted(deleted.at, deadline):
			out.note("for %s: a fault was active before its deadline", in.describeDeletion(deleted))
		case !in.observed(deadline):
			out.note("for %s: the run ended less than %s (timeouts.delete) after it", in.describeDeletion(deleted), in.timeouts().Delete)
		default:
			out.reportLeftovers(in, deleted, deadline)
		}
	}
	return out, nil
}

// noteWhatReconcilerFuzzerTook records the objects reconciler-fuzzer deleted
// inside the window after the CR went. A deleteManaged op takes an object out
// of the target's hands, and the target had until the deadline, so whether it
// would have cleaned that object is nobody's to say. Judging it either way
// would be a guess; a silent pass reads as cleanup that happened.
func (out *Result) noteWhatReconcilerFuzzerTook(in Input, deleted deletion, deadline time.Time) {
	states := in.statesAt([]time.Time{deleted.at, deadline})
	had, since := states[0], states[1]
	for _, op := range in.Ops {
		// Only a DeleteManaged op names an object it took.
		was, hadIt := had.version(op.Deleted)
		if !hadIt {
			continue
		}
		if !op.Time.After(deleted.at) || op.Time.After(deadline) {
			continue
		}
		// An object the run recreated carries the same name and a new UID, so
		// reconciler-fuzzer took the one that came after and not the one this CR
		// left.
		if took, found := in.versionAt(op.Deleted, op.Time); !found || took.UID != was.UID ||
			!in.leftBy(deleted, took, had, since) {
			continue
		}
		out.note("for %s: %s deleted %s %s within %s (timeouts.delete) of it, so the target never got the chance to clean it up",
			in.describeDeletion(deleted), describe(op), observe.KindName(op.Deleted.GVK), op.Deleted.Name, in.timeouts().Delete)
	}
}

// versionAt is the object as the Observer last saw it at or before t.
func (in Input) versionAt(key observe.Key, t time.Time) (observe.Version, bool) {
	var latest observe.Version
	var found bool
	for _, v := range in.History.History(key) {
		if !v.Time.After(t) {
			latest, found = v, true
		}
	}
	return latest, found
}

// cleanedBy reports whether reconciler-fuzzer saw the run namespace empty by t,
// which leaves nothing for G3 to find at t.
func (in Input) cleanedBy(t time.Time) bool {
	return !in.Cleaned.IsZero() && !in.Cleaned.After(t)
}

// deletion is one deletion of the primary CR, timestamped as the Observer saw
// it: metadata.deletionTimestamp holds whole seconds only. Its deadline is
// timeouts.delete later.
type deletion struct {
	key          observe.Key
	uid          types.UID
	at, deadline time.Time
}

// held is the deleted CR where the state still holds it by its finalizers.
func (s state) held(deleted deletion) (observe.Version, bool) {
	cr, found := s.version(deleted.key)
	return cr, found && cr.UID == deleted.uid && len(cr.Finalizers) > 0
}

func (in Input) crDeletions() []deletion { return in.crDeletionsBy(in.end()) }

func (in Input) crDeletionsBy(t time.Time) []deletion {
	if in.History == nil {
		return nil
	}
	var deletions []deletion
	seen := map[types.UID]bool{}
	for _, v := range in.History.VersionsOf(in.Target.Primary, t) {
		if seen[v.UID] || (v.DeletionTimestamp == nil && !v.Deleted) {
			continue
		}
		seen[v.UID] = true
		deletions = append(deletions, deletion{key: v.Key, uid: v.UID, at: v.Time, deadline: v.Time.Add(in.timeouts().Delete)})
	}
	return deletions
}

// reportLeftovers judges one deleted CR and the objects it had when it went.
// A CR or a child the run recreated carries a new UID, so it belongs to the
// CR that came after and is not this deletion's to answer for.
func (out *Result) reportLeftovers(in Input, deleted deletion, deadline time.Time) {
	states := in.statesAt([]time.Time{deleted.at, deadline})
	when, since := states[0], states[1]
	if cr, held := since.held(deleted); held {
		out.violate(Violation{
			Statement: fmt.Sprintf("the CR %s still carried the finalizers %v %s (timeouts.delete) after its deletion",
				cr.Name, cr.Finalizers, in.timeouts().Delete),
			At: deadline,
		}.quotingVersions(RecentHistory(cr.Key, in.History.History(cr.Key))))
	}
	had := map[observe.Key]types.UID{}
	for _, object := range when.managed(in) {
		had[object.Key] = object.UID
	}
	for _, left := range since.managed(in) {
		if uid, was := had[left.Key]; !was || uid != left.UID || !in.leftBy(deleted, left, when, since) {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the %s %s was still there %s (timeouts.delete) after %s was deleted%s",
				observe.KindName(left.GVK), left.Name, in.timeouts().Delete, in.answering(deleted, left), orphaned(left, deleted.uid)),
			At: deadline,
		}.quotingVersions(RecentHistory(left.Key, in.History.History(left.Key))))
	}
}

// describeDeletion names the CR and what deleted it where reconciler-fuzzer can
// tell.
func (in Input) describeDeletion(deleted deletion) string {
	return "the deletion of " + deleted.key.Name + in.deletedBy(deleted)
}

// deletedBy names the first delete or recreate that found the CR of this UID,
// or else the teardown. The Observer can record the deletion after a later op
// or the teardown has begun.
func (in Input) deletedBy(deleted deletion) string {
	for _, op := range in.Ops {
		if op.Type != OpDelete && op.Type != OpRecreate || op.CR != deleted.key || op.Time.After(deleted.at) {
			continue
		}
		if was, _ := in.versionAt(deleted.key, op.Time); was.UID == deleted.uid {
			return " by " + describe(op)
		}
	}
	if !in.Teardown.IsZero() && !in.Teardown.After(deleted.at) {
		return " by the teardown"
	}
	return ""
}

// answering names the deleted CR as the one that answers for the object.
func (in Input) answering(deleted deletion, left observe.Version) string {
	if len(in.namedCRs(left)) == 0 {
		return deleted.key.Name + ", the last CR it may belong to,"
	}
	return "the CR " + deleted.key.Name
}

// orphaned names what the garbage collector could not reach: an object with
// no ownerReference to the CR is the target's own to delete.
func orphaned(left observe.Version, cr types.UID) string {
	if ownedBy(left.OwnerReferences, cr) {
		return ""
	}
	return ", orphaned: it carries no ownerReference to the CR"
}

func ownedBy(refs []metav1.OwnerReference, owner types.UID) bool {
	return slices.ContainsFunc(refs, func(ref metav1.OwnerReference) bool { return ref.UID == owner })
}
