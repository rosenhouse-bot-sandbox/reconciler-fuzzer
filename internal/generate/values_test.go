package generate

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func parseSchema(t *testing.T, declared string) *schema {
	t.Helper()
	var parsed schema
	if err := json.Unmarshal([]byte(declared), &parsed); err != nil {
		t.Fatalf("The test's own schema %s does not parse: %v.", declared, err)
	}
	return &parsed
}

func TestIntegerRangeReadsTheSchemasBounds(t *testing.T) {
	for _, testCase := range []struct {
		declared string
		lo, hi   int64
	}{
		{`{"type":"integer","format":"int32","minimum":0,"maximum":10}`, 0, 10},
		{`{"type":"integer","minimum":2,"maximum":5}`, 2, 5},
		{`{"type":"integer","minimum":2,"maximum":5,"exclusiveMinimum":true,"exclusiveMaximum":true}`, 3, 4},
		{`{"type":"integer","minimum":2}`, 2, 2 + defaultSpan},
		{`{"type":"integer","maximum":5}`, 5 - defaultSpan, 5},
		{`{"type":"integer"}`, 0, defaultSpan},
		{`{"type":"integer","format":"int32","maximum":2147483647}`, 2147483647 - defaultSpan, 2147483647},
		{`{"type":"integer","minimum":-9e18}`, -safeInteger, -safeInteger + defaultSpan},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			lo, hi := integerRange(parseSchema(t, testCase.declared))
			if lo != testCase.lo || hi != testCase.hi {
				t.Errorf("integerRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
}

func TestListAndStringLengthsReadTheSchemasBounds(t *testing.T) {
	for _, testCase := range []struct {
		declared string
		lo, hi   int
	}{
		{`{"type":"array"}`, 0, defaultMaxItems},
		{`{"type":"array","minItems":5}`, 5, 5},
		{`{"type":"array","maxItems":1}`, 0, 1},
		{`{"type":"array","minItems":1,"maxItems":3}`, 1, 3},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			if lo, hi := itemRange(parseSchema(t, testCase.declared)); lo != testCase.lo || hi != testCase.hi {
				t.Errorf("itemRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
	for _, testCase := range []struct {
		declared string
		lo, hi   int
	}{
		{`{"type":"object"}`, 0, defaultMaxItems},
		{`{"type":"object","minProperties":5}`, 5, 5},
		{`{"type":"object","maxProperties":1}`, 0, 1},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			if lo, hi := propertyRange(parseSchema(t, testCase.declared)); lo != testCase.lo || hi != testCase.hi {
				t.Errorf("propertyRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
	for _, testCase := range []struct {
		declared string
		lo, hi   int
	}{
		{`{"type":"string"}`, defaultMinLength, defaultMaxLength},
		{`{"type":"string","minLength":20}`, 20, 20},
		{`{"type":"string","maxLength":2}`, defaultMinLength, 2},
		{`{"type":"string","minLength":3,"maxLength":4}`, 3, 4},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			if lo, hi := lengthRange(parseSchema(t, testCase.declared)); lo != testCase.lo || hi != testCase.hi {
				t.Errorf("lengthRange is %d to %d, want %d to %d.", lo, hi, testCase.lo, testCase.hi)
			}
		})
	}
}

func TestGeneratedValuesMatchTheirSchema(t *testing.T) {
	for _, declared := range []string{
		`{"type":"boolean"}`,
		`{"type":"integer","minimum":-3,"maximum":3}`,
		`{"type":"number","minimum":-1.5,"maximum":2.5}`,
		`{"type":"number","minimum":0,"maximum":1,"exclusiveMinimum":true,"exclusiveMaximum":true}`,
		`{"type":"string","minLength":3,"maxLength":4}`,
		`{"type":"string","pattern":"^ab[0-9]{2}$"}`,
		`{"type":"string","enum":["1h","24h"]}`,
		`{"type":"array","minItems":1,"maxItems":2,"items":{"type":"integer","minimum":1,"maximum":9}}`,
		`{"type":"array","x-kubernetes-list-type":"set","minItems":2,"maxItems":3,
		  "items":{"type":"string","minLength":5,"maxLength":8}}`,
		`{"type":"object","required":["a"],"properties":{"a":{"type":"integer","minimum":1,"maximum":2},
		  "c":{"type":"boolean"}}}`,
		`{"type":"object","minProperties":1,"maxProperties":2,
		  "additionalProperties":{"type":"integer","minimum":1,"maximum":9}}`,
		`{"type":"object","additionalProperties":{"type":"object","required":["a"],"properties":{"a":{"type":"boolean"}}}}`,
	} {
		t.Run(declared, func(t *testing.T) {
			parsed := parseSchema(t, declared)
			values, err := valuesOf(parsed)
			if err != nil {
				t.Fatalf("valuesOf failed: %v.", err)
			}
			rapid.Check(t, func(rt *rapid.T) {
				value := values.Draw(rt, "value")
				if err := validate(parsed, value, "the value"); err != nil {
					rt.Fatalf("%v.", err)
				}
			})
		})
	}
}

func TestOptionalPropertiesTheSchemaSaysTooLittleAboutStayOut(t *testing.T) {
	parsed := parseSchema(t, `{"type":"object","required":["a"],"properties":{
	  "a":{"type":"integer","minimum":1,"maximum":2},
	  "when":{"type":"string","format":"date-time"}}}`)
	values, err := valuesOf(parsed)
	if err != nil {
		t.Fatalf("valuesOf failed: %v.", err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		object := values.Draw(rt, "value").(map[string]any)
		if _, present := object["when"]; present {
			t.Errorf("The object carries a when of %#v, and the schema says only that it is a date-time.", object["when"])
		}
	})
}

func TestSchemasThatSayTooLittleAreConfigurationErrors(t *testing.T) {
	for _, testCase := range []struct{ declared, reports string }{
		{`{"type":"string","format":"date-time"}`, "format"},
		{`{"x-kubernetes-int-or-string":true}`, "x-kubernetes-int-or-string"},
		{`{"type":"object","x-kubernetes-preserve-unknown-fields":true}`, "no property"},
		{`{"type":"object","additionalProperties":true}`, "no property"},
		{`{"type":"object","minProperties":3,"maxProperties":1,"additionalProperties":{"type":"string"}}`, "no map"},
		{`{"type":"array"}`, "what the array holds"},
		{`{"type":"integer","minimum":5,"maximum":3}`, "no integer"},
		{`{"type":"string","minLength":5,"maxLength":3}`, "no string"},
		{`{"type":"array","minItems":5,"maxItems":3,"items":{"type":"boolean"}}`, "no list"},
		{`{"type":"array","x-kubernetes-list-type":"map","x-kubernetes-list-map-keys":["name"],
		  "items":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}}}}`, "keyed"},
		{`{"type":"object","required":["when"],"properties":{"when":{"type":"string","format":"date-time"}}}`, "when"},
	} {
		t.Run(testCase.declared, func(t *testing.T) {
			_, err := valuesOf(parseSchema(t, testCase.declared))
			if err == nil {
				t.Fatal("valuesOf generated from a schema that does not say what the value may be.")
			}
			if !strings.Contains(err.Error(), testCase.reports) {
				t.Errorf("valuesOf reported %q, which does not mention %q.", err, testCase.reports)
			}
		})
	}
}

func TestAMapFieldIsDrawn(t *testing.T) {
	cfg := loadConfig(t, rulesConfig)
	cfg.Primaries[0].Values.Mutate = []string{"spec.config"}
	word := regexp.MustCompile(`^[a-z0-9]{1,12}$`)
	keyed := 0
	for _, drawn := range generatedAt(t, cfg, "spec", "config") {
		config, isMap := drawn.(map[string]any)
		if !isMap || len(config) > 3 {
			t.Fatalf("spec.config is %#v, want a map of at most the CRD's 3 keys.", drawn)
		}
		for key, value := range config {
			if text, isString := value.(string); !word.MatchString(key) || !isString || len(text) > 8 {
				t.Fatalf("spec.config holds %q: %#v, want a word keying a string of at most 8 runes.", key, value)
			}
		}
		if len(config) > 0 {
			keyed++
		}
	}
	if keyed == 0 {
		t.Error("No draw put a key in spec.config.")
	}
}

func TestAnIntOrStringNamesTheOverlayThatSaysWhichItIs(t *testing.T) {
	for _, declared := range []string{
		`{"x-kubernetes-int-or-string":true}`,
		// A word is rarely what an int-or-string's string form means.
		`{"x-kubernetes-int-or-string":true,"type":"string"}`,
	} {
		t.Run(declared, func(t *testing.T) {
			_, err := valuesOf(parseSchema(t, declared))
			if err == nil {
				t.Fatal("valuesOf guessed which form an int-or-string takes.")
			}
			for _, want := range []string{"x-kubernetes-int-or-string", "values.overlay", "type: integer", "type: string", "pattern", "enum"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("valuesOf reported %q, which does not mention %q.", err, want)
				}
			}
		})
	}
}

func TestAnIntOrStringFollowsItsOverlay(t *testing.T) {
	percent := regexp.MustCompile(`^[1-9]0%$`)
	for _, testCase := range []struct {
		overlay map[string]any
		allowed func(any) bool
	}{
		{map[string]any{"type": "integer", "minimum": 1, "maximum": 3}, func(value any) bool {
			whole, isNumber := value.(float64)
			return isNumber && whole >= 1 && whole <= 3
		}},
		{map[string]any{"type": "string", "pattern": percent.String()}, func(value any) bool {
			text, isString := value.(string)
			return isString && percent.MatchString(text)
		}},
	} {
		t.Run(testCase.overlay["type"].(string), func(t *testing.T) {
			cfg := loadConfig(t, rulesConfig)
			cfg.Primaries[0].Values.Mutate = []string{"spec.surge"}
			cfg.Primaries[0].Values.Overlay = map[string]map[string]any{"spec.surge": testCase.overlay}
			drawn := generatedAt(t, cfg, "spec", "surge")
			for _, surge := range drawn {
				if !testCase.allowed(surge) {
					t.Fatalf("spec.surge is %#v, which the overlay %v does not allow.", surge, testCase.overlay)
				}
			}
			if len(drawn) == 0 {
				t.Error("No draw changed spec.surge.")
			}
		})
	}
}

// generatedAt are the values that the creates and updates of 100 plans
// write at path, where they differ from the sample's.
func generatedAt(t *testing.T, cfg *config.Config, path ...string) []any {
	t.Helper()
	g := newGenerator(t, cfg)
	kept, _, _ := unstructured.NestedFieldNoCopy(g.sample.Object, path...)
	var values []any
	for seed := uint64(1); seed <= 100; seed++ {
		p, err := g.Plan(seed)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range actions([]*plan.Plan{p}) {
			raw := a.Object
			if a.Do == plan.Update {
				raw = a.Patch
			} else if a.Do != plan.Create {
				continue
			}
			value, found, err := unstructured.NestedFieldNoCopy(decode(t, raw), path...)
			if found && err == nil && value != nil && !equalJSON(value, kept) {
				values = append(values, value)
			}
		}
	}
	return values
}
