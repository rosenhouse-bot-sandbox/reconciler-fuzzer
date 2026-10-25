package config

import (
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Reference is one references entry: a field by which a CR of From names an
// object it does not own. Load reads each kind's scope from the CRDs, or
// else from a table of core kinds. It refuses an entry whose From is not
// listed, whose To has no known scope or no Ready, or whose To the From
// entry's owns names; a Namespace on a cluster-scoped or fake To; a
// namespaced To of a cluster-scoped From without a Namespace; a Namespace
// with [*] that does not read the list item Name reads; and a second entry
// of one From and Name.
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

// ReferencedNames returns the name that each item of the name path holds in
// obj, in list order, with the namespace path's value. An empty, absent or
// non-string name names nothing. Where namespace has [*], it reads the item
// that name reads. Namespace is empty where its path is unset or holds no
// string.
func ReferencedNames(name, namespace string, obj map[string]any) []types.NamespacedName {
	return nil
}

// DeleteOrder is the listed kinds, each next the first in config order that
// no reference of a kind still to come names as To. Where references form a
// cycle, the first remaining kind in config order comes next.
func (c *Config) DeleteOrder() []schema.GroupKind {
	return c.ListedKinds()
}
