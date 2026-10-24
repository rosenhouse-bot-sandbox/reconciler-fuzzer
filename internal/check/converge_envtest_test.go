//go:build envtest

package check

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/model"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestSuggestedBuiltinModelsWorkOnARealAPIServer(t *testing.T) {
	type row struct {
		name, kind        string
		spec              map[string]any
		createdUnderStuck bool
		// ready and stuck map each status field to its value, or to nil
		// when it is absent.
		ready, stuck map[string]any
		// refused says the API server refuses each stuck write.
		refused bool
	}
	container := map[string]any{"name": "c", "image": "busybox"}
	job := map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{container}, "restartPolicy": "Never"}}}
	rows := []row{
		{name: "pod", kind: "Pod", spec: map[string]any{"containers": []any{container}},
			ready: map[string]any{"phase": "Running"}, stuck: map[string]any{"phase": "Running"}},
		{name: "pvc", kind: "PersistentVolumeClaim", spec: map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "1Gi"}}},
			ready: map[string]any{"phase": "Bound"}, stuck: map[string]any{"phase": "Pending"}},
		{name: "job-created-under-stuck", kind: "Job", spec: job, createdUnderStuck: true,
			ready: map[string]any{"succeeded": 1, "active": nil}, stuck: map[string]any{"active": 1, "succeeded": nil}},
		// The API server refuses to lower succeeded, so a done Job stays done.
		{name: "done-job", kind: "Job", spec: job, ready: map[string]any{"succeeded": 1, "active": nil}, refused: true},
	}

	ctx := context.Background()
	var watch []schema.GroupVersionKind
	for kind := range builtinStatusKinds {
		watch = append(watch, schema.GroupVersionKind{Group: kind.Group, Version: kind.Version, Kind: kind.Kind})
		if !slices.ContainsFunc(rows, func(r row) bool { return r.kind == kind.Kind }) {
			t.Errorf("No row checks the suggested model of %s.", kind.Kind)
		}
	}
	calls := &recordedCalls{}
	k := kube.New(kube.Options{Watch: watch})
	endpoints, err := k.Start(ctx, &system.Env{Record: calls.record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	harness := dynamic.NewForConfigOrDie(&rest.Config{Host: endpoints.Harness.URL()})
	kinds, stages := map[string]trace.Kind{}, map[string]*model.Stage{}
	for kind, keys := range builtinStatusKinds {
		var suggested struct {
			Status      map[string]any `json:"status"`
			StuckStatus map[string]any `json:"stuckStatus"`
		}
		if err := yaml.UnmarshalStrict([]byte("{"+keys+"}"), &suggested); err != nil {
			t.Fatalf("The suggestion for %s: %v", kind.Kind, err)
		}
		gvk := schema.GroupVersionKind{Group: kind.Group, Version: kind.Version, Kind: kind.Kind}
		s, err := model.NewStage(k, config.Model{Kind: gvk, Stage: config.ReadyCondition, After: 100 * time.Millisecond, Status: suggested.Status, StuckStatus: suggested.StuckStatus})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, &system.Env{Record: calls.record}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Stop(ctx) })
		kinds[kind.Kind], stages[kind.Kind] = kind, s
	}

	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			kind, s := kinds[c.kind], stages[c.kind]
			objects := harness.Resource(schema.GroupVersionResource{Group: kind.Group, Version: kind.Version, Resource: kind.Resource}).Namespace(k.Namespace())
			await := func(ready string, fields map[string]any) {
				t.Helper()
				var status map[string]any
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					got, err := objects.Get(ctx, c.name, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					status, _ = got.Object["status"].(map[string]any)
					if readyIn(status) == ready && shows(status, fields) {
						return
					}
				}
				t.Fatalf("The %s never showed Ready %s and %v. Its status is %v.", c.name, ready, fields, status)
			}
			stuck := func() func() {
				end, err := s.Fault(plan.Fault{Match: plan.Match{Group: kind.Group, Resource: kind.Resource, Name: c.name}, Stuck: true})
				if err != nil {
					t.Fatal(err)
				}
				return end
			}
			var end func()
			if c.createdUnderStuck {
				end = stuck()
			}
			apiVersion := strings.TrimPrefix(kind.Group+"/"+kind.Version, "/")
			if _, err := k.Create(ctx, map[string]any{"apiVersion": apiVersion, "kind": kind.Kind, "metadata": map[string]any{"name": c.name}, "spec": c.spec}); err != nil {
				t.Fatal(err)
			}
			if !c.createdUnderStuck {
				await("True", c.ready)
				end = stuck()
			}
			if c.refused {
				calls.awaitRefused(t, trace.Caller(s.Name()), c.name)
				await("True", c.ready)
			} else {
				await("False", c.stuck)
			}
			end()
			await("True", c.ready)
			for deadline := time.Now().Add(10 * time.Second); s.Pending(); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("The stage still has work pending after the fault's end.")
				}
			}
		})
	}
}

// readyIn returns the status of status's Ready condition.
func readyIn(status map[string]any) any {
	conditions, _ := status["conditions"].([]any)
	for _, c := range conditions {
		if c, _ := c.(map[string]any); c["type"] == "Ready" {
			return c["status"]
		}
	}
	return nil
}

// shows reports whether status holds each field of want, and lacks each
// one that want maps to nil.
func shows(status, want map[string]any) bool {
	for key, value := range want {
		got, found := status[key]
		if value == nil && found || value != nil && fmt.Sprint(got) != fmt.Sprint(value) {
			return false
		}
	}
	return true
}

type recordedCalls struct {
	mu    sync.Mutex
	calls []trace.Call
}

func (r *recordedCalls) record(e trace.Event) {
	if e.Call != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, *e.Call)
	}
}

// awaitRefused waits until the API server refuses a status write by caller
// to the object name.
func (r *recordedCalls) awaitRefused(t *testing.T, caller trace.Caller, name string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r.mu.Lock()
		refused := slices.ContainsFunc(r.calls, func(c trace.Call) bool {
			return c.Caller == caller && c.Ref.Name == name && c.Subresource == "status" && c.Status == 422
		})
		r.mu.Unlock()
		if refused {
			return
		}
	}
	t.Fatalf("The API server refused no status write by %s to %s.", caller, name)
}
