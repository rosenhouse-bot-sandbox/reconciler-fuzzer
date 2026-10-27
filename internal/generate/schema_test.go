package generate

import (
	"slices"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func primarySchemaOf(t *testing.T, path string) *schema {
	t.Helper()
	cfg := loadConfig(t, path)
	crds, err := readManifests(cfg.CRDs)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := primaryCRD(crds, cfg.Primaries[0].Kind)
	if err != nil {
		t.Fatal(err)
	}
	read, err := overlaid(root, cfg.Primaries[0].Values.Overlay)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func dottedOf(fields []field) []string {
	var paths []string
	for _, f := range fields {
		paths = append(paths, f.dotted)
	}
	return paths
}

func TestSchemaBoundsTheToysCount(t *testing.T) {
	count := primarySchemaOf(t, toyConfig).Properties["spec"].Properties["count"]
	if count == nil || count.Type != "integer" || *count.Minimum != 0 || *count.Maximum != 10 {
		t.Errorf("spec.count is %+v, want an integer from 0 to 10.", count)
	}
}

func TestPathsTheSchemaDoesNotDescribeAreConfigurationErrors(t *testing.T) {
	for name, declare := range map[string]func(*config.Values){
		"mutate":  func(v *config.Values) { v.Mutate = []string{"spec.nonesuch"} },
		"overlay": func(v *config.Values) { v.Overlay = map[string]map[string]any{"spec.nonesuch": {"maximum": 3}} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := loadConfig(t, toyConfig)
			declare(&cfg.Primaries[0].Values)
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "spec.nonesuch") {
				t.Errorf("New returned %v, want an error naming spec.nonesuch.", err)
			}
		})
	}
}

func TestAnOverlayKeywordReconcilerFuzzerDoesNotReadIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		path, dotted string
		overlay      map[string]any
		unread       string
	}{
		{toyConfig, "spec.count", map[string]any{"maximun": 0}, "maximun"},
		{rulesConfig, "spec.tags", map[string]any{"items": map[string]any{"patern": "^a$"}}, "items.patern"},
		{rulesConfig, "spec.config", map[string]any{"additionalProperties": map[string]any{"maxLenght": 3}},
			"additionalProperties.maxLenght"},
	} {
		t.Run(testCase.unread, func(t *testing.T) {
			cfg := loadConfig(t, testCase.path)
			cfg.Primaries[0].Values.Overlay = map[string]map[string]any{testCase.dotted: testCase.overlay}
			_, err := New(cfg)
			if err == nil {
				t.Fatalf("New accepted the overlay %v.", testCase.overlay)
			}
			for _, want := range []string{"values.overlay " + testCase.dotted, testCase.unread, "maximum", "pattern"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}

func TestWithoutMutateEverySpecPathTheSchemaDescribesMoves(t *testing.T) {
	g := newGenerator(t, loadConfig(t, rulesConfig))
	want := []string{"spec.config", "spec.count", "spec.left", "spec.maxUnavailable", "spec.minCount", "spec.mode", "spec.tags"}
	if got := dottedOf(g.kinds[0].fields); !slices.Equal(got, want) {
		t.Errorf("The generator mutates %v, want %v.", got, want)
	}
}

func TestNewReportsTheSpecPathsItLeavesAlone(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	leftAlone := newGenerator(t, cfg).LeftAlone()
	want := [][]string{
		{"spec.right", "the CRD refuses every value reconciler-fuzzer drew for it in the sample", "exactly one of left and right"},
		{"spec.surge", "x-kubernetes-int-or-string"},
	}
	if len(leftAlone) != len(want) {
		t.Fatalf("New leaves %q alone, want spec.right and spec.surge.", leftAlone)
	}
	for i, says := range want {
		for _, part := range says {
			if !strings.Contains(leftAlone[i], part) {
				t.Errorf("New reports %q, which does not say %q.", leftAlone[i], part)
			}
		}
	}
	cfg.Primaries[0].Values.Mutate = []string{"spec.count"}
	if leftAlone := newGenerator(t, cfg).LeftAlone(); len(leftAlone) != 0 {
		t.Errorf("New leaves %q alone, and values.mutate names every path that moves.", leftAlone)
	}
}

func TestMutateIsAnAllowlistOfSchemaPaths(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	cfg.Primaries[0].Values.Mutate = []string{"spec.left", "spec.count"}
	g := newGenerator(t, cfg)
	if got := dottedOf(g.kinds[0].fields); !slices.Equal(got, []string{"spec.count", "spec.left"}) {
		t.Errorf("The generator mutates %v.", got)
	}
	if !slices.Equal(g.kinds[0].fields[1].path, []string{"spec", "left"}) {
		t.Errorf("The field's path is %v, want it split on the dots.", g.kinds[0].fields[1].path)
	}
}

func TestDistinctPathsAreNotMutated(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	cfg.Primaries[0].Values.Mutate = []string{"spec.left", "spec.count"}
	cfg.Primaries[0].Values.Distinct = []string{"spec.left"}
	if got := dottedOf(newGenerator(t, cfg).kinds[0].fields); !slices.Equal(got, []string{"spec.count"}) {
		t.Errorf("The generator mutates %v, want spec.count alone.", got)
	}
	cfg.Primaries[0].Values.Distinct = []string{"spec.count"}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "values.distinct spec.count") {
		t.Errorf("New returned %v for a distinct path that holds no string.", err)
	}
}

func TestAPrimaryKindTheCRDsDoNotDescribeIsAConfigurationError(t *testing.T) {
	cfg := loadConfig(t, toyConfig)
	cfg.Primaries[0].Kind.Version = "v9"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "describe no") {
		t.Errorf("New returned %v.", err)
	}
}
