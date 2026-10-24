//go:build envtest

package check

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

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
		// The API server refuses to lower a done Job's succeeded.
		{name: "done-job", kind: "Job", spec: job, ready: map[string]any{"succeeded": 1, "active": nil}, refused: true},
	}

	ctx := context.Background()
	kinds := map[string]trace.Kind{}
	var watch []schema.GroupVersionKind
	for kind := range builtinStatusKinds {
		kinds[kind.Kind] = kind
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
	stages := map[string]*model.Stage{}
	for _, m := range loadSuggestedModels(t) {
		s, err := model.NewStage(k, m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, &system.Env{Record: calls.record}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Stop(ctx) })
		stages[m.Kind.Kind] = s
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
			startStuck := func() func() {
				end, err := s.Fault(plan.Fault{Match: plan.Match{Group: kind.Group, Resource: kind.Resource, Name: c.name}, Stuck: true})
				if err != nil {
					t.Fatal(err)
				}
				return end
			}
			var end func()
			if c.createdUnderStuck {
				end = startStuck()
			}
			apiVersion := strings.TrimPrefix(kind.Group+"/"+kind.Version, "/")
			if _, err := k.Create(ctx, map[string]any{"apiVersion": apiVersion, "kind": kind.Kind, "metadata": map[string]any{"name": c.name}, "spec": c.spec}); err != nil {
				t.Fatal(err)
			}
			if !c.createdUnderStuck {
				await("True", c.ready)
				end = startStuck()
			}
			caller := trace.Caller(s.Name())
			if c.refused {
				calls.awaitMore(t, caller, c.name, 422, 0)
				await("True", c.ready)
			} else {
				await("False", c.stuck)
			}
			accepted := calls.count(caller, c.name, 200)
			end()
			calls.awaitMore(t, caller, c.name, 200, accepted)
			await("True", c.ready)
			for deadline := time.Now().Add(10 * time.Second); s.Pending(); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("The stage still has work pending after the fault's end.")
				}
			}
		})
	}
}

// loadSuggestedModels loads the models entry that report.md suggests for
// each built-in kind, as a config would.
func loadSuggestedModels(t *testing.T) []config.Model {
	t.Helper()
	dir := t.TempDir()
	content := "launch: {binary: controller}\nprimary: {kind: v1/ConfigMap, sample: sample.yaml, ready: \"true\"}\nmodels:\n"
	for kind := range builtinStatusKinds {
		content += "  - " + suggestedModel(kind) + "\n"
	}
	for file, data := range map[string]string{"sample.yaml": "", "config.yaml": content} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return c.Models
}

// readyIn returns the Ready condition's status, such as "True". It does not
// use ReadyConditionCEL, which is false for a PVC or a Job, because their
// conditions lose observedGeneration (#163).
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

// count returns how many status writes by caller to the object name got
// the response code.
func (r *recordedCalls) count(caller trace.Caller, name string, code int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Caller == caller && c.Ref.Name == name && c.Subresource == "status" && c.Status == code {
			n++
		}
	}
	return n
}

// awaitMore waits until more than n such writes got the response code.
func (r *recordedCalls) awaitMore(t *testing.T, caller trace.Caller, name string, code, n int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); r.count(caller, name, code) <= n; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s made no more than %d status writes to %s that got %d.", caller, n, name, code)
		}
	}
}
