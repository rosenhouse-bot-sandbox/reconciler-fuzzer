package examples_test

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/generate"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestACKS3PlansLoad(t *testing.T) {
	load(t, "ack-s3")
	paths, _ := filepath.Glob(filepath.Join("ack-s3", "*", "*.json"))
	for _, name := range []string{"plans/create.json", "plans/tags.json", "plans/delete.json", "plans/damage.json", "controls/retain.json"} {
		if !slices.Contains(paths, filepath.Join("ack-s3", name)) {
			t.Errorf("ack-s3 has no %s", name)
		}
	}
	for _, path := range paths {
		if _, err := plan.Load(path); err != nil {
			t.Error(err)
		}
	}
}

func TestBucketReady(t *testing.T) {
	bucket := func(specWritten string, conditions ...map[string]any) map[string]any {
		obj := cr(map[string]any{"conditions": anys(conditions)})
		obj["metadata"].(map[string]any)["managedFields"] = []any{
			map[string]any{"manager": "reconciler-fuzzer", "time": specWritten, "fieldsV1": map[string]any{"f:spec": map[string]any{}}},
			map[string]any{"manager": "ack-s3-controller", "time": "2026-10-21T10:00:09Z", "subresource": "status",
				"fieldsV1": map[string]any{"f:status": map[string]any{}}},
		}
		return obj
	}
	synced := func(status, stamped string) map[string]any {
		return map[string]any{"type": "ACK.ResourceSynced", "status": status, "lastTransitionTime": stamped}
	}
	requireReady(t, load(t, "ack-s3"), readiness{
		"no status":                    {cr(nil), false},
		"not synced":                   {bucket("2026-10-21T10:00:00Z", synced("False", "2026-10-21T10:00:05Z")), false},
		"synced after the spec":        {bucket("2026-10-21T10:00:00Z", synced("True", "2026-10-21T10:00:05Z")), true},
		"synced two seconds later":     {bucket("2026-10-21T10:00:03Z", synced("True", "2026-10-21T10:00:05Z")), true},
		"synced the next second":       {bucket("2026-10-21T10:00:04Z", synced("True", "2026-10-21T10:00:05Z")), false},
		"synced in the spec's second":  {bucket("2026-10-21T10:00:05Z", synced("True", "2026-10-21T10:00:05Z")), false},
		"synced before the spec":       {bucket("2026-10-21T10:00:06Z", synced("True", "2026-10-21T10:00:05Z")), false},
		"only Ready":                   {bucket("2026-10-21T10:00:00Z", map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": "2026-10-21T10:00:05Z"}), false},
		"synced without a stamp":       {bucket("2026-10-21T10:00:00Z", map[string]any{"type": "ACK.ResourceSynced", "status": "True"}), false},
		"synced, and no spec recorded": {cr(map[string]any{"conditions": anys([]map[string]any{synced("True", "2026-10-21T10:00:05Z")})}), false},
	})
}

func anys(maps []map[string]any) []any {
	var list []any
	for _, m := range maps {
		list = append(list, m)
	}
	return list
}

func TestBucketMatchesSpec(t *testing.T) {
	c := load(t, "ack-s3")
	i := slices.IndexFunc(c.Properties, func(p config.Property) bool { return p.Name == "bucket-matches-spec" })
	if i < 0 {
		t.Fatal("ack-s3 has no property bucket-matches-spec")
	}
	program, err := config.CompileProperty(c.Properties[i].Expr)
	if err != nil {
		t.Fatal(err)
	}
	holds, err := program.Bind(nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := map[string]any{"spec": map[string]any{
		"name":       "rf-a",
		"tagging":    map[string]any{"tagSet": []any{map[string]any{"key": "team", "value": "a"}, map[string]any{"key": "empty"}}},
		"versioning": map[string]any{"status": "Enabled"},
	}}
	bucket := func(name, versioning string, tags map[string]any) map[string]map[string][]any {
		return map[string]map[string][]any{"aws": {"buckets": {map[string]any{"name": name, "versioning": versioning, "tags": tags}}}}
	}
	tags := map[string]any{"team": "a", "empty": "", "services.k8s.aws/namespace": "ns"}
	for name, tc := range map[string]struct {
		crs   []any
		world map[string]map[string][]any
		want  bool
	}{
		"no CR":              {nil, bucket("rf-b", "", map[string]any{}), true},
		"a matching bucket":  {[]any{spec}, bucket("rf-a", "Enabled", tags), true},
		"no bucket":          {[]any{spec}, bucket("rf-b", "Enabled", tags), false},
		"a stale tag":        {[]any{spec}, bucket("rf-a", "Enabled", map[string]any{"team": "b", "empty": ""}), false},
		"a missing tag":      {[]any{spec}, bucket("rf-a", "Enabled", map[string]any{"team": "a"}), false},
		"versioning not set": {[]any{spec}, bucket("rf-a", "", tags), false},
	} {
		if got, err := holds(tc.crs, tc.world); got != tc.want || err != nil {
			t.Errorf("%s: holds = %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// TestSomeACKS3SeedDamagesTheBucket holds the seeds file to a seed that
// deletes a bucket behind the controller's back.
func TestSomeACKS3SeedDamagesTheBucket(t *testing.T) {
	g, err := generate.New(load(t, "ack-s3"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("ack-s3", "seeds"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(data)) {
		seed, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(p.Steps, func(s plan.Step) bool {
			return slices.ContainsFunc(s.Actions, func(a plan.Action) bool { return a.Do == plan.Damage && a.Target != nil && a.Target.System == "aws" })
		}) {
			return
		}
	}
	t.Error("No seed in ack-s3/seeds damages an aws bucket.")
}
