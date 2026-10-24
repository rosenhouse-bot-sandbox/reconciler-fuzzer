//go:build envtest

package model

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
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
		if err := k.Delete(ctx, namespace, ""); err != nil {
			t.Fatal(err)
		}
		waitGone(t, namespace)
	})

	t.Run("models write as themselves and fall idle", func(t *testing.T) {
		for _, write := range []struct {
			caller trace.Caller
			verb   string
		}{{trace.GC, "delete"}, {trace.GC, "patch"}, {trace.Namespaces, "delete"}, {trace.Namespaces, "update"}} {
			// A call is recorded when its response ends, which can follow the
			// version the observer recorded.
			deadline := time.Now().Add(5 * time.Second)
			for recorded.count(func(e trace.Event) bool {
				return e.Call != nil && e.Call.Caller == write.caller && e.Call.Verb == write.verb && e.Call.Status < 300
			}) == 0 {
				if time.Now().After(deadline) {
					t.Errorf("Recorded no successful %s by %s.", write.verb, write.caller)
					break
				}
				time.Sleep(20 * time.Millisecond)
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

var gadgetKind = schema.GroupVersionKind{Group: "test.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}

// startStage starts envtest with the Gadget CRD and a stage of m, and
// creates Gadget g.
func startStage(t *testing.T, m config.Model) (*kube.Kube, *Stage, *events, trace.Ref) {
	t.Helper()
	ctx := context.Background()
	recorded := &events{}
	k := kube.New(kube.Options{CRDs: []string{"testdata/crds"}, Watch: []schema.GroupVersionKind{gadgetKind}})
	if _, err := k.Start(ctx, &system.Env{Record: recorded.record, Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	s, err := NewStage(k, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, &system.Env{Record: recorded.record}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop(ctx) })
	ref, err := k.Create(ctx, map[string]any{"apiVersion": "test.reconciler-fuzzer/v1", "kind": "Gadget", "metadata": map[string]any{"name": "g"}, "spec": map[string]any{"size": int64(1)}})
	if err != nil {
		t.Fatal(err)
	}
	return k, s, recorded, ref
}

// lastVersion is the last version of ref the observer recorded.
func lastVersion(recorded *events, ref trace.Ref) trace.Version {
	var last trace.Version
	for _, e := range recordedEvents(recorded) {
		if e.Version != nil && e.Version.Ref == ref {
			last = *e.Version
		}
	}
	return last
}

func TestAStageMakesAGadgetReadyThenStuckThenRecovered(t *testing.T) {
	_, s, recorded, ref := startStage(t, config.Model{Kind: gadgetKind, Stage: config.ReadyCondition, After: 100 * time.Millisecond,
		Status: map[string]any{"phase": "Running"}, StuckStatus: map[string]any{"phase": "Pending"}})
	ready, err := config.CompileReady(config.ReadyConditionCEL)
	if err != nil {
		t.Fatal(err)
	}
	await := func(t *testing.T, what string, want bool, message string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			last := lastVersion(recorded, ref)
			var obj map[string]any
			json.Unmarshal(last.Body, &obj)
			if got, err := ready(obj); err == nil && got == want && strings.Contains(string(last.Body), message) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("The gadget never became %s.", what)
	}

	await(t, "ready", true, `"phase":"Running"`)
	end, err := s.Fault(plan.Fault{Match: plan.Match{Group: gadgetKind.Group, Resource: "gadgets"}, Stuck: true})
	if err != nil {
		t.Fatal(err)
	}
	await(t, "stuck", false, "Stuck")
	await(t, "pending", false, `"phase":"Pending"`)
	end()
	await(t, "recovered", true, "fault 1 ended")
	await(t, "running", true, `"phase":"Running"`)
	// The observer can record the last write's version before the write's call.
	deadline := time.Now().Add(10 * time.Second)
	for recorded.count(func(e trace.Event) bool {
		return e.Call != nil && e.Call.Caller == s.caller() && e.Call.Subresource == "status" && e.Call.Status == 200
	}) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("The stage's writes are not calls by its caller.")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAStageHoldsAGadgetsDeletionWithItsFinalizer(t *testing.T) {
	const hold = 500 * time.Millisecond
	k, _, recorded, ref := startStage(t, config.Model{Kind: gadgetKind, Stage: config.ReadyCondition, After: 100 * time.Millisecond,
		Finalizer: &config.Finalizer{Name: "example.com/cleanup", After: hold}})
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(string(lastVersion(recorded, ref).Body), `"example.com/cleanup"`) {
		if time.Now().After(deadline) {
			t.Fatal("The gadget never got the stage's finalizer.")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleted := time.Now()
	if err := k.Delete(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := k.WaitGone(ctx, ref); err != nil {
		t.Fatalf("The gadget is not gone: %v", err)
	}
	if elapsed := time.Since(deleted); elapsed < hold || elapsed > hold+time.Second {
		t.Errorf("The gadget was gone %v after its delete, want %v to %v.", elapsed, hold, hold+time.Second)
	}
}
