package observe_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

// unreachable points at nothing, so these tests fail unless Start validates
// before it touches the API server.
func unreachable() *rest.Config { return &rest.Config{Host: "http://127.0.0.1:1"} }

// knowsNothing resolves no kind at all.
func knowsNothing() meta.RESTMapper { return meta.NewDefaultRESTMapper(nil) }

func requireStartRejects(t *testing.T, opts observe.Options, reason string) {
	t.Helper()
	o, err := observe.Start(unreachable(), opts)
	if err == nil {
		o.Stop()
		t.Fatalf("Start accepted %+v.", opts)
	}
	if o != nil {
		t.Error("Start returned an Observer together with an error.")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("Start returned %q, which does not mention %q.", err, reason)
	}
}

func TestStartRejectsOptionsWithoutANamespace(t *testing.T) {
	requireStartRejects(t, observe.Options{
		Kinds:  []schema.GroupVersionKind{configMapGVK},
		Mapper: knowsNothing(),
	}, "namespace")
}

func TestStartRejectsOptionsWithNoKindToWatch(t *testing.T) {
	requireStartRejects(t, observe.Options{Namespace: namespace, Mapper: knowsNothing()}, "kind")
}

func TestStartRejectsOptionsWithoutARESTMapper(t *testing.T) {
	requireStartRejects(t, observe.Options{
		Namespace: namespace,
		Kinds:     []schema.GroupVersionKind{configMapGVK},
	}, "RESTMapper")
}

// The Observer resolves what it watches through the mapper the run built, and
// never discovers for itself.
func TestStartResolvesTheWatchedKindsThroughTheGivenMapper(t *testing.T) {
	requireStartRejects(t, observe.Options{
		Namespace: namespace,
		Kinds:     []schema.GroupVersionKind{configMapGVK},
		Mapper:    knowsNothing(),
	}, "resolving the resource of v1/ConfigMap")
}

func TestStartRejectsAClusterScopedKind(t *testing.T) {
	namespaceGVK := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(namespaceGVK, meta.RESTScopeRoot)

	requireStartRejects(t, observe.Options{
		Namespace: namespace,
		Kinds:     []schema.GroupVersionKind{namespaceGVK},
		Mapper:    mapper,
	}, "watching v1/Namespace: it is cluster-scoped")
}
