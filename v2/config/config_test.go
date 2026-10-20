package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
)

func TestToyConfigLoads(t *testing.T) {
	c, err := config.Load("../targets/toy-widget/config.yaml")
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
