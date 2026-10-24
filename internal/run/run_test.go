package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestExpandLaunch(t *testing.T) {
	l := config.Launch{
		Binary: "/bin/toy",
		Args:   []string{"--namespace=${systems.kube.namespace}", "--bug=0", "--cloud=${systems.cloud.url}"},
		Env:    map[string]string{"KUBECONFIG": "${systems.kube.kubeconfig}", "CLOUD": "${systems.cloud.url}/v1"},
	}
	vars := map[string]map[string]string{
		"kube":  {"namespace": "rf-1", "kubeconfig": "/out/kubeconfig"},
		"cloud": {"url": "http://127.0.0.1:1"},
	}
	o, err := expandLaunch(l, []string{"--bug=3"}, vars)
	if err != nil {
		t.Fatal(err)
	}
	if o.Path != "/bin/toy" || !reflect.DeepEqual(o.Args, []string{"--namespace=rf-1", "--bug=0", "--cloud=http://127.0.0.1:1", "--bug=3"}) ||
		!reflect.DeepEqual(o.Env, map[string]string{"KUBECONFIG": "/out/kubeconfig", "CLOUD": "http://127.0.0.1:1/v1"}) {
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

var clusterRoleKind = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}

func TestPrimaryKindsWatchEveryListedAndOwnsKind(t *testing.T) {
	secretKind := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	primaries := []config.Primary{
		{Kind: gadgetKind, Owns: []config.Owns{{Kind: clusterRoleKind, Name: "'x'"}, {System: "cloud", Collection: "buckets", Name: "'y'"}}},
		{Kind: sprocketKind, Owns: []config.Owns{{Kind: secretKind, Name: "'z'"}}},
	}
	if got, want := primaryKinds(primaries), []schema.GroupVersionKind{gadgetKind, clusterRoleKind, sprocketKind, secretKind}; !reflect.DeepEqual(got, want) {
		t.Errorf("primaryKinds = %v, want %v", got, want)
	}
}

func TestLoadSamplesReadsEachEntrysSampleInOrder(t *testing.T) {
	samples, err := loadSamples(samplePrimaries(t, gadgetKind, sprocketKind))
	want := []map[string]any{
		{"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Gadget"},
		{"apiVersion": "toy.reconciler-fuzzer/v2", "kind": "Sprocket"},
	}
	if err != nil || !reflect.DeepEqual(samples, want) {
		t.Errorf("loadSamples = %v, %v; want %v", samples, err, want)
	}
}

func TestLoadSamplesRefusesASampleOfAnotherKind(t *testing.T) {
	for _, sampleKind := range []schema.GroupVersionKind{sprocketKind, {Group: "other", Version: "v1", Kind: "Gadget"}} {
		primaries := samplePrimaries(t, sampleKind)
		primaries[0].Kind = gadgetKind
		if _, err := loadSamples(primaries); err == nil || !strings.Contains(err.Error(), sampleKind.GroupKind().String()) || !strings.Contains(err.Error(), gadgetKind.GroupKind().String()) {
			t.Errorf("loadSamples of a %s for a %s entry = %v, want an error that names both.", sampleKind.GroupKind(), gadgetKind.GroupKind(), err)
		}
	}
}

func TestLoadSamplesTakesASampleOfAnotherVersion(t *testing.T) {
	primaries := samplePrimaries(t, schema.GroupVersionKind{Group: gadgetKind.Group, Version: "v9", Kind: gadgetKind.Kind})
	primaries[0].Kind = gadgetKind
	if _, err := loadSamples(primaries); err != nil {
		t.Error(err)
	}
}

// samplePrimaries are entries of kinds, each with a sample of its kind.
func samplePrimaries(t *testing.T, kinds ...schema.GroupVersionKind) []config.Primary {
	var primaries []config.Primary
	for _, kind := range kinds {
		path := filepath.Join(t.TempDir(), "sample.yaml")
		if err := os.WriteFile(path, []byte("apiVersion: "+kind.GroupVersion().String()+"\nkind: "+kind.Kind+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		primaries = append(primaries, config.Primary{Kind: kind, Sample: path})
	}
	return primaries
}

func TestAPIMapTakesTheSystemsAPI(t *testing.T) {
	for _, api := range []string{"crud", "vault", "s3"} {
		if m, err := apiMap(config.System{Name: "cloud", API: api}); err != nil || m == nil {
			t.Errorf("apiMap(%s) = %v, %v", api, m, err)
		}
	}
	if _, err := apiMap(config.System{Name: "cloud", API: "rest"}); err == nil {
		t.Error("apiMap took an unknown api")
	}
}

func TestFakeOptionsCarryTheFixturesAndToken(t *testing.T) {
	fixtures := filepath.Join(t.TempDir(), "secrets.yaml")
	os.WriteFile(fixtures, []byte("- {resource: secret, name: app/db, body: {password: p1}}\n"), 0o644)
	s := config.System{
		Name: "vault", API: "vault", Token: "root", Fixtures: []string{fixtures}, Dependency: &config.Dependency{},
		Process: &config.Process{Command: []string{"/bin/vault", "server"}, Dir: "/cfg"},
	}
	o, err := fakeOptions(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []fake.Fixture{{Resource: "secret", Name: "app/db", Body: json.RawMessage(`{"password":"p1"}`)}}
	if o.Name != "vault" || !reflect.DeepEqual(o.Command, s.Process.Command) || o.Dir != "/cfg" || !o.SeenAll ||
		!reflect.DeepEqual(o.Fixtures, want) || !reflect.DeepEqual(o.Vars, map[string]string{"token": "root"}) {
		t.Errorf("fakeOptions = %+v", o)
	}
	s.Token, s.Fixtures, s.Dependency = "", nil, nil
	if o, err := fakeOptions(s); err != nil || o.Vars != nil || o.Fixtures != nil || o.SeenAll {
		t.Errorf("fakeOptions = %+v, %v, want no vars or fixtures", o, err)
	}
	s.Fixtures = []string{filepath.Join(t.TempDir(), "missing.yaml")}
	if _, err := fakeOptions(s); err == nil {
		t.Error("fakeOptions read a missing fixtures file")
	}
}

func TestFlattenNamesEachSystemsVars(t *testing.T) {
	got := flatten(map[string]map[string]string{"vault": {"url": "u", "token": "t"}, "kube": {"namespace": "rf"}})
	want := map[string]string{"systems.vault.url": "u", "systems.vault.token": "t", "systems.kube.namespace": "rf"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("flatten = %v", got)
	}
}
