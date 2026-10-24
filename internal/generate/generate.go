// Package generate draws plans from a seed. rapid draws CRD-valid values;
// it neither runs steps nor shrinks.
package generate

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"os"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	gvk "k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Generator draws plans for one config. A draw does no I/O.
type Generator struct {
	// kinds are the listed kinds, in config order.
	kinds []*crKind
	// listed are the config's listed kinds, which each create must make.
	listed []gvk.GroupKind
	// resources are the listed kinds first, then the fixture and RBAC
	// resources, which damage targets.
	resources []gvk.GroupResource
	// granted holds the verbs the RBAC grants on each resource and
	// subresource, such as "widgets/status".
	granted map[gvk.GroupResource][]string
	// requests are what faults match, each with the weight of its draw.
	requests []request
	fixtures []fixture
	// watchable are the resources the RBAC lets the controller watch,
	// which holdWatch faults hold. A kind appears once per unit of its
	// holdWeight.
	watchable []gvk.GroupResource
	// owned are the kube kinds that the entries' owns name.
	owned []gvk.GroupResource
	// models are the resources of the model kinds, which model faults match.
	models []gvk.GroupResource
	// fakes are the fake services' names. modelFakes take model faults,
	// and readOnly fakes take no damage and no write faults.
	fakes      []string
	modelFakes map[string]bool
	readOnly   map[string]bool
	// track is the longest dependency track, which a model fault's period
	// adds.
	track  time.Duration
	stable time.Duration
	plans  *rapid.Generator[*plan.Plan]
}

// crKind is what the generator draws the CRs of one listed kind from.
type crKind struct {
	groupKind gvk.GroupKind
	resource  gvk.GroupResource
	rules     *crdRules
	sample    *unstructured.Unstructured
	fields    []field
	leftAlone []string
	distinct  [][]string
}

// fixture is one fixture whose strings changeFixture may set.
type fixture struct {
	target plan.Target
	paths  [][]string
}

var slots = []string{"a", "b", "c"}

// New reads each listed kind's CRD schema, sample and values, and the
// fixtures of cfg.
func New(cfg *config.Config) (*Generator, error) {
	g, err := build(cfg)
	if err != nil {
		return nil, fmt.Errorf("generating plans: %w", err)
	}
	return g, nil
}

func build(cfg *config.Config) (*Generator, error) {
	g := &Generator{listed: cfg.ListedKinds(), stable: cfg.Timeouts.Stable, granted: map[gvk.GroupResource][]string{}, modelFakes: map[string]bool{}, readOnly: map[string]bool{}}
	for _, p := range cfg.Primaries {
		k, err := newCRKind(cfg.CRDs, p)
		if err != nil && len(cfg.Primaries) > 1 {
			err = fmt.Errorf("%s: %w", p.Kind.Kind, err)
		}
		if err != nil {
			return nil, err
		}
		g.kinds = append(g.kinds, k)
		g.addResource(k.resource)
		for _, o := range p.Owns {
			if o.System == "" {
				gvr, _ := meta.UnsafeGuessKindToResource(o.Kind)
				g.owned = append(g.owned, gvr.GroupResource())
			}
		}
	}
	if err := g.readFixtures(cfg); err != nil {
		return nil, err
	}
	if err := g.readRBAC(cfg.Launch.RBAC); err != nil {
		return nil, err
	}
	g.addRequests()
	for n, gr := range g.resources {
		if verbs := g.granted[gr]; slices.Contains(verbs, "watch") || slices.Contains(verbs, "*") {
			for range g.holdWeight(n, gr) {
				g.watchable = append(g.watchable, gr)
			}
		}
	}
	if err := g.readModels(cfg); err != nil {
		return nil, err
	}
	for _, f := range cfg.Fakes() {
		g.fakes = append(g.fakes, f.Name)
		g.modelFakes[f.Name] = f.Map().ModelFaults
		g.readOnly[f.Name] = f.ReadOnly
		if f.Dependency != nil {
			g.track = max(g.track, f.Dependency.Track)
		}
	}
	g.plans = rapid.Custom(g.plan)
	return g, nil
}

// newCRKind reads one entry's CRD rules and schema, its sample, and the
// fields and distinct paths its values name.
func newCRKind(crds []string, p config.Primary) (*crKind, error) {
	crd, plural, err := primaryCRD(crds, p.Kind)
	if err != nil {
		return nil, err
	}
	// The API server judges a CR against the CRD as declared, so the rules
	// are read before the overlay changes it.
	rules, err := newCRDRules(crd)
	if err != nil {
		return nil, err
	}
	k := &crKind{groupKind: p.Kind.GroupKind(), resource: gvk.GroupResource{Group: p.Kind.Group, Resource: plural}, rules: rules, sample: &unstructured.Unstructured{}}
	if k.sample.Object, err = readObject(p.Sample); err != nil {
		return nil, err
	}
	if held := k.sample.GroupVersionKind().GroupKind(); held != k.groupKind {
		return nil, fmt.Errorf("the sample holds a %s, not a %s", held, k.groupKind)
	}
	if err := rules.refusal(k.sample.Object, nil); err != nil {
		return nil, fmt.Errorf("the CRD refuses the sample: %w", err)
	}
	narrowed, err := overlaid(crd, p.Values.Overlay)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err := mutableFields(p.Values.Mutate, narrowed, k.sample.Object)
	if err != nil {
		return nil, err
	}
	if k.fields, k.leftAlone, err = k.drawable(len(p.Values.Mutate) > 0, fields, leftAlone); err != nil {
		return nil, err
	}
	for _, dotted := range p.Values.Distinct {
		path := strings.Split(dotted, ".")
		if _, found, _ := unstructured.NestedString(k.sample.Object, path...); !found {
			return nil, fmt.Errorf("values.distinct %s: the sample holds no string there", dotted)
		}
		k.distinct = append(k.distinct, path)
		k.fields = slices.DeleteFunc(k.fields, func(f field) bool { return f.dotted == dotted })
	}
	for _, slot := range slots[1:] {
		if err := rules.refusal(k.base(slot).Object, nil); err != nil {
			return nil, fmt.Errorf("the CRD refuses the CR of slot %s: %w", slot, err)
		}
	}
	return k, nil
}

// drawable keeps the fields the CRD accepts a drawn value of in the sample.
// A refused field that values.mutate names is a configuration error, and any
// other is left alone.
func (k *crKind) drawable(declared bool, fields []field, leftAlone []string) ([]field, []string, error) {
	var kept []field
	for _, mutable := range fields {
		refused := k.rules.acceptsADraw(k.sample, mutable)
		switch {
		case refused == nil:
			kept = append(kept, mutable)
		case declared:
			return nil, nil, fmt.Errorf("values.mutate %s: %w", mutable.dotted, refused)
		default:
			leftAlone = append(leftAlone, leftAloneNote(mutable.dotted, refused))
		}
	}
	slices.Sort(leftAlone)
	return kept, leftAlone, nil
}

// LeftAlone says which spec paths generation never changes, and why. Only a
// kind without values.mutate has any. With several kinds listed, each note
// starts with its kind.
func (g *Generator) LeftAlone() []string {
	if len(g.kinds) == 1 {
		return g.kinds[0].leftAlone
	}
	var notes []string
	for _, k := range g.kinds {
		for _, note := range k.leftAlone {
			notes = append(notes, k.groupKind.Kind+": "+note)
		}
	}
	return notes
}

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := yaml.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return normalized(object).(map[string]any), nil
}

func (g *Generator) addResource(gr gvk.GroupResource) {
	if !trace.Ignored(trace.Call{Ref: trace.Ref{System: "kube", Group: gr.Group, Resource: gr.Resource}}) && !slices.Contains(g.resources, gr) {
		g.resources = append(g.resources, gr)
	}
}

func (g *Generator) readFixtures(cfg *config.Config) error {
	for _, path := range cfg.Fixtures {
		documents, err := readDocuments(path)
		if err != nil {
			return err
		}
		paths := cfg.FixtureValues[path]
		if len(paths) > 0 && len(documents) != 1 {
			return fmt.Errorf("values.fixtures %s: the file holds %d objects, want 1", path, len(documents))
		}
		for _, document := range documents {
			object := unstructured.Unstructured{Object: document}
			gvr, _ := meta.UnsafeGuessKindToResource(object.GroupVersionKind())
			g.addResource(gvr.GroupResource())
			if len(paths) == 0 {
				continue
			}
			f := fixture{target: plan.Target{Group: gvr.Group, Resource: gvr.Resource, Namespace: object.GetNamespace(), Name: object.GetName()}}
			for _, dotted := range paths {
				f.paths = append(f.paths, strings.Split(dotted, "."))
			}
			g.fixtures = append(g.fixtures, f)
		}
	}
	for _, s := range cfg.Systems {
		for _, path := range s.Fixtures {
			paths := cfg.FixtureValues[path]
			if len(paths) == 0 {
				continue
			}
			entries, err := config.ReadSystemFixtures(path)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				f := fixture{target: plan.Target{System: s.Name, Resource: entry.Resource, Name: entry.Name}}
				for _, dotted := range paths {
					f.paths = append(f.paths, strings.Split(dotted, "."))
				}
				g.fixtures = append(g.fixtures, f)
			}
		}
	}
	return nil
}

// readRBAC adds the resources the rules name, without wildcards and
// subresources, and records the verbs each rule grants.
func (g *Generator) readRBAC(paths []string) error {
	documents, err := readManifests(paths)
	if err != nil {
		return fmt.Errorf("launch.rbac: %w", err)
	}
	for _, document := range documents {
		var role struct {
			Rules []struct {
				APIGroups []string `json:"apiGroups"`
				Resources []string `json:"resources"`
				Verbs     []string `json:"verbs"`
			} `json:"rules"`
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(document, &role); err != nil {
			return fmt.Errorf("launch.rbac: %w", err)
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				for _, resource := range rule.Resources {
					if group == "*" || resource == "*" {
						continue
					}
					gr := gvk.GroupResource{Group: group, Resource: resource}
					g.granted[gr] = append(g.granted[gr], rule.Verbs...)
					if !strings.Contains(resource, "/") {
						g.addResource(gr)
					}
				}
			}
		}
	}
	return nil
}

// readModels reads each model kind's resource from its CRD, or guesses it,
// and the longest dependency track.
func (g *Generator) readModels(cfg *config.Config) error {
	if len(cfg.Models) == 0 {
		return nil
	}
	crds, err := readManifests(cfg.CRDs)
	if err != nil {
		return fmt.Errorf("reading the CRDs: %w", err)
	}
	for _, m := range cfg.Models {
		gvr, _ := meta.UnsafeGuessKindToResource(m.Kind)
		gr := gvr.GroupResource()
		if plural := pluralOf(crds, m.Kind.GroupKind()); plural != "" {
			gr.Resource = plural
		}
		g.models = append(g.models, gr)
		if m.Dependency != nil {
			g.track = max(g.track, m.Dependency.Track)
		}
	}
	return nil
}

// Plan draws the plan for seed. The same seed draws the same plan.
func (g *Generator) Plan(seed uint64) (p *plan.Plan, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("drawing the plan of seed %d: %v", seed, recovered)
		}
	}()
	p = g.plans.Example(int(seed))
	p.Seed = seed
	if err := errors.Join(p.Validate(), p.ValidateKinds(g.listed)); err != nil {
		return nil, fmt.Errorf("the plan of seed %d is invalid: %w", seed, err)
	}
	return p, nil
}

// state is what the plan so far leaves each slot holding: its CR as last
// written, or nil once deleted.
type state map[string]map[string]any

func (s state) live() []string {
	return slices.DeleteFunc(slices.Clone(slots), func(slot string) bool { return s[slot] == nil })
}

func (s state) free() []string {
	return slices.DeleteFunc(slices.Clone(slots), func(slot string) bool { return s[slot] != nil })
}

// kindOf is the listed kind of a CR the plan holds, by its apiVersion and
// kind.
func (g *Generator) kindOf(cr map[string]any) *crKind {
	held := (&unstructured.Unstructured{Object: cr}).GroupVersionKind().GroupKind()
	return g.kinds[slices.IndexFunc(g.kinds, func(k *crKind) bool { return k.groupKind == held })]
}

// updatable are the live slots whose kind has fields to change.
func (g *Generator) updatable(at state) []string {
	return slices.DeleteFunc(at.live(), func(slot string) bool { return len(g.kindOf(at[slot]).fields) == 0 })
}

// ofResource are the slots whose CR is of the resource gr.
func (g *Generator) ofResource(at state, slots []string, gr gvk.GroupResource) []string {
	return slices.DeleteFunc(slices.Clone(slots), func(slot string) bool { return g.kindOf(at[slot]).resource != gr })
}

func (g *Generator) plan(t *rapid.T) *plan.Plan {
	at := state{}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{g.create(t, at, "a")}}}}
	for range 2 + uniform(t, 6, "steps") {
		p.Steps = append(p.Steps, g.step(t, at))
	}
	return p
}

// step draws one step: a single restart, kill or damage, or a fault with
// one, in about 30% of steps, else 1 to 4 actions and maybe a fault.
func (g *Generator) step(t *rapid.T, at state) plan.Step {
	var step plan.Step
	switch {
	case uniform(t, 10, "single") < 3:
		singles := []plan.Do{plan.Restart, plan.Kill, ""}
		if g.damageable() {
			singles = append(singles, plan.Damage)
		}
		switch do := sampled(t, singles, "single"); do {
		case "":
			step.Faults = []plan.Fault{g.fault(t)}
			if step.Faults[0].HoldWatch {
				step.Actions = g.holdTraffic(t, at, step.Faults[0])
			} else {
				step.Actions = []plan.Action{g.traffic(t, at, step.Faults[0])}
			}
		case plan.Damage:
			step.Actions = []plan.Action{g.damage(t, at)}
		default:
			step.Actions = []plan.Action{{Do: do}}
		}
	case len(at.live()) > 0 && uniform(t, 10, "burst") < 3:
		step.Actions = g.burst(t, at)
	default:
		if rapid.Bool().Draw(t, "fault") {
			step.Faults = []plan.Fault{g.fault(t)}
		}
		if len(step.Faults) > 0 && step.Faults[0].HoldWatch {
			step.Actions = g.holdTraffic(t, at, step.Faults[0])
			break
		}
		for range 1 + uniform(t, 4, "actions") {
			step.Actions = append(step.Actions, g.action(t, at))
		}
	}
	if len(step.Faults) > 0 || slices.ContainsFunc(step.Actions, func(a plan.Action) bool { return a.Do == plan.Kill }) {
		step.Period = g.period(t)
		if len(step.Faults) > 0 && step.Faults[0].ModelFault() {
			step.Period += plan.Duration(g.track)
		}
	}
	return step
}

// burst puts a delete then a create of one slot's kind, or two updates,
// within 50 ms.
func (g *Generator) burst(t *rapid.T, at state) []plan.Action {
	slot := sampled(t, at.live(), "slot")
	k := g.kindOf(at[slot])
	if len(k.fields) > 0 && rapid.Bool().Draw(t, "updates") {
		return within50ms(t, []plan.Action{g.update(t, at, slot), g.update(t, at, slot)})
	}
	return within50ms(t, []plan.Action{g.delete(t, at, slot), g.createOf(t, at, slot, k)})
}

// holdTraffic is the action a holdWatch takes: a live CR, of the held kind
// when one is, deleted and created again as it was within 50 ms, which a
// stale cache can mistake for the old CR, or else a create.
func (g *Generator) holdTraffic(t *rapid.T, at state, f plan.Fault) []plan.Action {
	live := at.live()
	if held := g.ofResource(at, live, gvk.GroupResource{Group: f.Match.Group, Resource: f.Match.Resource}); len(held) > 0 {
		live = held
	}
	if len(live) == 0 {
		return []plan.Action{g.create(t, at, sampled(t, at.free(), "slot"))}
	}
	slot := sampled(t, live, "slot")
	cr := at[slot]
	deleted := g.delete(t, at, slot)
	at[slot] = cr
	return within50ms(t, []plan.Action{deleted, {Do: plan.Create, Slot: slot, Object: marshal(cr)}})
}

func within50ms(t *rapid.T, actions []plan.Action) []plan.Action {
	offsets := []int{uniform(t, 51, "at"), uniform(t, 51, "at")}
	slices.Sort(offsets)
	for i := range actions {
		actions[i].At = plan.Duration(time.Duration(offsets[i]) * time.Millisecond)
	}
	return actions
}

func (g *Generator) action(t *rapid.T, at state) plan.Action {
	legal := []plan.Do{plan.Restart, plan.Kill}
	if len(at.free()) > 0 {
		legal = append(legal, plan.Create)
	}
	if len(at.live()) > 0 {
		legal = append(legal, plan.Delete)
	}
	if len(g.updatable(at)) > 0 {
		legal = append(legal, plan.Update)
	}
	if g.damageable() {
		legal = append(legal, plan.Damage)
	}
	if len(g.fixtures) > 0 {
		legal = append(legal, plan.ChangeFixture)
	}
	switch do := sampled(t, legal, "do"); do {
	case plan.Create:
		return g.create(t, at, sampled(t, at.free(), "slot"))
	case plan.Update:
		return g.update(t, at, sampled(t, g.updatable(at), "slot"))
	case plan.Delete:
		return g.delete(t, at, sampled(t, at.live(), "slot"))
	case plan.Damage:
		return g.damage(t, at)
	case plan.ChangeFixture:
		f := sampled(t, g.fixtures, "fixture")
		target := f.target
		return plan.Action{Do: do, Target: &target, Patch: marshal(nest(sampled(t, f.paths, "path"), fixtureWords.Draw(t, "word")))}
	default:
		return plan.Action{Do: do}
	}
}

// create creates the slot's CR, of a listed kind drawn uniformly.
func (g *Generator) create(t *rapid.T, at state, slot string) plan.Action {
	return g.createOf(t, at, slot, sampled(t, g.kinds, "kind"))
}

func (g *Generator) createOf(t *rapid.T, at state, slot string, k *crKind) plan.Action {
	cr := k.cr(t, slot)
	at[slot] = cr.Object
	return plan.Action{Do: plan.Create, Slot: slot, Object: marshal(cr.Object)}
}

// update changes one field of the slot's CR, or restarts the controller
// when its CRD refused every patch drawn.
func (g *Generator) update(t *rapid.T, at state, slot string) plan.Action {
	k := g.kindOf(at[slot])
	for range updateDraws {
		mutable := sampled(t, k.fields, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent.
		patch := nest(mutable.path, value)
		patched := mergePatch(runtime.DeepCopyJSON(at[slot]), patch)
		if k.rules.refusal(patched, at[slot]) == nil {
			at[slot] = patched
			return plan.Action{Do: plan.Update, Slot: slot, Patch: marshal(patch)}
		}
	}
	return plan.Action{Do: plan.Restart}
}

// delete deletes the slot's CR, with Foreground propagation 1 time in 4.
func (g *Generator) delete(t *rapid.T, at state, slot string) plan.Action {
	at[slot] = nil
	a := plan.Action{Do: plan.Delete, Slot: slot}
	if uniform(t, 4, "propagation") == 0 {
		a.Propagation = plan.Foreground
	}
	return a
}

// unlisted are the resources after the listed kinds, which damage targets.
func (g *Generator) unlisted() []gvk.GroupResource { return g.resources[len(g.kinds):] }

// damageable says whether a kube resource or a fake can take damage.
func (g *Generator) damageable() bool { return len(g.unlisted()) > 0 || len(g.writable()) > 0 }

// writable are the fakes that are not read-only.
func (g *Generator) writable() []string {
	return slices.DeleteFunc(slices.Clone(g.fakes), func(name string) bool { return g.readOnly[name] })
}

// damage deletes an object of a drawn resource, of a live slot when one is.
// With writable fakes, half the draws, or all without a kube resource,
// target one drawn uniformly.
func (g *Generator) damage(t *rapid.T, at state) plan.Action {
	if fakes := g.writable(); len(fakes) > 0 && (len(g.unlisted()) == 0 || uniform(t, 2, "damage a fake") == 0) {
		return g.damageOf(t, at, plan.Target{System: sampled(t, fakes, "fake")})
	}
	gr := sampled(t, g.unlisted(), "resource")
	return g.damageOf(t, at, plan.Target{Group: gr.Group, Resource: gr.Resource})
}

func (g *Generator) damageOf(t *rapid.T, at state, target plan.Target) plan.Action {
	a := plan.Action{Do: plan.Damage, Target: &target}
	if live := at.live(); len(live) > 0 {
		a.Slot = sampled(t, live, "slot")
	}
	return a
}

// traffic is an action that exercises a fault during its period. A request
// fault takes a damage of its resource, or an update of a live CR of its
// kind for a fault on a listed kind. Stuck takes an update of a live CR,
// which gives its dependencies a new generation to keep not ready, or else
// a create. SlowDelete takes a delete of a live CR, whose dependencies'
// deletion it holds; a later step may create the slot again. Otherwise a
// restart.
func (g *Generator) traffic(t *rapid.T, at state, f plan.Fault) plan.Action {
	if f.System != "" {
		return g.fakeTraffic(t, at, f)
	}
	gr := gvk.GroupResource{Group: f.Match.Group, Resource: f.Match.Resource}
	updatable := g.updatable(at)
	ofFault := g.ofResource(at, updatable, gr)
	switch {
	case f.Stuck && len(updatable) > 0:
		return g.update(t, at, sampled(t, updatable, "slot"))
	case f.Stuck && len(at.free()) > 0:
		return g.create(t, at, sampled(t, at.free(), "slot"))
	case f.SlowDelete && len(at.live()) > 0:
		return g.delete(t, at, sampled(t, at.live(), "slot"))
	case f.ModelFault():
		return plan.Action{Do: plan.Restart}
	case slices.Contains(g.unlisted(), gr):
		return g.damageOf(t, at, plan.Target{Group: gr.Group, Resource: gr.Resource})
	case len(ofFault) > 0:
		return g.update(t, at, sampled(t, ofFault, "slot"))
	}
	return plan.Action{Do: plan.Restart}
}

// fakeTraffic is an action that exercises a fault on a fake. A delete or
// slowDelete takes a delete of a live CR. Stuck, a create or any verb
// takes a create, which makes a new resource. Without a CR to delete or a
// slot to create, or for another verb, it takes damage of the fake, which
// the controller repairs. A read-only fake's read takes a create, or an
// update of a live CR, which the controller reads the fake for.
func (g *Generator) fakeTraffic(t *rapid.T, at state, f plan.Fault) plan.Action {
	if g.readOnly[f.System] {
		switch updatable := g.updatable(at); {
		case len(at.free()) > 0:
			return g.create(t, at, sampled(t, at.free(), "slot"))
		case len(updatable) > 0:
			return g.update(t, at, sampled(t, updatable, "slot"))
		}
		return plan.Action{Do: plan.Restart}
	}
	deletes := f.SlowDelete || f.Match.Verb == "delete"
	switch creates := f.Stuck || f.Match.Verb == "create" || f.Match.Verb == ""; {
	case deletes && len(at.live()) > 0:
		return g.delete(t, at, sampled(t, at.live(), "slot"))
	case (creates || deletes) && len(at.free()) > 0:
		return g.create(t, at, sampled(t, at.free(), "slot"))
	}
	return g.damageOf(t, at, plan.Target{System: f.System})
}

// nths favor the first match and every match, which a step's few requests
// reach.
var nths = []int{0, 0, 1, 1, 1, 1, 2, 3}

// request is a kind of request a fault can match.
type request struct {
	match  plan.Match
	weight int
}

// verbWeights favor the writes whose outcome a fault can hide, creates most.
var verbWeights = map[string]int{"get": 1, "list": 1, "create": 6, "update": 2, "patch": 2, "delete": 2}

// favored is how much more often faults match the listed kinds and the
// core kinds the controller may create, such as Secrets.
const favored = 4

// holdWeight is favored for the listed kinds, g.resources[:len(g.kinds)],
// and for the kube kinds the entries' owns name, else 1. Without owned
// kinds it is kindWeight.
func (g *Generator) holdWeight(n int, gr gvk.GroupResource) int {
	if len(g.owned) == 0 {
		return g.kindWeight(n, gr)
	}
	if n < len(g.kinds) || slices.Contains(g.owned, gr) {
		return favored
	}
	return 1
}

// kindWeight is favored for the listed kinds, g.resources[:len(g.kinds)],
// and for core kinds the RBAC lets the controller create, else 1.
func (g *Generator) kindWeight(n int, gr gvk.GroupResource) int {
	if n < len(g.kinds) || gr.Group == "" && slices.Contains(g.granted[gr], "create") {
		return favored
	}
	return 1
}

// addRequests adds a request for each verb the RBAC grants on each resource
// and its status, or get and list on a resource it does not name.
func (g *Generator) addRequests() {
	for n, gr := range g.resources {
		kindWeight := g.kindWeight(n, gr)
		for _, subresource := range []string{"", "status"} {
			named := gr
			if subresource != "" {
				named.Resource += "/" + subresource
			}
			verbs, granted := g.granted[named]
			if !granted && subresource != "" {
				continue
			}
			if !granted {
				verbs = []string{"get", "list"}
			}
			if slices.Contains(verbs, "*") {
				verbs = slices.Collect(maps.Keys(verbWeights))
			}
			slices.Sort(verbs)
			for _, verb := range slices.Compact(verbs) {
				if weight := verbWeights[verb]; weight > 0 {
					m := plan.Match{Verb: verb, Group: gr.Group, Resource: gr.Resource, Subresource: subresource}
					g.requests = append(g.requests, request{m, kindWeight * weight})
				}
			}
		}
	}
}

// fault draws a request and one of five request effects. With models, it
// draws one of seven effects instead, and the last two are stuck and
// slowDelete on a model kind drawn uniformly. With a watchable kind, two
// more effects are a holdWatch of one drawn uniformly. With fakes, half the
// draws are a fake fault instead.
func (g *Generator) fault(t *rapid.T) plan.Fault {
	if len(g.fakes) > 0 && uniform(t, 2, "fault on a fake") == 0 {
		return g.fakeFault(t)
	}
	total := 0
	for _, r := range g.requests {
		total += r.weight
	}
	n := uniform(t, total, "request")
	i := 0
	for ; n >= g.requests[i].weight; i++ {
		n -= g.requests[i].weight
	}
	f := plan.Fault{Match: g.requests[i].match, Nth: sampled(t, nths, "nth")}
	effects := 5
	if len(g.models) > 0 {
		effects = 7
	}
	if len(g.watchable) > 0 {
		effects += 2
	}
	switch effect := uniform(t, effects, "effect"); {
	case effect >= effects-2 && len(g.watchable) > 0:
		gr := sampled(t, g.watchable, "hold")
		return plan.Fault{Match: plan.Match{Group: gr.Group, Resource: gr.Resource}, HoldWatch: true}
	case effect == 0:
		f.Error = &plan.Error{Code: sampled(t, []int{500, 503, 429}, "code")}
	case effect == 1:
		delay := plan.Duration(g.stable / time.Duration(sampled(t, []int{4, 2, 1}, "delay")))
		f.Delay = &delay
	case effect == 2:
		f.Drop = true
	case effect == 3:
		f.DropResponse = true
	case effect == 4:
		f.KillAfter = true
	default:
		gr := sampled(t, g.models, "model")
		f = plan.Fault{Match: plan.Match{Group: gr.Group, Resource: gr.Resource}, Stuck: effect == 5, SlowDelete: effect == 6}
	}
	return f
}

// fakeVerbs are the crud map's verbs, each drawn equally, and readVerbs
// those a read-only fake's faults match.
var (
	fakeVerbs = []string{"create", "get", "list", "update", "patch", "delete"}
	readVerbs = []string{"get", "list"}
)

// fakeFault draws a fake uniformly, then one of seven effects: five request
// effects on the first or every request, of a verb or of any verb, or
// stuck or slowDelete on every collection. A fake without model faults
// draws one of the five. A controller may use few of the verbs, so half
// the request faults match any. A read-only fake's match a read verb.
func (g *Generator) fakeFault(t *rapid.T) plan.Fault {
	f := plan.Fault{System: sampled(t, g.fakes, "fake")}
	effects := 5
	if g.modelFakes[f.System] {
		effects = 7
	}
	switch effect := uniform(t, effects, "effect"); effect {
	case 5:
		f.Stuck = true
		return f
	case 6:
		f.SlowDelete = true
		return f
	case 0:
		f.Error = &plan.Error{Code: sampled(t, []int{500, 503, 409}, "code")}
	case 1:
		delay := plan.Duration(g.stable / time.Duration(sampled(t, []int{4, 2, 1}, "delay")))
		f.Delay = &delay
	case 2:
		f.Drop = true
	case 3:
		f.DropResponse = true
	case 4:
		f.KillAfter = true
	}
	switch {
	case g.readOnly[f.System]:
		f.Match.Verb = sampled(t, readVerbs, "verb")
	case uniform(t, 2, "any verb") == 0:
		f.Match.Verb = sampled(t, fakeVerbs, "verb")
	}
	f.Nth = uniform(t, 2, "nth")
	return f
}

// period lasts 0.5 s to twice stable, in tenths of a second.
func (g *Generator) period(t *rapid.T) plan.Duration {
	const least, tick = 500 * time.Millisecond, 100 * time.Millisecond
	ticks := max(0, int((2*g.stable-least)/tick))
	return plan.Duration(least + time.Duration(uniform(t, ticks+1, "period"))*tick)
}

// cr is the slot's base with a subset of the mutable fields changed. A
// change its CRD refuses is undone.
func (k *crKind) cr(t *rapid.T, slot string) *unstructured.Unstructured {
	cr := k.base(slot)
	for _, mutable := range k.fields {
		if !rapid.Bool().Draw(t, "mutate "+mutable.dotted) {
			continue
		}
		value, present := mutable.draw(t)
		changed := cr.DeepCopy()
		if !present {
			unstructured.RemoveNestedField(changed.Object, mutable.path...)
		} else if err := unstructured.SetNestedField(changed.Object, value, mutable.path...); err != nil {
			t.Fatalf("The sample does not take a %s: %v.", mutable.dotted, err)
		}
		if k.rules.refusal(changed.Object, nil) == nil {
			cr = changed
		}
	}
	return cr
}

// base is the sample as the slot's CR. Each slot after a appends -<slot> to
// the sample's name and to its value at each distinct path.
func (k *crKind) base(slot string) *unstructured.Unstructured {
	cr := k.sample.DeepCopy()
	if slot == slots[0] {
		return cr
	}
	cr.SetName(cr.GetName() + "-" + slot)
	for _, path := range k.distinct {
		value, _, _ := unstructured.NestedString(cr.Object, path...)
		_ = unstructured.SetNestedField(cr.Object, value+"-"+slot, path...)
	}
	return cr
}

// updateDraws bounds how often an update is drawn again where the CRD
// refuses it.
const updateDraws = 8

// draw is one value for the field, or its removal where the schema allows
// the field to be absent.
func (f field) draw(t *rapid.T) (any, bool) {
	if f.optional && rapid.Bool().Draw(t, "remove "+f.dotted) {
		return nil, false
	}
	return f.values.Draw(t, f.dotted), true
}

// uniform draws a number below n, each with equal chance. rapid's own
// integers favor small numbers.
func uniform(t *rapid.T, n int, label string) int {
	if n < 1 {
		panic(fmt.Sprintf("drawing %s from none", label))
	}
	width := bits.Len(uint(n - 1))
	for {
		i := 0
		for range width {
			i <<= 1
			if rapid.Bool().Draw(t, label) {
				i |= 1
			}
		}
		if i < n {
			return i
		}
	}
}

// sampled draws an element of the slice, each with equal chance.
func sampled[E any](t *rapid.T, slice []E, label string) E {
	return slice[uniform(t, len(slice), label)]
}

// fixtureWords are short words whose length is a multiple of four, so that
// a Secret's data reads each as base64.
var fixtureWords = rapid.Custom(func(t *rapid.T) any {
	length := 4 * rapid.IntRange(1, 3).Draw(t, "quads")
	return rapid.StringOfN(wordRunes, length, length, -1).Draw(t, "word")
})

// nest wraps a value in the objects its path names, which is the merge
// patch that changes that one field.
func nest(path []string, value any) map[string]any {
	nested := value
	for i := len(path) - 1; i >= 0; i-- {
		nested = map[string]any{path[i]: nested}
	}
	return nested.(map[string]any)
}

// mergePatch applies an RFC 7386 merge patch to object in place.
func mergePatch(object, patch map[string]any) map[string]any {
	if object == nil {
		object = map[string]any{}
	}
	for key, value := range patch {
		nested, isObject := value.(map[string]any)
		switch {
		case value == nil:
			delete(object, key)
		case isObject:
			into, _ := object[key].(map[string]any)
			object[key] = mergePatch(into, nested)
		default:
			object[key] = value
		}
	}
	return object
}

func marshal(object map[string]any) json.RawMessage {
	data, err := json.Marshal(object)
	if err != nil {
		panic(err)
	}
	return data
}
