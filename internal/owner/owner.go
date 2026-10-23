// Package owner reads instances, their creators, owning CRs and roles from
// a trace. Checks and the runner's damage action use it.
package owner

import (
	"cmp"
	"encoding/json"
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
	// Rule is 1 for an ownerReference chain to a CR, 2 for a resource that
	// names its CR, 3 for a CR that names the resource, and 4 for the set
	// of CRs alive at the instance's creation.
	Rule int
	// CRs holds CR UIDs in order: one for rules 1 to 3, the set for rule 4.
	// An empty rule-4 set marks a shared instance.
	CRs []string
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

// OwnsRule is one primary.owns entry over a CR's body.
type OwnsRule struct {
	System, Group, Resource string
	Name                    func(cr map[string]any) (string, error)
	// Namespace is nil when unset: the CR's namespace, or cluster-scoped.
	Namespace func(cr map[string]any) (string, error)
}

// Compile builds Rules from a Run event's owners, owns and systems.
func Compile(owners map[string]string, owns []trace.Owns, systems []trace.System) (Rules, error) {
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
	for _, o := range owns {
		rule := OwnsRule{System: cmp.Or(o.System, "kube"), Group: o.Kind.Group, Resource: o.Kind.Resource}
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
	return rules, nil
}

// Sources returns c's rules 2 and 3 for Compile. resource maps an owns
// kind to its resource and says whether the kind is namespaced.
func Sources(c *config.Config, resource func(schema.GroupVersionKind) (string, bool, error)) (map[string]string, []trace.Owns, error) {
	var owners map[string]string
	for _, s := range c.Systems {
		if s.Owner != "" {
			if owners == nil {
				owners = map[string]string{}
			}
			owners[s.Name] = s.Owner
		}
	}
	var owns []trace.Owns
	for _, o := range c.Primary.Owns {
		if o.System != "" {
			owns = append(owns, trace.Owns{System: o.System, Kind: trace.Kind{Resource: o.Collection}, Name: o.Name})
			continue
		}
		r, namespaced, err := resource(o.Kind)
		if err != nil {
			return nil, nil, fmt.Errorf("primary.owns kind %s: %w", o.Kind.Kind, err)
		}
		if o.Namespace != "" && !namespaced {
			return nil, nil, fmt.Errorf("primary.owns kind %s is cluster-scoped, so it takes no namespace", o.Kind.Kind)
		}
		owns = append(owns, trace.Owns{
			Kind: trace.Kind{Group: o.Kind.Group, Version: o.Kind.Version, Kind: o.Kind.Kind, Resource: r},
			Name: o.Name, Namespace: o.Namespace,
		})
	}
	return owners, owns, nil
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

// Periodic returns c's primary.periodic for the run event, or nil.
func Periodic(c *config.Config) *trace.Periodic {
	if p := c.Primary.Periodic; p != nil {
		return &trace.Periodic{Every: p.Every, Paths: p.Paths}
	}
	return nil
}

// Key is the owner's CR set as a map key, so a change of rule keeps it.
func (o Owner) Key() string { return strings.Join(o.CRs, ",") }

// Named returns the one CR that rule 1, 2 or 3 names.
func (o Owner) Named() (string, bool) {
	if o.Rule == 4 {
		return "", false
	}
	return o.CRs[0], true
}

// Shared reports whether no CR owns the instance.
func (o Owner) Shared() bool { return o.Rule == 4 && len(o.CRs) == 0 }

// World indexes a trace's instances.
type World struct {
	rules     Rules
	primary   trace.Ref
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
		w.primary = run.Primary
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
	slices.SortStableFunc(live, func(a, b Instance) int { return trace.CompareRefs(a.Ref, b.Ref) })
	return live
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

// Primary reports whether i is of the primary kind.
func (w *World) Primary(i Instance) bool {
	return i.Ref.System == w.primary.System && i.Ref.Group == w.primary.Group && i.Ref.Resource == w.primary.Resource
}

// Owner is the instance's owning CR at t: the first rule that matches.
func (w *World) Owner(i Instance, t time.Duration) Owner {
	if cr, found := w.follow(i, t, map[string]bool{}); found {
		return Owner{Rule: 1, CRs: []string{cr}}
	}
	if cr, found := w.named(i, t); found {
		return Owner{Rule: 2, CRs: []string{cr}}
	}
	if cr, found := w.naming(i, t); found {
		return Owner{Rule: 3, CRs: []string{cr}}
	}
	var crs []string
	for _, other := range w.instances {
		if w.Primary(other) && other.Live(i.Created) {
			crs = append(crs, other.UID)
		}
	}
	slices.Sort(crs)
	return Owner{Rule: 4, CRs: crs}
}

// named applies rule 2: i's system's owner CEL names a CR by UID,
// namespace/name or name. A bare name prefers i's namespace, and skips the
// rule when it names CRs in two other namespaces.
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
	var best Instance
	bestHome, ambiguous := false, false
	for _, cr := range w.instances {
		if !w.Primary(cr) || cr.Ref.Name != name || cr.Created > t || qualified && cr.Ref.Namespace != namespace {
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

// naming applies rule 3: a primary.owns entry over a CR's body at t names
// i. A CR live at t wins, then the latest created.
func (w *World) naming(i Instance, t time.Duration) (string, bool) {
	var best Instance
	found := false
	for _, rule := range w.rules.Owns {
		if rule.System != i.Ref.System || rule.Group != i.Ref.Group || rule.Resource != i.Ref.Resource {
			continue
		}
		for _, cr := range w.instances {
			if !w.Primary(cr) || !w.names(rule, cr, i, t) {
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

// follow walks i's ownerReferences at t, controller first, to a CR.
func (w *World) follow(i Instance, t time.Duration, seen map[string]bool) (string, bool) {
	seen[i.UID] = true
	var body struct {
		Metadata struct {
			OwnerReferences []ownerReference `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if json.Unmarshal(w.Body(i, t), &body) != nil {
		return "", false
	}
	refs := body.Metadata.OwnerReferences
	slices.SortStableFunc(refs, func(a, b ownerReference) int { return boolInt(b.Controller) - boolInt(a.Controller) })
	for _, ref := range refs {
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

type ownerReference struct {
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Slot is the slot whose create action made the CR with uid, or empty.
func (w *World) Slot(uid string) string { return w.slots[uid] }

// Select resolves target at t among the live instances that creator made.
// A slot narrows the match to instances whose owner holds that slot's live
// CR; an empty slot matches any owner.
func (w *World) Select(creator trace.Caller, target plan.Target, slot string, t time.Duration) (trace.Ref, bool) {
	system := cmp.Or(target.System, "kube")
	cr := ""
	if slot != "" {
		for _, i := range w.instances {
			if w.Primary(i) && i.Live(t) && w.slots[i.UID] == slot {
				cr = i.UID
			}
		}
		if cr == "" {
			return trace.Ref{}, false
		}
	}
	nth := max(target.Nth, 1)
	for _, i := range w.instances {
		if !i.Live(t) || i.Creator != creator || i.Ref.System != system || !matches(target.Resource, i.Ref.Resource) ||
			!matches(target.Group, i.Ref.Group) || !matches(target.Namespace, i.Ref.Namespace) || !matches(target.Name, i.Ref.Name) || !matches(target.Role, i.Role) {
			continue
		}
		if cr != "" && !slices.Contains(w.Owner(i, t).CRs, cr) {
			continue
		}
		if nth--; nth == 0 {
			return i.Ref, true
		}
	}
	return trace.Ref{}, false
}

func matches(want, got string) bool { return want == "" || want == got }
