package generate

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

// schema is the part of a CRD's OpenAPI v3 schema the generator reads. What
// it leaves out, it cannot generate from.
type schema struct {
	Type             string   `json:"type"`
	Format           string   `json:"format"`
	Enum             []any    `json:"enum"`
	Minimum          *float64 `json:"minimum"`
	Maximum          *float64 `json:"maximum"`
	ExclusiveMinimum bool     `json:"exclusiveMinimum"`
	ExclusiveMaximum bool     `json:"exclusiveMaximum"`
	MinLength        *int64   `json:"minLength"`
	MaxLength        *int64   `json:"maxLength"`
	Pattern          string   `json:"pattern"`
	MinItems         *int64   `json:"minItems"`
	MaxItems         *int64   `json:"maxItems"`
	// ListType is x-kubernetes-list-type: a set holds no duplicate item, and a
	// map holds no duplicate key.
	ListType    string             `json:"x-kubernetes-list-type"`
	IntOrString bool               `json:"x-kubernetes-int-or-string"`
	Items       *schema            `json:"items"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
	// AdditionalProperties is a map's values.
	AdditionalProperties mapValues `json:"additionalProperties"`
	MinProperties        *int64    `json:"minProperties"`
	MaxProperties        *int64    `json:"maxProperties"`
}

// mapValues is additionalProperties, a schema or a boolean. A boolean says
// nothing about the values.
type mapValues struct{ schema *schema }

func (m *mapValues) UnmarshalJSON(data []byte) error {
	var allowed bool
	if json.Unmarshal(data, &allowed) == nil {
		return nil
	}
	m.schema = &schema{}
	return json.Unmarshal(data, m.schema)
}

// field is one path the generator may change, and the values it may take.
type field struct {
	path   []string
	dotted string
	values *rapid.Generator[any]
	// optional says the schema does not require the field, so a mutation may
	// remove it.
	optional bool
}

// overlaid merges each overlay into the raw schema, in place, and reads the
// result.
func overlaid(root map[string]any, overlays map[string]map[string]any) (*schema, error) {
	for _, dotted := range slices.Sorted(maps.Keys(overlays)) {
		overlay := overlays[dotted]
		if unread := unreadKeywords(overlay, ""); len(unread) > 0 {
			return nil, fmt.Errorf("values.overlay %s: reconciler-fuzzer does not read %s; it reads %s",
				dotted, strings.Join(unread, ", "), strings.Join(keywords, ", "))
		}
		node, err := schemaNode(root, strings.Split(dotted, "."))
		if err != nil {
			return nil, fmt.Errorf("values.overlay %s: %w", dotted, err)
		}
		mergeInto(node, overlay)
	}
	return asSchema(root)
}

// primaryCRD finds the primary kind's openAPIV3Schema and plural among the
// config's CRDs.
func primaryCRD(cfg *config.Config) (map[string]any, string, error) {
	documents, err := readManifests(cfg.CRDs)
	if err != nil {
		return nil, "", fmt.Errorf("reading the CRDs: %w", err)
	}
	kind := cfg.Primary.Kind
	for _, document := range documents {
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		if spec["group"] != kind.Group || names["kind"] != kind.Kind {
			continue
		}
		plural, _ := names["plural"].(string)
		versions, _ := spec["versions"].([]any)
		for _, entry := range versions {
			version, _ := entry.(map[string]any)
			if version["name"] != kind.Version {
				continue
			}
			node, _ := version["schema"].(map[string]any)
			root, _ := node["openAPIV3Schema"].(map[string]any)
			if root == nil {
				return nil, "", fmt.Errorf("the CRD of %s carries no schema for %s", kind.Kind, kind.Version)
			}
			return root, plural, nil
		}
	}
	return nil, "", fmt.Errorf("the CRDs describe no %s", kind)
}

// keywords are the schema keywords the generator reads.
var keywords = func() []string {
	var read []string
	fields := reflect.TypeFor[schema]()
	for i := range fields.NumField() {
		read = append(read, fields.Field(i).Tag.Get("json"))
	}
	slices.Sort(read)
	return read
}()

// unreadKeywords are the keywords in an overlay that the generator ignores,
// as dotted paths inside it.
func unreadKeywords(overlay map[string]any, prefix string) []string {
	var unread []string
	for _, key := range slices.Sorted(maps.Keys(overlay)) {
		nested, _ := overlay[key].(map[string]any)
		switch {
		case !slices.Contains(keywords, key):
			unread = append(unread, prefix+key)
		case key == "items" || key == "additionalProperties":
			unread = append(unread, unreadKeywords(nested, prefix+key+".")...)
		case key == "properties":
			for _, name := range slices.Sorted(maps.Keys(nested)) {
				property, _ := nested[name].(map[string]any)
				unread = append(unread, unreadKeywords(property, prefix+key+"."+name+".")...)
			}
		}
	}
	return unread
}

// schemaNode walks a dotted path into a schema's properties.
func schemaNode(root map[string]any, path []string) (map[string]any, error) {
	node := root
	for i, segment := range path {
		properties, _ := node["properties"].(map[string]any)
		next, _ := properties[segment].(map[string]any)
		if next == nil {
			return nil, fmt.Errorf("the schema describes no %s", strings.Join(path[:i+1], "."))
		}
		node = next
	}
	return node, nil
}

// mergeInto merges the overlay into the schema node. The overlay wins wherever
// the two disagree, and keeps what it does not mention.
func mergeInto(node, overlay map[string]any) {
	for key, value := range overlay {
		tightening, isObject := value.(map[string]any)
		into, alsoObject := node[key].(map[string]any)
		if isObject && alsoObject {
			mergeInto(into, tightening)
			continue
		}
		node[key] = value
	}
}

// asSchema reads the raw schema into the part of it the generator uses.
func asSchema(root map[string]any) (*schema, error) {
	data, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("reading the schema: %w", err)
	}
	var read schema
	if err := json.Unmarshal(data, &read); err != nil {
		return nil, fmt.Errorf("reading the schema: %w", err)
	}
	return &read, nil
}

// mutableFields are the paths the generator may change: the allowlist in
// values.mutate, or every path under spec where it is absent. Without the
// allowlist, it also says which spec paths it leaves alone and why.
func mutableFields(mutate []string, s *schema, sample map[string]any) ([]field, []string, error) {
	if len(mutate) == 0 {
		fields, leftAlone := specFields(s, sample)
		return fields, leftAlone, nil
	}
	var fields []field
	for _, dotted := range slices.Compact(slices.Sorted(slices.Values(mutate))) {
		mutable, err := mutableField(s, dotted)
		if err != nil {
			return nil, nil, fmt.Errorf("values.mutate %s: %w", dotted, err)
		}
		fields = append(fields, mutable)
	}
	return fields, nil, nil
}

// mutableField reads the schema at a dotted path.
func mutableField(s *schema, dotted string) (field, error) {
	path := strings.Split(dotted, ".")
	at, optional := s, false
	for i, segment := range path {
		next := at.Properties[segment]
		if next == nil {
			return field{}, fmt.Errorf("the schema describes no %s", strings.Join(path[:i+1], "."))
		}
		optional = !slices.Contains(at.Required, segment)
		at = next
	}
	values, err := valuesOf(at)
	if err != nil {
		return field{}, err
	}
	return field{path: path, dotted: dotted, values: values, optional: optional}, nil
}

// specFields are the paths under spec that the schema says enough about to
// generate values for, in path order. Only spec moves: apiVersion, kind and
// metadata are not the target's input, and status is a subresource a CR op
// cannot write. A path the schema says too little about is left alone rather
// than guessed at.
func specFields(s *schema, sample map[string]any) ([]field, []string) {
	spec := s.Properties["spec"]
	if spec == nil {
		return nil, nil
	}
	w := &walker{sample: sample}
	return w.walk(spec, []string{"spec"}, !slices.Contains(s.Required, "spec")), w.leftAlone
}

// walker reads the target's sample, which says which objects a field may be
// set inside.
type walker struct {
	sample    map[string]any
	leftAlone []string
}

// walk descends into an object the sample carries and into one that requires
// no property of its own. It takes any other object whole, so that a field is
// never set inside an object the sample lacks and whose required properties
// would then be missing.
func (w *walker) walk(s *schema, path []string, optional bool) []field {
	if s.Type == "object" && len(s.Properties) > 0 && (len(s.Required) == 0 || w.carries(path)) {
		var fields []field
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			child := append(slices.Clone(path), name)
			fields = append(fields, w.walk(s.Properties[name], child, !slices.Contains(s.Required, name))...)
		}
		return fields
	}
	dotted := strings.Join(path, ".")
	values, err := valuesOf(s)
	if err != nil {
		w.leftAlone = append(w.leftAlone, leftAloneNote(dotted, err))
		return nil
	}
	return []field{{path: path, dotted: dotted, values: values, optional: optional}}
}

func leftAloneNote(dotted string, why error) string {
	return fmt.Sprintf("generation leaves %s alone: %v", dotted, why)
}

func (w *walker) carries(path []string) bool {
	value, found, err := unstructured.NestedFieldNoCopy(w.sample, path...)
	if err != nil || !found {
		return false
	}
	_, isObject := value.(map[string]any)
	return isObject
}
