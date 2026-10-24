package check

import (
	"encoding/json"
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
// A damage touches each group under its root, so a damage under a CR that the
// controller made touches that CR's owner too.
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
			kinds[group{kind: g.kind, root: g.root}] = true
		}
		missing, replaced, changed := map[string][]string{}, map[string][]string{}, map[string][]string{}
		for _, g := range slices.Collect(maps.Keys(merge(before, after))) {
			was, is := before[g], after[g]
			k := kind(g.kind)
			if touched(g.root) {
				if len(was) > 0 && len(is) == 0 && (damaged[g] || !kinds[group{kind: g.kind, root: g.root}]) {
					missing[k] = append(missing[k], v.describeAll(was[:1], from)...)
				}
				continue
			}
			for _, i := range was {
				if j := slices.IndexFunc(is, func(j owner.Instance) bool { return j.UID == i.UID }); j < 0 {
					replaced[k] = append(replaced[k], v.describeAll([]owner.Instance{i}, from)...)
				} else if paths := differ(v.w.Body(i, from), v.w.Body(is[j], to), v.s.modelled(i.Ref), v.s.Ignore[i.Ref.System], v.s.periodicPaths(i.Ref)); len(paths) > 0 {
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
// changed, and whether a root key shares a root or a CR with one of theirs.
// It takes each damaged ref's instance at t, because the controller can
// recreate a deleted instance before the action's event.
func (v *view) damaged(step int, t time.Duration) (map[group]bool, func(root string) bool) {
	groups, keys, crs := map[group]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range v.ix.Actions(step) {
		if e.Action.Do != "damage" || e.Action.Ref == nil || e.Action.Error != "" {
			continue
		}
		if i, found := v.w.Latest(*e.Action.Ref, t); found {
			g := v.groupOf(i, t)
			groups[g], keys[g.root] = true, true
			for _, cr := range v.roots[g.root].CRs {
				crs[cr] = true
			}
		}
	}
	return groups, func(root string) bool {
		return keys[root] || slices.ContainsFunc(v.roots[root].CRs, func(uid string) bool { return crs[uid] })
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

// differ lists the top-level fields, and metadata's fields, in which two
// bodies differ, ignoring what every write changes, status when a model
// writes it, the top-level fields ignore lists, and changes only under
// periodic paths.
func differ(a, b json.RawMessage, modelled bool, ignore, periodic []string) []string {
	var was, is map[string]any
	if json.Unmarshal(a, &was) != nil || json.Unmarshal(b, &is) != nil {
		return []string{"(body)"}
	}
	var paths []string
	for _, side := range []map[string]any{was, is} {
		dropWriteFields(side)
		if modelled {
			delete(side, "status")
		}
		for _, key := range ignore {
			delete(side, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(merge(was, is))) {
		x, y := was[key], is[key]
		mx, okx := x.(map[string]any)
		my, oky := y.(map[string]any)
		switch {
		case key == "metadata" && okx && oky:
			for _, sub := range slices.Sorted(maps.Keys(merge(mx, my))) {
				if path := "metadata." + sub; differsOutside(periodic, path, mx[sub], my[sub]) {
					paths = append(paths, path)
				}
			}
		case differsOutside(periodic, key, x, y):
			paths = append(paths, key)
		}
	}
	return paths
}

// differsOutside reports whether x and y, at path, differ outside periodic
// paths.
func differsOutside(periodic []string, path string, x, y any) bool {
	return !reflect.DeepEqual(x, y) && !onlyUnder(leafDiff(path, x, y, nil), periodic)
}

// dropWriteFields deletes the metadata fields that every write changes.
func dropWriteFields(body map[string]any) {
	if metadata, _ := body["metadata"].(map[string]any); metadata != nil {
		delete(metadata, "resourceVersion")
		delete(metadata, "managedFields")
		delete(metadata, "generation")
	}
}
