package target_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func toyTargetPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "targets", "toy-widget", "target.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func loadToy(t *testing.T) *target.Target {
	t.Helper()
	toy, err := target.Load(toyTargetPath(t))
	if err != nil {
		t.Fatalf("Load rejected the toy target: %v", err)
	}
	return toy
}

func TestLoadToyWidget(t *testing.T) {
	toy := loadToy(t)
	dir := filepath.Dir(toyTargetPath(t))

	if toy.Name != "toy-widget" || toy.Version != "dev" {
		t.Errorf("Load read name %q and version %q.", toy.Name, toy.Version)
	}
	if want := []string{filepath.Join(dir, "crds")}; !reflect.DeepEqual(toy.CRDs, want) {
		t.Errorf("Load resolved crds to %v, want %v.", toy.CRDs, want)
	}
	if want := (schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"}); toy.Primary != want {
		t.Errorf("Load read primary %v, want %v.", toy.Primary, want)
	}
	if want := []schema.GroupVersionKind{{Version: "v1", Kind: "ConfigMap"}}; !reflect.DeepEqual(toy.Manages, want) {
		t.Errorf("Load read manages %v, want %v.", toy.Manages, want)
	}
	if toy.Sample.GroupVersionKind() != toy.Primary || toy.Sample.GetName() != "widget" {
		t.Errorf("Load read sample %v/%v.", toy.Sample.GroupVersionKind(), toy.Sample.GetName())
	}
	if count, found, err := unstructured.NestedInt64(toy.Sample.Object, "spec", "count"); count != 3 || !found || err != nil {
		t.Errorf("Load read sample spec.count as (%v, %v, %v), want 3.", count, found, err)
	}
	if len(toy.Fixtures) != 1 || toy.Fixtures[0].GetName() != "widget-config" {
		t.Errorf("Load read the fixtures %v, want the ConfigMap widget-config.", toy.Fixtures)
	}
	if toy.Selector != nil {
		t.Errorf("Load read selector %v from a target that declares none.", toy.Selector)
	}
	if toy.Equal != nil || len(toy.EqualIgnore) != 0 {
		t.Errorf("Load read an equality hook or equalIgnore from a target that declares neither.")
	}
	if len(toy.Generate.Mutate) != 0 || len(toy.Generate.Overlay) != 0 {
		t.Errorf("Load read generate %+v from a target that declares none.", toy.Generate)
	}
	wantLaunch := target.LaunchSpec{
		Binary: "bin/toy-widget",
		Args:   []string{"--kubeconfig=$KUBECONFIG", "--label-from=widget-config", "--bug=0"},
		Env:    map[string]string{"WATCH_NAMESPACE": "$NAMESPACE"},
	}
	if !reflect.DeepEqual(toy.Launch, wantLaunch) {
		t.Errorf("Load read launch %+v, want %+v.", toy.Launch, wantLaunch)
	}
	wantTimeouts := target.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, Delete: 10 * time.Second}
	if toy.Timeouts != wantTimeouts {
		t.Errorf("Load read timeouts %+v, want %+v.", toy.Timeouts, wantTimeouts)
	}
	if want := (target.Thresholds{ErrLoop: 5}); toy.Thresholds != want {
		t.Errorf("Load read thresholds %+v, want %+v.", toy.Thresholds, want)
	}

	if len(toy.Properties) != 1 {
		t.Fatalf("Load read %d properties, want 1.", len(toy.Properties))
	}
	p1 := toy.Properties[0]
	if p1.ID != "P1" || p1.When != target.Checkpoint {
		t.Errorf("Load read property %q with when %q.", p1.ID, p1.When)
	}
	if !strings.Contains(p1.Description, "status.ready") {
		t.Errorf("Load read property description %q.", p1.Description)
	}
}

func widgetCR(generation, count int64, status map[string]any) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "toy.reconciler-fuzzer/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "widget", "generation": generation},
		"spec":       map[string]any{"count": count},
	}}
	if status != nil {
		cr.Object["status"] = status
	}
	return cr
}

// widgetBare is a Widget as the API server never returns one: no metadata, no
// spec and no status.
func widgetBare() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "toy.reconciler-fuzzer/v1",
		"kind":       "Widget",
	}}
}

func configMap(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
		"data":       map[string]any{"index": "0"},
	}}
}

func TestToyReady(t *testing.T) {
	ready := loadToy(t).Ready

	for _, tc := range []struct {
		name string
		cr   *unstructured.Unstructured
		want bool
	}{
		{"converged", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(3)}), true},
		{"converged on count 0", widgetCR(2, 0, map[string]any{"observedGeneration": int64(2), "ready": int64(0)}), true},
		{"stale generation", widgetCR(3, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(3)}), false},
		{"fewer children than count", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2), "ready": int64(1)}), false},
		{"no status at all", widgetCR(1, 3, nil), false},
		{"empty status", widgetCR(1, 3, map[string]any{}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ready(tc.cr)
			if err != nil {
				t.Fatalf("ready returned an error on a well-formed Widget: %v", err)
			}
			if got != tc.want {
				t.Errorf("ready returned %v, want %v.", got, tc.want)
			}
		})
	}
}

func TestToyPropertyP1(t *testing.T) {
	eval := loadToy(t).Properties[0].Eval

	for _, tc := range []struct {
		name    string
		cr      *unstructured.Unstructured
		managed []*unstructured.Unstructured
		want    bool
	}{
		{"status matches the children", widgetCR(2, 2, map[string]any{"ready": int64(2)}),
			[]*unstructured.Unstructured{configMap("widget-0"), configMap("widget-1")}, true},
		{"status ahead of the children", widgetCR(2, 2, map[string]any{"ready": int64(2)}),
			[]*unstructured.Unstructured{configMap("widget-0")}, false},
		{"other kinds do not count", widgetCR(2, 1, map[string]any{"ready": int64(1)}),
			[]*unstructured.Unstructured{widgetCR(2, 1, nil)}, false},
		{"no status yet", widgetCR(2, 1, nil), nil, true},
		{"no children yet", widgetCR(2, 1, map[string]any{"ready": int64(0)}), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := eval(tc.cr, tc.managed)
			if err != nil {
				t.Fatalf("P1 returned an error: %v", err)
			}
			if got != tc.want {
				t.Errorf("P1 returned %v, want %v.", got, tc.want)
			}
		})
	}
}

// writeTarget writes target.yaml plus the extra files, and returns the path of
// the target.yaml.
func writeTarget(t *testing.T, targetYAML string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"target.yaml": targetYAML}
	for name, content := range extra {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "target.yaml")
}

const sampleWidget = `apiVersion: toy.reconciler-fuzzer/v1
kind: Widget
metadata:
  name: widget
spec:
  count: 3
`

// minimalTarget is a valid declaration that each error case breaks in one way.
const minimalTarget = `name: min
primary: toy.reconciler-fuzzer/v1/Widget
sample: widget.yaml
launch: {binary: bin/min}
`

// minimalTargetWithEnv ends in an open launch.env block.
const minimalTargetWithEnv = `name: min
primary: toy.reconciler-fuzzer/v1/Widget
sample: widget.yaml
launch:
  binary: bin/min
  env:
`

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name       string
		targetYAML string
		sample     string
		wants      []string
	}{
		{"unknown field", minimalTarget + "reday: 'true'\n", "", []string{"reday"}},
		{"unknown nested field", minimalTarget + "timeouts:\n  settel: 5s\n", "", []string{"settel"}},
		{"a list where a string goes", minimalTarget + "version: [v1]\n", "", []string{"version", "array"}},
		{"primary without a version", "name: min\nprimary: Widget\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary", "Widget"}},
		{"primary group read as a version", "name: min\nprimary: apps/Deployment\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary", "apps"}},
		{"managed kind with too many slashes", minimalTarget + "manages:\n  - a/b/c/d\n", "", []string{"manages", "a/b/c/d"}},
		{"managed kind with an empty segment", minimalTarget + "manages:\n  - /v1/ConfigMap\n", "", []string{"manages", "/v1/ConfigMap"}},
		{"ready that does not compile", minimalTarget + "ready: status.ready ==\n", "", []string{"ready"}},
		{"ready that yields a string", minimalTarget + "ready: '\"yes\"'\n", "", []string{"ready", "bool"}},
		{"ready over an unbound variable", minimalTarget + "ready: managed.size() > 0\n", "", []string{"ready", "managed"}},
		{"property that does not compile", minimalTarget + "properties:\n  - id: P1\n    cel: managed.filter(\n", "", []string{"P1"}},
		{"property that yields an int", minimalTarget + "properties:\n  - id: P1\n    cel: managed.size()\n", "", []string{"P1", "bool"}},
		{"property without an id", minimalTarget + "properties:\n  - cel: 'true'\n", "", []string{"id"}},
		{"property with an unknown when", minimalTarget + "properties:\n  - id: P1\n    cel: 'true'\n    when: sometimes\n", "", []string{"when", "sometimes"}},
		{"unknown ready hook", minimalTarget + "ready: 'go:nosuchhook'\n", "", []string{"nosuchhook"}},
		{"unknown equal hook", minimalTarget + "equal: 'go:nosuchhook'\n", "", []string{"nosuchhook"}},
		{"unparsable timeout", minimalTarget + "timeouts:\n  settle: soon\n", "", []string{"settle", "soon"}},
		{"unparsable selector", minimalTarget + "selector: 'app in'\n", "", []string{"selector"}},
		{"missing sample", "name: min\nprimary: toy.reconciler-fuzzer/v1/Widget\nsample: nosuch.yaml\nlaunch: {binary: bin/min}\n", "", []string{"nosuch.yaml"}},
		{"missing fixture", minimalTarget + "fixtures:\n  - nosuch.yaml\n", "", []string{"nosuch.yaml"}},
		{"sample that is not an object", minimalTarget, "- 1\n- 2\n", []string{"widget.yaml"}},
		{"no name", "primary: toy.reconciler-fuzzer/v1/Widget\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"name"}},
		{"no primary", "name: min\nsample: widget.yaml\nlaunch: {binary: bin/min}\n", "", []string{"primary"}},
		{"no sample", "name: min\nprimary: toy.reconciler-fuzzer/v1/Widget\nlaunch: {binary: bin/min}\n", "", []string{"sample"}},
		{"no launch binary", "name: min\nprimary: toy.reconciler-fuzzer/v1/Widget\nsample: widget.yaml\n", "", []string{"launch.binary"}},
		{"sample of another kind", minimalTarget, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n", []string{"widget.yaml", "Secret", "Widget"}},
		{"sample holding two objects", minimalTarget, sampleWidget + "---\n" + sampleWidget, []string{"widget.yaml", "2 objects"}},
		{"sample holding no object", minimalTarget, "# just a comment\n", []string{"widget.yaml", "no object"}},
		{"sample with no name", minimalTarget, "apiVersion: toy.reconciler-fuzzer/v1\nkind: Widget\nmetadata:\n  generateName: widget-\n",
			[]string{"widget.yaml", "no metadata.name"}},
		{"sample that is not YAML", minimalTarget, "name: \"unterminated\n", []string{"widget.yaml"}},
		{"missing crds path", minimalTarget + "crds: [nosuch/]\n", "", []string{"crds", "nosuch"}},
		{"managed group read as a version", minimalTarget + "manages:\n  - apps/Deployment\n", "", []string{"manages", "apps"}},
		{"a kind not recreated that is not managed", minimalTarget + "manages: [v1/ConfigMap]\nnotRecreated: [v1/Secret]\n", "",
			[]string{"notRecreated", "v1/Secret", "manages"}},
		{"a malformed kind not recreated", minimalTarget + "manages: [v1/ConfigMap]\nnotRecreated: [ConfigMap]\n", "",
			[]string{"notRecreated", `"ConfigMap"`, "want group/version/Kind"}},
		{"duplicate property id", minimalTarget + "properties:\n  - id: P1\n    cel: 'true'\n  - id: P1\n    cel: 'false'\n", "", []string{"P1", "twice"}},
		{"property without cel", minimalTarget + "properties:\n  - id: P1\n", "", []string{"P1", "cel"}},
		{"equal written as CEL", minimalTarget + "equal: 'a == b'\n", "", []string{"equal", "go:"}},
		{"an ignored list index", minimalTarget + "equalIgnore:\n  - status.conditions[0].message\n", "",
			[]string{"equalIgnore", "status.conditions[0].message", "offset 17", "[*]"}},
		{"an ignored annotation whose key the dots split", minimalTarget + "equalIgnore: [metadata.annotations.probe.example.com/started-at]\n", "",
			[]string{"equalIgnore", `metadata.annotations["probe.example.com/started-at"]`, "block-style list"}},
		{"timeout of zero", minimalTarget + "timeouts:\n  stable: 0s\n", "", []string{"stable", "positive"}},
		{"negative timeout", minimalTarget + "timeouts:\n  delete: -1s\n", "", []string{"delete", "positive"}},
		{"errloop of zero", minimalTarget + "thresholds:\n  errloop: 0\n", "", []string{"errloop", "positive"}},
		{"negative quiet", minimalTarget + "thresholds:\n  quiet: -1\n", "", []string{"quiet -1", "negative"}},
		{"no CR at all", minimalTarget + "generate:\n  maxCRs: 0\n", "", []string{"generate.maxCRs 0", "at least 1"}},
		{"a launch env that sets the kubeconfig", minimalTargetWithEnv + "    KUBECONFIG: /elsewhere\n", "", []string{"launch.env", "KUBECONFIG"}},
		{"a launch env name holding an equals sign", minimalTargetWithEnv + "    A=B: x\n", "", []string{"launch.env", `"A=B"`}},
		{"an empty launch env name", minimalTargetWithEnv + "    '': x\n", "", []string{"launch.env", `""`}},
		{"a launch env that sets the kubeconfig after another name", minimalTargetWithEnv + "    A: x\n    KUBECONFIG: /elsewhere\n", "", []string{"launch.env", "KUBECONFIG"}},
		{"a zero byte in a launch env name", minimalTargetWithEnv + "    \"A\\0B\": x\n", "", []string{"launch.env", `"A\x00B"`}},
		{"a zero byte in a launch env value", minimalTargetWithEnv + "    A: \"x\\0y\"\n", "", []string{"launch.env", "value of A", "NUL"}},
		{"an octal launch env value", minimalTargetWithEnv + "    UMASK: 0022\n", "", []string{"launch.env", "UMASK: 0022 as 18;", "quote"}},
		{"a decimal launch env value", minimalTargetWithEnv + "    VERSION: 1.10\n", "", []string{"launch.env", "VERSION: 1.10 as 1.1;", "quote"}},
		{"a yes launch env value", minimalTargetWithEnv + "    VERBOSE: yes\n", "", []string{"launch.env", "VERBOSE: yes as true;", "quote"}},
		{"an ON launch env name", minimalTargetWithEnv + "    ON: x\n", "", []string{"launch.env", "name ON", "quote"}},
		{"an octal launch env value merged in", minimalTargetWithEnv + "    <<: {UMASK: 0022}\n", "", []string{"launch.env", "UMASK: 0022 as 18;"}},
		// Decoding matches keys regardless of case, but the check reads the
		// text under launch and env alone.
		{"a launch env in capitals", strings.Replace(minimalTargetWithEnv, "  env:", "  Env:", 1) + "    UMASK: 0022\n", "", []string{"launch.env", "write launch and env in lower case"}},
		{"a launch in capitals", strings.Replace(minimalTargetWithEnv, "launch:", "Launch:", 1) + "    UMASK: 0022\n", "", []string{"launch.env", "write launch and env in lower case"}},
		// A settle wait carves T_stable of quiet out of T_settle, so these
		// leave the target no time to react and every op expires. The wants
		// carry the durations: the temp directory's path holds the case name,
		// so a bare "stable" would match whatever the loader said.
		{"stable as wide as settle", minimalTarget + "timeouts:\n  settle: 5s\n  stable: 5s\n", "", []string{"stable 5s", "settle 5s"}},
		{"stable wider than settle", minimalTarget + "timeouts:\n  settle: 5s\n  stable: 10s\n", "", []string{"stable 10s", "settle 5s"}},
		{"a stable the default settle cannot hold", minimalTarget + "timeouts:\n  stable: 40s\n", "", []string{"stable 40s", "settle 30s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := sampleWidget
			if tc.sample != "" {
				sample = tc.sample
			}
			path := writeTarget(t, tc.targetYAML, map[string]string{"widget.yaml": sample})

			loaded, err := target.Load(path)
			if err == nil {
				t.Fatalf("Load accepted %s; it returned %+v.", tc.name, loaded)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load reported %q, which does not name %q.", err, want)
				}
			}
		})
	}
}

func TestLoadKeepsAnAbsolutePath(t *testing.T) {
	sample := filepath.Join(t.TempDir(), "widget.yaml")
	if err := os.WriteFile(sample, []byte(sampleWidget), 0o644); err != nil {
		t.Fatal(err)
	}
	path := writeTarget(t, "name: min\nprimary: toy.reconciler-fuzzer/v1/Widget\nlaunch: {binary: bin/min}\nsample: "+sample+"\n", nil)

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected an absolute sample path: %v", err)
	}
	if loaded.Sample.GetName() != "widget" {
		t.Errorf("Load read sample %q.", loaded.Sample.GetName())
	}
}

func TestLoadRejectsAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "target.yaml")

	if _, err := target.Load(missing); err == nil || !strings.Contains(err.Error(), missing) {
		t.Errorf("Load reported %v for a target.yaml that does not exist.", err)
	}
}

// TestLoadReportsTheResolvedPath covers the mistake of writing a path relative
// to the working directory where target.yaml's own directory is the base.
func TestLoadReportsTheResolvedPath(t *testing.T) {
	path := writeTarget(t, "name: min\nprimary: toy.reconciler-fuzzer/v1/Widget\nsample: targets/toy-widget/widget.yaml\n", nil)

	_, err := target.Load(path)
	if err == nil {
		t.Fatal("Load accepted a sample path that does not exist.")
	}
	if want := filepath.Join(filepath.Dir(path), "targets/toy-widget/widget.yaml"); !strings.Contains(err.Error(), want) {
		t.Errorf("Load reported %q, which does not name the path it looked in, %q.", err, want)
	}
}

// TestLoadResolvesPathsAgainstTheTargetDirectory pins the two different bases
// of DESIGN.md §8.1: files sit beside target.yaml, and the binary is relative
// to the working directory.
func TestLoadResolvesPathsAgainstTheTargetDirectory(t *testing.T) {
	path := writeTarget(t, `name: min
primary: toy.reconciler-fuzzer/v1/Widget
crds: [crds/]
sample: widget.yaml
fixtures: [fixture.yaml]
launch:
  binary: bin/min
`, map[string]string{
		"widget.yaml":  sampleWidget,
		"fixture.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: fixture\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: also-a-fixture\n",
		"crds/w.yaml":  "# a comment and no CRD\n",
	})
	dir := filepath.Dir(path)
	t.Chdir(t.TempDir())

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load failed from a working directory elsewhere: %v", err)
	}
	if want := []string{filepath.Join(dir, "crds")}; !reflect.DeepEqual(loaded.CRDs, want) {
		t.Errorf("Load resolved crds to %v, want %v.", loaded.CRDs, want)
	}
	// A fixture file holds as many objects as it declares.
	if len(loaded.Fixtures) != 2 || loaded.Fixtures[0].GetName() != "fixture" || loaded.Fixtures[1].GetName() != "also-a-fixture" {
		t.Errorf("Load read %d fixtures, want 2.", len(loaded.Fixtures))
	}
	if loaded.Launch.Binary != "bin/min" {
		t.Errorf("Load resolved launch.binary to %q; it is relative to the working directory.", loaded.Launch.Binary)
	}
}

func TestLoadPointsAtAMisspelledKey(t *testing.T) {
	for _, test := range []struct {
		name, yaml string
		want       []string
	}{
		{"at the top", minimalTarget + "reday: 'true'\n",
			[]string{"line 5: reday is not a key; did you mean ready?"}},
		{"in capitals", minimalTarget + "Reday: 'true'\n",
			[]string{"line 5: Reday is not a key; did you mean ready?"}},
		{"in a block", minimalTarget + "timeouts:\n  setle: 5s\n",
			[]string{"line 6: timeouts.setle is not a key; did you mean settle?"}},
		{"in a list item", minimalTarget + "properties:\n  - id: P1\n    cell: 'true'\n",
			[]string{"line 7: properties[0].cell is not a key; did you mean cel?"}},
		{"with no key near it", minimalTarget + "timeouts:\n  zzz: 5s\n",
			[]string{"line 6: timeouts.zzz is not a key; timeouts takes settle, stable and delete"}},
		{"with no key near it at the top", minimalTarget + "zzz: 1\n",
			[]string{"line 5: zzz is not a key; target.yaml takes name, version, crds,", "timeouts and thresholds"}},
		{"after keys any map takes", minimalTarget + "generate:\n  overlay:\n    spec.count: {maximum: 3}\n" +
			"thresholds:\n  errlop: 5\n",
			[]string{"line 9: thresholds.errlop is not a key; did you mean errloop?"}},
		{"after a merge", minimalTarget + "timeouts:\n  <<: {settle: 5s}\n  stabel: 2s\n",
			[]string{"line 7: timeouts.stabel is not a key; did you mean stable?"}},
		{"three edits away", minimalTarget + "timeouts:\n  setxxx: 5s\n",
			[]string{"line 6: timeouts.setxxx is not a key; timeouts takes settle, stable and delete"}},
		{"as near to delete as to settle", minimalTarget + "timeouts:\n  detele: 5s\n",
			[]string{"did you mean settle?"}},
		// A swap of two adjacent letters is one edit.
		{"with two letters swapped", minimalTarget + "timeouts:\n  satble: 5s\n",
			[]string{"line 6: timeouts.satble is not a key; did you mean stable?"}},
		{"with the two letters of a short key swapped", minimalTarget + "properties:\n  - di: P1\n",
			[]string{"line 6: properties[0].di is not a key; did you mean id?"}},
		{"too short to be near", minimalTarget + "properties:\n  - xy: P1\n",
			[]string{"line 6: properties[0].xy is not a key; properties[0] takes id, description, cel and when"}},
		// The decoder matches a key whatever its case.
		{"below a capital", minimalTarget + "Timeouts:\n  setle: 5s\n",
			[]string{"line 6: Timeouts.setle is not a key; did you mean settle?"}},
		{"under a fixture generation may change", minimalTarget + "generate:\n  fixtures:\n    secret.yaml:\n      mutat: [data.token]\n",
			[]string{"line 8: generate.fixtures.secret.yaml.mutat is not a key; did you mean mutate?"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeTarget(t, test.yaml, map[string]string{"widget.yaml": sampleWidget})

			_, err := target.Load(path)

			if err == nil {
				t.Fatal("Load accepted a key target.yaml does not take.")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load returned %q, want %q.", err, want)
				}
			}
		})
	}
}

// clusterScopedCRDs define a cluster-scoped Widget and Gadget and a
// namespaced Thing. Another group's Thing is cluster-scoped.
const clusterScopedCRDs = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: elsewhere.reconciler-fuzzer
  names: {kind: Thing, plural: things}
  scope: Cluster
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: toy.reconciler-fuzzer
  names: {kind: Widget, plural: widgets}
  scope: Cluster
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: toy.reconciler-fuzzer
  names: {kind: Gadget, plural: gadgets}
  scope: Cluster
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
spec:
  group: toy.reconciler-fuzzer
  names: {kind: Thing, plural: things}
  scope: Namespaced
`

// namespacedPrimaryTarget declares a namespaced primary (Thing, defined by
// clusterScopedCRDs) so that cluster-scoped fixture tests are not blocked by
// the primary's scope.
const namespacedPrimaryTarget = `name: min
primary: toy.reconciler-fuzzer/v1/Thing
sample: thing.yaml
crds: [crds/]
launch: {binary: bin/min}
`

const sampleThing = `apiVersion: toy.reconciler-fuzzer/v1
kind: Thing
metadata:
  name: thing
spec:
  count: 1
`

func TestLoadAcceptsAClusterScopedFixture(t *testing.T) {
	path := writeTarget(t, namespacedPrimaryTarget+`fixtures: [ingress-class.yaml]
`, map[string]string{
		"thing.yaml":         sampleThing,
		"crds/toys.yaml":     clusterScopedCRDs,
		"ingress-class.yaml": "apiVersion: networking.k8s.io/v1\nkind: IngressClass\nmetadata:\n  name: default\n",
	})

	loaded, err := target.Load(path)

	if err != nil {
		t.Fatalf("Load refused a cluster-scoped fixture: %v", err)
	}
	if len(loaded.Fixtures) != 1 || loaded.Fixtures[0].GetName() != "default" {
		t.Errorf("Load read %d fixtures, want the IngressClass default.", len(loaded.Fixtures))
	}
	ingressClassGVK := schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass"}
	if !slices.Contains(loaded.ClusterFixtures, ingressClassGVK) {
		t.Errorf("ClusterFixtures is %v, want it to contain IngressClass.", loaded.ClusterFixtures)
	}
}

func TestLoadAcceptsAClusterScopedFixtureDefinedByCRD(t *testing.T) {
	path := writeTarget(t, namespacedPrimaryTarget+`fixtures: [gadget.yaml]
`, map[string]string{
		"thing.yaml":     sampleThing,
		"crds/toys.yaml": clusterScopedCRDs,
		"gadget.yaml":    "apiVersion: toy.reconciler-fuzzer/v1\nkind: Gadget\nmetadata:\n  name: shared\n",
	})

	loaded, err := target.Load(path)

	if err != nil {
		t.Fatalf("Load refused a cluster-scoped fixture whose CRD defines it: %v", err)
	}
	if len(loaded.Fixtures) != 1 || loaded.Fixtures[0].GetName() != "shared" {
		t.Errorf("Load read %d fixtures, want the Gadget shared.", len(loaded.Fixtures))
	}
	gadgetGVK := schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}
	if !slices.Contains(loaded.ClusterFixtures, gadgetGVK) {
		t.Errorf("ClusterFixtures is %v, want it to contain Gadget.", loaded.ClusterFixtures)
	}
}

func TestLoadRefusesEveryClusterScopedPrimaryAndManagedKindButNotFixtures(t *testing.T) {
	path := writeTarget(t, minimalTarget+`crds: [crds/]
manages: [toy.reconciler-fuzzer/v1/Gadget, toy.reconciler-fuzzer/v1/Thing, v1/ConfigMap]
fixtures: [fixtures.yaml]
`, map[string]string{
		"widget.yaml":    sampleWidget,
		"crds/toys.yaml": clusterScopedCRDs,
		"fixtures.yaml":  "apiVersion: toy.reconciler-fuzzer/v1\nkind: Gadget\nmetadata:\n  name: shared-gadget\n---\napiVersion: toy.reconciler-fuzzer/v1\nkind: Thing\nmetadata:\n  name: a-thing\n",
	})

	_, err := target.Load(path)

	if err == nil {
		t.Fatal("Load accepted cluster-scoped primary and managed kinds.")
	}
	for _, want := range []string{"cluster-scoped", "primary toy.reconciler-fuzzer/v1/Widget", "managed toy.reconciler-fuzzer/v1/Gadget"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load returned %q, which does not name %q.", err, want)
		}
	}
	for _, allowed := range []string{"fixture", "Thing", "a-thing", "ConfigMap", "shared-gadget"} {
		if strings.Contains(err.Error(), allowed) {
			t.Errorf("Load returned %q, which names %s.", err, allowed)
		}
	}
}

func TestLoadRefusesBuiltInClusterScopedPrimaryAndManagedButAcceptsFixtures(t *testing.T) {
	path := writeTarget(t, minimalTarget+`crds: [crds/]
manages: [v1/ConfigMap, rbac.authorization.k8s.io/v1/ClusterRole, example.com/v1/ClusterRole]
fixtures: [webhook.yaml]
`, map[string]string{
		"widget.yaml":    sampleWidget,
		"crds/toys.yaml": clusterScopedCRDs,
		"webhook.yaml":   "apiVersion: admissionregistration.k8s.io/v1\nkind: ValidatingWebhookConfiguration\nmetadata:\n  name: widget-validator\n",
	})

	_, err := target.Load(path)

	if err == nil {
		t.Fatal("Load accepted cluster-scoped primary and managed kinds.")
	}
	for _, want := range []string{"primary toy.reconciler-fuzzer/v1/Widget", "managed rbac.authorization.k8s.io/v1/ClusterRole"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load returned %q, which does not name %q.", err, want)
		}
	}
	for _, allowed := range []string{"ConfigMap", "example.com", "fixture", "ValidatingWebhookConfiguration", "widget-validator"} {
		if strings.Contains(err.Error(), allowed) {
			t.Errorf("Load returned %q, which names %s.", err, allowed)
		}
	}
}

func TestLoadRefusesAClusterScopedFixtureThatSetsANamespace(t *testing.T) {
	path := writeTarget(t, minimalTarget+"fixtures: [role.yaml]\n", map[string]string{
		"widget.yaml": sampleWidget,
		"role.yaml":   "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: reader\n  namespace: default\n",
	})

	_, err := target.Load(path)

	if err == nil {
		t.Fatal("Load accepted a cluster-scoped fixture with a namespace.")
	}
	for _, want := range []string{"ClusterRole", "has no namespace", "drop it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load returned %q, which does not say %q.", err, want)
		}
	}
}

func TestLoadRefusesGenerateFixturesNamingAClusterScopedObject(t *testing.T) {
	path := writeTarget(t, minimalTarget+`fixtures: [role.yaml]
generate:
  fixtures:
    role.yaml:
      mutate: []
`, map[string]string{
		"widget.yaml": sampleWidget,
		"role.yaml":   "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: reader\n",
	})

	_, err := target.Load(path)

	if err == nil {
		t.Fatal("Load accepted generate.fixtures naming a cluster-scoped object.")
	}
	for _, want := range []string{"generate.fixtures", "role.yaml", "cluster-scoped"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load returned %q, which does not say %q.", err, want)
		}
	}
}

func TestLoadReadsEveryManifestExtensionOfACRDDirectory(t *testing.T) {
	for _, file := range []string{"crds/widget.json", "crds/widget.yml"} {
		path := writeTarget(t, minimalTarget+"crds: [crds/]\n", map[string]string{
			"widget.yaml": sampleWidget,
			file: `{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
				"spec": {"group": "toy.reconciler-fuzzer", "names": {"kind": "Widget", "plural": "widgets"}, "scope": "Cluster"}}`,
		})

		_, err := target.Load(path)

		if err == nil || !strings.Contains(err.Error(), "primary toy.reconciler-fuzzer/v1/Widget") {
			t.Errorf("Load returned %v, want it to refuse the cluster-scoped Widget %s defines.", err, file)
		}
	}
}

func TestLoadLeavesACRDWithoutAScopeToTheCluster(t *testing.T) {
	path := writeTarget(t, minimalTarget+"crds: [crds/]\n", map[string]string{
		"widget.yaml": sampleWidget,
		"crds/widget.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
			"spec:\n  group: toy.reconciler-fuzzer\n  names: {kind: Widget, plural: widgets}\n",
	})

	if _, err := target.Load(path); err != nil {
		t.Errorf("Load refused a kind whose CRD sets no scope: %v", err)
	}
}

func TestLoadRejectsACRDFileThatIsNotYAML(t *testing.T) {
	path := writeTarget(t, minimalTarget+"crds: [crds/]\n", map[string]string{
		"widget.yaml":   sampleWidget,
		"crds/bad.yaml": "spec: [unterminated\n",
	})

	_, err := target.Load(path)

	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("Load returned %v, want an error naming bad.yaml.", err)
	}
}

func TestLoadRefusesAFixtureOfANamespacedKindThatNamesANamespace(t *testing.T) {
	for _, kind := range []struct{ apiVersion, kind string }{{"v1", "Secret"}, {"toy.reconciler-fuzzer/v1", "Thing"}} {
		path := writeTarget(t, minimalTarget+"crds: [crds/]\nfixtures: [config.yaml, issuer.yaml, more.yaml]\n", map[string]string{
			"widget.yaml": sampleWidget,
			"crds/thing.yaml": "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n" +
				"spec:\n  group: toy.reconciler-fuzzer\n  names: {kind: Thing, plural: things}\n  scope: Namespaced\n",
			"config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n",
			"more.yaml":   "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: more\n",
			"issuer.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: other\n---\n" +
				"apiVersion: " + kind.apiVersion + "\nkind: " + kind.kind + "\nmetadata:\n  name: ca\n  namespace: default\n",
		})

		_, err := target.Load(path)

		want := "fixture " + filepath.Join(filepath.Dir(path), "issuer.yaml") + ": the fixture " + kind.apiVersion + "/" + kind.kind +
			" ca sets metadata.namespace default; drop it," +
			" because reconciler-fuzzer creates fixtures in each run's own namespace, and the target may look for this one in default"
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("Load returned %v, want it to end %q.", err, want)
		}
	}
}

func TestLoadLeavesAFixtureOfAKindItDoesNotKnowToTheCluster(t *testing.T) {
	path := writeTarget(t, minimalTarget+"fixtures: [gadget.yaml]\n", map[string]string{
		"widget.yaml": sampleWidget,
		"gadget.yaml": "apiVersion: toy.reconciler-fuzzer/v1\nkind: Gadget\nmetadata:\n  name: shared\n  namespace: default\n",
	})

	if _, err := target.Load(path); err != nil {
		t.Errorf("Load refused a fixture whose scope only the cluster knows: %v", err)
	}
}

func TestLoadLaunchEnv(t *testing.T) {
	path := writeTarget(t, minimalTargetWithEnv+"    WATCH_NAMESPACE: $NAMESPACE\n    EMPTY: ''\n    NULL_IS_EMPTY:\n    UMASK: '0022'\n    PORT: 8080\n",
		map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected launch.env: %v", err)
	}
	want := map[string]string{"WATCH_NAMESPACE": "$NAMESPACE", "EMPTY": "", "NULL_IS_EMPTY": "", "UMASK": "0022", "PORT": "8080"}
	if !reflect.DeepEqual(loaded.Launch.Env, want) {
		t.Errorf("Load read launch.env %v, want %v.", loaded.Launch.Env, want)
	}
}

func TestLoadDefaults(t *testing.T) {
	path := writeTarget(t, minimalTarget, map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a minimal target: %v", err)
	}

	wantTimeouts := target.Timeouts{Settle: 30 * time.Second, Stable: 10 * time.Second, Delete: 60 * time.Second}
	if loaded.Timeouts != wantTimeouts {
		t.Errorf("Load defaulted timeouts to %+v, want %+v.", loaded.Timeouts, wantTimeouts)
	}
	if want := (target.Thresholds{ErrLoop: 10}); loaded.Thresholds != want {
		t.Errorf("Load defaulted thresholds to %+v, want %+v.", loaded.Thresholds, want)
	}

	// The default Ready predicate of DESIGN.md §6 ignores status.ready.
	for _, tc := range []struct {
		name string
		cr   *unstructured.Unstructured
		want bool
	}{
		{"observed", widgetCR(2, 3, map[string]any{"observedGeneration": int64(2)}), true},
		{"stale", widgetCR(3, 3, map[string]any{"observedGeneration": int64(2)}), false},
		{"unobserved", widgetCR(1, 3, nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loaded.Ready(tc.cr)
			if err != nil {
				t.Fatalf("the default ready predicate returned an error: %v", err)
			}
			if got != tc.want {
				t.Errorf("the default ready predicate returned %v, want %v.", got, tc.want)
			}
		})
	}
}

// A report quotes the ready expression, so the target keeps its text.
func TestLoadKeepsTheReadyExpression(t *testing.T) {
	for _, tc := range []struct {
		name, declared, want string
	}{
		{"declared", "ready: has(status.ready)\n", "has(status.ready)"},
		{"default", "", target.DefaultReady},
		{"hook", "ready: 'go:alwaysReady'\n", "go:alwaysReady"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTarget(t, minimalTarget+tc.declared, map[string]string{"widget.yaml": sampleWidget})

			loaded, err := target.Load(path)

			if err != nil {
				t.Fatalf("Load rejected the target: %v", err)
			}
			if loaded.ReadyExpr != tc.want {
				t.Errorf("Load kept the ready expression %q, want %q.", loaded.ReadyExpr, tc.want)
			}
		})
	}
}

func TestLoadDefaultsEachTimeoutSeparately(t *testing.T) {
	path := writeTarget(t, minimalTarget+"timeouts:\n  stable: 1s\n  delete: 2s\nthresholds: {}\n", map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected a partial timeouts block: %v", err)
	}
	want := target.Timeouts{Settle: 30 * time.Second, Stable: time.Second, Delete: 2 * time.Second}
	if loaded.Timeouts != want {
		t.Errorf("Load read timeouts %+v, want %+v.", loaded.Timeouts, want)
	}
	if loaded.Thresholds.ErrLoop != 10 {
		t.Errorf("Load read errloop %d, want the default 10.", loaded.Thresholds.ErrLoop)
	}
}

// A threshold the target leaves out takes its default.
func TestLoadThresholds(t *testing.T) {
	defaults := target.DefaultThresholds
	t.Cleanup(func() { target.DefaultThresholds = defaults })
	target.DefaultThresholds = target.Thresholds{ErrLoop: 9, Quiet: 4}
	for _, tc := range []struct {
		declared string
		want     target.Thresholds
	}{
		{"thresholds:\n  quiet: 3\n", target.Thresholds{ErrLoop: 9, Quiet: 3}},
		{"thresholds:\n  quiet: 0\n  errloop: 7\n", target.Thresholds{ErrLoop: 7, Quiet: 0}},
		{"thresholds:\n  errloop: 7\n", target.Thresholds{ErrLoop: 7, Quiet: 4}},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			path := writeTarget(t, minimalTarget+tc.declared, map[string]string{"widget.yaml": sampleWidget})

			loaded, err := target.Load(path)

			if err != nil {
				t.Fatalf("Load rejected the thresholds: %v", err)
			}
			if loaded.Thresholds != tc.want {
				t.Errorf("Load read thresholds %+v, want %+v.", loaded.Thresholds, tc.want)
			}
		})
	}
}

func TestLoadNotRecreated(t *testing.T) {
	path := writeTarget(t, minimalTarget+`manages:
  - v1/Secret
  - cert-manager.io/v1/CertificateRequest
notRecreated:
  - cert-manager.io/v1/CertificateRequest
`, map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)

	if err != nil {
		t.Fatalf("Load rejected notRecreated: %v", err)
	}
	want := []schema.GroupVersionKind{{Group: "cert-manager.io", Version: "v1", Kind: "CertificateRequest"}}
	if !reflect.DeepEqual(loaded.NotRecreated, want) {
		t.Errorf("Load read notRecreated %v, want %v.", loaded.NotRecreated, want)
	}
}

const secretFixtures = `apiVersion: v1
kind: Secret
metadata:
  name: token
data:
  token: czNjcjN0
  app.properties: YT1i
---
apiVersion: v1
kind: Secret
metadata:
  name: other
data:
  token: b3RoZXI=
  app.properties: Yz1k
`

func TestLoadReadsTheFixturesGenerationMayChange(t *testing.T) {
	path := writeTarget(t, minimalTarget+`fixtures: [issuer.yaml, secrets.yaml]
generate:
  fixtures:
    secrets.yaml:
      mutate:
        - data.token
        - data["app.properties"]
`, map[string]string{
		"widget.yaml":  sampleWidget,
		"issuer.yaml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: issuer\n",
		"secrets.yaml": secretFixtures,
	})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected generate.fixtures: %v", err)
	}
	var read []string
	for _, fixture := range loaded.Generate.Fixtures {
		for _, mutable := range fixture.Mutate {
			read = append(read, fmt.Sprintf("%s %s %s", fixture.GVK.Kind, fixture.Name, mutable))
		}
	}
	want := []string{
		"Secret token data.token", `Secret token data["app.properties"]`,
		"Secret other data.token", `Secret other data["app.properties"]`,
	}
	if !slices.Equal(read, want) {
		t.Errorf("Load read the fixture paths %q, want %q.", read, want)
	}
}

func TestLoadRejectsAFixtureGenerationCannotChange(t *testing.T) {
	for _, test := range []struct {
		name, generate, fixtures, want string
	}{
		{"a file fixtures does not list", "    absent.yaml: {}\n", secretFixtures,
			"generate.fixtures absent.yaml: fixtures lists no such file"},
		{"a path that holds no string", "    secrets.yaml:\n      mutate: [data.tokne]\n", secretFixtures,
			"generate.fixtures secrets.yaml: the v1/Secret token holds no string at data.tokne"},
		{"a path to a map", "    secrets.yaml:\n      mutate: [data]\n", secretFixtures,
			"generate.fixtures secrets.yaml: the v1/Secret token holds no string at data"},
		{"every value of a map", "    secrets.yaml:\n      mutate: ['data[*]']\n", secretFixtures,
			`generate.fixtures secrets.yaml: mutate "data[*]": name one string, not [*]`},
		{"a malformed path", "    secrets.yaml:\n      mutate: ['data[0]']\n", secretFixtures,
			`generate.fixtures secrets.yaml: mutate "data[0]": offset 4`},
		{"a fixture with no name", "    secrets.yaml: {}\n", "apiVersion: v1\nkind: Secret\nmetadata:\n  generateName: token-\n",
			"generate.fixtures secrets.yaml: a v1/Secret there sets no metadata.name, and a fixture op names its fixture"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeTarget(t, minimalTarget+"fixtures: [secrets.yaml]\ngenerate:\n  fixtures:\n"+test.generate,
				map[string]string{"widget.yaml": sampleWidget, "secrets.yaml": test.fixtures})

			_, err := target.Load(path)

			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Load returned %v, want an error saying %q.", err, test.want)
			}
		})
	}
}

func TestLoadGenerateAndSelector(t *testing.T) {
	ignored := []string{
		"status.lastSyncTime",
		`metadata.annotations["probe.example.com/started-at"]`,
		"status.conditions[*].lastHeartbeatTime",
		`["top.level"].x`,
	}
	path := writeTarget(t, minimalTarget+`selector: app=widget,tier in (a,b)
equalIgnore:
  - `+ignored[0]+`
  - `+ignored[1]+`
  - `+ignored[2]+`
  - '`+ignored[3]+`'
generate:
  mutate: [spec.count]
  overlay:
    spec.count: {minimum: 1, maximum: 3}
  maxCRs: 2
  distinct: [spec.secretName]
`, map[string]string{"widget.yaml": sampleWidget})

	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected generate, selector and equalIgnore: %v", err)
	}
	if loaded.Selector == nil || !loaded.Selector.Matches(labels.Set{"app": "widget", "tier": "a"}) {
		t.Errorf("Load read selector %v, which does not match app=widget,tier=a.", loaded.Selector)
	}
	var read []string
	for _, path := range loaded.EqualIgnore {
		read = append(read, path.String())
	}
	if !reflect.DeepEqual(read, ignored) {
		t.Errorf("Load read equalIgnore %q, want %q.", read, ignored)
	}
	if want := []string{"spec.count"}; !reflect.DeepEqual(loaded.Generate.Mutate, want) {
		t.Errorf("Load read generate.mutate %v, want %v.", loaded.Generate.Mutate, want)
	}
	overlay, err := json.Marshal(loaded.Generate.Overlay["spec.count"])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"maximum":3,"minimum":1}`; string(overlay) != want {
		t.Errorf("Load read the overlay of spec.count as %s, want %s.", overlay, want)
	}
	if loaded.Generate.MaxCRs != 2 || !reflect.DeepEqual(loaded.Generate.Distinct, []string{"spec.secretName"}) {
		t.Errorf("Load read generate.maxCRs %d and generate.distinct %v, want 2 and [spec.secretName].",
			loaded.Generate.MaxCRs, loaded.Generate.Distinct)
	}
}

func TestLoadFaultsOff(t *testing.T) {
	path := writeTarget(t, minimalTarget+"generate:\n  faults: false\n", map[string]string{"widget.yaml": sampleWidget})
	loaded, err := target.Load(path)
	if err != nil {
		t.Fatalf("Load rejected generate.faults: false: %v", err)
	}
	if !loaded.Generate.NoFaults {
		t.Error("Load left NoFaults false for a target with generate.faults: false.")
	}
}

func TestLoadFaultsDefaultsToOn(t *testing.T) {
	toy := loadToy(t)
	if toy.Generate.NoFaults {
		t.Error("Load set NoFaults on the toy target, which declares no generate.faults.")
	}
}
