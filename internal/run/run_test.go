package run

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestExpandLaunch(t *testing.T) {
	l := config.Launch{
		Binary: "/bin/toy",
		Args:   []string{"--namespace=${systems.kube.namespace}", "--bug=0"},
		Env:    map[string]string{"KUBECONFIG": "${systems.kube.kubeconfig}"},
	}
	vars := map[string]string{"namespace": "rf-1", "kubeconfig": "/out/kubeconfig"}
	o, err := expandLaunch(l, []string{"--bug=3"}, vars)
	if err != nil {
		t.Fatal(err)
	}
	if o.Path != "/bin/toy" || !reflect.DeepEqual(o.Args, []string{"--namespace=rf-1", "--bug=0", "--bug=3"}) ||
		!reflect.DeepEqual(o.Env, map[string]string{"KUBECONFIG": "/out/kubeconfig"}) {
		t.Errorf("expandLaunch = %+v", o)
	}
	if _, err := expandLaunch(l, []string{"${systems.kube.url}"}, vars); err == nil || !strings.Contains(err.Error(), "systems.kube.url") {
		t.Errorf("expandLaunch = %v, want an unknown key error", err)
	}
	l.Args = nil
	l.Env["X"] = "${nope}"
	if _, err := expandLaunch(l, nil, vars); err == nil || !strings.Contains(err.Error(), "X") {
		t.Errorf("expandLaunch = %v, want an env error naming X", err)
	}
}

var (
	gadgetKind   = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}
	sprocketKind = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v2", Kind: "Sprocket"}
	testModels   = []config.Model{
		{Kind: gadgetKind, Dependency: &config.Dependency{Required: true, Track: time.Second}},
		{Kind: sprocketKind},
	}
)

func TestModelKindsWatchEveryModelAndSeeOnlyDependencies(t *testing.T) {
	watch, seen := modelKinds(testModels)
	if !reflect.DeepEqual(watch, []schema.GroupVersionKind{gadgetKind, sprocketKind}) || !reflect.DeepEqual(seen, []schema.GroupVersionKind{gadgetKind}) {
		t.Errorf("modelKinds = %v, %v", watch, seen)
	}
	if watch, seen := modelKinds(nil); watch != nil || seen != nil {
		t.Errorf("modelKinds(nil) = %v, %v, want none", watch, seen)
	}
}

func TestTraceModelsNameKindsBothWays(t *testing.T) {
	got, err := traceModels(testModels, func(gvk schema.GroupVersionKind, _ string) (trace.Ref, error) {
		return trace.Ref{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind) + "s"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []trace.Model{
		{Kind: trace.Kind{Group: gadgetKind.Group, Version: "v1", Kind: "Gadget", Resource: "gadgets"}, Ready: config.ReadyConditionCEL, Dependency: &trace.Dependency{Required: true, Track: time.Second}},
		{Kind: trace.Kind{Group: sprocketKind.Group, Version: "v2", Kind: "Sprocket", Resource: "sprockets"}, Ready: config.ReadyConditionCEL},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("traceModels = %+v, want %+v", got, want)
	}
}
