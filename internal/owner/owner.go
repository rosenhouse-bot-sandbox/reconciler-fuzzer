// Package owner reads instances, their creators, owning CRs and roles from
// a trace. Checks and the runner's damage action use it.
package owner

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Instance is one life of an object, from its create to its delete.
type Instance struct {
	Ref trace.Ref
	UID string
	// Created is the At of its first version. Gone is the At of its delete
	// version, or of the first its system's gone CEL accepts, or zero while
	// it lives.
	Created time.Duration
	Gone    time.Duration
	// Creator is the caller of the create, or the 201 apply, whose response
	// carried UID, or trace.Cluster.
	Creator trace.Caller
	// Role is its system's role CEL over its first body. Without one, a
	// kube instance's is its generateName prefix, or else its name.
	Role string
}

// Live reports whether the instance exists at t.
func (i Instance) Live(t time.Duration) bool {
	return i.Created <= t && (i.Gone == 0 || i.Gone > t)
}

// Owner is an instance's owning CR.
type Owner struct {
	// Rule is 1 for an ownerReference chain to a CR or to an unlisted owner,
	// 2 for a resource that names its CR, 3 for a CR that names the resource,
	// and 4 for the set of other CRs alive at the instance's creation that the
	// controller did not create.
	Rule int
	// CRs holds CR UIDs in order: one for rules 1 to 3, the set for rule 4.
	// It is empty for a shared instance.
	CRs []string
	// Unlisted is, for rule 1 with no CR, the UID of the live instance that
	// the controller did not create and that ownerReferences name, directly
	// or through live instances that the controller created.
	Unlisted string
}

// Rules hold the declared owning-CR rules 2 and 3.
type Rules struct {
	// Owners maps a system's name to rule 2 over its resources' bodies.
	Owners map[string]func(self map[string]any) (string, error)
	// Owns are rule 3.
	Owns []OwnsRule
	// Roles and Gone map a fake's name to its role and gone CEL.
	Roles map[string]func(self map[string]any) (string, error)
	Gone  map[string]func(self map[string]any) (bool, error)
}

// OwnsRule is one owns entry over the body of a CR of its entry's kind.
type OwnsRule struct {
	System, Group, Resource string
	// CRGroup and CRResource name the entry's kind.
	CRGroup, CRResource string
	Name                func(cr map[string]any) (string, error)
	// Namespace is nil when unset: the CR's namespace, or cluster-scoped.
	Namespace func(cr map[string]any) (string, error)
}

// Compile builds Rules from a Run event's owners, primaries and systems.
func Compile(owners map[string]string, primaries []trace.Primary, systems []trace.System) (Rules, error) {
	rules := Rules{
		Owners: map[string]func(map[string]any) (string, error){},
		Roles:  map[string]func(map[string]any) (string, error){},
		Gone:   map[string]func(map[string]any) (bool, error){},
	}
	for system, expr := range owners {
		eval, err := config.CompileString(expr)
		if err != nil {
			return Rules{}, fmt.Errorf("owner of system %s: %w", system, err)
		}
		rules.Owners[system] = eval
	}
	for _, s := range systems {
		var err error
		if s.Role != "" {
			if rules.Roles[s.Name], err = config.CompileString(s.Role); err != nil {
				return Rules{}, fmt.Errorf("role of system %s: %w", s.Name, err)
			}
		}
		if s.Gone != "" {
			if rules.Gone[s.Name], err = config.CompileReady(s.Gone); err != nil {
				return Rules{}, fmt.Errorf("gone of system %s: %w", s.Name, err)
			}
		}
	}
	for _, p := range primaries {
		for _, o := range p.Owns {
			rule := OwnsRule{System: cmp.Or(o.System, "kube"), Group: o.Kind.Group, Resource: o.Kind.Resource, CRGroup: p.Kind.Group, CRResource: p.Kind.Resource}
			var err error
			if rule.Name, err = config.CompileString(o.Name); err != nil {
				return Rules{}, fmt.Errorf("owns %s name: %w", o.Kind.Kind, err)
			}
			if o.Namespace != "" {
				if rule.Namespace, err = config.CompileString(o.Namespace); err != nil {
					return Rules{}, fmt.Errorf("owns %s namespace: %w", o.Kind.Kind, err)
				}
			}
			rules.Owns = append(rules.Owns, rule)
		}
	}
	return rules, nil
}

// Sources returns c's rule 2 by system, and its primaries entries, which
// hold rule 3, for Compile and the run event. resource maps a kind to its
// resource and says whether the kind is namespaced.
func Sources(c *config.Config, resource func(schema.GroupVersionKind) (string, bool, error)) (map[string]string, []trace.Primary, error) {
	var owners map[string]string
	for _, s := range c.Systems {
		if s.Owner != "" {
			if owners == nil {
				owners = map[string]string{}
			}
			owners[s.Name] = s.Owner
		}
	}
	var primaries []trace.Primary
	for _, p := range c.Primaries {
		entry, err := source(p, resource)
		if err != nil {
			return nil, nil, fmt.Errorf("primaries kind %s: %w", p.Kind.Kind, err)
		}
		primaries = append(primaries, entry)
	}
	return owners, primaries, nil
}

// source describes one primaries entry for the run event.
func source(p config.Primary, resource func(schema.GroupVersionKind) (string, bool, error)) (trace.Primary, error) {
	r, _, err := resource(p.Kind)
	if err != nil {
		return trace.Primary{}, err
	}
	entry := trace.Primary{Kind: trace.Kind{Group: p.Kind.Group, Version: p.Kind.Version, Kind: p.Kind.Kind, Resource: r},
		Ready: p.Ready, Retain: p.Retain, Halted: p.Halted}
	if p.Periodic != nil {
		entry.Periodic = &trace.Periodic{Every: p.Periodic.Every, Paths: p.Periodic.Paths}
	}
	for _, o := range p.Owns {
		if o.System != "" {
			entry.Owns = append(entry.Owns, trace.Owns{System: o.System, Kind: trace.Kind{Resource: o.Collection}, Name: o.Name})
			continue
		}
		r, namespaced, err := resource(o.Kind)
		if err != nil {
			return trace.Primary{}, fmt.Errorf("owns kind %s: %w", o.Kind.Kind, err)
		}
		if o.Namespace != "" && !namespaced {
			return trace.Primary{}, fmt.Errorf("owns kind %s is cluster-scoped, so it takes no namespace", o.Kind.Kind)
		}
		entry.Owns = append(entry.Owns, trace.Owns{
			Kind: trace.Kind{Group: o.Kind.Group, Version: o.Kind.Version, Kind: o.Kind.Kind, Resource: r},
			Name: o.Name, Namespace: o.Namespace,
		})
	}
	return entry, nil
}

// Systems returns c's fakes for the run event, with their API map's Ignore.
func Systems(c *config.Config) []trace.System {
	var systems []trace.System
	for _, f := range c.Fakes() {
		s := trace.System{Name: f.Name, API: f.API, Role: f.Role, Ready: f.Ready, Gone: f.Gone, Poll: f.Poll, Ignore: f.Map().Ignore}
		if f.Dependency != nil {
			s.Dependency = &trace.Dependency{Required: f.Dependency.Required, Track: f.Dependency.Track}
		}
		systems = append(systems, s)
	}
	return systems
}

// Properties returns c's properties for the run event.
func Properties(c *config.Config) []trace.Property {
	var properties []trace.Property
	for _, p := range c.Properties {
		properties = append(properties, trace.Property{Name: p.Name, Expr: p.Expr, When: string(p.When)})
	}
	return properties
}

// Key is the owner's CR set as a map key, so a change of rule keeps it. An
// unlisted owner's key is its UID.
func (o Owner) Key() string {
	if o.Unlisted != "" {
		return o.Unlisted
	}
	return strings.Join(o.CRs, ",")
}

// Named returns the one CR that rule 1, 2 or 3 names.
func (o Owner) Named() (string, bool) {
	if o.Rule == 4 || len(o.CRs) == 0 {
		return "", false
	}
	return o.CRs[0], true
}

// Shared reports whether no CR owns the instance.
func (o Owner) Shared() bool { return len(o.CRs) == 0 }

// World indexes a trace's instances.
type World struct {
	rules Rules
	// listed are the CR kinds, from the run event's primaries.
	listed    []trace.Kind
	instances []Instance
	byUID     map[string]int
	versions  map[string][]trace.Event
	slots     map[string]string
	decoded   map[decodedKey]map[string]any
}

type decodedKey struct {
	uid string
	at  time.Duration
}

// New indexes every instance in ix.
func New(ix *trace.Index, rules Rules) *World {
	w := &World{rules: rules, byUID: map[string]int{}, versions: map[string][]trace.Event{}, slots: map[string]string{}, decoded: map[decodedKey]map[string]any{}}
	if run, ok := ix.Run(); ok {
		for _, p := range run.Primaries {
			w.listed = append(w.listed, p.Kind)
		}
	}
	creates := map[string]trace.Call{}
	var actions []trace.Event
	for _, e := range ix.Events() {
		switch {
		case e.Version != nil:
			w.addVersion(e)
		case e.Call != nil && e.Call.Subresource == "" && e.Call.ResultUID != "" &&
			(e.Call.Verb == "create" && e.Call.Status >= 200 && e.Call.Status <= 299 || e.Call.Status == 201):
			if _, seen := creates[e.Call.ResultUID]; !seen {
				creates[e.Call.ResultUID] = *e.Call
			}
		case e.Action != nil && e.Action.Do == string(plan.Create) && e.Action.Ref != nil && e.Action.Error == "":
			actions = append(actions, e)
		}
	}
	for n := range w.instances {
		i := &w.instances[n]
		i.Creator = trace.Cluster
		if c, found := creates[i.UID]; found && names(c, i.Ref) {
			i.Creator = c.Caller
		}
		i.Role = w.role(w.versions[i.UID][0].Version)
	}
	for _, e := range actions {
		if i, found := w.Latest(*e.Action.Ref, e.At); found {
			w.slots[i.UID] = e.Action.Slot
		}
	}
	return w
}

func (w *World) addVersion(e trace.Event) {
	v := e.Version
	n, seen := w.byUID[v.UID]
	if !seen {
		n = len(w.instances)
		w.byUID[v.UID] = n
		w.instances = append(w.instances, Instance{Ref: v.Ref, UID: v.UID, Created: e.At})
	}
	if i := &w.instances[n]; i.Gone == 0 && i.Ref == v.Ref {
		w.versions[v.UID] = append(w.versions[v.UID], e)
		if v.Deleted || w.gone(v) {
			i.Gone = e.At
		}
	}
}

// gone reports whether v's system's gone CEL accepts it.
func (w *World) gone(v *trace.Version) bool {
	eval := w.rules.Gone[v.Ref.System]
	if eval == nil {
		return false
	}
	var self map[string]any
	if json.Unmarshal(v.Body, &self) != nil {
		return false
	}
	gone, err := eval(self)
	return err == nil && gone
}

// names reports whether the create call c made the object ref.
func names(c trace.Call, ref trace.Ref) bool {
	if c.Object() == ref {
		return true
	}
	c.Ref.Name = ref.Name
	return c.Ref == ref && c.ResultName == ref.Name
}

// role is the system's role CEL over v, or else, on kube, its generateName
// or name.
func (w *World) role(v *trace.Version) string {
	if eval := w.rules.Roles[v.Ref.System]; eval != nil {
		var self map[string]any
		_ = json.Unmarshal(v.Body, &self)
		role, _ := eval(self) // A role that fails to evaluate is empty.
		return role
	}
	if v.Ref.System != "kube" {
		return ""
	}
	var body struct {
		Metadata struct {
			GenerateName string `json:"generateName"`
		} `json:"metadata"`
	}
	if json.Unmarshal(v.Body, &body) == nil && body.Metadata.GenerateName != "" {
		return body.Metadata.GenerateName
	}
	return v.Ref.Name
}

// Latest is the last instance of ref created at or before t, gone or not.
func (w *World) Latest(ref trace.Ref, t time.Duration) (Instance, bool) {
	var latest Instance
	found := false
	for _, i := range w.instances {
		if i.Ref == ref && i.Created <= t {
			latest, found = i, true
		}
	}
	return latest, found
}

// Instances returns the instances live at t, ordered by Ref then Created.
func (w *World) Instances(t time.Duration) []Instance {
	var live []Instance
	for _, i := range w.instances {
		if i.Live(t) {
			live = append(live, i)
		}
	}
	slices.SortStableFunc(live, func(a, b Instance) int { return compareRefs(a.Ref, b.Ref) })
	return live
}

func compareRefs(a, b trace.Ref) int {
	return cmp.Or(cmp.Compare(a.System, b.System), cmp.Compare(a.Group, b.Group), cmp.Compare(a.Resource, b.Resource),
		cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
}

// All returns every instance, live or gone, in order of first version.
func (w *World) All() []Instance { return slices.Clone(w.instances) }

// Instance returns the instance with uid.
func (w *World) Instance(uid string) (Instance, bool) {
	n, found := w.byUID[uid]
	if !found {
		return Instance{}, false
	}
	return w.instances[n], true
}

// Versions returns the instance's versions in order.
func (w *World) Versions(i Instance) []trace.Event { return w.versions[i.UID] }

// Object is the instance's latest body at t, decoded and shared between
// callers, which must not change it. It is nil before the instance exists
// or when its body does not decode to an object.
func (w *World) Object(i Instance, t time.Duration) map[string]any { return w.object(i, t) }

// Body is the instance's latest body at t.
func (w *World) Body(i Instance, t time.Duration) json.RawMessage {
	if v, found := w.version(i, t); found {
		return v.Version.Body
	}
	return nil
}

func (w *World) version(i Instance, t time.Duration) (trace.Event, bool) {
	versions := w.versions[i.UID]
	n := sort.Search(len(versions), func(k int) bool { return versions[k].At > t })
	if n == 0 {
		return trace.Event{}, false
	}
	return versions[n-1], true
}

// object is the instance's body at t, decoded. It is nil before the
// instance exists or when its body does not decode to an object.
func (w *World) object(i Instance, t time.Duration) map[string]any {
	v, found := w.version(i, t)
	if !found {
		return nil
	}
	key := decodedKey{i.UID, v.At}
	if obj, done := w.decoded[key]; done {
		return obj
	}
	var obj map[string]any
	_ = json.Unmarshal(v.Version.Body, &obj) // An undecodable body names no owner.
	w.decoded[key] = obj
	return obj
}

// Primary reports whether i is a CR: a kube instance of a listed kind.
func (w *World) Primary(i Instance) bool { return slices.ContainsFunc(w.listed, i.Ref.OfKind) }

// Root returns o, or, if o names one CR that the controller created, that
// CR's root at t. A cycle stops the climb. So a CR that the controller made
// shares a root with its resources.
func (w *World) Root(o Owner, t time.Duration) Owner {
	for seen := map[string]bool{}; ; {
		uid, _ := o.Named()
		cr, _ := w.Instance(uid)
		if cr.Creator != trace.Controller || seen[uid] {
			return o
		}
		seen[uid] = true
		o = w.Owner(cr, t)
	}
}

// Owner is the instance's owning CR at t: the first rule that matches.
func (w *World) Owner(i Instance, t time.Duration) Owner {
	if cr, found := w.follow(i, t, map[string]bool{}); found {
		return Owner{Rule: 1, CRs: []string{cr}}
	}
	if owner, found := w.unlisted(i, t, map[string]bool{}); found {
		return Owner{Rule: 1, Unlisted: owner}
	}
	if cr, found := w.named(i, t); found {
		return Owner{Rule: 2, CRs: []string{cr}}
	}
	if cr, found := w.naming(i, t); found {
		return Owner{Rule: 3, CRs: []string{cr}}
	}
	// The set skips CRs that the controller created, so a resource that the
	// controller remakes after making one keeps its set.
	var crs []string
	for _, other := range w.instances {
		if w.Primary(other) && other.Creator != trace.Controller && other.Live(i.Created) && other.UID != i.UID {
			crs = append(crs, other.UID)
		}
	}
	slices.Sort(crs)
	return Owner{Rule: 4, CRs: crs}
}

// named applies rule 2: i's system's owner CEL names a CR by UID,
// namespace/name or name. A name that CRs of two kinds match names none.
func (w *World) named(i Instance, t time.Duration) (string, bool) {
	eval := w.rules.Owners[i.Ref.System]
	if eval == nil {
		return "", false
	}
	self := w.object(i, t)
	if self == nil {
		return "", false
	}
	name, err := eval(self)
	if err != nil {
		return "", false
	}
	if cr, found := w.Instance(name); found && w.Primary(cr) {
		return cr.UID, true
	}
	namespace, name, qualified := strings.Cut(name, "/")
	if !qualified {
		name, namespace = namespace, ""
	}
	match := ""
	for _, kind := range w.listed {
		cr, found := w.namedOfKind(kind, i, namespace, name, qualified, t)
		if !found {
			continue
		}
		if match != "" {
			return "", false
		}
		match = cr
	}
	return match, match != ""
}

// namedOfKind finds the CR of kind that rule 2 names by namespace and
// name. A bare name prefers i's namespace, and names none when it names CRs
// in two other namespaces.
func (w *World) namedOfKind(kind trace.Kind, i Instance, namespace, name string, qualified bool, t time.Duration) (string, bool) {
	var best Instance
	bestHome, ambiguous := false, false
	for _, cr := range w.instances {
		if !cr.Ref.OfKind(kind) || cr.Ref.Name != name || cr.Created > t || qualified && cr.Ref.Namespace != namespace {
			continue
		}
		switch home := cr.Ref.Namespace == i.Ref.Namespace; {
		case home:
			best, bestHome = cr, true
		case bestHome:
		case best.UID != "" && best.Ref.Namespace != cr.Ref.Namespace:
			ambiguous = true
		default:
			best = cr
		}
	}
	return best.UID, best.UID != "" && (bestHome || !ambiguous)
}

// naming applies rule 3: an owns entry over the body at t of a CR of its
// entry's kind names i. A CR live at t wins, then the latest created.
func (w *World) naming(i Instance, t time.Duration) (string, bool) {
	var best Instance
	found := false
	for _, rule := range w.rules.Owns {
		if rule.System != i.Ref.System || rule.Group != i.Ref.Group || rule.Resource != i.Ref.Resource {
			continue
		}
		kind := trace.Kind{Group: rule.CRGroup, Resource: rule.CRResource}
		for _, cr := range w.instances {
			if !cr.Ref.OfKind(kind) || !w.names(rule, cr, i, t) {
				continue
			}
			if !found || cr.Live(t) || !best.Live(t) {
				best, found = cr, true
			}
		}
	}
	return best.UID, found
}

func (w *World) names(rule OwnsRule, cr, i Instance, t time.Duration) bool {
	self := w.object(cr, t)
	if self == nil {
		return false
	}
	if name, err := rule.Name(self); err != nil || name != i.Ref.Name {
		return false
	}
	namespace := ""
	switch {
	case rule.Namespace != nil:
		var err error
		if namespace, err = rule.Namespace(self); err != nil {
			return false
		}
	case i.Ref.Namespace != "":
		namespace = cr.Ref.Namespace
	}
	return namespace == i.Ref.Namespace
}

// follow walks i's ownerReferences at t to a CR.
func (w *World) follow(i Instance, t time.Duration, seen map[string]bool) (string, bool) {
	seen[i.UID] = true
	for _, ref := range w.ownerReferences(i, t) {
		next, found := w.Instance(ref.UID)
		if !found || seen[next.UID] {
			continue
		}
		if w.Primary(next) {
			return next.UID, true
		}
		if cr, found := w.follow(next, t, seen); found {
			return cr, true
		}
	}
	return "", false
}

// unlisted finds the first instance live at t that the controller did not
// create, such as a fixture, that i's ownerReferences at t name, directly or
// through live instances that the controller created. Owner asks only once
// follow found no CR.
func (w *World) unlisted(i Instance, t time.Duration, seen map[string]bool) (string, bool) {
	seen[i.UID] = true
	for _, ref := range w.ownerReferences(i, t) {
		owner, found := w.Instance(ref.UID)
		if !found || !owner.Live(t) || seen[owner.UID] {
			continue
		}
		if owner.Creator != trace.Controller {
			return owner.UID, true
		}
		if uid, found := w.unlisted(owner, t, seen); found {
			return uid, true
		}
	}
	return "", false
}

// ownerReferences are i's ownerReferences at t, controller first.
func (w *World) ownerReferences(i Instance, t time.Duration) []ownerReference {
	metadata, _ := w.object(i, t)["metadata"].(map[string]any)
	items, _ := metadata["ownerReferences"].([]any)
	var refs []ownerReference
	for _, item := range items {
		fields, _ := item.(map[string]any)
		uid, _ := fields["uid"].(string)
		controller, _ := fields["controller"].(bool)
		refs = append(refs, ownerReference{UID: uid, Controller: controller})
	}
	slices.SortStableFunc(refs, func(a, b ownerReference) int { return boolInt(b.Controller) - boolInt(a.Controller) })
	return refs
}

type ownerReference struct {
	UID        string
	Controller bool
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Slot is the slot whose create action made the CR with uid, or empty.
func (w *World) Slot(uid string) string { return w.slots[uid] }

// ErrNoMatch says no instance matches a target.
var ErrNoMatch = errors.New("no instance matches")

// Select resolves target at t among the live instances that creator made.
// A slot narrows the match to instances whose root holds that slot's live
// CR; an empty slot matches any owner. An error wraps ErrNoMatch. It names
// the target, and the slot's object that is not a CR or else the
// candidates of the target's kind.
func (w *World) Select(creator trace.Caller, target plan.Target, slot string, t time.Duration) (trace.Ref, error) {
	system := cmp.Or(target.System, "kube")
	cr := ""
	if slot != "" {
		var held Instance
		for _, i := range w.instances {
			if i.Live(t) && w.slots[i.UID] == slot {
				held = i
			}
		}
		switch {
		case held.UID == "":
			return trace.Ref{}, fmt.Errorf("%w %s: slot %s holds no live CR", ErrNoMatch, target, slot)
		case !w.Primary(held):
			kind := schema.GroupResource{Group: held.Ref.Group, Resource: held.Ref.Resource}
			return trace.Ref{}, fmt.Errorf("%w %s: slot %s holds %s %s, which primaries does not list", ErrNoMatch, target, slot, kind, held.Ref.Name)
		}
		cr = held.UID
	}
	var ofKind []Instance
	for _, i := range w.instances {
		if i.Live(t) && i.Creator == creator && i.Ref.System == system && matches(target.Resource, i.Ref.Resource) &&
			matches(target.Group, i.Ref.Group) && (cr == "" || slices.Contains(w.Root(w.Owner(i, t), t).CRs, cr)) {
			ofKind = append(ofKind, i)
		}
	}
	nth := max(target.Nth, 1)
	for _, i := range ofKind {
		if !matches(target.Namespace, i.Ref.Namespace) || !matches(target.Name, i.Ref.Name) || !matches(target.Role, i.Role) {
			continue
		}
		if nth--; nth == 0 {
			return i.Ref, nil
		}
	}
	return trace.Ref{}, fmt.Errorf("%w %s: %s", ErrNoMatch, target, candidates(creator, target, slot, ofKind))
}

// candidates names up to five of the live instances of target's kind that
// creator made, in slot if set.
func candidates(creator trace.Caller, target plan.Target, slot string, ofKind []Instance) string {
	kind := target.Resource
	if target.Group != "" {
		kind += "." + target.Group
	}
	if target.Resource == "" {
		kind = "objects"
	}
	if system := cmp.Or(target.System, "kube"); system != "kube" {
		kind = system + " " + kind
	}
	if slot != "" {
		kind += " in slot " + slot
	}
	said := fmt.Sprintf("the %s has no live %s", creator, kind)
	if len(ofKind) > 0 {
		const shown = 5
		var names []string
		for _, i := range ofKind[:min(len(ofKind), shown)] {
			name := i.Ref.Name
			if i.Ref.Namespace != "" {
				name = i.Ref.Namespace + "/" + name
			}
			if target.Resource == "" {
				name = i.Ref.Resource + "/" + name
			}
			names = append(names, name)
		}
		said = fmt.Sprintf("the %s's live %s are %s", creator, kind, strings.Join(names, ", "))
		if len(ofKind) > shown {
			said += fmt.Sprintf(" and %d more", len(ofKind)-shown)
		}
	}
	if creator == trace.Harness && target.Resource == "customresourcedefinitions" {
		said += "; a CRD from crds is not a fixture"
	}
	return said
}

func matches(want, got string) bool { return want == "" || want == got }
