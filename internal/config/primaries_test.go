package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

const twoPrimaries = `launch:
  binary: bin/controller
  rbac: [role.yaml]
crds: [crd.yaml]
primaries:
  - kind: toy/v1/Cluster
    sample: sample.yaml
    ready: self.status.ready
    retain: has(self.keep)
    halted: has(self.halted)
    periodic: {every: 1m, paths: [status.at]}
    owns: [{kind: v1/Secret, name: self.metadata.name}]
    values: {overlay: {spec.size: {maximum: 3}}, distinct: [spec.name], mutate: [spec.size]}
  - {kind: toy/v1/Database, sample: sample.yaml, ready: "false"}
fixtures: [fixture.yaml]
`

func TestPrimariesLoadEachEntry(t *testing.T) {
	t.Skip("enabled by the config package")
	c, err := config.Load(writeConfig(t, twoPrimaries))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Primaries) != 2 {
		t.Fatalf("Primaries = %+v, want 2 entries.", c.Primaries)
	}
	cluster, database := c.Primaries[0], c.Primaries[1]
	if cluster.Kind != (schema.GroupVersionKind{Group: "toy", Version: "v1", Kind: "Cluster"}) || database.Kind.Kind != "Database" {
		t.Errorf("kinds = %v and %v, want Cluster then Database.", cluster.Kind, database.Kind)
	}
	if cluster.Retain != "has(self.keep)" || cluster.Halted != "has(self.halted)" || cluster.Periodic == nil || cluster.Periodic.Every != time.Minute ||
		len(cluster.Owns) != 1 || cluster.Owns[0].Kind.Kind != "Secret" || !strings.HasSuffix(cluster.Sample, "/sample.yaml") {
		t.Errorf("Cluster entry = %+v", cluster)
	}
	if !slices.Equal(cluster.Values.Mutate, []string{"spec.size"}) || !slices.Equal(cluster.Values.Distinct, []string{"spec.name"}) ||
		cluster.Values.Overlay["spec.size"]["maximum"] != float64(3) {
		t.Errorf("Cluster values = %+v", cluster.Values)
	}
	if database.Retain != "" || database.Periodic != nil || len(database.Values.Mutate) != 0 {
		t.Errorf("Database entry = %+v, want none of Cluster's keys.", database)
	}
	want := []schema.GroupKind{{Group: "toy", Kind: "Cluster"}, {Group: "toy", Kind: "Database"}}
	if got := c.ListedKinds(); !slices.Equal(got, want) {
		t.Errorf("ListedKinds = %v, want %v.", got, want)
	}
}

func TestReadyOfSeveralPrimariesTakesTheCRsKind(t *testing.T) {
	t.Skip("enabled by the config package")
	c, err := config.Load(writeConfig(t, twoPrimaries))
	if err != nil {
		t.Fatal(err)
	}
	cr := func(kind string) map[string]any {
		return map[string]any{"apiVersion": "toy/v1", "kind": kind, "status": map[string]any{"ready": true}}
	}
	if ready, err := c.Ready(cr("Cluster")); !ready || err != nil {
		t.Errorf("Ready(Cluster) = %v, %v; want true.", ready, err)
	}
	if ready, err := c.Ready(cr("Database")); ready || err != nil {
		t.Errorf("Ready(Database) = %v, %v; want false.", ready, err)
	}
	if _, err := c.Ready(cr("User")); err == nil {
		t.Error("Ready(User) gives no error, though primaries lists no User.")
	}
	if keep, err := c.Retain(map[string]any{"apiVersion": "toy/v1", "kind": "Database", "keep": true}); keep || err != nil {
		t.Errorf("Retain(Database) = %v, %v; want false, as only Cluster sets retain.", keep, err)
	}
}

func TestPrimaryIsOneEntryWithTheTopLevelValues(t *testing.T) {
	t.Skip("enabled by the config package")
	values := "values: {overlay: {spec.size: {maximum: 3}}, mutate: [spec.size], fixtures: {fixture.yaml: [data.label]}}\n"
	short, err := config.Load(writeConfig(t, minimal(t)+values))
	if err != nil {
		t.Fatal(err)
	}
	long, err := config.Load(writeConfig(t, `launch:
  binary: bin/controller
  rbac: [role.yaml]
crds: [crd.yaml]
primaries:
  - kind: v1/ConfigMap
    sample: sample.yaml
    ready: "true"
    values: {overlay: {spec.size: {maximum: 3}}, mutate: [spec.size]}
fixtures: [fixture.yaml]
values: {fixtures: {fixture.yaml: [data.label]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(short.Primaries) != 1 || len(long.Primaries) != 1 {
		t.Fatalf("Primaries = %+v and %+v, want one entry each.", short.Primaries, long.Primaries)
	}
	s, l := short.Primaries[0], long.Primaries[0]
	if s.Kind != l.Kind || s.Ready != l.Ready || filepath.Base(s.Sample) != filepath.Base(l.Sample) || !reflect.DeepEqual(s.Values, l.Values) {
		t.Errorf("primary loads as %+v, primaries as %+v; want the same entry.", s, l)
	}
}

func TestPrimariesRefusals(t *testing.T) {
	t.Skip("enabled by the config package")
	entry := "  - {kind: toy/v1/Cluster, sample: sample.yaml, ready: \"true\"}\n"
	head := "launch: {binary: bin/controller, rbac: [role.yaml]}\ncrds: [crd.yaml]\n"
	for name, c := range map[string]struct{ config, want string }{
		"a kind listed twice":       {head + "primaries:\n" + entry + "  - {kind: toy/v2/Cluster, sample: sample.yaml, ready: \"true\"}\n", "primaries[1].kind"},
		"an entry without ready":    {head + "primaries:\n" + entry + "  - {kind: toy/v1/Database, sample: sample.yaml}\n", "primaries[1].ready is required"},
		"primary and primaries":     {head + "primaries:\n" + entry + "primary: {kind: toy/v1/Database, sample: sample.yaml, ready: \"true\"}\n", "not both"},
		"top-level mutate":          {head + "primaries:\n" + entry + "values: {mutate: [spec.size]}\n", "values.mutate"},
		"values inside primary":     {head + "primary: {kind: toy/v1/Cluster, sample: sample.yaml, ready: \"true\", values: {mutate: [spec.size]}}\n", "primary.values"},
		"a model of a listed kind":  {head + "primaries:\n" + entry + "models: [{kind: toy/v1/Cluster, after: 1s}]\n", "models[0].kind"},
		"an owns entry's own index": {head + "primaries:\n" + entry + "  - {kind: toy/v1/Database, sample: sample.yaml, ready: \"true\", owns: [{kind: v1/Secret}]}\n", "primaries[1].owns[0].name"},
	} {
		if _, err := config.Load(writeConfig(t, c.config)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Load = %v, want an error that names %s.", name, err, c.want)
		}
	}
}

func TestAFixtureOfAListedKindFails(t *testing.T) {
	t.Skip("enabled by the config package")
	path := writeConfig(t, twoPrimaries)
	fixture := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\n---\napiVersion: toy/v1\nkind: Database\nmetadata: {name: db}\n"
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "fixture.yaml"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "fixture.yaml") || !strings.Contains(err.Error(), "Database") {
		t.Errorf("Load = %v, want an error that names fixture.yaml and Database.", err)
	}
}
