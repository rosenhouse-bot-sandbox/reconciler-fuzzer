package config_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func TestToyConfigLoads(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := (schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}); c.Primary.Kind != want {
		t.Errorf("kind = %v", c.Primary.Kind)
	}
	if c.Timeouts != (config.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, BackoffCap: 60 * time.Second}) {
		t.Errorf("timeouts = %+v", c.Timeouts)
	}
	if !filepath.IsAbs(c.Launch.Binary) || !strings.HasSuffix(c.Launch.Binary, "/bin/toy-widget") {
		t.Errorf("binary = %s", c.Launch.Binary)
	}
	if c.Launch.Env["WATCH_NAMESPACE"] != "${systems.kube.namespace}" {
		t.Errorf("env = %v", c.Launch.Env)
	}
	widget := func(generation, observed, count, ready any) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"generation": generation},
			"spec":     map[string]any{"count": count},
			"status":   map[string]any{"observedGeneration": observed, "ready": ready},
		}
	}
	for name, c2 := range map[string]struct {
		obj  map[string]any
		want bool
	}{
		"ready":          {widget(2, 2, 3, 3), true},
		"stale":          {widget(2, 1, 3, 3), false},
		"short":          {widget(2, 2, 3, 2), false},
		"status no keys": {map[string]any{"metadata": map[string]any{"generation": 1}, "spec": map[string]any{"count": 1}, "status": map[string]any{}}, false},
	} {
		got, err := c.Ready(c2.obj)
		if err != nil || got != c2.want {
			t.Errorf("%s: Ready = %v, %v; want %v", name, got, err, c2.want)
		}
	}
}

func TestReadyThatIsNotBoolIsAnError(t *testing.T) {
	path := writeConfig(t, strings.Replace(minimal(t), `ready: "true"`, `ready: self.spec`, 1))
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ready(map[string]any{"spec": "x"}); err == nil {
		t.Fatal("a string result is not an error")
	}
}

func TestReadyThatDoesNotCompileFails(t *testing.T) {
	for _, ready := range []string{`ready: "self.("`, `ready: "1 + 2"`, `ready: "other == 1"`} {
		path := writeConfig(t, strings.Replace(minimal(t), `ready: "true"`, ready, 1))
		if _, err := config.Load(path); err == nil {
			t.Errorf("%s loads", ready)
		}
	}
}

func TestReadyCanUseStringExtensions(t *testing.T) {
	path := writeConfig(t, strings.Replace(minimal(t), `ready: "true"`, `ready: "self.name.lowerAscii() == 'a'"`, 1))
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Ready(map[string]any{"name": "A"}); !ok || err != nil {
		t.Fatalf("Ready = %v, %v", ok, err)
	}
}

func TestUnknownKeyNamesItsLine(t *testing.T) {
	for _, c := range []struct{ old, new, line string }{
		{"crds:", "crdz: []\ncrds:", "line 4"},
		{"  ready:", "  readdy: x\n  ready:", "line 8"},
		{"  rbac:", "  extra: x\n  rbac:", "line 3"},
	} {
		path := writeConfig(t, strings.Replace(minimal(t), c.old, c.new, 1))
		_, err := config.Load(path)
		if err == nil || !strings.Contains(err.Error(), c.line) {
			t.Errorf("err = %v, want %s", err, c.line)
		}
	}
}

func TestMissingPathFails(t *testing.T) {
	for _, file := range []string{"crd.yaml", "sample.yaml", "role.yaml", "fixture.yaml"} {
		path := writeConfig(t, minimal(t))
		if err := os.Remove(filepath.Join(filepath.Dir(path), file)); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), file) {
			t.Errorf("without %s: err = %v", file, err)
		}
	}
}

func TestPathsResolveAgainstTheConfigDirectory(t *testing.T) {
	path := writeConfig(t, minimal(t))
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if c.Launch.Binary != filepath.Join(dir, "bin/controller") || c.Primary.Sample != filepath.Join(dir, "sample.yaml") ||
		c.CRDs[0] != filepath.Join(dir, "crd.yaml") || c.Fixtures[0] != filepath.Join(dir, "fixture.yaml") ||
		c.Launch.RBAC[0] != filepath.Join(dir, "role.yaml") {
		t.Fatalf("config = %+v", c)
	}
}

func TestDefaults(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets != config.DefaultBudgets || c.Budgets != (config.Budgets{Reads: 2, Writes: 4}) {
		t.Errorf("budgets = %+v", c.Budgets)
	}
	if c.Timeouts != (config.Timeouts{Settle: 30 * time.Second, Stable: 3 * time.Second, BackoffCap: 60 * time.Second}) {
		t.Errorf("timeouts = %+v", c.Timeouts)
	}
}

func TestSetValuesOverrideDefaults(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"budgets: {reads: 7}\ntimeouts: {backoffCap: 1m30s}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets != (config.Budgets{Reads: 7, Writes: 4}) {
		t.Errorf("budgets = %+v", c.Budgets)
	}
	if c.Timeouts != (config.Timeouts{Settle: 30 * time.Second, Stable: 3 * time.Second, BackoffCap: 90 * time.Second}) {
		t.Errorf("timeouts = %+v", c.Timeouts)
	}
}

func TestNonPositiveValuesFail(t *testing.T) {
	for _, extra := range []string{
		"budgets: {reads: 0}", "budgets: {writes: -1}",
		"timeouts: {settle: 0s}", "timeouts: {stable: -1s}", "timeouts: {backoffCap: 0s}", "timeouts: {stable: soon}",
	} {
		if _, err := config.Load(writeConfig(t, minimal(t)+extra+"\n")); err == nil {
			t.Errorf("%s loads", extra)
		}
	}
}

func TestRequiredKeys(t *testing.T) {
	for _, line := range []string{"  binary: bin/controller", "  kind: v1/ConfigMap", "  sample: sample.yaml", `  ready: "true"`} {
		path := writeConfig(t, strings.Replace(minimal(t), line+"\n", "", 1))
		if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "is required") {
			t.Errorf("without %q: err = %v", line, err)
		}
	}
}

func TestKind(t *testing.T) {
	for kind, want := range map[string]schema.GroupVersionKind{
		"v1/ConfigMap":       {Version: "v1", Kind: "ConfigMap"},
		"apps/v1/Deployment": {Group: "apps", Version: "v1", Kind: "Deployment"},
		"x.io/v1beta2/Thing": {Group: "x.io", Version: "v1beta2", Kind: "Thing"},
	} {
		c, err := config.Load(writeConfig(t, strings.Replace(minimal(t), "v1/ConfigMap", kind, 1)))
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if c.Primary.Kind != want {
			t.Errorf("%s parses as %v", kind, c.Primary.Kind)
		}
	}
	for _, kind := range []string{"ConfigMap", "apps/Deployment", "/v1/Thing", "a/b/c/d", "v1/"} {
		if _, err := config.Load(writeConfig(t, strings.Replace(minimal(t), "v1/ConfigMap", kind, 1))); err == nil {
			t.Errorf("%s parses", kind)
		}
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"systems.kube.namespace": "rf-1", "a": "b"}
	got, err := config.Expand("ns=${systems.kube.namespace} ${a}$", vars)
	if err != nil || got != "ns=rf-1 b$" {
		t.Fatalf("Expand = %q, %v", got, err)
	}
	if _, err := config.Expand("${nope}", vars); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown key: err = %v", err)
	}
	if _, err := config.Expand("x${a", vars); err == nil {
		t.Fatal("an unterminated ${ expands")
	}
}

func minimal(t *testing.T) string {
	data, err := os.ReadFile("testdata/minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writeConfig writes content next to copies of the testdata files.
func writeConfig(t *testing.T, content string) string {
	dir := t.TempDir()
	for _, file := range []string{"crd.yaml", "sample.yaml", "role.yaml", "fixture.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRetain(t *testing.T) {
	path := writeConfig(t, strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"`+"\n  retain: has(self.keep)", 1))
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Primary.Retain != "has(self.keep)" {
		t.Errorf("retain = %q", c.Primary.Retain)
	}
	for obj, want := range map[string]bool{`{"keep": 1}`: true, `{}`: false} {
		var self map[string]any
		if err := json.Unmarshal([]byte(obj), &self); err != nil {
			t.Fatal(err)
		}
		if got, err := c.Retain(self); got != want || err != nil {
			t.Errorf("Retain(%s) = %v, %v", obj, got, err)
		}
	}
	if _, err := config.Load(writeConfig(t, strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"`+"\n  retain: self.(", 1))); err == nil {
		t.Error("a retain that does not compile loads")
	}
}

func TestValues(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+`values:
  overlay:
    spec.count: {minimum: 1, maximum: 3}
  distinct: [spec.name]
  mutate: [spec.count]
  fixtures:
    fixture.yaml: [data.label]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Values.Overlay["spec.count"]["maximum"] != float64(3) || c.Values.Distinct[0] != "spec.name" || c.Values.Mutate[0] != "spec.count" {
		t.Errorf("values = %+v", c.Values)
	}
	if got := c.Values.Fixtures[c.Fixtures[0]]; len(got) != 1 || got[0] != "data.label" {
		t.Errorf("fixtures = %v", c.Values.Fixtures)
	}
	if _, err := config.Load(writeConfig(t, minimal(t)+"values: {fixtures: {other.yaml: [data.x]}}\n")); err == nil || !strings.Contains(err.Error(), "other.yaml") {
		t.Errorf("a fixture the config does not list: err = %v", err)
	}
}

func TestModels(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+`models:
  - kind: example.com/v1/Gadget
    stages: readyCondition
    after: 200ms
    status: {address: 10.0.0.1}
    dependency: {track: 5s}
  - kind: example.com/v1/Doohickey
    after: 1s
    dependency: {required: false}
  - kind: example.com/v1/Gizmo
    after: 0s
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models) != 3 {
		t.Fatalf("models = %+v", c.Models)
	}
	gadget, doohickey, gizmo := c.Models[0], c.Models[1], c.Models[2]
	if want := (schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Gadget"}); gadget.Kind != want {
		t.Errorf("kind = %v", gadget.Kind)
	}
	if gadget.Stage != config.ReadyCondition || gadget.After != 200*time.Millisecond || gadget.Status["address"] != "10.0.0.1" {
		t.Errorf("gadget = %+v", gadget)
	}
	if gadget.Dependency == nil || !gadget.Dependency.Required || gadget.Dependency.Track != 5*time.Second {
		t.Errorf("gadget dependency = %+v", gadget.Dependency)
	}
	if doohickey.Stage != config.ReadyCondition || doohickey.Dependency == nil || doohickey.Dependency.Required {
		t.Errorf("doohickey = %+v, %+v", doohickey, doohickey.Dependency)
	}
	if gizmo.Dependency != nil || gizmo.After != 0 {
		t.Errorf("gizmo = %+v", gizmo)
	}
}

func TestModelsRefuseMistakes(t *testing.T) {
	for _, models := range []string{
		"- {after: 1s}",
		"- {kind: Gadget, after: 1s}",
		"- {kind: example.com/v1/Gadget}",
		"- {kind: example.com/v1/Gadget, after: -1s}",
		"- {kind: example.com/v1/Gadget, after: 1s, stages: flap}",
		"- {kind: example.com/v1/Gadget, after: 1s, dependency: {track: 0s}}",
		"- {kind: v1/ConfigMap, after: 1s}",
		"- {kind: example.com/v1/Gadget, after: 1s}\n- {kind: example.com/v1/Gadget, after: 2s}",
	} {
		if _, err := config.Load(writeConfig(t, minimal(t)+"models:\n"+models+"\n")); err == nil {
			t.Errorf("models %q loaded, want an error", models)
		}
	}
}

func TestModelReady(t *testing.T) {
	ready, err := config.CompileReady(config.ReadyConditionCEL)
	if err != nil {
		t.Fatal(err)
	}
	gadget := func(generation int, conditions ...any) map[string]any {
		return map[string]any{"metadata": map[string]any{"generation": generation}, "status": map[string]any{"conditions": conditions}}
	}
	condition := func(status string, observed int) map[string]any {
		return map[string]any{"type": "Ready", "status": status, "observedGeneration": observed}
	}
	for name, c := range map[string]struct {
		obj  map[string]any
		want bool
	}{
		"ready":     {gadget(2, condition("True", 2)), true},
		"stale":     {gadget(2, condition("True", 1)), false},
		"false":     {gadget(2, condition("False", 2)), false},
		"other":     {gadget(1, map[string]any{"type": "Synced", "status": "True", "observedGeneration": 1}), false},
		"no status": {map[string]any{"metadata": map[string]any{"generation": 1}}, false},
	} {
		if got, err := ready(c.obj); err != nil || got != c.want {
			t.Errorf("%s: ready = %v, %v; want %v", name, got, err, c.want)
		}
	}
}

func TestCompileString(t *testing.T) {
	name, err := config.CompileString("self.metadata.name + '-0'")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := name(map[string]any{"metadata": map[string]any{"name": "a"}}); got != "a-0" || err != nil {
		t.Errorf("name = %q, %v", got, err)
	}
	if got, err := config.CompileString("self.x.lowerAscii()"); err != nil {
		t.Errorf("string extensions: %v", err)
	} else if s, _ := got(map[string]any{"x": "AB"}); s != "ab" {
		t.Errorf("lowerAscii = %q", s)
	}
	dyn, err := config.CompileString("self.x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dyn(map[string]any{"x": 1}); !errors.Is(err, config.ErrNotString) {
		t.Errorf("a number: err = %v, want ErrNotString", err)
	}
	if _, err := dyn(map[string]any{}); err == nil {
		t.Error("a missing field is no error")
	}
	for _, expr := range []string{"1 + 2", "true", "self.(", "other"} {
		if _, err := config.CompileString(expr); err == nil {
			t.Errorf("%s compiles", expr)
		}
	}
}

func TestOwns(t *testing.T) {
	c, err := config.Load(writeConfig(t, strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"
  owns:
  - {kind: v1/Secret, name: self.spec.secretName}
  - {kind: rbac.authorization.k8s.io/v1/ClusterRole, name: "'cr-' + self.metadata.name", namespace: "''"}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	want := []config.Owns{
		{Kind: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Name: "self.spec.secretName"},
		{Kind: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, Name: "'cr-' + self.metadata.name", Namespace: "''"},
	}
	if !reflect.DeepEqual(c.Primary.Owns, want) {
		t.Errorf("owns = %+v", c.Primary.Owns)
	}
}

func TestOwnsRefusesMistakes(t *testing.T) {
	for entry, key := range map[string]string{
		"{kind: v1/Secret}":                                    "primary.owns[0].name",
		"{kind: Secret, name: self.x}":                         "primary.owns[0].kind",
		"{name: self.x}":                                       "primary.owns[0].kind",
		"{kind: v1/Secret, name: '1 + 2'}":                     "primary.owns[0].name",
		"{kind: v1/Secret, name: self.x, namespace: 'self.('}": "primary.owns[0].namespace",
		"{kind: v1/Secret, name: self.x, nmespace: self.y}":    "nmespace",
	} {
		_, err := config.Load(writeConfig(t, strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"`+"\n  owns: ["+entry+"]", 1)))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s: err = %v, want one naming %s", entry, err, key)
		}
	}
}

func TestSystems(t *testing.T) {
	c, err := config.Load(writeConfig(t, minimal(t)+"systems: [{name: kube, owner: \"self.metadata.labels['uid']\"}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []config.System{{Name: "kube", Owner: "self.metadata.labels['uid']"}}; !reflect.DeepEqual(c.Systems, want) {
		t.Errorf("systems = %+v", c.Systems)
	}
	if c, err := config.Load(writeConfig(t, minimal(t)+"systems: [{name: kube}]\n")); err != nil || c.Systems[0].Owner != "" {
		t.Errorf("no owner: %+v, %v", c, err)
	}
}

func TestSystemsRefuseMistakes(t *testing.T) {
	for systems, want := range map[string]string{
		"[{name: s3}]":                   "systems[0].name s3: the only system is kube until fake services land",
		"[{name: kube}, {name: kube}]":   "systems[1].name kube",
		"[{name: kube, owner: '1 + 2'}]": "systems[0].owner",
		"[{name: kube, ownr: self.x}]":   "did you mean owner",
		"[{name: kube, process: x}]":     "systems[0] takes name and owner",
	} {
		_, err := config.Load(writeConfig(t, minimal(t)+"systems: "+systems+"\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want one containing %q", systems, err, want)
		}
	}
}

func TestUnknownPrimaryKeyListsOwns(t *testing.T) {
	_, err := config.Load(writeConfig(t, strings.Replace(minimal(t), `  ready: "true"`, `  ready: "true"`+"\n  zzzzzz: x", 1)))
	if err == nil || !strings.Contains(err.Error(), "retain and owns") {
		t.Errorf("err = %v", err)
	}
}
