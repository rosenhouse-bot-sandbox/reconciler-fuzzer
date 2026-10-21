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

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Instance is one life of an object, from its create to its delete.
type Instance struct {
	Ref trace.Ref
	UID string
	// Created is the At of its first version. Gone is the At of its delete
	// version, or zero while it lives.
	Created time.Duration
	Gone    time.Duration
	// Creator is the caller of the create, or the 201 apply, whose response
	// carried UID, or trace.Cluster.
	Creator trace.Caller
	// Role is the generateName prefix, or else the name.
	Role string
}

// Live reports whether the instance exists at t.
func (i Instance) Live(t time.Duration) bool {
	return i.Created <= t && (i.Gone == 0 || i.Gone > t)
}

// Owner is an instance's owning CR.
type Owner struct {
	// Rule is 1 for an ownerReference chain to a CR, and 4 for the set of
	// CRs alive at the instance's creation.
	Rule int
	// CRs holds CR UIDs in order: one for rule 1, the set for rule 4. An
	// empty rule-4 set marks a shared instance.
	CRs []string
}

// Key is the owner as a map key.
func (o Owner) Key() string {
	return fmt.Sprintf("%d:%s", o.Rule, strings.Join(o.CRs, ","))
}

// Shared reports whether no CR owns the instance.
func (o Owner) Shared() bool { return o.Rule == 4 && len(o.CRs) == 0 }

// World indexes a trace's instances.
type World struct {
	primary   trace.Ref
	instances []Instance
	byUID     map[string]int
	versions  map[string][]trace.Event
	slots     map[string]string
}

// New indexes every instance in ix.
func New(ix *trace.Index) *World {
	w := &World{byUID: map[string]int{}, versions: map[string][]trace.Event{}, slots: map[string]string{}}
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
		i.Role = role(w.versions[i.UID][0].Version)
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
		if v.Deleted {
			i.Gone = e.At
		}
	}
}

// names reports whether the create call c made the object ref.
func names(c trace.Call, ref trace.Ref) bool {
	if c.Object() == ref {
		return true
	}
	c.Ref.Name = ref.Name
	return c.Ref == ref && c.ResultName == ref.Name
}

func role(v *trace.Version) string {
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

// Body is the instance's latest body at t.
func (w *World) Body(i Instance, t time.Duration) json.RawMessage {
	versions := w.versions[i.UID]
	n := sort.Search(len(versions), func(k int) bool { return versions[k].At > t })
	if n == 0 {
		return nil
	}
	return versions[n-1].Version.Body
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
	var crs []string
	for _, other := range w.instances {
		if w.Primary(other) && other.Live(i.Created) {
			crs = append(crs, other.UID)
		}
	}
	slices.Sort(crs)
	return Owner{Rule: 4, CRs: crs}
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
		if !i.Live(t) || i.Creator != creator || i.Ref.System != system || i.Ref.Resource != target.Resource ||
			!matches(target.Group, i.Ref.Group) || !matches(target.Name, i.Ref.Name) || !matches(target.Role, i.Role) {
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
