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
	crds, err := readCRDs(c.CRDs)
	if err != nil {
		return nil, err
	}
	var references []Reference
	for i, d := range declared {
		key := fmt.Sprintf("references[%d]", i)
		r, err := c.reference(key, d, crds)
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

func (c *Config) reference(key string, d declaredReference, crds map[schema.GroupKind]crd) (Reference, error) {
	r := Reference{Name: d.Name, Namespace: d.Namespace, Ready: d.Ready, Required: d.Required == nil || *d.Required}
	if d.From == "" {
		return Reference{}, fmt.Errorf("%s.from is required", key)
	}
	var err error
	if r.From, err = parseGVK(d.From); err != nil {
		return Reference{}, fmt.Errorf("%s.from %q: %w", key, d.From, err)
	}
	from := c.listed(r.From.GroupKind())
	switch {
	case from < 0:
		return Reference{}, fmt.Errorf("%s.from %s: primaries does not list this kind", key, d.From)
	case c.Primaries[from].Kind != r.From:
		return Reference{}, fmt.Errorf("%s.from %s: primaries lists this kind at version %s", key, d.From, c.Primaries[from].Kind.Version)
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
		if err := c.atDeclaredVersion(r.To); err != nil {
			return Reference{}, fmt.Errorf("%s.to %s: %w", key, d.To, err)
		}
		var known bool
		if r.Namespaced, known = scopeOf(r.To.GroupKind(), crds); !known {
			return Reference{}, fmt.Errorf("%s.to %s: %s", key, d.To, unknownKind(r.To, crds))
		}
	}

	if r.Name == "" {
		return Reference{}, fmt.Errorf("%s.name is required", key)
	}
	name, err := parsePath(r.Name)
	if err != nil {
		return Reference{}, fmt.Errorf("%s.name %q: %w", key, r.Name, err)
	}
	if slices.ContainsFunc(c.Primaries[from].Owns, func(o Owns) bool {
		return o.System == r.System && o.Collection == r.Collection && o.Kind.GroupKind() == r.To.GroupKind() && o.Name == "self."+r.Name
	}) {
		return Reference{}, fmt.Errorf("%s.name %s: the from entry owns the %s it names", key, r.Name, d.To)
	}
	if slices.Contains(c.Primaries[from].Values.Distinct, r.Name) {
		return Reference{}, fmt.Errorf("%s.name %s: the from entry's values.distinct lists it", key, r.Name)
	}
	if err := stringField(crds[r.From.GroupKind()].schemas[r.From.Version], name); err != nil {
		return Reference{}, fmt.Errorf("%s.name %s: %w", key, r.Name, err)
	}
	if err := checkNamespace(key+".namespace", r, name, crds); err != nil {
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
// lists otherwise than name does, that the to kind has no use for, or that
// the from kind's schema describes as no string. Without a namespace path,
// it refuses a namespaced to of a from that the CRDs say is cluster-scoped.
func checkNamespace(key string, r Reference, name []pathSegment, crds map[schema.GroupKind]crd) error {
	if r.Namespace == "" {
		if fromNamespaced, known := scopeOf(r.From.GroupKind(), crds); r.Namespaced && known && !fromNamespaced {
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
	if err := stringField(crds[r.From.GroupKind()].schemas[r.From.Version], namespace); err != nil {
		return fmt.Errorf("%s %s: %w", key, r.Namespace, err)
	}
	return nil
}

// stringField refuses a path that root, an openAPIV3Schema, describes as
// no string: it names a field that an object's properties leave out, or a
// field of another type. A list needs [*], and [*] needs a list. Where the
// schema says too little, as under metadata, which the API server defines,
// any path passes.
func stringField(root map[string]any, path []pathSegment) error {
	at := root
	for i, s := range path {
		properties, _ := at["properties"].(map[string]any)
		next, described := properties[s.field].(map[string]any)
		switch {
		case i == 0 && s.field == "metadata":
			return nil
		case !described && (len(properties) == 0 || at["x-kubernetes-preserve-unknown-fields"] == true):
			return nil
		case !described:
			return fmt.Errorf("the from kind's schema describes no %s", dotted(path[:i+1]))
		}
		want := "object"
		switch {
		case s.each:
			want = "array"
		case i == len(path)-1:
			want = "string"
		}
		if t, _ := next["type"].(string); t != "" && t != want {
			return fmt.Errorf("the from kind's schema gives %s type %s, not %s", strings.TrimSuffix(dotted(path[:i+1]), "[*]"), t, want)
		}
		if s.each {
			next, _ = next["items"].(map[string]any)
			if t, _ := next["type"].(string); t != "" && t != "object" {
				return fmt.Errorf("the from kind's schema gives %s type %s, not object", dotted(path[:i+1]), t)
			}
		}
		at = next
	}
	return nil
}

// dotted writes path as a field path.
func dotted(path []pathSegment) string {
	parts := make([]string, len(path))
	for i, s := range path {
		parts[i] = s.field
		if s.each {
			parts[i] += "[*]"
		}
	}
	return strings.Join(parts, ".")
}

// listed is the index of kind's primaries entry, or -1.
func (c *Config) listed(kind schema.GroupKind) int {
	return slices.IndexFunc(c.Primaries, func(p Primary) bool { return p.Kind.GroupKind() == kind })
}

// atDeclaredVersion refuses a kind that primaries or models declare at
// another version.
func (c *Config) atDeclaredVersion(kind schema.GroupVersionKind) error {
	var declared []schema.GroupVersionKind
	for _, p := range c.Primaries {
		declared = append(declared, p.Kind)
	}
	for _, m := range c.Models {
		declared = append(declared, m.Kind)
	}
	for _, d := range declared {
		if d.GroupKind() == kind.GroupKind() && d.Version != kind.Version {
			return fmt.Errorf("the config declares this kind at version %s", d.Version)
		}
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

// builtinKinds are the kinds that Kubernetes 1.37 serves without a CRD and
// that a client can watch, by whether they are namespaced.
var builtinKinds = map[bool][]string{
	true: {
		"v1/ConfigMap", "v1/Endpoints", "v1/Event", "v1/LimitRange", "v1/PersistentVolumeClaim", "v1/Pod", "v1/PodTemplate",
		"v1/ReplicationController", "v1/ResourceQuota", "v1/Secret", "v1/Service", "v1/ServiceAccount",
		"apps/v1/ControllerRevision", "apps/v1/DaemonSet", "apps/v1/Deployment", "apps/v1/ReplicaSet", "apps/v1/StatefulSet",
		"autoscaling/v2/HorizontalPodAutoscaler", "batch/v1/CronJob", "batch/v1/Job",
		"certificates.k8s.io/v1/PodCertificateRequest", "coordination.k8s.io/v1/Lease", "discovery.k8s.io/v1/EndpointSlice",
		"events.k8s.io/v1/Event", "networking.k8s.io/v1/Ingress", "networking.k8s.io/v1/NetworkPolicy",
		"policy/v1/PodDisruptionBudget", "rbac.authorization.k8s.io/v1/Role", "rbac.authorization.k8s.io/v1/RoleBinding",
		"resource.k8s.io/v1/ResourceClaim", "resource.k8s.io/v1/ResourceClaimTemplate", "storage.k8s.io/v1/CSIStorageCapacity",
	},
	false: {
		"v1/Namespace", "v1/Node", "v1/PersistentVolume",
		"admissionregistration.k8s.io/v1/MutatingAdmissionPolicy", "admissionregistration.k8s.io/v1/MutatingAdmissionPolicyBinding",
		"admissionregistration.k8s.io/v1/MutatingWebhookConfiguration", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy",
		"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding", "admissionregistration.k8s.io/v1/ValidatingWebhookConfiguration",
		"apiextensions.k8s.io/v1/CustomResourceDefinition", "apiregistration.k8s.io/v1/APIService",
		"certificates.k8s.io/v1/CertificateSigningRequest", "certificates.k8s.io/v1/ClusterTrustBundle",
		"flowcontrol.apiserver.k8s.io/v1/FlowSchema", "flowcontrol.apiserver.k8s.io/v1/PriorityLevelConfiguration",
		"networking.k8s.io/v1/IngressClass", "networking.k8s.io/v1/IPAddress", "networking.k8s.io/v1/ServiceCIDR",
		"node.k8s.io/v1/RuntimeClass", "rbac.authorization.k8s.io/v1/ClusterRole", "rbac.authorization.k8s.io/v1/ClusterRoleBinding",
		"resource.k8s.io/v1/DeviceClass", "resource.k8s.io/v1/DeviceTaintRule", "resource.k8s.io/v1/ResourceSlice",
		"scheduling.k8s.io/v1/PriorityClass", "storage.k8s.io/v1/CSIDriver", "storage.k8s.io/v1/CSINode", "storage.k8s.io/v1/StorageClass",
		"storage.k8s.io/v1/VolumeAttachment", "storage.k8s.io/v1/VolumeAttributesClass", "storagemigration.k8s.io/v1/StorageVersionMigration",
	},
}

// builtins holds each of builtinKinds by group and kind.
var builtins = func() map[schema.GroupKind]builtin {
	kinds := map[schema.GroupKind]builtin{}
	for namespaced, spellings := range builtinKinds {
		for _, spelling := range spellings {
			kind, err := parseGVK(spelling)
			if err != nil {
				panic(err)
			}
			kinds[kind.GroupKind()] = builtin{spelling, namespaced}
		}
	}
	return kinds
}()

type builtin struct {
	spelling   string
	namespaced bool
}

// scopeOf says whether kind is namespaced, by the CRDs or the built-in
// kinds, and whether either knows it.
func scopeOf(kind schema.GroupKind, crds map[schema.GroupKind]crd) (namespaced, known bool) {
	if c, found := crds[kind]; found {
		return c.namespaced, true
	}
	b, found := builtins[kind]
	return b.namespaced, found
}

// unknownKind says that no CRD and no built-in kind is kind, and suggests
// those whose kind differs from kind's only in case.
func unknownKind(kind schema.GroupVersionKind, crds map[schema.GroupKind]crd) string {
	var near []string
	for known, b := range builtins {
		if strings.EqualFold(known.Kind, kind.Kind) {
			near = append(near, b.spelling)
		}
	}
	for known := range crds {
		if strings.EqualFold(known.Kind, kind.Kind) {
			near = append(near, known.WithVersion(kind.Version).GroupVersion().String()+"/"+known.Kind)
		}
	}
	why := "no CRD in crds and no built-in kind has this group and kind"
	if len(near) == 0 {
		return why
	}
	slices.Sort(near)
	return fmt.Sprintf("%s; did you mean %s?", why, strings.Join(near, " or "))
}

// crd is what references read of a CustomResourceDefinition.
type crd struct {
	namespaced bool
	// schemas holds each version's openAPIV3Schema.
	schemas map[string]map[string]any
}

// readCRDs reads each CustomResourceDefinition in paths, by the kind it
// describes. A CRD without a scope is namespaced, as Kubernetes defaulted
// it before apiextensions.k8s.io/v1.
func readCRDs(paths []string) (map[schema.GroupKind]crd, error) {
	documents, err := ReadManifests(paths)
	if err != nil {
		return nil, fmt.Errorf("crds: %w", err)
	}
	crds := map[schema.GroupKind]crd{}
	for _, document := range documents {
		if document["kind"] != "CustomResourceDefinition" {
			continue
		}
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		group, _ := spec["group"].(string)
		kind, _ := names["kind"].(string)
		c := crd{namespaced: spec["scope"] != "Cluster", schemas: map[string]map[string]any{}}
		versions, _ := spec["versions"].([]any)
		for _, v := range versions {
			version, _ := v.(map[string]any)
			name, _ := version["name"].(string)
			validation, _ := version["schema"].(map[string]any)
			c.schemas[name], _ = validation["openAPIV3Schema"].(map[string]any)
		}
		crds[schema.GroupKind{Group: group, Kind: kind}] = c
	}
	return crds, nil
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

// DeleteOrder lists each listed kind before the kinds it names as To.
// Config order breaks ties and cycles.
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
