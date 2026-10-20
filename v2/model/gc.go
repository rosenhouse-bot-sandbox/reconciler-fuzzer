package model

import (
	"context"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// cluster is what the gc reads live.
type cluster interface {
	// scope says whether gvk is namespaced, and whether the server serves it.
	scope(gvk schema.GroupVersionKind) (namespaced, served bool)
	// read returns nil when no object has the name.
	read(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*metav1.ObjectMeta, error)
}

type readKey struct {
	kind      schema.GroupKind
	namespace string
	name      string
}

// standing is what one ownerReference's owner is to its dependent.
type standing int

const (
	solid standing = iota
	dangling
	// waiting is an owner in foreground deletion.
	waiting
)

type gcSweep struct {
	ctx    context.Context
	c      cluster
	reads  map[readKey]*metav1.ObjectMeta
	failed bool
}

// collect returns the gc's writes, in object order, and whether a live read
// failed.
func collect(ctx context.Context, objects []object, c cluster) ([]op, bool) {
	s := &gcSweep{ctx: ctx, c: c, reads: map[readKey]*metav1.ObjectMeta{}}
	dependents := map[types.UID][]object{}
	for _, o := range objects {
		for _, ref := range o.OwnerReferences {
			dependents[ref.UID] = append(dependents[ref.UID], o)
		}
	}
	// orphaned holds the owner UIDs to remove from each dependent.
	orphaned := map[trace.Ref][]types.UID{}
	for _, o := range objects {
		if o.deleting() && slices.Contains(o.Finalizers, metav1.FinalizerOrphanDependents) {
			for _, d := range dependents[o.UID] {
				orphaned[d.ref] = append(orphaned[d.ref], o.UID)
			}
		}
	}

	var ops []op
	for _, o := range objects {
		edit := op{ref: o.ref, apiVersion: o.APIVersion, uid: string(o.UID), rv: o.ResourceVersion, patch: map[string]any{}}
		drop := orphaned[o.ref]
		if len(o.OwnerReferences) > 0 && !o.deleting() {
			stands, decided := s.owners(o)
			switch {
			case !decided:
			case !slices.Contains(stands, solid):
				edit.delete = true
				edit.foreground = slices.Contains(stands, waiting) && len(dependents[o.UID]) > 0
			default:
				for i, ref := range o.OwnerReferences {
					if stands[i] != solid {
						drop = append(drop, ref.UID)
					}
				}
			}
		}
		if len(drop) > 0 {
			edit.patch["ownerReferences"] = slices.DeleteFunc(slices.Clone(o.OwnerReferences), func(ref metav1.OwnerReference) bool {
				return slices.Contains(drop, ref.UID)
			})
		}
		if o.deleting() {
			finalizers := slices.DeleteFunc(slices.Clone(o.Finalizers), func(f string) bool {
				return f == metav1.FinalizerDeleteDependents && !blocked(o.UID, dependents[o.UID]) ||
					f == metav1.FinalizerOrphanDependents && len(dependents[o.UID]) == 0
			})
			if len(finalizers) < len(o.Finalizers) {
				edit.patch["finalizers"] = append([]string{}, finalizers...)
			}
		}
		if len(edit.patch) == 0 {
			edit.patch = nil
		}
		if edit.delete || edit.patch != nil {
			ops = append(ops, edit)
		}
	}
	return ops, s.failed
}

// owners returns the standing of each of o's owners. It decides nothing for
// a cluster-scoped dependent that names a namespaced owner kind.
func (s *gcSweep) owners(o object) ([]standing, bool) {
	stands := make([]standing, len(o.OwnerReferences))
	for i, ref := range o.OwnerReferences {
		gvk := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
		namespaced, served := s.c.scope(gvk)
		if !served {
			continue
		}
		namespace := ""
		if namespaced {
			if o.ref.Namespace == "" {
				return nil, false
			}
			namespace = o.ref.Namespace
		}
		owner, err := s.read(gvk, namespace, ref.Name)
		switch {
		case err != nil:
			s.failed = true
		case owner == nil || owner.UID != ref.UID:
			stands[i] = dangling
		case owner.DeletionTimestamp != nil && slices.Contains(owner.Finalizers, metav1.FinalizerDeleteDependents):
			stands[i] = waiting
		}
	}
	return stands, true
}

// read reads each owner once per sweep.
func (s *gcSweep) read(gvk schema.GroupVersionKind, namespace, name string) (*metav1.ObjectMeta, error) {
	key := readKey{gvk.GroupKind(), namespace, name}
	if owner, found := s.reads[key]; found {
		return owner, nil
	}
	owner, err := s.c.read(s.ctx, gvk, namespace, name)
	if err == nil {
		s.reads[key] = owner
	}
	return owner, err
}

// blocked says whether a dependent blocks its owner's foreground deletion.
func blocked(owner types.UID, dependents []object) bool {
	return slices.ContainsFunc(dependents, func(d object) bool {
		return slices.ContainsFunc(d.OwnerReferences, func(ref metav1.OwnerReference) bool {
			return ref.UID == owner && ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion
		})
	})
}

// gcWakes says whether v can give the gc work: v names owners, v is being
// deleted with a gc finalizer, or v is a named owner going away.
func gcWakes(v object, deleted bool, held map[trace.Ref]object) bool {
	if len(v.OwnerReferences) > 0 {
		return true
	}
	if v.deleting() && slices.ContainsFunc(v.Finalizers, func(f string) bool {
		return f == metav1.FinalizerDeleteDependents || f == metav1.FinalizerOrphanDependents
	}) {
		return true
	}
	if !deleted && !v.deleting() {
		return false
	}
	for _, o := range held {
		if slices.ContainsFunc(o.OwnerReferences, func(ref metav1.OwnerReference) bool { return ref.UID == v.UID }) {
			return true
		}
	}
	return false
}
