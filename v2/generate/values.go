package generate

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"

	"pgregory.net/rapid"
)

const (
	// defaultSpan bounds a number, a string's length or a list's length where
	// the schema leaves one side open.
	defaultSpan = 100
	// safeInteger is the largest integer a JSON number carries exactly. An
	// update's patch is a map, so its numbers pass through float64.
	safeInteger = 1 << 53

	defaultMaxItems  = 3
	defaultMinLength = 1
	defaultMaxLength = 12
)

// wordRunes are what an unconstrained string is made of: the schema does not
// say what the target reads, so the safest string is a short plain word.
var wordRunes = rapid.RuneFrom([]rune("abcdefghijklmnopqrstuvwxyz0123456789"))

// valuesOf builds the generator of the values a schema allows, or reports that
// the schema does not say enough to generate them.
func valuesOf(s *schema) (*rapid.Generator[any], error) {
	if len(s.Enum) > 0 {
		enum := slices.Clone(s.Enum)
		for i, value := range enum {
			enum[i] = normalized(value)
		}
		return rapid.SampledFrom(enum), nil
	}
	if s.IntOrString && (s.Type == "" || s.Type == "string" && s.Pattern == "") {
		return nil, errors.New("the schema takes an integer or a string (x-kubernetes-int-or-string) and does not say which: " +
			"give values.overlay type: integer, or type: string with a pattern or an enum")
	}
	switch s.Type {
	case "boolean":
		return rapid.Bool().AsAny(), nil
	case "integer":
		lo, hi := integerRange(s)
		if lo > hi {
			return nil, fmt.Errorf("no integer lies between the schema's minimum and maximum")
		}
		return rapid.Int64Range(lo, hi).AsAny(), nil
	case "number":
		lo, hi := numberRange(s)
		if lo > hi {
			return nil, fmt.Errorf("no number lies between the schema's minimum and maximum")
		}
		return rapid.Float64Range(lo, hi).AsAny(), nil
	case "string":
		return stringValues(s)
	case "array":
		return listValues(s)
	case "object":
		return objectValues(s)
	}
	return nil, fmt.Errorf("the schema does not say what values a %q takes", s.Type)
}

func stringValues(s *schema) (*rapid.Generator[any], error) {
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			return nil, fmt.Errorf("the schema's pattern does not compile: %w", err)
		}
		return rapid.StringMatching(s.Pattern).AsAny(), nil
	}
	if s.Format != "" {
		return nil, fmt.Errorf("the schema takes a string of format %q, which only an enum or a pattern says how to write", s.Format)
	}
	lo, hi := lengthRange(s)
	if lo > hi {
		return nil, errors.New("no string lies between the schema's minLength and maxLength")
	}
	return rapid.StringOfN(wordRunes, lo, hi, -1).AsAny(), nil
}

func listValues(s *schema) (*rapid.Generator[any], error) {
	if s.Items == nil {
		return nil, errors.New("the schema does not say what the array holds")
	}
	items, err := valuesOf(s.Items)
	if err != nil {
		return nil, err
	}
	lo, hi := itemRange(s)
	if lo > hi {
		return nil, errors.New("no list lies between the schema's minItems and maxItems")
	}
	switch s.ListType {
	case "map":
		// The API server rejects two items with one key, and the schema's keys
		// are not the generator's to keep apart. An overlay saying the list is
		// atomic takes that back.
		return nil, errors.New("the schema's list is keyed by x-kubernetes-list-map-keys")
	case "set":
		return rapid.SliceOfNDistinct(items, lo, hi, key).AsAny(), nil
	}
	return rapid.SliceOfN(items, lo, hi).AsAny(), nil
}

// key identifies a list item, so that a set holds no duplicates.
func key(item any) string { return fmt.Sprint(item) }

func objectValues(s *schema) (*rapid.Generator[any], error) {
	if s.AdditionalProperties.schema != nil {
		return mapValuesOf(s)
	}
	type property struct {
		name     string
		values   *rapid.Generator[any]
		required bool
	}
	var properties []property
	for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
		required := slices.Contains(s.Required, name)
		values, err := valuesOf(s.Properties[name])
		if err != nil {
			if required {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			continue // An optional property the schema says too little about stays out.
		}
		properties = append(properties, property{name: name, values: values, required: required})
	}
	if len(properties) == 0 {
		return nil, errors.New("the schema names no property the object may carry")
	}
	return rapid.Custom(func(t *rapid.T) any {
		object := map[string]any{}
		for _, p := range properties {
			if !p.required && !rapid.Bool().Draw(t, "has "+p.name) {
				continue
			}
			object[p.name] = p.values.Draw(t, p.name)
		}
		return object
	}), nil
}

// mapValuesOf draws a map keyed by words.
func mapValuesOf(s *schema) (*rapid.Generator[any], error) {
	values, err := valuesOf(s.AdditionalProperties.schema)
	if err != nil {
		return nil, err
	}
	lo, hi := propertyRange(s)
	if lo > hi {
		return nil, errors.New("no map lies between the schema's minProperties and maxProperties")
	}
	keys := rapid.StringOfN(wordRunes, defaultMinLength, defaultMaxLength, -1)
	return rapid.MapOfN(keys, values, lo, hi).AsAny(), nil
}

// integerRange is the range the schema allows, inside the integers JSON
// carries exactly.
func integerRange(s *schema) (int64, int64) {
	lo, hi := int64(-safeInteger), int64(safeInteger)
	if s.Format == "int32" {
		lo, hi = math.MinInt32, math.MaxInt32
	}
	switch {
	case s.Minimum != nil && s.Maximum != nil:
		lo = max(lo, lowest(*s.Minimum, s.ExclusiveMinimum))
		hi = min(hi, highest(*s.Maximum, s.ExclusiveMaximum))
	case s.Minimum != nil:
		lo = max(lo, lowest(*s.Minimum, s.ExclusiveMinimum))
		hi = min(hi, lo+defaultSpan)
	case s.Maximum != nil:
		hi = min(hi, highest(*s.Maximum, s.ExclusiveMaximum))
		lo = max(lo, hi-defaultSpan)
	default:
		lo, hi = max(lo, 0), min(hi, defaultSpan)
	}
	return lo, hi
}

// lowest is the smallest integer the minimum allows.
func lowest(minimum float64, exclusive bool) int64 {
	if exclusive {
		minimum = math.Floor(minimum) + 1
	}
	return int64(math.Max(math.Ceil(minimum), -safeInteger))
}

// highest is the largest integer the maximum allows.
func highest(maximum float64, exclusive bool) int64 {
	if exclusive {
		maximum = math.Ceil(maximum) - 1
	}
	return int64(math.Min(math.Floor(maximum), safeInteger))
}

func numberRange(s *schema) (float64, float64) {
	lo, hi := 0.0, float64(defaultSpan)
	if s.Minimum != nil {
		lo = *s.Minimum
		hi = lo + defaultSpan
	}
	if s.Maximum != nil {
		hi = *s.Maximum
		if s.Minimum == nil {
			lo = hi - defaultSpan
		}
	}
	if s.ExclusiveMinimum {
		lo = math.Nextafter(lo, math.Inf(1))
	}
	if s.ExclusiveMaximum {
		hi = math.Nextafter(hi, math.Inf(-1))
	}
	return lo, hi
}

func lengthRange(s *schema) (int, int) {
	return span(s.MinLength, s.MaxLength, defaultMinLength, defaultMaxLength)
}

func itemRange(s *schema) (int, int) {
	return span(s.MinItems, s.MaxItems, 0, defaultMaxItems)
}

func propertyRange(s *schema) (int, int) {
	return span(s.MinProperties, s.MaxProperties, 0, defaultMaxItems)
}

// span reads a length range, widening the default where the schema's own bound
// pushes past it.
func span(minimum, maximum *int64, defaultLo, defaultHi int) (int, int) {
	lo, hi := defaultLo, defaultHi
	if minimum != nil {
		lo = int(*minimum)
		hi = max(hi, lo)
	}
	if maximum != nil {
		hi = int(*maximum)
	}
	return lo, hi
}

// normalized makes a value decoded from a schema one an unstructured object
// may hold: a whole number becomes an int64, as the API server's own decoder
// writes it.
func normalized(value any) any {
	switch typed := value.(type) {
	case float64:
		if typed == math.Trunc(typed) && math.Abs(typed) <= safeInteger {
			return int64(typed)
		}
	case []any:
		for i, item := range typed {
			typed[i] = normalized(item)
		}
	case map[string]any:
		for name, item := range typed {
			typed[name] = normalized(item)
		}
	}
	return value
}
