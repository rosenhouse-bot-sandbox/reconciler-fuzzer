package model

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func namespace(name string, finalizers ...corev1.FinalizerName) object {
	body, _ := json.Marshal(corev1.Namespace{Spec: corev1.NamespaceSpec{Finalizers: finalizers}})
	o := object{ref: trace.Ref{System: "kube", Resource: "namespaces", Name: name}, body: body}
	o.APIVersion, o.Kind, o.Name, o.UID, o.ResourceVersion = "v1", "Namespace", name, "ns-uid", "ns-rv"
	return o
}

func finalization(o object) op {
	return op{ref: o.ref, apiVersion: o.APIVersion, uid: string(o.UID), rv: o.ResourceVersion, finalize: o.body}
}

func TestNamespaceDeletesWhatRemainsInADeletingNamespace(t *testing.T) {
	terminating := deleting(namespace(ns, corev1.FinalizerKubernetes))
	child := configMap("child", "c")
	going := deleting(configMap("going", "g"), "x")
	elsewhere := configMap("elsewhere", "e")
	elsewhere.ref.Namespace = "other"
	assertOps(t, finish([]object{terminating, child, going, elsewhere}), []op{deletion(child)})
}

func TestNamespaceFinalizesAnEmptyDeletingNamespace(t *testing.T) {
	terminating := deleting(namespace(ns, corev1.FinalizerKubernetes))
	elsewhere := configMap("elsewhere", "e")
	elsewhere.ref.Namespace = "other"
	assertOps(t, finish([]object{terminating, elsewhere}), []op{finalization(terminating)})
}

func TestNamespaceLeavesOtherNamespacesAlone(t *testing.T) {
	objects := []object{namespace(ns, corev1.FinalizerKubernetes), deleting(namespace("done")), configMap("child", "c")}
	assertOps(t, finish(objects), nil)
}

func TestNamespaceWakesOnADeletingNamespaceAndWhatIsInIt(t *testing.T) {
	terminating := deleting(namespace(ns, corev1.FinalizerKubernetes))
	held := map[trace.Ref]object{terminating.ref: terminating}
	other := configMap("x", "x")
	other.ref.Namespace = "other"
	cases := []struct {
		name string
		v    object
		held map[trace.Ref]object
		want bool
	}{
		{"a deleting namespace", terminating, nil, true},
		{"a live namespace", namespace("live", corev1.FinalizerKubernetes), nil, false},
		{"an object in a deleting namespace", configMap("x", "x"), held, true},
		{"an object elsewhere", other, held, false},
		{"a cluster-scoped object", node("n", "n"), held, false},
	}
	for _, tc := range cases {
		if got := namespaceWakes(tc.v, false, tc.held); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
