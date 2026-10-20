package model

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

const ns = "run"

// fakeCluster serves ConfigMaps (namespaced), Nodes (cluster-scoped) and
// nothing else, and holds the objects a live read finds.
type fakeCluster struct {
	found  map[readKey]*metav1.ObjectMeta
	broken bool
	reads  []readKey
}

func (f *fakeCluster) scope(gvk schema.GroupVersionKind) (namespaced, served bool) {
	switch gvk.Kind {
	case "ConfigMap":
		return true, true
	case "Node":
		return false, true
	}
	return false, false
}

func (f *fakeCluster) read(_ context.Context, gvk schema.GroupVersionKind, namespace, name string) (*metav1.ObjectMeta, error) {
	key := readKey{gvk.GroupKind(), namespace, name}
	f.reads = append(f.reads, key)
	if f.broken {
		return nil, errors.New("unreachable")
	}
	return f.found[key], nil
}

func (f *fakeCluster) put(kind, namespace, name, uid string, finalizers ...string) *metav1.ObjectMeta {
	if f.found == nil {
		f.found = map[readKey]*metav1.ObjectMeta{}
	}
	meta := &metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid), Finalizers: finalizers}
	f.found[readKey{schema.GroupKind{Kind: kind}, namespace, name}] = meta
	return meta
}

func configMap(name, uid string, owners ...metav1.OwnerReference) object {
	o := object{ref: trace.Ref{System: "kube", Resource: "configmaps", Namespace: ns, Name: name}}
	o.APIVersion, o.Kind = "v1", "ConfigMap"
	o.Name, o.Namespace, o.UID, o.ResourceVersion = name, ns, types.UID(uid), "rv-"+uid
	o.OwnerReferences = owners
	return o
}

func node(name, uid string, owners ...metav1.OwnerReference) object {
	o := configMap(name, uid, owners...)
	o.ref.Resource, o.ref.Namespace, o.Namespace, o.Kind = "nodes", "", "", "Node"
	return o
}

func ownedBy(kind, name, uid string) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: kind, Name: name, UID: types.UID(uid)}
}

func blocking(ref metav1.OwnerReference) metav1.OwnerReference {
	block := true
	ref.BlockOwnerDeletion = &block
	return ref
}

func deleting(o object, finalizers ...string) object {
	now := metav1.Now()
	o.DeletionTimestamp, o.Finalizers = &now, finalizers
	return o
}

func deletion(o object) op {
	return op{ref: o.ref, apiVersion: o.APIVersion, uid: string(o.UID), rv: o.ResourceVersion, delete: true}
}

func patch(o object, fields map[string]any) op {
	return op{ref: o.ref, apiVersion: o.APIVersion, uid: string(o.UID), rv: o.ResourceVersion, patch: fields}
}

func assertOps(t *testing.T, got, want []op) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ops:\n got %+v\nwant %+v", got, want)
	}
}

func TestGCCollectsADependentWhoseOwnersAreAllGone(t *testing.T) {
	dependent := configMap("child", "c", ownedBy("ConfigMap", "a", "a"), ownedBy("ConfigMap", "b", "b"))
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "b", "another")
	ops, failed := collect(context.Background(), []object{dependent}, c)
	assertOps(t, ops, []op{deletion(dependent)})
	if failed {
		t.Error("collect reported a failed read.")
	}
}

func TestGCKeepsADependentWithALiveOwner(t *testing.T) {
	dependent := configMap("child", "c", ownedBy("ConfigMap", "b", "b"))
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "b", "b")
	ops, _ := collect(context.Background(), []object{dependent}, c)
	assertOps(t, ops, nil)
}

func TestGCCountsAnUnreadableOwnerAsLiveAndFails(t *testing.T) {
	c := &fakeCluster{broken: true}
	ops, failed := collect(context.Background(), []object{configMap("child", "c", ownedBy("ConfigMap", "a", "a"))}, c)
	assertOps(t, ops, nil)
	if !failed {
		t.Error("collect did not report the failed read.")
	}
}

func TestGCCountsAnUnservedOwnerAsLive(t *testing.T) {
	c := &fakeCluster{}
	ops, failed := collect(context.Background(), []object{configMap("child", "c", ownedBy("Widget", "a", "a"))}, c)
	assertOps(t, ops, nil)
	if failed || len(c.reads) > 0 {
		t.Errorf("collect read an unserved owner, or failed: reads %v, failed %v.", c.reads, failed)
	}
}

func TestGCLooksUpOwnersByTheirKindsScope(t *testing.T) {
	dependent := configMap("child", "c", ownedBy("ConfigMap", "a", "a"), ownedBy("Node", "n", "n"))
	c := &fakeCluster{}
	c.put("ConfigMap", "elsewhere", "a", "a")
	c.put("Node", "", "n", "n")
	ops, _ := collect(context.Background(), []object{dependent}, c)
	assertOps(t, ops, []op{patch(dependent, map[string]any{"ownerReferences": dependent.OwnerReferences[1:]})})
	want := []readKey{{schema.GroupKind{Kind: "ConfigMap"}, ns, "a"}, {schema.GroupKind{Kind: "Node"}, "", "n"}}
	if !reflect.DeepEqual(c.reads, want) {
		t.Errorf("reads: got %v, want %v", c.reads, want)
	}
}

func TestGCReadsEachOwnerOncePerSweep(t *testing.T) {
	owner := ownedBy("ConfigMap", "a", "a")
	c := &fakeCluster{}
	collect(context.Background(), []object{configMap("x", "x", owner), configMap("y", "y", owner)}, c)
	if len(c.reads) != 1 {
		t.Errorf("reads: got %v, want one", c.reads)
	}
}

func TestGCNeverCollectsAClusterScopedDependentOfANamespacedKind(t *testing.T) {
	ops, _ := collect(context.Background(), []object{node("n", "n", ownedBy("ConfigMap", "a", "a"))}, &fakeCluster{})
	assertOps(t, ops, nil)
}

func TestGCCollectsAClusterScopedDependentOfAGoneClusterScopedOwner(t *testing.T) {
	dependent := node("n", "n", ownedBy("Node", "a", "a"))
	ops, _ := collect(context.Background(), []object{dependent}, &fakeCluster{})
	assertOps(t, ops, []op{deletion(dependent)})
}

func TestGCSkipsObjectsWithoutOwnersAndObjectsBeingDeleted(t *testing.T) {
	objects := []object{configMap("free", "f"), deleting(configMap("going", "g", ownedBy("ConfigMap", "a", "a")), "x")}
	ops, _ := collect(context.Background(), objects, &fakeCluster{})
	assertOps(t, ops, nil)
}

func TestGCDeletesTheDependentsOfAForegroundOwner(t *testing.T) {
	owner := deleting(configMap("owner", "o"), metav1.FinalizerDeleteDependents)
	leaf := configMap("leaf", "l", ownedBy("ConfigMap", "owner", "o"))
	parent := configMap("parent", "p", blocking(ownedBy("ConfigMap", "owner", "o")))
	grandchild := configMap("grandchild", "g", ownedBy("ConfigMap", "parent", "p"))
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "owner", "o", metav1.FinalizerDeleteDependents).DeletionTimestamp = owner.DeletionTimestamp
	c.put("ConfigMap", ns, "parent", "p")
	foreground := deletion(parent)
	foreground.foreground = true
	ops, _ := collect(context.Background(), []object{owner, leaf, parent, grandchild}, c)
	assertOps(t, ops, []op{deletion(leaf), foreground})
}

func TestGCRemovesTheForegroundFinalizerOnceNoBlockingDependentRemains(t *testing.T) {
	owner := deleting(configMap("owner", "o"), metav1.FinalizerDeleteDependents, "other")
	free := configMap("free", "f", ownedBy("ConfigMap", "owner", "o"))
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "owner", "o", metav1.FinalizerDeleteDependents).DeletionTimestamp = owner.DeletionTimestamp
	unblocking := blocking(ownedBy("ConfigMap", "owner", "o"))
	*unblocking.BlockOwnerDeletion = false
	unblocked := deleting(configMap("unblocked", "u", unblocking), "x")
	ops, _ := collect(context.Background(), []object{owner, deleting(free, "x"), unblocked}, c)
	assertOps(t, ops, []op{patch(owner, map[string]any{"finalizers": []string{"other"}})})

	blocked := deleting(configMap("blocked", "b", blocking(ownedBy("ConfigMap", "owner", "o"))), "x")
	ops, _ = collect(context.Background(), []object{owner, blocked}, c)
	assertOps(t, ops, nil)
}

func TestGCOrphansDependentsThenRemovesTheOrphanFinalizer(t *testing.T) {
	owner := deleting(configMap("owner", "o"), metav1.FinalizerOrphanDependents)
	other := ownedBy("ConfigMap", "other", "x")
	dependent := configMap("child", "c", ownedBy("ConfigMap", "owner", "o"), other)
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "owner", "o", metav1.FinalizerOrphanDependents).DeletionTimestamp = owner.DeletionTimestamp
	c.put("ConfigMap", ns, "other", "x")
	ops, _ := collect(context.Background(), []object{owner, dependent}, c)
	assertOps(t, ops, []op{patch(dependent, map[string]any{"ownerReferences": []metav1.OwnerReference{other}})})

	ops, _ = collect(context.Background(), []object{owner, configMap("child", "c", other)}, c)
	assertOps(t, ops, []op{patch(owner, map[string]any{"finalizers": []string{}})})
}

func TestGCRemovesGoneAndWaitingOwnersFromADependentWithALiveOwner(t *testing.T) {
	live := ownedBy("ConfigMap", "live", "l")
	dependent := configMap("child", "c", ownedBy("ConfigMap", "gone", "g"), live, ownedBy("ConfigMap", "owner", "o"))
	c := &fakeCluster{}
	c.put("ConfigMap", ns, "live", "l")
	c.put("ConfigMap", ns, "owner", "o", metav1.FinalizerDeleteDependents).DeletionTimestamp = &metav1.Time{}
	ops, _ := collect(context.Background(), []object{dependent}, c)
	assertOps(t, ops, []op{patch(dependent, map[string]any{"ownerReferences": []metav1.OwnerReference{live}})})
}

func TestGCWakesOnVersionsThatCanMakeWork(t *testing.T) {
	owner := configMap("owner", "o")
	dependent := configMap("child", "c", ownedBy("ConfigMap", "owner", "o"))
	held := map[trace.Ref]object{dependent.ref: dependent}
	cases := []struct {
		name    string
		v       object
		deleted bool
		want    bool
	}{
		{"a dependent", dependent, false, true},
		{"an unowned update", owner, false, false},
		{"an owner deleted", owner, true, true},
		{"an owner being deleted", deleting(owner, "x"), false, true},
		{"an unreferenced object deleted", configMap("free", "f"), true, false},
		{"an owner orphaning", deleting(configMap("x", "x"), metav1.FinalizerOrphanDependents), false, true},
	}
	for _, tc := range cases {
		if got := gcWakes(tc.v, tc.deleted, held); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
