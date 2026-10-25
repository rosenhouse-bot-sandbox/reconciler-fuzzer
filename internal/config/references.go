package config

import (
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Reference is one references entry: a field by which a CR of From names an
// object it does not own.
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

// ReferencedNames returns the namespace and name that each item of the name
// path holds in obj, in list order, with the namespace path's value. An
// empty or absent name names nothing. Namespace is empty where its path is
// unset, empty or absent.
func ReferencedNames(name, namespace string, obj map[string]any) []types.NamespacedName {
	return nil
}

// DeleteOrder is the listed kinds in config order, except that each
// reference's From precedes its To. A cycle keeps config order.
func (c *Config) DeleteOrder() []schema.GroupKind {
	return c.ListedKinds()
}
