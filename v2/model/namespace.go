package model

import (
	"encoding/json"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

func isNamespace(ref trace.Ref) bool { return ref.Group == "" && ref.Resource == "namespaces" }

// terminating says whether o is a namespace the model must finish.
func terminating(o object) bool {
	if !isNamespace(o.ref) || !o.deleting() {
		return false
	}
	var namespace corev1.Namespace
	return json.Unmarshal(o.body, &namespace) == nil && slices.Contains(namespace.Spec.Finalizers, corev1.FinalizerKubernetes)
}

// finish deletes what remains in each terminating namespace, and finalizes
// one with nothing left.
func finish(objects []object) []op {
	var ops []op
	for _, n := range objects {
		if !terminating(n) {
			continue
		}
		remaining := false
		for _, o := range objects {
			if o.ref.Namespace != n.Name {
				continue
			}
			remaining = true
			if !o.deleting() {
				ops = append(ops, op{ref: o.ref, apiVersion: o.APIVersion, uid: string(o.UID), rv: o.ResourceVersion, delete: true})
			}
		}
		if !remaining {
			ops = append(ops, op{ref: n.ref, apiVersion: n.APIVersion, uid: string(n.UID), rv: n.ResourceVersion, finalize: n.body})
		}
	}
	return ops
}

// namespaceWakes says whether v is a terminating namespace or an object in
// one.
func namespaceWakes(v object, _ bool, held map[trace.Ref]object) bool {
	if terminating(v) {
		return true
	}
	if v.ref.Namespace == "" {
		return false
	}
	return terminating(held[trace.Ref{System: v.ref.System, Resource: "namespaces", Name: v.ref.Namespace}])
}
