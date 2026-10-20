package generate

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func countField(count int64, draws *int) field {
	values := rapid.Custom(func(t *rapid.T) any {
		*draws++
		return rapid.Just(count).Draw(t, "count")
	})
	return field{path: []string{"spec", "count"}, dotted: "spec.count", values: values}
}

func TestTheCRDJudgesWhatTheOverlayLoosens(t *testing.T) {
	cfg := loadConfig(t, toyConfig)
	cfg.Values.Overlay = map[string]map[string]any{"spec.count": {"minimum": 5, "maximum": 20}}
	drawn := generatedAt(t, cfg, "spec", "count")
	for _, count := range drawn {
		if count.(float64) > 10 {
			t.Fatalf("spec.count took %v, which the CRD's maximum of 10 refuses.", count)
		}
	}
	if len(drawn) == 0 {
		t.Error("No draw changed spec.count.")
	}
}

func TestAMutatePathTheCRDRefusesInTheSampleIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		path, mutate string
		overlay      map[string]map[string]any
		reported     string
	}{
		{toyConfig, "spec.count", map[string]map[string]any{"spec.count": {"minimum": 11, "maximum": 20}},
			"less than or equal to 10"},
		{rulesConfig, "spec.right", nil, "exactly one of left and right must be set"},
	} {
		t.Run(testCase.mutate, func(t *testing.T) {
			cfg := loadConfig(t, testCase.path)
			cfg.Values.Mutate, cfg.Values.Overlay = []string{testCase.mutate}, testCase.overlay
			_, err := New(cfg)
			for _, want := range []string{"values.mutate " + testCase.mutate, testCase.reported} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %v, which does not mention %q.", err, want)
				}
			}
		})
	}
}

func TestNewReportsAFieldReconcilerFuzzerCannotDraw(t *testing.T) {
	const cannotDraw = "reconciler-fuzzer cannot draw a value its schema allows; a set whose items allow fewer values than its minItems, or a pattern nothing matches, does this"
	for _, testCase := range []struct {
		name, path string
		overlay    map[string]any
	}{
		// No set of three items holds only a and b.
		{"enum", "spec.tags", map[string]any{"minItems": 3, "items": map[string]any{"enum": []any{"a", "b"}}}},
		{"pattern", "spec.tags", map[string]any{"minItems": 3, "items": map[string]any{"pattern": "^[ab]$"}}},
		{"no match", "spec.left", map[string]any{"pattern": "a$b"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := loadConfig(t, rulesConfig)
			cfg.Values.Mutate = []string{testCase.path}
			cfg.Values.Overlay = map[string]map[string]any{testCase.path: testCase.overlay}
			_, err := New(cfg)
			if want := "values.mutate " + testCase.path + ": " + cannotDraw; err == nil || !strings.HasSuffix(err.Error(), want) {
				t.Errorf("New reported %v, want it to end %q.", err, want)
			}
			cfg.Values.Mutate = nil
			if leftAlone := newGenerator(t, cfg).LeftAlone(); !slices.ContainsFunc(leftAlone, func(note string) bool {
				return strings.Contains(note, testCase.path) && strings.Contains(note, cannotDraw)
			}) {
				t.Errorf("New leaves %q alone, want %s among them.", leftAlone, testCase.path)
			}
		})
	}
}

func TestACannotDrawErrorQuotesAnyOtherPanic(t *testing.T) {
	g := newGenerator(t, loadConfig(t, rulesConfig))
	panics := field{path: []string{"spec", "count"}, dotted: "spec.count", values: rapid.Custom(func(*rapid.T) any {
		var missing *field
		return missing.dotted
	})}
	err := g.rules.acceptsADraw(g.sample, panics)
	if err == nil || !strings.Contains(err.Error(), "nil pointer dereference") || strings.Contains(err.Error(), "cannot draw") {
		t.Errorf("acceptsADraw returned %v, want the panic's reason and no guess at its cause.", err)
	}
}

func TestNewDrawsAFieldAHundredTimesToFindAValueTheCRDAccepts(t *testing.T) {
	g := newGenerator(t, loadConfig(t, rulesConfig))
	for _, testCase := range []struct {
		count    int64
		draws    int
		accepted bool
	}{
		{5, 1, true},
		// No count reaches minCount's default.
		{0, 100, false},
	} {
		draws := 0
		err := g.rules.acceptsADraw(g.sample, countField(testCase.count, &draws))
		if (err == nil) != testCase.accepted || draws != testCase.draws {
			t.Errorf("With every count %d, New drew %d times and returned %v, want %d draws and accepted=%t.",
				testCase.count, draws, err, testCase.draws, testCase.accepted)
		}
	}
}

func TestATransitionRuleSeesTheOldObjectsDefaults(t *testing.T) {
	g := newGenerator(t, loadConfig(t, rulesConfig))
	for _, testCase := range []struct {
		field   string
		value   int64
		refused bool
	}{
		{"count", 5, false},
		{"minCount", 2, true},
	} {
		updated := g.sample.DeepCopy()
		if err := unstructured.SetNestedField(updated.Object, testCase.value, "spec", testCase.field); err != nil {
			t.Fatal(err)
		}
		if err := g.rules.refusal(updated.Object, g.sample.Object); (err != nil) != testCase.refused {
			t.Errorf("Setting spec.%s to %d returned %v, want refused=%t.", testCase.field, testCase.value, err, testCase.refused)
		}
	}
}

func TestUpdatesNeverBreakATransitionRule(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	cfg.Values.Mutate = []string{"spec.count", "spec.minCount"}
	minCount := func(raw []byte) (float64, bool) {
		spec, _ := decode(t, raw)["spec"].(map[string]any)
		value, set := spec["minCount"]
		if !set || value == nil {
			return 1, set
		}
		return value.(float64), true
	}
	updates := 0
	g := newGenerator(t, cfg)
	for seed := uint64(1); seed <= 400; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		held := map[string]float64{}
		for _, a := range actions([]*plan.Plan{p}) {
			switch a.Do {
			case plan.Create:
				held[a.Slot], _ = minCount(a.Object)
			case plan.Update:
				if value, set := minCount(a.Patch); set {
					if value > held[a.Slot] {
						t.Errorf("seed %d raises minCount of slot %s from %v to %v.", seed, a.Slot, held[a.Slot], value)
					}
					held[a.Slot] = value
					updates++
				}
			}
		}
	}
	if updates == 0 {
		t.Error("No update changes minCount.")
	}
}

func TestASampleItsCRDRefusesIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		field    string
		value    any
		reported string
	}{
		{"maxUnavailable", int64(4), "maxUnavailable must not exceed count"},
		{"count", int64(11), "spec.count"},
		{"tags", []any{"a", "a"}, "spec.tags"},
	} {
		t.Run(testCase.field, func(t *testing.T) {
			cfg := loadConfig(t, rulesConfig)
			sample, err := readObject(cfg.Primary.Sample)
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(sample, testCase.value, "spec", testCase.field); err != nil {
				t.Fatal(err)
			}
			cfg.Primary.Sample = filepath.Join(t.TempDir(), "sample.json")
			if err := os.WriteFile(cfg.Primary.Sample, marshal(sample), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "refuses the sample") || !strings.Contains(err.Error(), testCase.reported) {
				t.Errorf("New returned %v, want it to say the CRD refuses the sample's %q.", err, testCase.reported)
			}
		})
	}
}
