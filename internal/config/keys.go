package config

import (
	"fmt"
	"reflect"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/suggest"
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
	for _, field := range keyFields(declared) {
		if strings.EqualFold(jsonName(field), key) {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// keyFields are declared's fields, with an embedded struct's fields in its
// place, as the JSON decoder promotes them.
func keyFields(declared reflect.Type) []reflect.StructField {
	var fields []reflect.StructField
	for field := range declared.Fields() {
		if field.Anonymous {
			fields = append(fields, keyFields(field.Type)...)
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

func jsonName(field reflect.StructField) string {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	return name
}

func notAKey(key *yamlv3.Node, keyPath, parent string, declared reflect.Type) error {
	var names []string
	for _, field := range keyFields(declared) {
		names = append(names, jsonName(field))
	}
	if near := suggest.Nearest(key.Value, names); near != "" {
		return fmt.Errorf("line %d: %s is not a key; did you mean %s?", key.Line, keyPath, near)
	}
	if parent == "" {
		parent = "the config"
	}
	last := len(names) - 1
	return fmt.Errorf("line %d: %s is not a key; %s takes %s and %s",
		key.Line, keyPath, parent, strings.Join(names[:last], ", "), names[last])
}
