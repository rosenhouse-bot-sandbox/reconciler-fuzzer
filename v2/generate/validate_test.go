package generate

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"unicode/utf8"
)

// validate is the test's own reading of an OpenAPI v3 schema. The generator
// must never produce a value it rejects.
func validate(s *schema, value any, path string) error {
	if len(s.Enum) > 0 {
		for _, allowed := range s.Enum {
			if equalJSON(allowed, value) {
				return nil
			}
		}
		return fmt.Errorf("%s is %v, which the enum %v does not allow", path, value, s.Enum)
	}
	switch s.Type {
	case "boolean":
		if _, isBool := value.(bool); !isBool {
			return wrongType(path, "boolean", value)
		}
	case "integer":
		n, isNumber := number(value)
		if !isNumber || n != math.Trunc(n) {
			return wrongType(path, "integer", value)
		}
		return withinBounds(s, n, path)
	case "number":
		n, isNumber := number(value)
		if !isNumber {
			return wrongType(path, "number", value)
		}
		return withinBounds(s, n, path)
	case "string":
		return validateString(s, value, path)
	case "array":
		return validateArray(s, value, path)
	case "object":
		return validateObject(s, value, path)
	}
	return nil // A schema that names no type constrains nothing.
}

func validateString(s *schema, value any, path string) error {
	text, isString := value.(string)
	if !isString {
		return wrongType(path, "string", value)
	}
	if length := int64(utf8.RuneCountInString(text)); (s.MinLength != nil && length < *s.MinLength) ||
		(s.MaxLength != nil && length > *s.MaxLength) {
		return fmt.Errorf("%s is %d runes long, outside minLength %v and maxLength %v",
			path, length, s.MinLength, s.MaxLength)
	}
	if s.Pattern != "" {
		matches, err := regexp.MatchString(s.Pattern, text)
		if err != nil {
			return fmt.Errorf("%s: the pattern %q does not compile: %w", path, s.Pattern, err)
		}
		if !matches {
			return fmt.Errorf("%s is %q, which the pattern %q does not match", path, text, s.Pattern)
		}
	}
	return nil
}

func validateArray(s *schema, value any, path string) error {
	items, isList := value.([]any)
	if !isList {
		return wrongType(path, "array", value)
	}
	if length := int64(len(items)); (s.MinItems != nil && length < *s.MinItems) ||
		(s.MaxItems != nil && length > *s.MaxItems) {
		return fmt.Errorf("%s holds %d items, outside minItems %v and maxItems %v",
			path, length, s.MinItems, s.MaxItems)
	}
	var keys []string
	for i, item := range items {
		if s.Items != nil {
			if err := validate(s.Items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		encoded, _ := json.Marshal(item)
		keys = append(keys, string(encoded))
	}
	slices.Sort(keys)
	if s.ListType == "set" && len(slices.Compact(keys)) != len(items) {
		return fmt.Errorf("%s is a set holding %v twice", path, items)
	}
	return nil
}

func validateObject(s *schema, value any, path string) error {
	object, isObject := value.(map[string]any)
	if !isObject {
		return wrongType(path, "object", value)
	}
	for _, name := range s.Required {
		if _, present := object[name]; !present {
			return fmt.Errorf("%s carries no %s, which the schema requires", path, name)
		}
	}
	if values := s.AdditionalProperties.schema; values != nil {
		return validateMap(s, values, object, path)
	}
	if len(s.Properties) == 0 {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		property := s.Properties[name]
		if property == nil {
			return fmt.Errorf("%s carries %s, which the schema does not describe", path, name)
		}
		if err := validate(property, object[name], join(path, name)); err != nil {
			return err
		}
	}
	return nil
}

func validateMap(s, values *schema, object map[string]any, path string) error {
	if count := int64(len(object)); (s.MinProperties != nil && count < *s.MinProperties) ||
		(s.MaxProperties != nil && count > *s.MaxProperties) {
		return fmt.Errorf("%s holds %d keys, outside minProperties %v and maxProperties %v",
			path, count, s.MinProperties, s.MaxProperties)
	}
	for _, name := range slices.Sorted(maps.Keys(object)) {
		if err := validate(values, object[name], join(path, name)); err != nil {
			return err
		}
	}
	return nil
}

func withinBounds(s *schema, value float64, path string) error {
	below := s.Minimum != nil && (value < *s.Minimum || (s.ExclusiveMinimum && value == *s.Minimum))
	above := s.Maximum != nil && (value > *s.Maximum || (s.ExclusiveMaximum && value == *s.Maximum))
	if below || above {
		return fmt.Errorf("%s is %v, outside minimum %v and maximum %v", path, value, s.Minimum, s.Maximum)
	}
	return nil
}

func wrongType(path, want string, value any) error {
	return fmt.Errorf("%s is %#v, want a %s", path, value, want)
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	}
	return 0, false
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// equalJSON compares values as the API server sees them, so that an integer
// drawn as an int64 equals the same number decoded as a float64.
func equalJSON(a, b any) bool {
	encodedA, errA := json.Marshal(a)
	encodedB, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(encodedA) == string(encodedB)
}
