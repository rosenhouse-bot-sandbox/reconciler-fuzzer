//go:build envtest

package kube

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const toy = "../../targets/toy-widget/"

var widgetGVK = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}

type events struct {
	mu   sync.Mutex
	list []trace.Event
}

func (e *events) record(ev trace.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) snapshot() []trace.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]trace.Event(nil), e.list...)
}

// holds says whether match holds for some recorded event now.
func (e *events) holds(match func(trace.Event) bool) bool {
	for _, ev := range e.snapshot() {
		if match(ev) {
			return true
		}
	}
	return false
}

// await polls until match holds for some recorded event.
func (e *events) await(t *testing.T, what string, match func(trace.Event) bool) trace.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range e.snapshot() {
			if match(ev) {
				return ev
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Recorded no %s.", what)
	return trace.Event{}
}

func TestKube(t *testing.T) {
	ctx := context.Background()
	recorded := &events{}
	k := New(Options{
		CRDs:     []string{toy + "crds"},
		RBAC:     []string{toy + "rbac/role.yaml"},
		Fixtures: []string{toy + "widget-config.yaml"},
		Watch:    []schema.GroupVersionKind{widgetGVK},
		Seen:     []schema.GroupVersionKind{widgetGVK},
		Warm:     []map[string]any{{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": map[string]any{"name": "warm"}, "spec": map[string]any{"count": int64(1)}}},
	})
	endpoints, err := k.Start(ctx, &system.Env{Record: recorded.record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { k.Stop(ctx) })
	ns := k.Namespace()

	t.Run("vars name the controller listener and namespace", func(t *testing.T) {
		vars := endpoints.Vars
		if vars["namespace"] != ns || vars["url"] != endpoints.Controller.URL() || endpoints.Harness.URL() == vars["url"] {
			t.Errorf("Vars are %v; namespace %s, controller %s.", vars, ns, endpoints.Controller.URL())
		}
		config, err := clientcmd.LoadFromFile(vars["kubeconfig"])
		if err != nil {
			t.Fatal(err)
		}
		current := config.Contexts[config.CurrentContext]
		if config.Clusters[current.Cluster].Server != vars["url"] || current.Namespace != ns || len(config.AuthInfos) != 0 {
			t.Errorf("Kubeconfig is %+v.", config)
		}
	})

	t.Run("the kubeconfig acts as the controller identity", func(t *testing.T) {
		config, err := clientcmd.BuildConfigFromFlags("", endpoints.Vars["kubeconfig"])
		if err != nil {
			t.Fatal(err)
		}
		client := dynamic.NewForConfigOrDie(config)
		widgets := schema.GroupVersionResource{Group: widgetGVK.Group, Version: "v1", Resource: "widgets"}
		if _, err := client.Resource(widgets).Namespace(ns).List(ctx, metav1.ListOptions{}); err != nil {
			t.Errorf("Listing widgets: %v", err)
		}
		_, err = kubernetes.NewForConfigOrDie(config).CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		if !apierrors.IsForbidden(err) {
			t.Errorf("Listing secrets = %v, want forbidden.", err)
		}
		admin := kubernetes.NewForConfigOrDie(k.admin)
		if _, err := admin.RbacV1().ClusterRoles().Get(ctx, "toy-widget-"+ns, metav1.GetOptions{}); err != nil {
			t.Errorf("Getting the run's ClusterRole: %v", err)
		}
	})

	t.Run("a cluster-scoped ref has no namespace", func(t *testing.T) {
		ref, err := k.Ref(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, ns)
		if err != nil || ref != (trace.Ref{System: Name, Resource: "namespaces", Name: ns}) {
			t.Errorf("Ref = %+v, %v.", ref, err)
		}
	})

	t.Run("the harness created the fixture", func(t *testing.T) {
		admin := kubernetes.NewForConfigOrDie(k.admin)
		fixture, err := admin.CoreV1().ConfigMaps(ns).Get(ctx, "widget-config", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		recorded.await(t, "harness create of the fixture", func(e trace.Event) bool {
			c := e.Call
			return c != nil && c.Caller == trace.Harness && c.Verb == "create" && c.Ref.Resource == "configmaps" &&
				c.ResultName == "widget-config" && c.ResultRV == fixture.ResourceVersion
		})
	})

	var widget trace.Ref
	t.Run("create records a call and the version it made", func(t *testing.T) {
		widget, err = k.Create(ctx, map[string]any{
			"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget",
			"metadata": map[string]any{"generateName": "w-"},
			"spec":     map[string]any{"count": int64(1)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if want, _ := k.Ref(widgetGVK, widget.Name); widget != want || !strings.HasPrefix(widget.Name, "w-") || widget.Namespace != ns {
			t.Fatalf("Create returned %+v, and Ref returns %+v.", widget, want)
		}
		if !recorded.holds(func(e trace.Event) bool { return e.Version != nil && e.Version.Ref == widget }) {
			t.Error("Create returned before the observer recorded the widget.")
		}
		create := recorded.await(t, "harness create of the widget", func(e trace.Event) bool {
			return e.Call != nil && e.Call.Caller == trace.Harness && e.Call.Verb == "create" && e.Call.ResultName == widget.Name
		}).Call
		recorded.await(t, "version of the created widget", func(e trace.Event) bool {
			return e.Version != nil && e.Version.Ref == widget && e.Version.RV == create.ResultRV && !e.Version.Deleted
		})
	})

	t.Run("the controller listener alone records Seen", func(t *testing.T) {
		config, _ := clientcmd.BuildConfigFromFlags("", endpoints.Vars["kubeconfig"])
		widgets := schema.GroupVersionResource{Group: widgetGVK.Group, Version: "v1", Resource: "widgets"}
		got, err := dynamic.NewForConfigOrDie(config).Resource(widgets).Namespace(ns).Get(ctx, widget.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		recorded.await(t, "the controller's Seen of the widget", func(e trace.Event) bool {
			return e.Seen != nil && e.Seen.Caller == trace.Controller && e.Seen.Ref == widget && e.Seen.RV == got.GetResourceVersion()
		})
		if _, err := k.harness.Resource(widgets).Namespace(ns).Get(ctx, widget.Name, metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		if recorded.holds(func(e trace.Event) bool { return e.Seen != nil && e.Seen.Caller != trace.Controller }) {
			t.Error("Another listener recorded a Seen.")
		}
	})

	t.Run("status kinds list the widget", func(t *testing.T) {
		want := trace.Kind{Group: widgetGVK.Group, Version: "v1", Kind: "Widget", Resource: "widgets"}
		if got := k.StatusKinds(); !slices.Contains(got, want) {
			t.Errorf("StatusKinds = %+v, want the widget's.", got)
		}
	})

	t.Run("a controller call on a new kind starts its watch", func(t *testing.T) {
		admin := kubernetes.NewForConfigOrDie(k.admin)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}}
		if _, err := admin.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		config, _ := clientcmd.BuildConfigFromFlags("", endpoints.Vars["kubeconfig"])
		kubernetes.NewForConfigOrDie(config).CoreV1().Secrets(ns).Get(ctx, "s", metav1.GetOptions{})
		recorded.await(t, "version of the secret", func(e trace.Event) bool {
			return e.Version != nil && e.Version.Ref.Resource == "secrets" && e.Version.Ref.Name == "s"
		})
	})

	t.Run("snapshot lists the widget", func(t *testing.T) {
		resources, err := k.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		found := false
		for _, r := range resources {
			kinds = append(kinds, r.Ref.Resource)
			found = found || (r.Ref == widget && r.UID != "" && strings.Contains(string(r.Body), `"count":1`))
		}
		if !found {
			t.Errorf("Snapshot holds %v, not the widget.", kinds)
		}
	})

	t.Run("patch, delete and wait gone record the deletion", func(t *testing.T) {
		if err := k.Patch(ctx, widget, []byte(`{"spec":{"count":2}}`)); err != nil {
			t.Fatal(err)
		}
		if !recorded.holds(func(e trace.Event) bool {
			return e.Version != nil && e.Version.Ref == widget && strings.Contains(string(e.Version.Body), `"count":2`)
		}) {
			t.Error("Patch returned before the observer recorded the patched widget.")
		}
		if err := k.Patch(ctx, widget, []byte(`{"metadata":{"finalizers":["test/hold"]}}`)); err != nil {
			t.Fatal(err)
		}
		if err := k.Delete(ctx, widget); err != nil {
			t.Fatal(err)
		}
		if !recorded.holds(func(e trace.Event) bool {
			return e.Version != nil && e.Version.Ref == widget && strings.Contains(string(e.Version.Body), "deletionTimestamp")
		}) {
			t.Error("Delete returned before the observer recorded the deletion.")
		}
		if err := k.Patch(ctx, widget, []byte(`{"metadata":{"finalizers":null}}`)); err != nil {
			t.Fatal(err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := k.WaitGone(waitCtx, widget); err != nil {
			t.Fatal(err)
		}
		if err := k.Delete(ctx, widget); !errors.Is(err, system.ErrGone) {
			t.Errorf("Deleting a missing widget = %v, want ErrGone.", err)
		}
		recorded.await(t, "deleted version", func(e trace.Event) bool {
			return e.Version != nil && e.Version.Ref == widget && e.Version.Deleted && strings.Contains(string(e.Version.Body), `"count":2`)
		})
	})

	t.Run("wait gone honours its context", func(t *testing.T) {
		fixture, _ := k.Ref(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "widget-config")
		waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if err := k.WaitGone(waitCtx, fixture); err == nil {
			t.Error("WaitGone returned nil for a live object.")
		}
	})

	gc, err := k.Listen(trace.GC)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("a model listener records its caller and acts as admin", func(t *testing.T) {
		if _, err := kubernetes.NewForConfigOrDie(gc).CoreV1().Secrets(ns).Get(ctx, "s", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		recorded.await(t, "the gc's get", func(e trace.Event) bool {
			return e.Call != nil && e.Call.Caller == trace.GC && e.Call.Verb == "get" && e.Call.Ref.Name == "s" && e.Call.Status == 200
		})
	})

	t.Run("stop is idempotent", func(t *testing.T) {
		if err := k.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if err := k.Stop(ctx); err != nil {
			t.Errorf("Second Stop: %v", err)
		}
		if _, err := k.Snapshot(ctx); err == nil {
			t.Error("Snapshot reached envtest after Stop.")
		}
		if _, err := http.Get(gc.Host); err == nil {
			t.Error("The gc listener answered after Stop.")
		}
	})
}
