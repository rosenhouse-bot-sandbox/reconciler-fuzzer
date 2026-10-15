package generate

import (
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestTheCRDJudgesWhatTheOverlayLoosens(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	loaded.Generate.Overlay = map[string]map[string]any{"spec.count": {"minimum": 5, "maximum": 20}}
	g := newGenerator(t, loaded, Options{})
	drawn := 0
	rapid.Check(t, func(rt *rapid.T) {
		for _, count := range generatedAt(g.sequence(rt), loaded.Sample, "spec", "count") {
			if count.(int64) > 10 {
				rt.Fatalf("spec.count took %v, which the CRD's maximum of 10 refuses.", count)
			}
			drawn++
		}
	})
	if drawn == 0 {
		t.Error("No draw changed spec.count.")
	}
}

func TestAMutatePathTheCRDRefusesInTheSampleIsAConfigurationError(t *testing.T) {
	for _, testCase := range []struct {
		path, mutate string
		overlay      map[string]map[string]any
		reported     string
	}{
		{toyTarget, "spec.count", map[string]map[string]any{"spec.count": {"minimum": 11, "maximum": 20}},
			"less than or equal to 10"},
		{rulesTarget, "spec.right", nil, "exactly one of left and right must be set"},
	} {
		t.Run(testCase.mutate, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			loaded.Generate = target.GenerateSpec{Mutate: []string{testCase.mutate}, Overlay: testCase.overlay}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted generate.mutate %s, which no drawn value moves.", testCase.mutate)
			}
			for _, want := range []string{"generate.mutate " + testCase.mutate, testCase.reported} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
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
			loaded := loadTarget(t, rulesTarget)
			overlay := map[string]map[string]any{testCase.path: testCase.overlay}
			loaded.Generate = target.GenerateSpec{Mutate: []string{testCase.path}, Overlay: overlay}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted generate.mutate %s, which reconciler-fuzzer cannot draw.", testCase.path)
			}
			// rapid's own reason names its internals, not the bound to relax.
			want := "generate.mutate " + testCase.path + ": " + cannotDraw
			if !strings.HasSuffix(err.Error(), want) {
				t.Errorf("New reported %q, want it to end %q.", err, want)
			}

			loaded.Generate.Mutate = nil
			g := newGenerator(t, loaded, Options{})
			if !slices.ContainsFunc(g.LeftAlone(), func(note string) bool {
				return strings.Contains(note, testCase.path) && strings.Contains(note, cannotDraw)
			}) {
				t.Errorf("New reports it leaves %q alone, want %s among them.", g.LeftAlone(), testCase.path)
			}
		})
	}
}

// A panic that is not rapid's failure to draw is a bug, and its reason is
// what a reader needs.
func TestACannotDrawErrorQuotesAnyOtherPanic(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
	panics := field{path: []string{"spec", "count"}, dotted: "spec.count", values: rapid.Custom(func(*rapid.T) any {
		var missing *field
		return missing.dotted
	})}

	err := g.rules.acceptsADraw(loaded.Sample, panics)

	if err == nil || !strings.Contains(err.Error(), "nil pointer dereference") || strings.Contains(err.Error(), "cannot draw") {
		t.Errorf("acceptsADraw returned %v, want the panic's reason and no guess at its cause.", err)
	}
}

func TestNewKeepsAFieldOnlyACreateMayChange(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate = target.GenerateSpec{
		Mutate:  []string{"spec.mode"},
		Overlay: map[string]map[string]any{"spec.mode": {"enum": []any{"safe"}}},
	}
	if _, err := New(loaded, Options{}); err != nil {
		t.Errorf("New refused a mode that a create may set and an update may not: %v.", err)
	}
}

func TestNewDrawsAFieldAHundredTimesToFindAValueTheCRDAccepts(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	g := newGenerator(t, loaded, Options{})
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
		err := g.rules.acceptsADraw(loaded.Sample, countField(testCase.count, &draws))
		if (err == nil) != testCase.accepted || draws != testCase.draws {
			t.Errorf("With every count %d, New drew %d times and returned %v, want %d draws and accepted=%t.",
				testCase.count, draws, err, testCase.draws, testCase.accepted)
		}
	}
}

func TestATransitionRuleSeesTheOldObjectsDefaults(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	rules := newGenerator(t, loaded, Options{}).rules
	for _, testCase := range []struct {
		field   string
		value   int64
		refused bool
	}{
		{"count", 5, false},
		{"minCount", 2, true},
	} {
		t.Run(testCase.field, func(t *testing.T) {
			updated := loaded.Sample.DeepCopy()
			if err := unstructured.SetNestedField(updated.Object, testCase.value, "spec", testCase.field); err != nil {
				t.Fatalf("Setting spec.%s failed: %v.", testCase.field, err)
			}
			if err := rules.refusal(updated.Object, loaded.Sample.Object); (err != nil) != testCase.refused {
				t.Errorf("Setting spec.%s to %d on a gadget without minCount returned %v, want refused=%t.",
					testCase.field, testCase.value, err, testCase.refused)
			}
		})
	}
}

func TestAFieldTheSampleCannotHoldIsRefused(t *testing.T) {
	g := newGenerator(t, loadTarget(t, rulesTarget), Options{})
	draws := 0
	inside := countField(5, &draws)
	inside.path = []string{"spec", "count", "inside"}
	if err := g.rules.acceptsADraw(g.target.Sample, inside); err == nil {
		t.Error("New drew a value inside the sample's spec.count, which holds an integer.")
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
			loaded := loadTarget(t, rulesTarget)
			if err := unstructured.SetNestedField(loaded.Sample.Object, testCase.value, "spec", testCase.field); err != nil {
				t.Fatalf("Setting the sample's %s failed: %v.", testCase.field, err)
			}
			_, err := New(loaded, Options{})
			if err == nil {
				t.Fatalf("New accepted a sample whose spec.%s is %v, which its CRD refuses.", testCase.field, testCase.value)
			}
			for _, want := range []string{"sample", testCase.reported} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("New reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}
