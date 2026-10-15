package target_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

var each = target.Step{Each: true}

func TestParsePath(t *testing.T) {
	for _, tc := range []struct {
		text, canonical string
		want            target.Path
	}{
		{"status.lastSyncTime", "", target.Path{{Key: "status"}, {Key: "lastSyncTime"}}},
		{`metadata.annotations["probe.example.com/started-at"]`, "",
			target.Path{{Key: "metadata"}, {Key: "annotations"}, {Key: "probe.example.com/started-at"}}},
		{"metadata.annotations.note", "", target.Path{{Key: "metadata"}, {Key: "annotations"}, {Key: "note"}}},
		{"status.annotations.pending.count", "",
			target.Path{{Key: "status"}, {Key: "annotations"}, {Key: "pending"}, {Key: "count"}}},
		{`spec.template.metadata.labels["app.kubernetes.io/name"]`, "",
			target.Path{{Key: "spec"}, {Key: "template"}, {Key: "metadata"}, {Key: "labels"}, {Key: "app.kubernetes.io/name"}}},
		{`data["say \"hi\""]`, "", target.Path{{Key: "data"}, {Key: `say "hi"`}}},
		{`data["*"]`, "", target.Path{{Key: "data"}, {Key: "*"}}},
		{`["a.b"].c`, "", target.Path{{Key: "a.b"}, {Key: "c"}}},
		{"status.conditions[*].lastHeartbeatTime", "",
			target.Path{{Key: "status"}, {Key: "conditions"}, each, {Key: "lastHeartbeatTime"}}},
		{"spec.ports[*]", "", target.Path{{Key: "spec"}, {Key: "ports"}, each}},
		{"spec.grid[*][*]", "", target.Path{{Key: "spec"}, {Key: "grid"}, each, each}},
		{`metadata["name"]`, "metadata.name", target.Path{{Key: "metadata"}, {Key: "name"}}},
		{`data["été"]`, "data.été", target.Path{{Key: "data"}, {Key: "été"}}},
		{`spec.resources.limits["nvidia.com/gpu"]`, "",
			target.Path{{Key: "spec"}, {Key: "resources"}, {Key: "limits"}, {Key: "nvidia.com/gpu"}}},
		{`metadata.annotations["example/note"]`, "", target.Path{{Key: "metadata"}, {Key: "annotations"}, {Key: "example/note"}}},
		{`data["a b"]`, "", target.Path{{Key: "data"}, {Key: "a b"}}},
		{`data["a.b&c"]`, "", target.Path{{Key: "data"}, {Key: "a.b&c"}}},
		{`data["b:"]`, "", target.Path{{Key: "data"}, {Key: "b:"}}},
		{"metadata.labels[*]", "", target.Path{{Key: "metadata"}, {Key: "labels"}, each}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			got, err := target.ParsePath(tc.text)
			if err != nil {
				t.Fatalf("ParsePath rejected it: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParsePath read %#v, want %#v.", got, tc.want)
			}
			canonical := tc.canonical
			if canonical == "" {
				canonical = tc.text
			}
			if got.String() != canonical {
				t.Errorf("The path prints as %s, want %s.", got, canonical)
			}
		})
	}
}

func TestParsePathRejects(t *testing.T) {
	for _, tc := range []struct {
		text  string
		wants []string
	}{
		{"", []string{"offset 0", "want a key"}},
		{".status", []string{"offset 0", "want a key"}},
		{"status..x", []string{"offset 7", "want a key"}},
		{"status.", []string{"offset 7", "want a key"}},
		{"status.[*]", []string{"offset 7", "want a key"}},
		{`data["x`, []string{"offset 4", "not closed"}},
		{`data["x"`, []string{"offset 8", "]"}},
		{`data["x"]y`, []string{"offset 9", "want . or ["}},
		{`data["\q"]`, []string{"offset 4"}},
		{`data[x]`, []string{"offset 4", `[*]`, `["key"]`}},
		{`data['x']`, []string{"offset 4", `["key"]`}},
		{"status.conditions[0].message", []string{"offset 17", "[*]", "position"}},
		{"status.conditions.*.message", []string{"offset 18", "[*]", "block-style list"}},
		{"data.a]b", []string{"offset 5", `["a]b"]`, "block-style list"}},
		{`data.a"b`, []string{"offset 5", `["a\"b"]`}},
		{"[*].x", []string{"offset 0", "key"}},
		{"metadata.annotations.probe.example.com/started-at",
			[]string{"offset 21", `metadata.annotations["probe.example.com/started-at"]`, "block-style list"}},
		{"metadata.labels.app.kubernetes.io/name", []string{"offset 16", `metadata.labels["app.kubernetes.io/name"]`}},
		{"spec.template.metadata.annotations.kubectl.kubernetes.io/restartedAt",
			[]string{"offset 35", `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]`}},
		{`metadata.annotations["note"].x`, []string{"offset 20", "one step below"}},
		{"metadata.labels[*].x", []string{"offset 15", "one step below"}},
		{"metadata.labels.foo[*]", []string{"offset 16", "one step below"}},
		{"metadata.labels.foo[", []string{"offset 16", "one step below"}},
		{"metadata.labels.a b", []string{"offset 16", `metadata.labels["a b"]`}},
		{"data.b:", []string{"offset 5", `data["b:"]`}},
		{"metadata.annotations.example/note", []string{"offset 21", `metadata.annotations["example/note"]`}},
		{"metadata.annotations.config.yaml", []string{"offset 21", `metadata.annotations["config.yaml"]`}},
		{"spec.selector.matchLabels.app.kubernetes.io/name", []string{"offset 41", "io/name", "whole key", "block-style list"}},
		{"status. lastSyncTime", []string{"offset 7", `status[" lastSyncTime"]`}},
		{"status.last\tSyncTime", []string{"offset 7", `status["last\tSyncTime"]`}},
		{"metadata.labels.", []string{"offset 16", "want a key"}},
		{`data["été"]x`, []string{"offset 11"}},
	} {
		t.Run(tc.text, func(t *testing.T) {
			parsed, err := target.ParsePath(tc.text)
			if err == nil || parsed != nil {
				t.Fatalf("ParsePath returned %#v and %v, want only an error.", parsed, err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("ParsePath reported %q, which does not say %q.", err, want)
				}
			}
		})
	}
}

func TestMustParsePathPanicsOnABadPath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustParsePath accepted a path ParsePath rejects.")
		}
	}()

	target.MustParsePath("status..x")
}

// A path that an object's fields could hold parses as it prints, whatever its
// keys hold.
func TestAPathParsesAsItPrints(t *testing.T) {
	awkward := rapid.SampledFrom([]string{"", ".", "[", "]", `"`, `\`, "*", "/", " ", "a b", "\t", " ", "[*]", `["x"]`, "a.b/c", "0", "\x00", "<&>", "été",
		":", "metadata", "labels", "annotations"})
	key := rapid.OneOf(awkward, rapid.String())
	rapid.Check(t, func(rt *rapid.T) {
		path := target.Path{{Key: key.Draw(rt, "first")}}
		for range rapid.IntRange(0, 5).Draw(rt, "steps") {
			if belowAStringKey(path) {
				break
			}
			if rapid.Bool().Draw(rt, "each") {
				path = append(path, each)
			} else {
				path = append(path, target.Step{Key: key.Draw(rt, "key")})
			}
		}

		parsed, err := target.ParsePath(path.String())
		if err != nil {
			rt.Fatalf("ParsePath rejected %s, which %#v printed: %v", path, path, err)
		}
		if !reflect.DeepEqual(parsed, path) {
			rt.Fatalf("%#v printed as %s, which parses as %#v.", path, path, parsed)
		}
	})
}

// belowAStringKey reports whether path ends one step below the labels or the
// annotations of some metadata, whose values are strings.
func belowAStringKey(path target.Path) bool {
	n := len(path)
	return n >= 3 && path[n-3] == target.Step{Key: "metadata"} &&
		(path[n-2] == target.Step{Key: "labels"} || path[n-2] == target.Step{Key: "annotations"})
}

func TestAnEmptyPathRemovesNothing(t *testing.T) {
	object := decode(t, `{"metadata": {"name": "w"}}`)

	if err := (target.Path{}).Remove(object); err != nil {
		t.Errorf("Removing an empty path reported %v.", err)
	}

	if want := decode(t, `{"metadata": {"name": "w"}}`); !reflect.DeepEqual(object, want) {
		t.Errorf("Removing an empty path left %v, want %v.", object, want)
	}
}

func TestPathRemove(t *testing.T) {
	for _, tc := range []struct {
		name, path, object, want string
	}{
		{"a key that holds a dot", `metadata.annotations["a.b/c"]`,
			`{"metadata": {"annotations": {"a.b/c": "1", "a": "2"}}}`,
			`{"metadata": {"annotations": {"a": "2"}}}`},
		{"the map it empties", `metadata.annotations["a.b/c"]`,
			`{"metadata": {"name": "w", "annotations": {"a.b/c": "1"}}}`,
			`{"metadata": {"name": "w"}}`},
		{"a field of every item", "status.conditions[*].lastHeartbeatTime",
			`{"status": {"conditions": [{"type": "A", "lastHeartbeatTime": "1"}, {"type": "B", "lastHeartbeatTime": "2"}]}}`,
			`{"status": {"conditions": [{"type": "A"}, {"type": "B"}]}}`},
		{"no item it empties", "spec.items[*].x",
			`{"spec": {"items": [{"x": 1}, {"x": 2, "y": 3}]}}`,
			`{"spec": {"items": [{}, {"y": 3}]}}`},
		{"every item", "spec.ports[*]",
			`{"spec": {"ports": [1, 2], "replicas": 1}}`,
			`{"spec": {"replicas": 1}}`},
		{"every item of every inner list", "spec.grid[*][*]",
			`{"spec": {"grid": [[1, 2], [3]]}}`,
			`{"spec": {"grid": [[], []]}}`},
		{"an empty map along the path, whether or not it emptied it", "spec.volumes[*].emptyDir.sizeLimit",
			`{"spec": {"volumes": [{"name": "a", "emptyDir": {}}, {"name": "b", "emptyDir": {"sizeLimit": "1Gi"}}]}}`,
			`{"spec": {"volumes": [{"name": "a"}, {"name": "b"}]}}`},
		{"an empty list along the path", "status.conditions[*].lastHeartbeatTime",
			`{"status": {"conditions": [], "ready": 1}}`,
			`{"status": {"ready": 1}}`},
		{"every value of a map", "metadata.labels[*]",
			`{"metadata": {"name": "w", "labels": {"a": "1", "b": "2"}}}`,
			`{"metadata": {"name": "w"}}`},
		{"a field of every value, even one it empties", "status.outputs[*].time",
			`{"status": {"outputs": {"a": {"time": 1, "x": 1}, "b": {"time": 2}}}}`,
			`{"status": {"outputs": {"a": {"x": 1}, "b": {}}}}`},
		{"every item of every list in a map", "spec.groups[*][*]",
			`{"spec": {"groups": {"a": [1], "b": [2, 3]}}}`,
			`{"spec": {"groups": {"a": [], "b": []}}}`},
		{"every value of every map in a list", "spec.rows[*][*]",
			`{"spec": {"rows": [{"a": 1}, {"b": 2}]}}`,
			`{"spec": {"rows": [{}, {}]}}`},
		{"every value of every map in a map", "status.outputs[*][*]",
			`{"status": {"outputs": {"x": {"a": 1}, "y": {"b": 2}}}}`,
			`{"status": {"outputs": {"x": {}, "y": {}}}}`},
		{"nothing below a string", "data.a.b",
			`{"data": {"a": "1"}}`,
			`{"data": {"a": "1"}}`},
		{"nothing below a null", "status.a.b",
			`{"status": {"a": null}}`,
			`{"status": {"a": null}}`},
		{"nothing that is not there", "status.missing",
			`{"status": {"ready": 1}}`,
			`{"status": {"ready": 1}}`},
		{"an empty map where the path names nothing", "status.missing",
			`{"status": {}, "spec": {}}`,
			`{"spec": {}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object, want := decode(t, tc.object), decode(t, tc.want)

			if err := target.MustParsePath(tc.path).Remove(object); err != nil {
				t.Errorf("Removing %s reported %v.", tc.path, err)
			}

			if !reflect.DeepEqual(object, want) {
				t.Errorf("Removing %s left %v, want %v.", tc.path, object, want)
			}
		})
	}
}

// A key names nothing in a list, so Remove says where [*] belongs, and
// removes whatever else the path names.
func TestPathRemoveReportsAKeyThatMeetsAList(t *testing.T) {
	for _, tc := range []struct {
		name, path, object, want, err string
	}{
		{"a list of maps", "status.conditions.lastHeartbeatTime",
			`{"status": {"conditions": [{"type": "A", "lastHeartbeatTime": "1"}]}}`,
			`{"status": {"conditions": [{"type": "A", "lastHeartbeatTime": "1"}]}}`,
			"status.conditions is a list; write status.conditions[*].lastHeartbeatTime; a path with brackets goes in a block-style list"},
		{"an empty list, which counts as absent", "status.conditions.type",
			`{"status": {"conditions": [], "ready": 1}}`,
			`{"status": {"ready": 1}}`,
			"status.conditions is a list; write status.conditions[*].type; a path with brackets goes in a block-style list"},
		{"a list in one item of several", "spec.groups[*].members.name",
			`{"spec": {"groups": [{"members": [{"name": "a"}]}, {"members": {"name": "b"}}]}}`,
			`{"spec": {"groups": [{"members": [{"name": "a"}]}, {}]}}`,
			"spec.groups[*].members is a list; write spec.groups[*].members[*].name; a path with brackets goes in a block-style list"},
		{"lists at different steps, of which the first", "spec.groups[*].members.name.first",
			`{"spec": {"groups": [{"members": [{"name": "a"}]}, {"members": {"name": [{"first": "b"}]}}]}}`,
			`{"spec": {"groups": [{"members": [{"name": "a"}]}, {"members": {"name": [{"first": "b"}]}}]}}`,
			"spec.groups[*].members is a list; write spec.groups[*].members[*].name.first; a path with brackets goes in a block-style list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object, want := decode(t, tc.object), decode(t, tc.want)

			err := target.MustParsePath(tc.path).Remove(object)

			if err == nil || err.Error() != tc.err {
				t.Errorf("Removing %s reported %v, want %q.", tc.path, err, tc.err)
			}
			if !reflect.DeepEqual(object, want) {
				t.Errorf("Removing %s left %v, want %v.", tc.path, object, want)
			}
		})
	}
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		t.Fatal(err)
	}
	return object
}
