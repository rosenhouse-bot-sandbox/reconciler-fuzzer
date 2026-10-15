package run

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

var (
	widgetKind        = schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}
	configMapKind     = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	widgetResource    = schema.GroupVersionResource{Group: "toy.reconciler-fuzzer", Version: "v1", Resource: "widgets"}
	configMapResource = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

// unreachable points at nothing, so a test that reaches it fails rather than
// discovering the resources for itself.
func unreachable() *rest.Config { return &rest.Config{Host: "http://127.0.0.1:1"} }

func mapperFor(kinds ...schema.GroupVersionKind) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range kinds {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}

func TestValidateOptions(t *testing.T) {
	toy := &target.Target{Name: "toy-widget", Primary: widgetKind}
	for _, test := range []struct {
		name   string
		target *target.Target
		opts   Options
		want   string
	}{
		{name: "an output directory and a target are enough", target: toy, opts: Options{Dir: "out"}},
		{name: "no target", opts: Options{Dir: "out"}, want: "target"},
		{name: "no output directory", target: toy, want: "output directory"},
		{
			name:   "a cluster with a controller manager and no config",
			target: toy,
			opts:   Options{Dir: "out", ControllerManager: true},
			want:   "controller manager",
		},
		{
			name:   "a cluster with a controller manager and a config",
			target: toy,
			opts:   Options{Dir: "out", ControllerManager: true, Config: &rest.Config{}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validate(test.target, test.opts)
			switch {
			case test.want == "" && err != nil:
				t.Errorf("validate returned %v, want the options accepted.", err)
			case test.want != "" && err == nil:
				t.Errorf("validate accepted the options, want an error naming %q.", test.want)
			case test.want != "" && !strings.Contains(err.Error(), test.want):
				t.Errorf("validate returned %q, want it to name %q.", err, test.want)
			}
		})
	}
}

var serviceAccountKind = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}

func inRunNamespace(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace("reconciler-fuzzer-run-1")
	u.SetName(name)
	return u
}

func TestAwaitNamespaceDefaults(t *testing.T) {
	serviceAccount := inRunNamespace(serviceAccountKind, "default")
	rootCA := inRunNamespace(configMapKind, "kube-root-ca.crt")
	both := []schema.GroupVersionKind{widgetKind, serviceAccountKind, configMapKind}
	for _, test := range []struct {
		name    string
		watched []schema.GroupVersionKind
		made    []*unstructured.Unstructured
		// later is what the cluster makes while the wait sleeps.
		later  []*unstructured.Unstructured
		within time.Duration
		want   string
	}{
		{name: "both made", watched: both, made: []*unstructured.Unstructured{serviceAccount, rootCA}},
		{
			name: "one made while it waits", watched: both, within: time.Minute,
			made: []*unstructured.Unstructured{serviceAccount}, later: []*unstructured.Unstructured{rootCA},
		},
		{
			name: "the ConfigMap never made", watched: both,
			made: []*unstructured.Unstructured{serviceAccount}, want: "v1/ConfigMap kube-root-ca.crt",
		},
		{
			name: "the ServiceAccount never made", watched: both,
			made: []*unstructured.Unstructured{rootCA}, want: "v1/ServiceAccount default",
		},
		{
			name: "an unwatched kind never made", watched: []schema.GroupVersionKind{widgetKind, configMapKind},
			made: []*unstructured.Unstructured{rootCA},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := observe.NewStore(observe.Options{Namespace: "reconciler-fuzzer-run-1"})
			for _, made := range test.made {
				store.Record(made.GroupVersionKind(), made, time.Now())
			}
			sleep := func(context.Context, time.Duration) error {
				for _, made := range test.later {
					store.Record(made.GroupVersionKind(), made, time.Now())
				}
				return nil
			}

			err := awaitNamespaceDefaults(t.Context(), store, test.watched, test.within, sleep)

			switch {
			case test.want == "" && err != nil:
				t.Errorf("awaitNamespaceDefaults returned %v, want nil.", err)
			case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
				t.Errorf("awaitNamespaceDefaults returned %v, want an error naming %q.", err, test.want)
			}
		})
	}
}

func TestExcludePresentLeavesWhatComesLaterManaged(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "reconciler-fuzzer-run-1", Manages: []schema.GroupVersionKind{configMapKind}})
	store.Record(configMapKind, inRunNamespace(configMapKind, "kube-root-ca.crt"), time.Now())

	excludePresent(store, []schema.GroupVersionKind{widgetKind, configMapKind})
	store.Record(configMapKind, inRunNamespace(configMapKind, "widget-0"), time.Now())

	var managed []string
	for _, v := range store.Managed() {
		managed = append(managed, v.Name)
	}
	if !slices.Equal(managed, []string{"widget-0"}) {
		t.Errorf("The managed objects are %v, want only widget-0.", managed)
	}
}

func TestNewNamespaceNameIsFreshAndServable(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		name := newNamespaceName()
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			t.Fatalf("The name %q is not a namespace name: %v", name, problems)
		}
		if !strings.HasPrefix(name, namespacePrefix) {
			t.Fatalf("The name %q does not start with %q.", name, namespacePrefix)
		}
		if seen[name] {
			t.Fatalf("The name %q is reused; a run namespace never is (DESIGN.md §5.5).", name)
		}
		seen[name] = true
	}
}

// The Observer watches what the Runner and the collector act on.
func TestTheObserverWatchesTheTargetsKinds(t *testing.T) {
	h := &Harness{
		Namespace: "reconciler-fuzzer-run-1",
		mapper:    mapperFor(),
		target:    &target.Target{Primary: widgetKind, Manages: []schema.GroupVersionKind{configMapKind}},
	}

	opts := h.observeOptions()

	if want := []schema.GroupVersionKind{widgetKind, configMapKind}; !slices.Equal(opts.Kinds, want) {
		t.Errorf("The Observer watches %v, want %v.", opts.Kinds, want)
	}
}

// Start builds the run's one mapper, so a discovery that fails ends the run.
func TestStartReportsADiscoveryThatFailed(t *testing.T) {
	toy := &target.Target{Name: "toy-widget", Primary: widgetKind}

	h, err := Start(t.Context(), toy, Options{Dir: t.TempDir(), Config: unreachable()})

	if err == nil {
		t.Fatal("Start discovered the API resources of a server that is not listening.")
	}
	if h != nil {
		t.Error("Start returned a Harness together with an error.")
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("Start returned %q, which does not say that discovery failed.", err)
	}
}

// The Runner resolves the kinds it acts on through the run's one mapper, and
// never discovers for itself.
func TestNewLiveRunResolvesThroughTheHarnessMapper(t *testing.T) {
	h := &Harness{
		Namespace: "reconciler-fuzzer-run-1",
		Config:    unreachable(),
		mapper:    mapperFor(widgetKind, configMapKind),
	}
	toy := &target.Target{Primary: widgetKind, Manages: []schema.GroupVersionKind{configMapKind}}

	live, err := newLiveRun(h, toy)

	if err != nil {
		t.Fatalf("newLiveRun returned an error: %v", err)
	}
	want := map[schema.GroupVersionKind]schema.GroupVersionResource{
		widgetKind:    widgetResource,
		configMapKind: configMapResource,
	}
	if !maps.Equal(live.resources, want) {
		t.Errorf("The Runner resolved %v, want %v.", live.resources, want)
	}
}

// The Runner sets how long a recreate waits for its CR, and moves that time
// once the Observer sees the deletion.
func TestAwaitCRGoneWaitsUntilTheInstantTheRunnerGives(t *testing.T) {
	cr := widget("widget")
	cr.SetNamespace("reconciler-fuzzer-run-1")
	live := liveOver(widgetsClient(cr))
	began := time.Now()
	moved, due := began.Add(100*time.Millisecond), began.Add(300*time.Millisecond)
	reads := 0

	gone, err := live.awaitCRGone(t.Context(), "widget", func() time.Time {
		if reads++; reads == 1 {
			return moved
		}
		return due
	})

	if err != nil || gone {
		t.Fatalf("The wait returned (%t, %v), want a CR that stayed.", gone, err)
	}
	if waited := time.Since(began); waited < due.Sub(began) {
		t.Errorf("The wait ended %v in, want it to last until %v.", waited, due.Sub(began))
	}
}

func TestAwaitCRGoneReportsAReadThatFailed(t *testing.T) {
	client := widgetsClient()
	client.PrependReactor("get", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcd is down")
	})

	_, err := liveOver(client).awaitCRGone(t.Context(), "widget", time.Now)

	if err == nil || !strings.Contains(err.Error(), "etcd is down") {
		t.Errorf("The wait returned %v, want the read's error.", err)
	}
}

// liveOver is a live run of a toy whose T_delete is too short to matter.
func liveOver(client dynamic.Interface) *liveRun {
	return &liveRun{
		h:         &Harness{Namespace: "reconciler-fuzzer-run-1"},
		target:    &target.Target{Primary: widgetKind, Timeouts: target.Timeouts{Delete: time.Millisecond}},
		client:    client,
		resources: map[schema.GroupVersionKind]schema.GroupVersionResource{widgetKind: widgetResource},
	}
}

func widgetsClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{widgetResource: "WidgetList"}, objects...)
}

// What the live run says names a kind as target.yaml writes it.
func TestTheLiveRunNamesKindsAsTargetYAMLDoes(t *testing.T) {
	fixture := widget("fixture")
	fixture.SetNamespace("reconciler-fuzzer-run-1")
	held := fixture.DeepCopy()
	held.SetFinalizers([]string{"toy.reconciler-fuzzer/hold"})
	held.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	type reactors map[string]clienttesting.ReactionFunc
	refused := func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("refused") }
	accepted := func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, nil }
	for _, test := range []struct {
		name     string
		objects  []runtime.Object
		reactors reactors
		do       func(context.Context, *liveRun) string
		want     string
	}{
		{name: "an unresolved kind", do: func(context.Context, *liveRun) string {
			_, err := newLiveRun(&Harness{Config: unreachable(), mapper: mapperFor()}, &target.Target{Primary: widgetKind})
			return fmt.Sprint(err)
		}, want: "resolving the resource of toy.reconciler-fuzzer/v1/Widget:"},
		{name: "a CR of another kind", do: func(ctx context.Context, live *liveRun) string {
			cr := &unstructured.Unstructured{}
			cr.SetGroupVersionKind(configMapKind)
			return fmt.Sprint(live.createCR(ctx, cr))
		}, want: "the op creates a v1/ConfigMap, and the target's primary CR is a toy.reconciler-fuzzer/v1/Widget"},
		{name: "a refused deleteManaged", reactors: reactors{"delete": refused},
			do: func(ctx context.Context, live *liveRun) string {
				_, err := live.deleteManaged(ctx, widgetKind, "child")
				return fmt.Sprint(err)
			}, want: "deleting the managed toy.reconciler-fuzzer/v1/Widget child: refused"},
		{name: "a refused patchFixture", reactors: reactors{"patch": refused},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.patchFixture(ctx, widgetKind, "fixture", map[string]any{}))
			}, want: "patching the fixture toy.reconciler-fuzzer/v1/Widget fixture: refused"},
		{name: "a refused deleteFixture", reactors: reactors{"delete": refused},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.deleteFixture(ctx, widgetKind, "fixture"))
			}, want: "deleting the fixture toy.reconciler-fuzzer/v1/Widget fixture: refused"},
		{name: "a refused read after deleteFixture", reactors: reactors{"delete": accepted, "get": refused},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.deleteFixture(ctx, widgetKind, "fixture"))
			}, want: "waiting for the fixture toy.reconciler-fuzzer/v1/Widget fixture to go: refused"},
		{name: "a fixture its finalizers hold", objects: []runtime.Object{held}, reactors: reactors{"delete": accepted},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.deleteFixture(ctx, widgetKind, "fixture"))
			}, want: "the fixture toy.reconciler-fuzzer/v1/Widget fixture was still there"},
		{name: "a fixture something recreated", objects: []runtime.Object{fixture.DeepCopy()},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.createFixture(ctx, fixture.DeepCopy()))
			}, want: "something created the fixture toy.reconciler-fuzzer/v1/Widget fixture again"},
		{name: "a refused createFixture", reactors: reactors{"create": refused},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.createFixture(ctx, fixture.DeepCopy()))
			}, want: "restoring the fixture toy.reconciler-fuzzer/v1/Widget fixture: refused"},
		{name: "a refused list of what is left", reactors: reactors{"list": refused},
			do: func(ctx context.Context, live *liveRun) string {
				_, err := live.forceFinalizers(ctx)
				return fmt.Sprint(err)
			}, want: "listing the toy.reconciler-fuzzer/v1/Widget left behind: refused"},
		{name: "a refused patch of finalizers", objects: []runtime.Object{held}, reactors: reactors{"patch": refused},
			do: func(ctx context.Context, live *liveRun) string {
				_, err := live.forceFinalizers(ctx)
				return fmt.Sprint(err)
			}, want: "clearing the finalizers of toy.reconciler-fuzzer/v1/Widget fixture: refused"},
		{name: "finalizers it forced off", objects: []runtime.Object{held},
			do: func(ctx context.Context, live *liveRun) string {
				forced, err := live.forceFinalizers(ctx)
				return fmt.Sprint(forced, err)
			}, want: "[toy.reconciler-fuzzer/v1/Widget fixture] <nil>"},
		{name: "a refused delete of what is left", reactors: reactors{"delete-collection": refused},
			do: func(ctx context.Context, live *liveRun) string {
				return fmt.Sprint(live.empty(ctx))
			}, want: "deleting the toy.reconciler-fuzzer/v1/Widget left behind: refused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := widgetsClient(test.objects...)
			for verb, react := range test.reactors {
				client.PrependReactor(verb, "widgets", react)
			}
			live := liveOver(client)
			live.emptied = []schema.GroupVersionKind{widgetKind}

			if got := test.do(t.Context(), live); !strings.Contains(got, test.want) {
				t.Errorf("The live run said %q, want %q.", got, test.want)
			}
		})
	}
}

func TestAwaitCleanWaitsOutItsWindow(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "reconciler-fuzzer-run-1"})
	cr := widget("widget")
	cr.SetNamespace("reconciler-fuzzer-run-1")
	store.Record(widgetKind, cr, time.Now())
	live := &liveRun{h: &Harness{Observer: &observe.Observer{Store: store}}, target: &target.Target{Primary: widgetKind}}
	began := time.Now()

	clean, err := live.awaitClean(t.Context(), 100*time.Millisecond)

	if err != nil || clean {
		t.Fatalf("The wait returned (%t, %v), want a namespace that stayed.", clean, err)
	}
	if waited := time.Since(began); waited < 100*time.Millisecond {
		t.Errorf("The wait ended %v in, want it to last 100ms.", waited)
	}
}

// The Runner names each fault on the run's proxy by the ID the proxy gave it.
func TestTheLiveRunDrivesTheProxysFaults(t *testing.T) {
	p, err := proxy.Start(unreachable(), proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop() })
	live := &liveRun{h: &Harness{Proxy: p}}
	spec := proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}}
	get := func() int {
		resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	kept, removed := live.addFault(spec), live.addFault(spec)

	live.removeFault(removed)
	get()

	if window := live.faultWindow(removed); window.Retired.IsZero() {
		t.Error("The fault removed is still applying.")
	}
	if window := live.faultWindow(kept); window.First.IsZero() || !window.Retired.IsZero() {
		t.Errorf("The fault kept ran %+v, want it applied and still applying.", window)
	}
	live.clearFaults()
	if status := get(); status == http.StatusInternalServerError {
		t.Error("A request after clearFaults was faulted.")
	}
}

// The fixtures resolve through the same mapper.
func TestApplyFixturesResolvesThroughTheHarnessMapper(t *testing.T) {
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(configMapKind)
	fixture.SetName("fixture")
	h := &Harness{
		Namespace: "reconciler-fuzzer-run-1",
		Config:    unreachable(),
		mapper:    mapperFor(widgetKind),
		target:    &target.Target{Primary: widgetKind, Fixtures: []*unstructured.Unstructured{fixture}},
	}

	err := h.applyFixtures(t.Context())

	if err == nil {
		t.Fatal("applyFixtures applied a fixture whose kind the mapper cannot resolve.")
	}
	if !strings.Contains(err.Error(), "resolving the fixture") {
		t.Errorf("applyFixtures returned %q, which does not say it could not resolve the fixture.", err)
	}
}

func TestApplyFixturesSkipsClusterScopedFixtures(t *testing.T) {
	clusterRoleKind := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widgetKind, meta.RESTScopeNamespace)
	mapper.Add(clusterRoleKind, meta.RESTScopeRoot)
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(clusterRoleKind)
	fixture.SetName("shared")
	h := &Harness{
		Namespace: "reconciler-fuzzer-run-1",
		Config:    unreachable(),
		mapper:    mapper,
		target: &target.Target{
			Primary:         widgetKind,
			Fixtures:        []*unstructured.Unstructured{fixture},
			ClusterFixtures: []schema.GroupVersionKind{clusterRoleKind},
		},
	}

	err := h.applyFixtures(t.Context())

	if err != nil {
		t.Errorf("applyFixtures refused a cluster-scoped fixture: %v", err)
	}
}

func TestApplyFixturesRefusesAFixtureThatSetsANamespace(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(widgetKind, meta.RESTScopeNamespace)
	mapper.Add(configMapKind, meta.RESTScopeNamespace)
	fixture := &unstructured.Unstructured{}
	fixture.SetGroupVersionKind(configMapKind)
	fixture.SetName("shared")
	fixture.SetNamespace("elsewhere")
	h := &Harness{
		Namespace: "reconciler-fuzzer-run-1",
		Config:    unreachable(),
		mapper:    mapper,
		target:    &target.Target{Primary: widgetKind, Fixtures: []*unstructured.Unstructured{fixture}},
	}

	err := h.applyFixtures(t.Context())

	want := "the fixture ConfigMap shared sets metadata.namespace elsewhere"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("applyFixtures returned %v, want it to say %q.", err, want)
	}
}

// A settle wait ends at the first reading of a ready that yields no bool,
// rather than waiting out settle on it.
func TestTheHarnessReadsAReadyThatYieldsNoBoolAsAnError(t *testing.T) {
	store := observe.NewStore(observe.Options{Namespace: "reconciler-fuzzer-run-x"})
	store.Record(widgetKind, widget("widget"), time.Now())
	yieldsAnInt := &target.Target{Primary: widgetKind, Ready: func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
	}}
	h := &Harness{target: yieldsAnInt, Observer: &observe.Observer{Store: store}, Launcher: launcherReporting{status: launch.Status{Running: true}}, Proxy: proxyThatSaw(t)}

	_, _, err := h.state(time.Now())

	if !errors.Is(err, target.ErrNotBool) {
		t.Errorf("Reading the run returned the error %v, want the ready that yields no bool.", err)
	}
}

func TestReady(t *testing.T) {
	holds := func(*unstructured.Unstructured) (bool, error) { return true, nil }
	fails := func(*unstructured.Unstructured) (bool, error) { return false, nil }
	errs := func(*unstructured.Unstructured) (bool, error) {
		return false, errors.New("no such field: status.ready")
	}
	yieldsAnInt := func(*unstructured.Unstructured) (bool, error) {
		return false, &target.EvalError{Predicate: "ready", Expr: "status.ready", Err: fmt.Errorf("%w: it yielded int64", target.ErrNotBool)}
	}
	widget := observe.Version{Object: &unstructured.Unstructured{}}
	named := func(name string) observe.Version {
		cr := &unstructured.Unstructured{}
		cr.SetName(name)
		return observe.Version{Object: cr}
	}
	holdsOnReady := func(cr *unstructured.Unstructured) (bool, error) { return cr.GetName() == "ready", nil }

	for _, test := range []struct {
		name      string
		predicate target.ReadyFunc
		observed  []observe.Version
		want      bool
		wantErr   bool
	}{
		{name: "it holds on the one CR", predicate: holds, observed: []observe.Version{widget}, want: true},
		{name: "it fails on one of two CRs", predicate: fails, observed: []observe.Version{widget, widget}},
		{name: "it fails on the second of two CRs", predicate: holdsOnReady, observed: []observe.Version{named("ready"), named("unready")}},
		{name: "it cannot be evaluated", predicate: errs, observed: []observe.Version{widget}},
		{name: "no CR is left to be ready", predicate: fails, want: true},
		{name: "it yields no bool", predicate: yieldsAnInt, observed: []observe.Version{widget}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ready(test.predicate, test.observed)
			if got != test.want {
				t.Errorf("ready reported %t, want %t.", got, test.want)
			}
			if (err != nil) != test.wantErr {
				t.Errorf("ready returned the error %v, want one: %t.", err, test.wantErr)
			}
		})
	}
}

// launcherReporting is a launcher that only reports status.
type launcherReporting struct {
	launch.Launcher
	status launch.Status
}

func (l launcherReporting) Status() launch.Status { return l.status }

// harnessOver reads a quiet, empty run namespace, the target as status says,
// and the requests the proxy recorded.
func harnessOver(status launch.Status, p *proxy.Proxy) *Harness {
	return &Harness{
		target:   &target.Target{Primary: widgetKind},
		Observer: &observe.Observer{Store: observe.NewStore(observe.Options{Namespace: "reconciler-fuzzer-run-1"})},
		Launcher: launcherReporting{status: status},
		Proxy:    p,
	}
}

// listConfigMaps is the path of a request that shows the target runs.
const listConfigMaps = "/api/v1/namespaces/reconciler-fuzzer-run-1/configmaps"

// proxyThatSaw is a proxy that has recorded a request to each path.
func proxyThatSaw(t *testing.T, paths ...string) *proxy.Proxy {
	t.Helper()
	p, err := proxy.Start(unreachable(), proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop() })
	for _, path := range paths {
		resp, err := http.Get(p.URL() + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	return p
}

// A target waiting out a restart backoff is down, whatever state it left, and
// so is one that has not shown it runs since it started.
func TestATargetIsReadyOnlyOnceItShowsItRuns(t *testing.T) {
	since := time.Now()
	listed := proxyThatSaw(t, listConfigMaps)
	startedLater := time.Now()
	for _, test := range []struct {
		name   string
		status launch.Status
		ready  bool
	}{
		{"running", launch.Status{Running: true, Started: since.Add(-time.Minute)}, true},
		{"waiting to restart", launch.Status{Running: true, Restarting: true, Started: since.Add(-time.Minute)}, false},
		{"started after its last request", launch.Status{Running: true, Started: startedLater}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ready, _, err := harnessOver(test.status, listed).state(since)

			if err != nil || ready != test.ready {
				t.Errorf("A target %s reads as ready: %t (%v), want %t.", test.name, ready, err, test.ready)
			}
		})
	}
}

// A restarted target has to hold still for T_stable too, or a wait converges
// before the new process has done anything.
func TestARestartCountsAsAChange(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	restarted := since.Add(time.Second)

	_, changed, err := harnessOver(launch.Status{Running: true, Started: restarted}, proxyThatSaw(t)).state(since)

	if err != nil || !changed.Equal(restarted) {
		t.Errorf("The run last changed at %v (%v), want the restart at %v.", changed, err, restarted)
	}
}

// A target slow to come back does its startup's work after its first request,
// so a wait converges only once it has run for T_stable.
func TestTheTargetsFirstRequestCountsAsAChange(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	p := proxyThatSaw(t, listConfigMaps, listConfigMaps)
	first := p.Log()[0].Start

	_, changed, err := harnessOver(launch.Status{Running: true, Started: since.Add(time.Second)}, p).state(since)

	if err != nil || !changed.Equal(first) {
		t.Errorf("The run last changed at %v (%v), want the target's first request at %v.", changed, err, first)
	}
}

// Each exit of a supervised target is recorded with the line its own process
// wrote as it stopped, though every process writes to one log.
func TestTheHarnessRecordsWhatTheTargetWroteAsItExited(t *testing.T) {
	dir := t.TempDir()
	panicked := filepath.Join(dir, "panicked")
	log, err := os.Create(filepath.Join(dir, targetLogFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	binary := launch.NewBinary(launch.Options{
		Path: "/bin/sh",
		Args: []string{"-c", `if [ -e "$0" ]; then echo "E0923 lost the lease"; exit 1; fi
			echo "panic: first"; echo "goroutine 1 [running]:"; : > "$0"; exit 2`, panicked},
		Log:     log,
		Backoff: launch.MaxBackoff,
	})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	live := &liveRun{h: &Harness{dir: dir, Launcher: binary}}
	if err := binary.Start(t.Context(), "kubeconfig"); err != nil {
		t.Fatal(err)
	}

	live.supervise(t.Context())

	for deadline := time.Now().Add(10 * time.Second); len(live.exits()) < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	exits := live.exits()
	if len(exits) != 2 {
		t.Fatalf("The harness recorded the exits %+v, want the first and the one after its restart.", exits)
	}
	for i, want := range []struct {
		status, said string
		backoff      time.Duration
	}{
		{"exit status 2", "panic: first", 0},
		{"exit status 1", "E0923 lost the lease", launch.MaxBackoff},
	} {
		exit := exits[i]
		if exit.Said != want.said || exit.Err == nil || exit.Err.Error() != want.status || exit.At.IsZero() {
			t.Errorf("Exit %d is %+v, want %s after the target wrote %q.", i+1, exit, want.status, want.said)
		}
		if down := exit.Restart.Sub(exit.At); down > want.backoff || down < want.backoff-time.Second {
			t.Errorf("Exit %d restarts %v after it, want %v.", i+1, down, want.backoff)
		}
	}
}

// The API server takes no strategic merge patch of a custom resource, and one
// would merge lists that the sequence's merge patch replaces.
func TestAFixtureTakesAJSONMergePatch(t *testing.T) {
	fixture := widget("fixture")
	fixture.SetNamespace("reconciler-fuzzer-run-1")
	client := widgetsClient(fixture)

	err := liveOver(client).patchFixture(t.Context(), widgetKind, "fixture", map[string]any{"spec": map[string]any{"count": 2}})

	if err != nil {
		t.Fatalf("The patch failed: %v", err)
	}
	patches := slices.DeleteFunc(client.Actions(), func(action clienttesting.Action) bool { return action.GetVerb() != "patch" })
	if len(patches) != 1 || patches[0].(clienttesting.PatchAction).GetPatchType() != types.MergePatchType {
		t.Errorf("patchFixture sent %v, want one %s.", patches, types.MergePatchType)
	}
}

func TestAFixtureIsGoneOnceNoObjectUnderDeletionHoldsItsName(t *testing.T) {
	deleting := &unstructured.Unstructured{}
	deleting.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	refused := errors.New("the API server refused the read")
	for _, test := range []struct {
		name    string
		fixture *unstructured.Unstructured
		err     error
		gone    bool
	}{
		{"not found", nil, apierrors.NewNotFound(configMapResource.GroupResource(), "fixture"), true},
		{"still being deleted", deleting, nil, false},
		{"another object under its name", &unstructured.Unstructured{}, nil, true},
		{"a failed read", nil, refused, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			gone, err := fixtureGone(test.fixture, test.err)

			if gone != test.gone || (test.err == refused) != errors.Is(err, refused) {
				t.Errorf("fixtureGone returned (%t, %v), want gone: %t.", gone, err, test.gone)
			}
		})
	}
}

// An interrupt has ended the run's context, and an API server may never answer.
func TestTheRunNamespaceIsDeletedAfterAnInterruptWithinABound(t *testing.T) {
	requests := make(chan string, 10)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Method + " " + r.URL.Path
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	core, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	deleted := make(chan error, 1)
	go func() { deleted <- deleteNamespace(ended, core, "reconciler-fuzzer-run", 100*time.Millisecond) }()

	select {
	case err := <-deleted:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Deleting the namespace returned %v, want it to give up.", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Deleting the namespace still waited on the API server after 10s.")
	}
	select {
	case got := <-requests:
		if want := "DELETE /api/v1/namespaces/reconciler-fuzzer-run"; got != want {
			t.Errorf("The API server got %q, want %q.", got, want)
		}
	default:
		t.Error("The API server got no request to delete the namespace.")
	}
}

// The API server serves no Widget, so Start fails once it has created the
// namespace, and takes the namespace back as Stop would.
func TestTheRunNamespaceIsDeletedOnTheBudgetBoundGivesIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api":
			fmt.Fprint(w, `{"kind":"APIVersions","versions":["v1"]}`)
		case "GET /apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "GET /api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`)
		case "POST /api/v1/namespaces":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"reconciler-fuzzer-run"}}`)
		default:
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		}
	}))
	t.Cleanup(server.Close)
	budgets := make(chan time.Duration, 10)
	config := &rest.Config{Host: server.URL, WrapTransport: func(next http.RoundTripper) http.RoundTripper {
		return roundTripper(func(r *http.Request) (*http.Response, error) {
			if deadline, ok := r.Context().Deadline(); ok && r.Method == http.MethodDelete {
				budgets <- time.Until(deadline)
			}
			return next.RoundTrip(r)
		})
	}}

	h, err := Start(t.Context(), toyTarget, Options{Dir: t.TempDir(), Config: config})

	if h != nil || err == nil {
		t.Fatalf("Start returned (%v, %v), want an error: the API server serves no Widget.", h, err)
	}
	select {
	case budget := <-budgets:
		if want := stopBudget - launch.StopWithin; budget > want || budget < want-time.Second {
			t.Errorf("Deleting the namespace had %v, want %v.", budget, want)
		}
	default:
		t.Error("The API server got no request to delete the namespace on a budget.")
	}
}

// A target sends requests as it stops, and the collector tries deletes until
// it stops.
func TestStopWritesTheRecordingsOnceTheRunIsDown(t *testing.T) {
	dir := t.TempDir()
	p := proxyThatSaw(t)
	h := &Harness{dir: dir, Proxy: p, Observer: &observe.Observer{Store: observe.NewStore(observe.Options{})}}
	h.down.push("stopping the target", func(context.Context) error {
		resp, err := http.Get(p.URL() + listConfigMaps)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	})

	if err := h.Stop(t.Context()); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, requestsFile))
	if err != nil || !strings.Contains(string(written), listConfigMaps) {
		t.Errorf("requests.jsonl holds %q (%v), want the request the target sent as it stopped.", written, err)
	}
}

func TestStopReportsARecordingItCannotWrite(t *testing.T) {
	for _, file := range []string{requestsFile, objectsFile, collectorFile} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, file), 0o755); err != nil {
			t.Fatal(err)
		}
		h := &Harness{dir: dir, Proxy: proxyThatSaw(t), Observer: &observe.Observer{Store: observe.NewStore(observe.Options{})},
			collector: &cluster.Collector{}}
		if err := h.Stop(t.Context()); err == nil || !strings.Contains(err.Error(), file) {
			t.Errorf("With a directory at %s, Stop returned %v, want an error that names it.", file, err)
		}
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The launcher supervises the target under the run's context, so a target that
// exits after an interrupt is not restarted.
func TestTheHarnessSupervisesUnderTheContextItIsGiven(t *testing.T) {
	dir := t.TempDir()
	binary := launch.NewBinary(launch.Options{Path: "/bin/sh", Args: []string{"-c", "exit 3"}})
	t.Cleanup(func() { _ = binary.Stop(context.Background()) })
	live := &liveRun{h: &Harness{dir: dir, Launcher: binary}}
	if err := binary.Start(t.Context(), "kubeconfig"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	live.supervise(ctx)

	select {
	case <-binary.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("The target was still supervised after its context ended.")
	}
	if exits := live.exits(); len(exits) != 0 {
		t.Errorf("The harness recorded the exits %+v, and supervision had ended.", exits)
	}
}
