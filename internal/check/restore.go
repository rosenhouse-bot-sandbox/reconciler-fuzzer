package check

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Restore fails a settled step without a create, update, delete or fixture
// change whose controller resources differ from the previous settled step's.
func Restore(ix *trace.Index, s Spec) []Finding {
	v := newView(ix, s)
	var findings []Finding
	for n, step := range v.steps {
		if n == 0 || v.steps[n-1].Step != step.Step-1 || v.has(step.Step, "create", "update", "delete", "changeFixture") {
			continue
		}
		from, to := v.steps[n-1].ConfirmEnd, step.ConfirmEnd
		damaged, touched := v.damaged(step.Step, from)
		before, after := v.groups(from), v.groups(to)
		kinds := map[group]bool{}
		for g := range after {
			kinds[group{kind: g.kind, owner: g.owner}] = true
		}
		missing, replaced, changed := map[string][]string{}, map[string][]string{}, map[string][]string{}
		for _, g := range slices.Collect(maps.Keys(merge(before, after))) {
			was, is := before[g], after[g]
			k := kind(g.kind)
			if touched(g.owner) {
				if len(was) > 0 && len(is) == 0 && (damaged[g] || !kinds[group{kind: g.kind, owner: g.owner}]) {
					missing[k] = append(missing[k], v.describeAll(was[:1], from)...)
				}
				continue
			}
			for _, i := range was {
				if j := slices.IndexFunc(is, func(j owner.Instance) bool { return j.UID == i.UID }); j < 0 {
					replaced[k] = append(replaced[k], v.describeAll([]owner.Instance{i}, from)...)
				} else if paths := differ(v.w.Body(i, from), v.w.Body(is[j], to), v.modelled(i.Ref)); len(paths) > 0 {
					changed[k] = append(changed[k], v.describeAll([]owner.Instance{i}, from)[0]+": "+strings.Join(paths, ", "))
				}
			}
			for _, j := range is {
				if !slices.ContainsFunc(was, func(i owner.Instance) bool { return i.UID == j.UID }) {
					replaced[k] = append(replaced[k], v.describeAll([]owner.Instance{j}, to)...)
				}
			}
		}
		findings = append(findings, byKind("Restore", step.Step, to, "did not restore damaged resources", sorted(missing))...)
		findings = append(findings, byKind("Restore", step.Step, to, "replaced resources", sorted(replaced))...)
		findings = append(findings, byKind("Restore", step.Step, to, "changed resources", sorted(changed))...)
	}
	return findings
}

// damaged returns the groups, at t, of the instances step's damage actions
// changed, and whether an owner key shares an owner or a CR with one.
func (v *view) damaged(step int, t time.Duration) (map[group]bool, func(key string) bool) {
	groups, keys, crs := map[group]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range v.ix.Actions(step) {
		if e.Action.Do != "damage" || e.Action.Ref == nil || e.Action.Error != "" {
			continue
		}
		if i, found := v.w.Latest(*e.Action.Ref, e.At); found {
			o := v.w.Owner(i, t)
			groups[v.groupOf(i, t)], keys[o.Key()] = true, true
			for _, cr := range o.CRs {
				crs[cr] = true
			}
		}
	}
	return groups, func(key string) bool {
		return keys[key] || slices.ContainsFunc(v.owners[key].CRs, func(uid string) bool { return crs[uid] })
	}
}

func merge[K comparable, V any](a, b map[K]V) map[K]V {
	m := maps.Clone(a)
	maps.Copy(m, b)
	return m
}

func sorted(details map[string][]string) map[string][]string {
	for _, d := range details {
		slices.Sort(d)
	}
	return details
}

// modelled reports whether a model writes the status of ref's kind.
func (v *view) modelled(ref trace.Ref) bool {
	return slices.ContainsFunc(v.s.Models, func(m Model) bool { return m.Kind.Group == ref.Group && m.Kind.Resource == ref.Resource })
}

// differ lists the top-level fields, and metadata's fields, in which two
// bodies differ, ignoring what every write changes, and status when a
// model writes it.
func differ(a, b json.RawMessage, modelled bool) []string {
	var was, is map[string]any
	if json.Unmarshal(a, &was) != nil || json.Unmarshal(b, &is) != nil {
		return []string{"(body)"}
	}
	var paths []string
	for _, side := range []map[string]any{was, is} {
		if metadata, _ := side["metadata"].(map[string]any); metadata != nil {
			delete(metadata, "resourceVersion")
			delete(metadata, "managedFields")
			delete(metadata, "generation")
		}
		if modelled {
			delete(side, "status")
		}
	}
	for _, key := range slices.Sorted(maps.Keys(merge(was, is))) {
		x, y := was[key], is[key]
		if reflect.DeepEqual(x, y) {
			continue
		}
		mx, okx := x.(map[string]any)
		my, oky := y.(map[string]any)
		if key != "metadata" || !okx || !oky {
			paths = append(paths, key)
			continue
		}
		for _, sub := range slices.Sorted(maps.Keys(merge(mx, my))) {
			if !reflect.DeepEqual(mx[sub], my[sub]) {
				paths = append(paths, fmt.Sprintf("metadata.%s", sub))
			}
		}
	}
	return paths
}
