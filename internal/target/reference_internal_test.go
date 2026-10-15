package target

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/reference"
)

const (
	keysHeading      = "## target.yaml"
	referenceExample = "../../docs/reference/target.yaml"
)

func TestTheReferenceListsEveryKey(t *testing.T) {
	keys := reference.Keys(reflect.TypeFor[declaration]())

	documented := reference.Listed(t, keysHeading)

	for _, key := range keys {
		if !slices.Contains(documented, key) {
			t.Errorf("%s has no row for the key %s.", reference.Path, key)
		}
	}
	for _, row := range documented {
		if !slices.Contains(keys, row) {
			t.Errorf("%s has a row for %s, which target.yaml does not take.", reference.Path, row)
		}
	}
}

func TestTheReferenceGivesEachDefault(t *testing.T) {
	for key, want := range map[string]time.Duration{
		"timeouts.settle": DefaultTimeouts.Settle,
		"timeouts.stable": DefaultTimeouts.Stable,
		"timeouts.delete": DefaultTimeouts.Delete,
	} {
		documented := documentedDefault(t, key)
		if got, err := time.ParseDuration(documented); err != nil || got != want {
			t.Errorf("The reference gives %s the default %q, want %s.", key, documented, want)
		}
	}
	for key, want := range map[string]string{
		"ready":              DefaultReady,
		"properties[*].when": string(Checkpoint),
		"thresholds.errloop": strconv.Itoa(DefaultThresholds.ErrLoop),
		"thresholds.quiet":   strconv.Itoa(DefaultThresholds.Quiet),
	} {
		if got := documentedDefault(t, key); got != want {
			t.Errorf("The reference gives %s the default %q, want %q.", key, got, want)
		}
	}
}

// documentedDefault is the code span in the Default cell of key's row.
func documentedDefault(t *testing.T, key string) string {
	t.Helper()
	spans := reference.Spans(reference.Row(t, keysHeading, key)[0])
	if len(spans) != 1 {
		t.Fatalf("The Default cell of %s holds %d code spans, want 1.", key, len(spans))
	}
	return spans[0]
}

// A target of the keys the reference calls required loads, and one that lacks
// any of them does not.
func TestTheReferenceSaysWhichKeysAreRequired(t *testing.T) {
	sample, err := filepath.Abs("../../targets/toy-widget/widget.yaml")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{
		"name":              "toy-widget",
		"primary":           "toy.reconciler-fuzzer/v1/Widget",
		"sample":            sample,
		"launch.binary":     "bin/toy-widget",
		"properties[*].id":  "P1",
		"properties[*].cel": "true",
	}
	var required []string
	for _, key := range reference.Keys(reflect.TypeFor[declaration]()) {
		if reference.Row(t, keysHeading, key)[0] == "required" {
			required = append(required, key)
		}
	}

	if err := loadKeys(t, values, required); err != nil {
		t.Errorf("A target of the required keys %v does not load: %v", required, err)
	}
	for _, key := range required {
		if err := loadKeys(t, values, slices.DeleteFunc(slices.Clone(required), func(k string) bool { return k == key })); err == nil {
			t.Errorf("A target without %s loads, and the reference says it is required.", key)
		}
	}
}

// loadKeys loads a target that sets each key to its value, where [*] stands
// for a list of one item.
func loadKeys(t *testing.T, values map[string]any, keys []string) error {
	t.Helper()
	declared := map[string]any{}
	for _, key := range keys {
		value, known := values[key]
		if !known {
			t.Fatalf("The test has no value for the required key %s.", key)
		}
		steps := strings.Split(key, ".")
		block := declared
		for _, step := range steps[:len(steps)-1] {
			name, isList := strings.CutSuffix(step, "[*]")
			if block[name] == nil {
				block[name] = map[string]any{}
				if isList {
					block[name] = []any{map[string]any{}}
				}
			}
			if isList {
				block = block[name].([]any)[0].(map[string]any)
			} else {
				block = block[name].(map[string]any)
			}
		}
		block[steps[len(steps)-1]] = value
	}
	data, err := yaml.Marshal(declared)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "target.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	return err
}

// equal replaces what equalIgnore feeds, and the loader refuses the two
// together.
func TestTheReferenceExampleSetsEveryKeyButEqual(t *testing.T) {
	data, err := os.ReadFile(referenceExample)
	if err != nil {
		t.Fatal(err)
	}
	var declared declaration
	if err := yaml.UnmarshalStrict(data, &declared); err != nil {
		t.Fatalf("%s does not decode: %v", referenceExample, err)
	}

	set := reference.SetKeys(reflect.ValueOf(declared))
	unset := slices.DeleteFunc(reference.Keys(reflect.TypeFor[declaration]()), func(key string) bool { return slices.Contains(set, key) })
	if !slices.Equal(unset, []string{"equal"}) {
		t.Errorf("%s leaves %v unset, want equal alone.", referenceExample, unset)
	}
	if _, err := Load(referenceExample); err != nil {
		t.Errorf("Load rejected the example: %v", err)
	}
}
