package invariant

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// maxValue bounds a quoted value, in runes.
const maxValue = 80

// side is one version's value at a path, and the form of it a report may show.
type side struct {
	value, shown any
	present      bool
}

// change is one field at which two objects differ.
type change struct {
	path          target.Path
	before, after side
}

// diff appends every field below path at which a and b differ, in the paths
// equalIgnore takes.
func diff(path target.Path, a, b side, changes []change) []change {
	if a.present == b.present && reflect.DeepEqual(a.value, b.value) {
		return changes
	}
	found := len(changes)
	aMap, aIsMap := a.value.(map[string]any)
	bMap, bIsMap := b.value.(map[string]any)
	aList, aIsList := a.value.([]any)
	bList, bIsList := b.value.([]any)
	switch {
	case len(path) > 0 && path[:len(path)-1].MapsToStrings():
		// A label or an annotation is one value.
	case (aIsMap || !a.present) && (bIsMap || !b.present):
		// A map only one side holds compares as an empty one.
		for _, key := range sortedKeys(aMap, bMap) {
			changes = diff(append(slices.Clip(path), target.Step{Key: key}), a.at(key), b.at(key), changes)
		}
	case aIsList && bIsList && len(aList) == len(bList):
		each := append(slices.Clip(path), target.Step{Each: true})
		for i := range aList {
			changes = diff(each, a.item(i), b.item(i), changes)
		}
	}
	if len(changes) == found { // No field below differs, so the value does.
		changes = append(changes, change{path, a, b})
	}
	return changes
}

func sortedKeys(a, b map[string]any) []string {
	keys := slices.AppendSeq(slices.Collect(maps.Keys(a)), maps.Keys(b))
	slices.Sort(keys)
	return slices.Compact(keys)
}

// at is the side's field at key. A form that redacted the whole field shows its
// marker for everything below it.
func (s side) at(key string) side {
	fields, _ := s.value.(map[string]any)
	value, present := fields[key]
	if shown, ok := s.shown.(map[string]any); ok {
		return side{value, shown[key], present}
	}
	return side{value, s.shown, present}
}

func (s side) item(i int) side {
	value := s.value.([]any)[i]
	if shown, ok := s.shown.([]any); ok {
		return side{value, shown[i], true}
	}
	return side{value, s.shown, true}
}

func (c change) difference(object Difference) Difference {
	object.Path = c.path.String()
	object.Before, object.After = c.before.quote(), c.after.quote()
	return object
}

// quote writes the shown value as compact JSON, cut at maxValue runes.
func (s side) quote() string {
	if !s.present {
		return "(absent)"
	}
	var encoded strings.Builder
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s.shown) // A comparable form holds only JSON values.
	text := []rune(strings.TrimSuffix(encoded.String(), "\n"))
	if len(text) > maxValue {
		return string(text[:maxValue]) + "…"
	}
	return string(text)
}
