package check

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Clean fails a settled step after which a resource the controller created
// for a deleted CR survives. Without a stop, it also fails the run's last
// step when any resource the controller created survives. After a stop, a
// CR deleted after the stop counts as live, since no controller is left to
// clean up after it. It reports each instance once.
func Clean(ix *trace.Index, s Spec) []Finding {
	v := newView(ix, s)
	run, _ := ix.Run()
	final, stopAt := run.PlanSteps+1, time.Duration(math.MaxInt64)
	stop, stopped := ix.Stop(math.MaxInt64)
	if stopped {
		final, stopAt = run.PlanSteps-1, stop.At
	}
	reported := map[string]bool{}
	var findings []Finding
	for _, step := range v.steps {
		last := step.Step == final
		if !last && !v.has(step.Step, "delete") {
			continue
		}
		end := step.ConfirmEnd
		left := map[string][]string{}
		for _, i := range v.resources(end) {
			o := v.w.Owner(i, end)
			if !reported[i.UID] && !o.Shared() && !v.retained(o, end) && (last && !stopped || v.allGone(o, end) && v.allDeleted(o, stopAt)) {
				reported[i.UID] = true
				left[kind(i.Ref)] = append(left[kind(i.Ref)], v.describe(i, o))
			}
		}
		if last {
			for _, g := range v.grown(end) {
				left[kind(g[0].Ref)] = append(left[kind(g[0].Ref)], v.describeAll(g, end)...)
			}
		}
		findings = append(findings, byKind("Clean", step.Step, end, "left resources behind", sorted(left))...)
	}
	return findings
}

// allGone reports whether every CR o names is gone at t.
func (v *view) allGone(o owner.Owner, t time.Duration) bool {
	return !slices.ContainsFunc(o.CRs, func(uid string) bool {
		cr, found := v.w.Instance(uid)
		return found && cr.Live(t)
	})
}

// allDeleted reports whether every CR o names is gone or being deleted at t.
func (v *view) allDeleted(o owner.Owner, t time.Duration) bool {
	return !slices.ContainsFunc(o.CRs, func(uid string) bool {
		cr, found := v.w.Instance(uid)
		return found && cr.Live(t) && !deleting(v.w.Body(cr, t))
	})
}

// retained reports whether, for every CR o names, its kind's retain holds
// over its last body.
func (v *view) retained(o owner.Owner, t time.Duration) bool {
	return len(o.CRs) > 0 && !slices.ContainsFunc(o.CRs, func(uid string) bool {
		cr, _ := v.w.Instance(uid)
		p, _ := v.s.primary(cr.Ref)
		var body map[string]any
		if p.Retain == nil || json.Unmarshal(v.w.Body(cr, t), &body) != nil {
			return true
		}
		keep, err := p.Retain(body)
		return err != nil || !keep
	})
}

// grown returns the groups of shared resources that hold more live
// instances at t than at the first create action.
func (v *view) grown(t time.Duration) [][]owner.Instance {
	before := map[group]int{}
	if first, found := v.firstCreate(); found {
		for g, instances := range v.groups(first) {
			before[g] = len(instances)
		}
	}
	var grown [][]owner.Instance
	for g, instances := range v.groups(t) {
		if g.owner == (owner.Owner{Rule: 4}).Key() && len(instances) > before[g] {
			grown = append(grown, instances[before[g]:])
		}
	}
	return grown
}

func (v *view) firstCreate() (time.Duration, bool) {
	for _, step := range v.ix.Steps() {
		for _, e := range v.ix.Actions(step.Step) {
			if e.Action.Do == "create" {
				return e.At, true
			}
		}
	}
	return 0, false
}

// view holds what Clean, Restore and Unique share: the instances and the
// settled steps.
type view struct {
	ix *trace.Index
	s  Spec
	w  *owner.World
	// steps are the settled steps, in order.
	steps []trace.StepSpan
	// roots maps each group's root key to its root.
	roots map[string]owner.Owner
}

func newView(ix *trace.Index, s Spec) *view {
	v := &view{ix: ix, s: s, w: owner.New(ix, s.Owners), roots: map[string]owner.Owner{}}
	for _, step := range ix.Steps() {
		if v.settled(step) {
			v.steps = append(v.steps, step)
		}
	}
	return v
}

// settled reports whether the step settled by its deadline and its end, and
// was still settled one stable later, within its confirm window.
func (v *view) settled(step trace.StepSpan) bool {
	if !step.Reached[trace.EndFaults] || !step.Reached[trace.Confirmed] {
		return false
	}
	at, ok := FirstSettled(v.ix, step.EndFaults, min(step.Deadline, step.ConfirmEnd), v.s)
	confirm := at + v.s.Stable
	return ok && confirm <= step.ConfirmEnd && len(Settled(v.ix, confirm, v.s)) == 0
}

// has reports whether step holds an action that does do.
func (v *view) has(step int, do ...string) bool {
	return slices.ContainsFunc(v.ix.Actions(step), func(e trace.Event) bool { return slices.Contains(do, e.Action.Do) })
}

// resources are the controller's resources that live at t.
func (v *view) resources(t time.Duration) []owner.Instance {
	return slices.DeleteFunc(v.w.Instances(t), func(i owner.Instance) bool { return !controllerResource(i) })
}

// controllerResource reports whether i is one of the controller's resources:
// one it created, of a kind outside trace.Ignored.
func controllerResource(i owner.Instance) bool {
	return i.Creator == trace.Controller && !trace.Ignored(trace.Call{Ref: i.Ref})
}

// group is what Clean, Restore and Unique compare: a kind, a namespace, an
// owner, its root and a role.
type group struct {
	kind  trace.Ref
	owner string
	// root is the key of the owner's root, which survives a remade CR.
	root string
	role string
}

// groups returns the controller's resources at t by group, each in
// creation order.
func (v *view) groups(t time.Duration) map[group][]owner.Instance {
	groups := map[group][]owner.Instance{}
	for _, i := range v.resources(t) {
		g := v.groupOf(i, t)
		groups[g] = append(groups[g], i)
	}
	for _, instances := range groups {
		slices.SortStableFunc(instances, func(a, b owner.Instance) int { return cmp.Compare(a.Created, b.Created) })
	}
	return groups
}

func (v *view) groupOf(i owner.Instance, t time.Duration) group {
	k := i.Ref
	k.Name = ""
	o := v.w.Owner(i, t)
	root := v.w.Root(o, t)
	v.roots[root.Key()] = root
	return group{kind: k, owner: o.Key(), root: root.Key(), role: i.Role}
}

// describe names an instance, its owning rule and its CRs, or its unlisted
// owner.
func (v *view) describe(i owner.Instance, o owner.Owner) string {
	if o.Unlisted != "" {
		unlisted, _ := v.w.Instance(o.Unlisted)
		return fmt.Sprintf("%s (rule 1, unlisted owner %s)", describe(i.Ref), describe(unlisted.Ref))
	}
	if o.Shared() {
		return describe(i.Ref) + " (shared)"
	}
	crs := make([]string, len(o.CRs))
	for n, uid := range o.CRs {
		crs[n] = crName(v.w, uid)
	}
	return fmt.Sprintf("%s (rule %d, %s)", describe(i.Ref), o.Rule, strings.Join(crs, ", "))
}

// crName names the CR with uid by its slot, or else by its kind and name.
func crName(w *owner.World, uid string) string {
	if slot := w.Slot(uid); slot != "" {
		return "slot " + slot
	}
	cr, _ := w.Instance(uid)
	return "CR " + describe(cr.Ref)
}

func (v *view) describeAll(instances []owner.Instance, t time.Duration) []string {
	described := make([]string, len(instances))
	for n, i := range instances {
		described[n] = v.describe(i, v.w.Owner(i, t))
	}
	return described
}

// kind is "system/group/resource", or "system/resource" for the core group.
func kind(ref trace.Ref) string {
	if ref.Group == "" {
		return ref.System + "/" + ref.Resource
	}
	return ref.System + "/" + ref.Group + "/" + ref.Resource
}

// byKind gives one finding for each kind that has details, in kind order.
func byKind(check string, step int, at time.Duration, summary string, details map[string][]string) []Finding {
	var findings []Finding
	for _, k := range slices.Sorted(maps.Keys(details)) {
		findings = append(findings, Finding{Check: check, Step: step, At: at, Summary: summary, Details: details[k], Kind: k})
	}
	return findings
}
