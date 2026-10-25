package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Reference is one references entry: a field by which a CR of From names an
// object it does not own. Load reads each kind's scope from the CRDs, or
// else from a table of core kinds. It refuses an entry whose From is not
// listed, whose From or To primaries lists at another version, whose To has
// no known scope or no Ready, whose To the From entry's owns names, or
// whose Name the From entry's values.distinct lists; a Namespace on a
// cluster-scoped or fake To; a namespaced To of a cluster-scoped From
// without a Namespace; a Namespace that does not read with [*] each list it
// shares with Name, or that reads another list; and a second entry of one
// From and Name.
type Reference struct {
	// From is a listed kind.
	From schema.GroupVersionKind
	// To is a kube kind. A fake's collection, written <system>:<collection>,
	// leaves To empty and sets System and Collection.
	To                 schema.GroupVersionKind
	System, Collection string
	// Namespaced says To is a namespaced kube kind.
	Namespaced bool
	// Name and Namespace are field paths in the From CR, as in
	// values.mutate. [*] names a field of each list item. Namespace is
	// empty when unset.
	Name, Namespace string
	// Ready is CEL over self, the to object: the entry's, or else the
	// ready of To's listed kind, model or fake.
	Ready string
	// Required defaults to true.
	Required bool
	// Track is zero when unset.
	Track time.Duration
}

type declaredReference struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Ready     string `json:"ready"`
	Required  *bool  `json:"required"`
	Track     string `json:"track"`
}

// references loads the references entries. Only a config with entries
// reads the CRDs.
func (c *Config) references(declared []declaredReference) ([]Reference, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	scopes, err := crdScopes(c.CRDs)
	if err != nil {
		return nil, err
	}
	var references []Reference
	for i, d := range declared {
		key := fmt.Sprintf("references[%d]", i)
		r, err := c.reference(key, d, scopes)
		if err != nil {
			return nil, err
		}
		if j := slices.IndexFunc(references, func(other Reference) bool {
			return other.From.GroupKind() == r.From.GroupKind() && other.Name == r.Name
		}); j >= 0 {
			return nil, fmt.Errorf("%s.name %s: references[%d] has this from and name", key, r.Name, j)
		}
		references = append(references, r)
	}
	return references, nil
}

func (c *Config) reference(key string, d declaredReference, scopes map[schema.GroupKind]bool) (Reference, error) {
	r := Reference{Name: d.Name, Namespace: d.Namespace, Ready: d.Ready, Required: d.Required == nil || *d.Required}
	if d.From == "" {
		return Reference{}, fmt.Errorf("%s.from is required", key)
	}
	var err error
	if r.From, err = parseGVK(d.From); err != nil {
		return Reference{}, fmt.Errorf("%s.from %q: %w", key, d.From, err)
	}
	from := c.listed(r.From.GroupKind())
	if from < 0 {
		return Reference{}, fmt.Errorf("%s.from %s: primaries does not list this kind", key, d.From)
	}
	if err := c.atListedVersion(r.From); err != nil {
		return Reference{}, fmt.Errorf("%s.from %s: %w", key, d.From, err)
	}

	var fake System
	switch {
	case d.To == "":
		return Reference{}, fmt.Errorf("%s.to is required", key)
	case strings.Contains(d.To, ":"):
		if fake, r.Collection, err = fakeKind(d.To, c.Fakes()); err != nil {
			return Reference{}, fmt.Errorf("%s.to %s: %w", key, d.To, err)
		}
		r.System = fake.Name
	default:
		if r.To, err = parseGVK(d.To); err != nil {
			return Reference{}, fmt.Errorf("%s.to %q: %w", key, d.To, err)
		}
		if err := c.atListedVersion(r.To); err != nil {
			return Reference{}, fmt.Errorf("%s.to %s: %w", key, d.To, err)
		}
		var known bool
		if r.Namespaced, known = scopeOf(r.To.GroupKind(), scopes); !known {
			return Reference{}, fmt.Errorf("%s.to %s: neither crds nor the core kinds give its scope", key, d.To)
		}
	}
	if slices.ContainsFunc(c.Primaries[from].Owns, func(o Owns) bool {
		return o.System == r.System && o.Collection == r.Collection && o.Kind.GroupKind() == r.To.GroupKind()
	}) {
		return Reference{}, fmt.Errorf("%s.to %s: the from entry owns this kind", key, d.To)
	}

	if r.Name == "" {
		return Reference{}, fmt.Errorf("%s.name is required", key)
	}
	name, err := parsePath(r.Name)
	if err != nil {
		return Reference{}, fmt.Errorf("%s.name %q: %w", key, r.Name, err)
	}
	if slices.Contains(c.Primaries[from].Values.Distinct, r.Name) {
		return Reference{}, fmt.Errorf("%s.name %s: the from entry's values.distinct lists it", key, r.Name)
	}
	if err := checkNamespace(key+".namespace", r, name, scopes); err != nil {
		return Reference{}, err
	}

	if r.Ready == "" {
		if r.Ready = c.defaultReady(r, fake); r.Ready == "" {
			return Reference{}, fmt.Errorf("%s.ready is required: %s is no listed kind, model or fake with a ready", key, d.To)
		}
	} else if _, err := CompileReady(r.Ready); err != nil {
		return Reference{}, fmt.Errorf("%s.ready: %w", key, err)
	}

	if d.Track != "" {
		if r.Track, err = time.ParseDuration(d.Track); err != nil || r.Track <= 0 {
			return Reference{}, fmt.Errorf("%s.track %q: want a positive duration", key, d.Track)
		}
	}
	return r, nil
}

// checkNamespace refuses a namespace path that is malformed, that reads
// lists otherwise than name does, or that the to kind has no use for.
// Without a namespace path, it refuses a namespaced to of a from that the
// CRDs say is cluster-scoped.
func checkNamespace(key string, r Reference, name []pathSegment, scopes map[schema.GroupKind]bool) error {
	if r.Namespace == "" {
		if fromNamespaced, known := scopeOf(r.From.GroupKind(), scopes); r.Namespaced && known && !fromNamespaced {
			return fmt.Errorf("%s is required: the from kind is cluster-scoped and the to kind namespaced", key)
		}
		return nil
	}
	namespace, err := parsePath(r.Namespace)
	switch {
	case err != nil:
		return fmt.Errorf("%s %q: %w", key, r.Namespace, err)
	case !listsAgree(namespace, name):
		return fmt.Errorf("%s %s: it must read with [*] each list it shares with name, and no other list", key, r.Namespace)
	case r.System != "":
		return fmt.Errorf("%s: a fake's resources have no namespace", key)
	case !r.Namespaced:
		return fmt.Errorf("%s: the to kind is cluster-scoped", key)
	}
	return nil
}

// listed is the index of kind's primaries entry, or -1.
func (c *Config) listed(kind schema.GroupKind) int {
	return slices.Index(c.ListedKinds(), kind)
}

// atListedVersion refuses a kind that primaries lists at another version.
func (c *Config) atListedVersion(kind schema.GroupVersionKind) error {
	if i := c.listed(kind.GroupKind()); i >= 0 && c.Primaries[i].Kind.Version != kind.Version {
		return fmt.Errorf("primaries lists this kind at version %s", c.Primaries[i].Kind.Version)
	}
	return nil
}

// defaultReady is the ready of the fake, or of the to kind's listed entry
// or model, or empty.
func (c *Config) defaultReady(r Reference, fake System) string {
	if r.System != "" {
		return fake.Ready
	}
	if i := c.listed(r.To.GroupKind()); i >= 0 {
		return c.Primaries[i].Ready
	}
	if slices.ContainsFunc(c.Models, func(m Model) bool { return m.Kind.GroupKind() == r.To.GroupKind() }) {
		return ReadyConditionCEL
	}
	return ""
}

// fakeKind reads <system>:<collection>, which names a collection of a
// declared fake.
func fakeKind(declared string, fakes []System) (System, string, error) {
	system, collection, _ := strings.Cut(declared, ":")
	if !systemName.MatchString(collection) {
		return System{}, "", fmt.Errorf("want <system>:<collection>, with a collection matching %s", systemName)
	}
	i := slices.IndexFunc(fakes, func(s System) bool { return s.Name == system })
	if i < 0 {
		return System{}, "", fmt.Errorf("systems declares no fake %s", system)
	}
	return fakes[i], collection, nil
}

// coreScopes says whether each kind of the core group is namespaced.
var coreScopes = map[string]bool{
	"ConfigMap": true, "Endpoints": true, "Event": true, "LimitRange": true, "PersistentVolumeClaim": true, "Pod": true,
	"PodTemplate": true, "ReplicationController": true, "ResourceQuota": true, "Secret": true, "Service": true, "ServiceAccount": true,
	"Namespace": false, "Node": false, "PersistentVolume": false,
}

// scopeOf says whether kind is namespaced, by the CRDs' scopes or the core
// kinds, and whether either knows it.
func scopeOf(kind schema.GroupKind, crds map[schema.GroupKind]bool) (namespaced, known bool) {
	if namespaced, known = crds[kind]; known || kind.Group != "" {
		return namespaced, known
	}
	namespaced, known = coreScopes[kind.Kind]
	return namespaced, known
}

// crdScopes says whether each kind that the CRDs describe is namespaced. A
// CRD without a scope is namespaced, as Kubernetes defaulted it before
// apiextensions.k8s.io/v1.
func crdScopes(paths []string) (map[schema.GroupKind]bool, error) {
	documents, err := ReadManifests(paths)
	if err != nil {
		return nil, fmt.Errorf("crds: %w", err)
	}
	scopes := map[schema.GroupKind]bool{}
	for _, document := range documents {
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		group, _ := spec["group"].(string)
		kind, _ := names["kind"].(string)
		scopes[schema.GroupKind{Group: group, Kind: kind}] = spec["scope"] != "Cluster"
	}
	return scopes, nil
}

// pathSegment is one dotted part of a field path. each says that the field
// holds a list, and that the path goes on in each item.
type pathSegment struct {
	field string
	each  bool
}

// parsePath splits a field path at its dots. A segment is a field, or a
// field and [*]. The last segment is a field.
func parsePath(dotted string) ([]pathSegment, error) {
	var path []pathSegment
	for _, part := range strings.Split(dotted, ".") {
		field, each := strings.CutSuffix(part, "[*]")
		if field == "" || strings.ContainsAny(field, "[]") {
			return nil, fmt.Errorf("%q is neither a field nor a field and [*]", part)
		}
		path = append(path, pathSegment{field, each})
	}
	if path[len(path)-1].each {
		return nil, errors.New("[*] names a field of each list item, so a field must follow it")
	}
	return path, nil
}

// lastEach is the index of path's last list segment, or -1.
func lastEach(path []pathSegment) int {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i].each {
			return i
		}
	}
	return -1
}

// listsAgree says whether namespace reads with [*] each list in the start it
// shares with name, and no other list.
func listsAgree(namespace, name []pathSegment) bool {
	shared := true
	for i, s := range namespace {
		shared = shared && i < len(name) && s.field == name[i].field
		if s.each != (shared && name[i].each) {
			return false
		}
	}
	return true
}

// ReferencedNames returns the name that each item of the name path holds in
// obj, in list order, with the namespace path's value. An empty, absent or
// non-string name names nothing. Where namespace has [*], it reads the item
// that name reads. Namespace is empty where its path is unset or holds no
// string.
func ReferencedNames(name, namespace string, obj map[string]any) []types.NamespacedName {
	namePath, err := parsePath(name)
	if err != nil {
		return nil
	}
	namespacePath, _ := parsePath(namespace)
	// The walk reads the namespace at the item that its last [*] reads, or
	// else at obj.
	shared := lastEach(namespacePath) + 1
	var names []types.NamespacedName
	var walk func(at any, i int, ns string)
	walk = func(at any, i int, ns string) {
		if i == shared {
			ns = stringAt(at, namespacePath[shared:])
		}
		object, _ := at.(map[string]any)
		value := object[namePath[i].field]
		switch {
		case i == len(namePath)-1:
			if name, _ := value.(string); name != "" {
				names = append(names, types.NamespacedName{Namespace: ns, Name: name})
			}
		case namePath[i].each:
			items, _ := value.([]any)
			for _, item := range items {
				walk(item, i+1, ns)
			}
		default:
			walk(value, i+1, ns)
		}
	}
	walk(obj, 0, "")
	return names
}

// stringAt is the string at path in at, or empty. path has no list
// segment.
func stringAt(at any, path []pathSegment) string {
	for _, s := range path {
		object, _ := at.(map[string]any)
		at = object[s.field]
	}
	value, _ := at.(string)
	return value
}

// DeleteOrder is the listed kinds, each next the first in config order that
// no reference from another kind still to come names as To. Where
// references form a cycle, the first remaining kind in config order comes
// next.
func (c *Config) DeleteOrder() []schema.GroupKind {
	remaining := c.ListedKinds()
	var order []schema.GroupKind
	for len(remaining) > 0 {
		next := max(0, slices.IndexFunc(remaining, func(kind schema.GroupKind) bool {
			return !slices.ContainsFunc(c.References, func(r Reference) bool {
				from := r.From.GroupKind()
				return r.To.GroupKind() == kind && from != kind && slices.Contains(remaining, from)
			})
		}))
		order = append(order, remaining[next])
		remaining = slices.Delete(remaining, next, next+1)
	}
	return order
}
