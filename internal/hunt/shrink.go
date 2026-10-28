package hunt

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// Fails reports whether a candidate plan still gives the finding being
// shrunk.
type Fails func(ctx context.Context, p *plan.Plan) (bool, error)

const shortestPeriod = plan.Duration(500 * time.Millisecond)

// Candidates returns the plans one greedy pass may try in place of p, in
// pass order: drop a step, drop an action or fault, halve a fault period
// (or clear the period of a step without faults), reset a spec field to its
// value in the sample of the CR's kind, merge two slots. Every candidate is
// valid.
func Candidates(p *plan.Plan, samples []map[string]any) []*plan.Plan {
	var valid []*plan.Plan
	try := func(change func(c *plan.Plan)) {
		c := clone(p)
		change(c)
		if c.Validate() == nil {
			valid = append(valid, c)
		}
	}
	for i := range p.Steps {
		try(func(c *plan.Plan) { c.Steps = slices.Delete(c.Steps, i, i+1) })
	}
	for i, step := range p.Steps {
		if len(step.Actions)+len(step.Faults) < 2 {
			continue // Dropping the step covers it.
		}
		for j := range step.Actions {
			try(func(c *plan.Plan) { c.Steps[i].Actions = slices.Delete(c.Steps[i].Actions, j, j+1) })
		}
		for k := range step.Faults {
			try(func(c *plan.Plan) { c.Steps[i].Faults = slices.Delete(c.Steps[i].Faults, k, k+1) })
		}
	}
	for i, step := range p.Steps {
		switch {
		case step.Period > shortestPeriod:
			try(func(c *plan.Plan) { halve(&c.Steps[i]) })
		case step.Period > 0 && len(step.Faults) == 0:
			try(func(c *plan.Plan) { c.Steps[i].Period = 0 })
		}
	}
	for i, step := range p.Steps {
		for j, a := range step.Actions {
			for _, change := range fieldChanges(a, samples) {
				try(func(c *plan.Plan) { change(&c.Steps[i].Actions[j]) })
			}
		}
	}
	used := usedSlots(p)
	for x, into := range used {
		for _, from := range used[x+1:] {
			try(func(c *plan.Plan) { rename(c, from, into) })
		}
	}
	return valid
}

// Shrink replaces p by its first failing candidate until no candidate
// fails, and returns the result. The result is p itself when no candidate
// failed. An error of fails ends it; the result is then the smallest failing
// plan so far.
func Shrink(ctx context.Context, p *plan.Plan, samples []map[string]any, fails Fails) (*plan.Plan, error) {
	for {
		shrunk := false
		for _, c := range Candidates(p, samples) {
			failed, err := fails(ctx, c)
			if err != nil {
				return p, err
			}
			if failed {
				p, shrunk = c, true
				break
			}
		}
		if !shrunk {
			return p, nil
		}
	}
}

func clone(p *plan.Plan) *plan.Plan {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	var c plan.Plan
	if err := json.Unmarshal(data, &c); err != nil {
		panic(err)
	}
	return &c
}

// halve halves the step's period, down to shortestPeriod, and its action
// offsets, to the millisecond.
func halve(step *plan.Step) {
	step.Period = max(step.Period/2, shortestPeriod)
	for i := range step.Actions {
		at := &step.Actions[i].At
		*at = plan.Duration(time.Duration(*at / 2).Truncate(time.Millisecond))
	}
}

// fieldChanges reset each spec field of a create's object to its value in
// the sample of the object's group and kind, or drop each field of an
// update patch that has more than one. A create of a kind without a sample
// keeps its fields.
func fieldChanges(a plan.Action, samples []map[string]any) []func(*plan.Action) {
	var changes []func(*plan.Action)
	switch {
	case a.Do == plan.Create && a.Object != nil:
		object := decode(a.Object)
		i := slices.IndexFunc(samples, func(sample map[string]any) bool { return groupKind(sample) == groupKind(object) })
		if i < 0 {
			return nil
		}
		sample := samples[i]
		specs, _ := sample["spec"].(map[string]any)
		for _, path := range differing(object["spec"], specs, []string{"spec"}) {
			changes = append(changes, func(c *plan.Action) {
				changed := decode(c.Object)
				value, present := lookup(sample, path)
				set(changed, path, value, present)
				c.Object = encode(changed)
			})
		}
	case a.Do == plan.Update:
		leaves := leaves(decode(a.Patch), nil)
		if len(leaves) < 2 {
			return nil // Dropping the action covers it.
		}
		for _, path := range leaves {
			changes = append(changes, func(c *plan.Action) {
				changed := decode(c.Patch)
				set(changed, path, nil, false)
				c.Patch = encode(changed)
			})
		}
	}
	return changes
}

// differing are the paths, below at, where ours and theirs differ, in
// sorted order. A path stops where either side is not an object.
func differing(ours, theirs any, at []string) [][]string {
	ourMap, ourObject := ours.(map[string]any)
	theirMap, theirObject := theirs.(map[string]any)
	if !ourObject || (!theirObject && theirs != nil) {
		if reflect.DeepEqual(normalize(ours), normalize(theirs)) {
			return nil
		}
		return [][]string{at}
	}
	var paths [][]string
	keys := slices.Sorted(maps.Keys(ourMap))
	for key := range theirMap {
		if _, ok := ourMap[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		paths = append(paths, differing(ourMap[key], theirMap[key], append(slices.Clone(at), key))...)
	}
	return paths
}

// leaves are the paths to the values in object that are not objects, in
// sorted order.
func leaves(object map[string]any, at []string) [][]string {
	var paths [][]string
	for _, key := range slices.Sorted(maps.Keys(object)) {
		path := append(slices.Clone(at), key)
		if child, ok := object[key].(map[string]any); ok {
			paths = append(paths, leaves(child, path)...)
		} else {
			paths = append(paths, path)
		}
	}
	return paths
}

// normalize makes numbers comparable whatever their Go type.
func normalize(value any) any {
	data, _ := json.Marshal(value)
	var decoded any
	_ = json.Unmarshal(data, &decoded)
	return decoded
}

func lookup(object map[string]any, path []string) (any, bool) {
	var at any = object
	for _, key := range path {
		m, ok := at.(map[string]any)
		if !ok {
			return nil, false
		}
		if at, ok = m[key]; !ok {
			return nil, false
		}
	}
	return at, true
}

// set writes value at path, or removes the path and any object it leaves
// empty.
func set(object map[string]any, path []string, value any, present bool) {
	key := path[0]
	if len(path) == 1 {
		if present {
			object[key] = value
		} else {
			delete(object, key)
		}
		return
	}
	child, ok := object[key].(map[string]any)
	if !ok {
		child = map[string]any{}
		object[key] = child
	}
	set(child, path[1:], value, present)
	if len(child) == 0 {
		delete(object, key)
	}
}

func groupKind(object map[string]any) schema.GroupKind {
	return (&unstructured.Unstructured{Object: object}).GroupVersionKind().GroupKind()
}

func decode(raw json.RawMessage) map[string]any {
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	return object
}

func encode(object map[string]any) json.RawMessage {
	data, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return data
}

func usedSlots(p *plan.Plan) []string {
	var used []string
	for _, step := range p.Steps {
		for _, a := range step.Actions {
			if a.Slot != "" && !slices.Contains(used, a.Slot) {
				used = append(used, a.Slot)
			}
		}
	}
	slices.Sort(used)
	return used
}

func rename(p *plan.Plan, from, to string) {
	for i := range p.Steps {
		for j := range p.Steps[i].Actions {
			if a := &p.Steps[i].Actions[j]; a.Slot == from {
				a.Slot = to
			}
		}
	}
}
