package generate

import (
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

// reference is a config reference from a listed kind, whose name creates
// and updates write with candidates.
type reference struct {
	// path is the name's field path, with each [*] its own segment.
	path     []string
	required bool
	// to is the listed to kind, with the name of its CR in each slot, or
	// nil.
	to        *crKind
	slotNames []string
	// fixtures are the fixtures of an unlisted to kind or of a fake's
	// collection.
	fixtures []plan.Target
}

// each is the segment of a reference's path that reads each item of a list.
const each = "[*]"

// missing is the name a reference's write holds 1 time in 4 when the
// reference is not required. No object has it.
const missing = "missing"

// readReferences gives each listed kind the references from it, and lists
// the kube fixtures of the to kinds other than Namespaces as referenced.
func (g *Generator) readReferences(cfg *config.Config) error {
	for _, r := range cfg.References {
		ref := &reference{required: r.Required}
		for _, part := range strings.Split(r.Name, ".") {
			field, isList := strings.CutSuffix(part, each)
			ref.path = append(ref.path, field)
			if isList {
				ref.path = append(ref.path, each)
			}
		}
		if i := slices.IndexFunc(g.kinds, func(k *crKind) bool { return k.groupKind == r.To.GroupKind() }); i >= 0 {
			ref.to = g.kinds[i]
			for _, slot := range slots {
				ref.slotNames = append(ref.slotNames, ref.to.base(slot).GetName())
			}
		} else {
			var err error
			if ref.fixtures, err = fixturesOf(cfg, r); err != nil {
				return err
			}
			for _, target := range ref.fixtures {
				// envtest runs no namespace controller, so a deleted
				// Namespace would never go.
				namespace := target.Group == "" && target.Resource == "namespaces"
				if target.System == "" && !namespace && !slices.Contains(g.referenced, target) {
					g.referenced = append(g.referenced, target)
				}
			}
		}
		from := g.kinds[slices.IndexFunc(g.kinds, func(k *crKind) bool { return k.groupKind == r.From.GroupKind() })]
		from.references = append(from.references, ref)
	}
	return nil
}

// fixturesOf are the fixtures of r's to kind, or of its fake's collection.
func fixturesOf(cfg *config.Config, r config.Reference) ([]plan.Target, error) {
	var targets []plan.Target
	if r.System == "" {
		documents, err := config.ReadManifests(cfg.Fixtures)
		if err != nil {
			return nil, err
		}
		for _, document := range documents {
			if (&unstructured.Unstructured{Object: document}).GroupVersionKind().GroupKind() == r.To.GroupKind() {
				targets = append(targets, kubeFixture(document))
			}
		}
		return targets, nil
	}
	for _, s := range cfg.Systems {
		if s.Name != r.System {
			continue
		}
		for _, path := range s.Fixtures {
			entries, err := config.ReadSystemFixtures(path)
			if err != nil {
				return nil, err
			}
			for _, entry := range entries {
				if entry.Resource == r.Collection {
					targets = append(targets, plan.Target{System: s.Name, Resource: entry.Resource, Name: entry.Name})
				}
			}
		}
	}
	return targets, nil
}

// under is what follows path, a field path of r's kind, in r's path, and
// whether path is at or above r's name.
func (r *reference) under(path []string) ([]string, bool) {
	rest := r.path
	for _, field := range path {
		if len(rest) == 0 || rest[0] != field {
			return nil, false
		}
		rest = rest[1:]
	}
	return rest, true
}

// refer writes a candidate at r's name under each parent that value, the
// drawn value at path, holds, where path is at or above r's name. It
// writes into a copy of value.
func (g *Generator) refer(t *rapid.T, at state, r *reference, path []string, value any) any {
	rest, holds := r.under(path)
	if !holds {
		return value
	}
	return writeNames(runtime.DeepCopyJSONValue(value), rest, func() (string, bool) { return g.candidate(t, at, r) })
}

// writeNames writes a drawn name at path in value, under each parent that
// value holds, and returns value. A draw may give no name, which leaves the
// value at path as it was.
func writeNames(value any, path []string, draw func() (string, bool)) any {
	if len(path) == 0 {
		if name, drawn := draw(); drawn {
			return name
		}
		return value
	}
	if path[0] == each {
		items, _ := value.([]any)
		for i, item := range items {
			items[i] = writeNames(item, path[1:], draw)
		}
		return value
	}
	object, isObject := value.(map[string]any)
	switch child, present := object[path[0]]; {
	case !isObject:
	case len(path) == 1:
		if name, drawn := draw(); drawn {
			object[path[0]] = name
		}
	case present:
		object[path[0]] = writeNames(child, path[1:], draw)
	}
	return value
}

// candidate draws the name that one write of r holds: missing 1 time in 4
// when r is not required, else a live candidate half the time when one is,
// else any candidate. It draws none when r has no candidate.
func (g *Generator) candidate(t *rapid.T, at state, r *reference) (string, bool) {
	if !r.required && uniform(t, 4, "missing") == 0 {
		return missing, true
	}
	names, live := g.candidates(at, r)
	switch {
	case len(live) > 0 && rapid.Bool().Draw(t, "live"):
		return sampled(t, live, "candidate"), true
	case len(names) > 0:
		return sampled(t, names, "candidate"), true
	}
	return "", false
}

// candidates are the names of r's candidates, and of those that are live:
// each slot's CR of a listed to kind, or else each fixture.
func (g *Generator) candidates(at state, r *reference) (names, live []string) {
	if r.to != nil {
		for i, slot := range slots {
			if cr := at[slot]; cr != nil && g.kindOf(cr) == r.to {
				live = append(live, r.slotNames[i])
			}
		}
		return r.slotNames, live
	}
	for _, target := range r.fixtures {
		names = append(names, target.Name)
		if !at.deleted(target) {
			live = append(live, target.Name)
		}
	}
	return names, live
}

// fixtureChanges are the live fixtures whose strings changeFixture may set,
// then the live referenced fixtures, which it deletes.
func (g *Generator) fixtureChanges(at state) []fixture {
	var changes []fixture
	for _, f := range g.fixtures {
		if !at.deleted(f.target) {
			changes = append(changes, f)
		}
	}
	for _, target := range g.referenced {
		if !at.deleted(target) {
			changes = append(changes, fixture{target: target})
		}
	}
	return changes
}

// deletedFixtures are the referenced fixtures that the plan deleted.
func (g *Generator) deletedFixtures(at state) []plan.Target {
	return slices.DeleteFunc(slices.Clone(g.referenced), func(target plan.Target) bool { return !at.deleted(target) })
}

// changeFixture sets a drawn word at one of f's paths, or deletes the
// fixture when f has none.
func changeFixture(t *rapid.T, at state, f fixture) plan.Action {
	target := f.target
	if len(f.paths) == 0 {
		at[gone(target)] = nil
		return plan.Action{Do: plan.ChangeFixture, Target: &target}
	}
	return plan.Action{Do: plan.ChangeFixture, Target: &target, Patch: marshal(nest(sampled(t, f.paths, "path"), fixtureWords.Draw(t, "word")))}
}
