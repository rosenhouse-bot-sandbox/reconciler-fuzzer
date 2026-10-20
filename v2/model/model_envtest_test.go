//go:build envtest

package model

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

type events struct {
	mu   sync.Mutex
	list []trace.Event
}

func (e *events) record(ev trace.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) count(match func(trace.Event) bool) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.list {
		if match(ev) {
			n++
		}
	}
	return n
}

func TestModelsAgainstEnvtest(t *testing.T) {
	ctx := context.Background()
	recorded := &events{}
	k := kube.New(kube.Options{Watch: []schema.GroupVersionKind{{Version: "v1", Kind: "ConfigMap"}, {Version: "v1", Kind: "Namespace"}}})
	endpoints, err := k.Start(ctx, &system.Env{Record: recorded.record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	for _, m := range New(k) {
		if _, err := m.Start(ctx, &system.Env{Record: recorded.record}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { m.(system.Stopper).Stop(ctx) })
	}
	harness := kubernetes.NewForConfigOrDie(&rest.Config{Host: endpoints.Harness.URL()})
	configMaps := harness.CoreV1().ConfigMaps(k.Namespace())
	pair := func(t *testing.T, name string) (owner, dependent trace.Ref) {
		t.Helper()
		created, err := configMaps.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		block := true
		ref := metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: name, UID: created.UID, BlockOwnerDeletion: &block}
		child := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-child", OwnerReferences: []metav1.OwnerReference{ref}}}
		if _, err := configMaps.Create(ctx, child, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		owner, _ = k.Ref(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, name)
		dependent, _ = k.Ref(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, name+"-child")
		return owner, dependent
	}
	waitGone := func(t *testing.T, ref trace.Ref) {
		t.Helper()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := k.WaitGone(ctx, ref); err != nil {
			t.Fatalf("%s/%s is not gone: %v", ref.Resource, ref.Name, err)
		}
	}
	deleteWith := func(t *testing.T, name string, policy metav1.DeletionPropagation) {
		t.Helper()
		if err := configMaps.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("gc collects in the background", func(t *testing.T) {
		owner, dependent := pair(t, "background")
		deleteWith(t, owner.Name, metav1.DeletePropagationBackground)
		waitGone(t, dependent)
	})

	t.Run("gc collects in the foreground", func(t *testing.T) {
		owner, dependent := pair(t, "foreground")
		deleteWith(t, owner.Name, metav1.DeletePropagationForeground)
		waitGone(t, dependent)
		waitGone(t, owner)
	})

	t.Run("gc orphans", func(t *testing.T) {
		owner, dependent := pair(t, "orphan")
		deleteWith(t, owner.Name, metav1.DeletePropagationOrphan)
		waitGone(t, owner)
		child, err := configMaps.Get(ctx, dependent.Name, metav1.GetOptions{})
		if err != nil || len(child.OwnerReferences) > 0 {
			t.Errorf("The orphan is %+v, %v.", child, err)
		}
	})

	t.Run("namespace finishes a deleted namespace", func(t *testing.T) {
		namespace, err := k.Create(ctx, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "doomed"}})
		if err != nil {
			t.Fatal(err)
		}
		inside := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "inside", Namespace: "doomed"}}
		if _, err := harness.CoreV1().ConfigMaps("doomed").Create(ctx, inside, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := k.Delete(ctx, namespace); err != nil {
			t.Fatal(err)
		}
		waitGone(t, namespace)
	})

	t.Run("models write as themselves and fall idle", func(t *testing.T) {
		for _, write := range []struct {
			caller trace.Caller
			verb   string
		}{{trace.GC, "delete"}, {trace.GC, "patch"}, {trace.Namespaces, "delete"}, {trace.Namespaces, "update"}} {
			if recorded.count(func(e trace.Event) bool {
				return e.Call != nil && e.Call.Caller == write.caller && e.Call.Verb == write.verb && e.Call.Status < 300
			}) == 0 {
				t.Errorf("Recorded no successful %s by %s.", write.verb, write.caller)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && len(trace.NewIndex(recordedEvents(recorded)).Pending(time.Hour)) > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if pending := trace.NewIndex(recordedEvents(recorded)).Pending(time.Hour); len(pending) > 0 {
			t.Errorf("Models still pending: %v", pending)
		}
	})
}

func recordedEvents(e *events) []trace.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	events := make([]trace.Event, len(e.list))
	for i, ev := range e.list {
		ev.Seq, ev.At = uint64(i+1), time.Duration(i+1)
		events[i] = ev
	}
	return events
}
