package config

import (
	"fmt"
	"reflect"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
)

// unknownKey finds the first key in the config that the declaration has no
// field for, and says where it is. It is nil where it finds none.
func unknownKey(data []byte) error {
	var document yamlv3.Node
	_ = yamlv3.Unmarshal(data, &document) // It leaves YAML it cannot parse empty.
	if len(document.Content) == 0 {
		return nil
	}
	return keyIn(document.Content[0], reflect.TypeFor[declaration](), "")
}

func keyIn(node *yamlv3.Node, declared reflect.Type, path string) error {
	if declared.Kind() == reflect.Pointer {
		declared = declared.Elem()
	}
	switch {
	case declared.Kind() == reflect.Slice && node.Kind == yamlv3.SequenceNode:
		for i, item := range node.Content {
			if err := keyIn(item, declared.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case declared.Kind() == reflect.Map && node.Kind == yamlv3.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			if err := keyIn(node.Content[i+1], declared.Elem(), path+"."+node.Content[i].Value); err != nil {
				return err
			}
		}
	case declared.Kind() == reflect.Struct && node.Kind == yamlv3.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				continue
			}
			keyPath := key.Value
			if path != "" {
				keyPath = path + "." + key.Value
			}
			field, known := fieldNamed(declared, key.Value)
			if !known {
				return notAKey(key, keyPath, path, declared)
			}
			if err := keyIn(value, field.Type, keyPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// fieldNamed matches a key as the JSON decoder does, whatever its case.
func fieldNamed(declared reflect.Type, key string) (reflect.StructField, bool) {
	for field := range declared.Fields() {
		if strings.EqualFold(jsonName(field), key) {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}

func notAKey(key *yamlv3.Node, keyPath, parent string, declared reflect.Type) error {
	var names []string
	for field := range declared.Fields() {
		names = append(names, jsonName(field))
	}
	if near := nearest(key.Value, names); near != "" {
		return fmt.Errorf("line %d: %s is not a key; did you mean %s?", key.Line, keyPath, near)
	}
	if parent == "" {
		parent = "the config"
	}
	last := len(names) - 1
	return fmt.Errorf("line %d: %s is not a key; %s takes %s and %s",
		key.Line, keyPath, parent, strings.Join(names[:last], ", "), names[last])
}

// nearest is the name closest to key within two edits, or empty.
func nearest(key string, names []string) string {
	best, bestDistance := "", 3
	for _, name := range names {
		if distance := editDistance(strings.ToLower(key), strings.ToLower(name)); distance < bestDistance && distance < len(key) {
			best, bestDistance = name, distance
		}
	}
	return best
}

// editDistance counts the fewest insertions, deletions, substitutions and
// swaps of adjacent letters that turn a into b.
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			substitution := d[i-1][j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, substitution)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}
