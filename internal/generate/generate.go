// Package generate draws plans from a seed. rapid draws CRD-valid values;
// it neither runs steps nor shrinks.
package generate

import (
	"encoding/json"
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
	rules     *crdRules
	sample    *unstructured.Unstructured
	fields    []field
	leftAlone []string
	distinct  [][]string
	// resources are the primary first, then the fixture and RBAC resources,
	// which damage targets.
	resources []gvk.GroupResource
	// granted holds the verbs the RBAC grants on each resource and
	// subresource, such as "widgets/status".
	granted map[gvk.GroupResource][]string
	// requests are what faults match, each with the weight of its draw.
	requests []request
	fixtures []fixture
	stable   time.Duration
	plans    *rapid.Generator[*plan.Plan]
}

// fixture is one fixture whose strings changeFixture may set.
type fixture struct {
	target plan.Target
	paths  [][]string
}

var slots = []string{"a", "b", "c"}

// New reads the primary kind's CRD schema, the sample, the values and the
// fixtures of cfg.
func New(cfg *config.Config) (*Generator, error) {
	g, err := build(cfg)
	if err != nil {
		return nil, fmt.Errorf("generating plans: %w", err)
	}
	return g, nil
}

func build(cfg *config.Config) (*Generator, error) {
	crd, plural, err := primaryCRD(cfg)
	if err != nil {
		return nil, err
	}
	// The API server judges a CR against the CRD as declared, so the rules
	// are read before the overlay changes it.
	rules, err := newCRDRules(crd)
	if err != nil {
		return nil, err
	}
	g := &Generator{rules: rules, stable: cfg.Timeouts.Stable, sample: &unstructured.Unstructured{}, granted: map[gvk.GroupResource][]string{}}
	if g.sample.Object, err = readObject(cfg.Primary.Sample); err != nil {
		return nil, err
	}
	if err := rules.refusal(g.sample.Object, nil); err != nil {
		return nil, fmt.Errorf("the CRD refuses the sample: %w", err)
	}
	primary, err := overlaid(crd, cfg.Values.Overlay)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err := mutableFields(cfg.Values.Mutate, primary, g.sample.Object)
	if err != nil {
		return nil, err
	}
	if g.fields, g.leftAlone, err = g.drawable(len(cfg.Values.Mutate) > 0, fields, leftAlone); err != nil {
		return nil, err
	}
	for _, dotted := range cfg.Values.Distinct {
		path := strings.Split(dotted, ".")
		if _, found, _ := unstructured.NestedString(g.sample.Object, path...); !found {
			return nil, fmt.Errorf("values.distinct %s: the sample holds no string there", dotted)
		}
		g.distinct = append(g.distinct, path)
		g.fields = slices.DeleteFunc(g.fields, func(f field) bool { return f.dotted == dotted })
	}
	for _, slot := range slots[1:] {
		if err := rules.refusal(g.base(slot).Object, nil); err != nil {
			return nil, fmt.Errorf("the CRD refuses the CR of slot %s: %w", slot, err)
		}
	}
	g.addResource(gvk.GroupResource{Group: cfg.Primary.Kind.Group, Resource: plural})
	if err := g.readFixtures(cfg); err != nil {
		return nil, err
	}
	if err := g.readRBAC(cfg.Launch.RBAC); err != nil {
		return nil, err
	}
	g.addRequests()
	g.plans = rapid.Custom(g.plan)
	return g, nil
}

// drawable keeps the fields the CRD accepts a drawn value of in the sample.
// A refused field that values.mutate names is a configuration error, and any
// other is left alone.
func (g *Generator) drawable(declared bool, fields []field, leftAlone []string) ([]field, []string, error) {
	var kept []field
	for _, mutable := range fields {
		refused := g.rules.acceptsADraw(g.sample, mutable)
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
// config without values.mutate has any.
func (g *Generator) LeftAlone() []string { return g.leftAlone }

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
	if !trace.Ignored(trace.Call{Ref: trace.Ref{Group: gr.Group, Resource: gr.Resource}}) && !slices.Contains(g.resources, gr) {
		g.resources = append(g.resources, gr)
	}
}

func (g *Generator) readFixtures(cfg *config.Config) error {
	for _, path := range cfg.Fixtures {
		documents, err := readDocuments(path)
		if err != nil {
			return err
		}
		paths := cfg.Values.Fixtures[path]
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
			f := fixture{target: plan.Target{Group: gvr.Group, Resource: gvr.Resource, Name: object.GetName()}}
			for _, dotted := range paths {
				f.paths = append(f.paths, strings.Split(dotted, "."))
			}
			g.fixtures = append(g.fixtures, f)
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

// Plan draws the plan for seed. The same seed draws the same plan.
func (g *Generator) Plan(seed uint64) (p *plan.Plan, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("drawing the plan of seed %d: %v", seed, recovered)
		}
	}()
	p = g.plans.Example(int(seed))
	p.Seed = seed
	if err := p.Validate(); err != nil {
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
		if len(g.resources) > 1 {
			singles = append(singles, plan.Damage)
		}
		switch do := sampled(t, singles, "single"); do {
		case "":
			step.Faults = []plan.Fault{g.fault(t)}
			step.Actions = []plan.Action{g.traffic(t, at, step.Faults[0].Match)}
		case plan.Damage:
			step.Actions = []plan.Action{g.damage(t, at)}
		default:
			step.Actions = []plan.Action{{Do: do}}
		}
	case len(at.live()) > 0 && uniform(t, 10, "burst") < 3:
		step.Actions = g.burst(t, at)
	default:
		for range 1 + uniform(t, 4, "actions") {
			step.Actions = append(step.Actions, g.action(t, at))
		}
		if rapid.Bool().Draw(t, "fault") {
			step.Faults = []plan.Fault{g.fault(t)}
		}
	}
	if len(step.Faults) > 0 || slices.ContainsFunc(step.Actions, func(a plan.Action) bool { return a.Do == plan.Kill }) {
		step.Period = g.period(t)
	}
	return step
}

// burst puts a delete then a create of one slot, or two updates, within
// 50 ms.
func (g *Generator) burst(t *rapid.T, at state) []plan.Action {
	slot := sampled(t, at.live(), "slot")
	var actions []plan.Action
	if len(g.fields) > 0 && rapid.Bool().Draw(t, "updates") {
		actions = []plan.Action{g.update(t, at, slot), g.update(t, at, slot)}
	} else {
		actions = []plan.Action{g.delete(at, slot), g.create(t, at, slot)}
	}
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
		if len(g.fields) > 0 {
			legal = append(legal, plan.Update)
		}
	}
	if len(g.resources) > 1 {
		legal = append(legal, plan.Damage)
	}
	if len(g.fixtures) > 0 {
		legal = append(legal, plan.ChangeFixture)
	}
	switch do := sampled(t, legal, "do"); do {
	case plan.Create:
		return g.create(t, at, sampled(t, at.free(), "slot"))
	case plan.Update:
		return g.update(t, at, sampled(t, at.live(), "slot"))
	case plan.Delete:
		return g.delete(at, sampled(t, at.live(), "slot"))
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

func (g *Generator) create(t *rapid.T, at state, slot string) plan.Action {
	cr := g.cr(t, slot)
	at[slot] = cr.Object
	return plan.Action{Do: plan.Create, Slot: slot, Object: marshal(cr.Object)}
}

// update changes one field, or restarts the controller when the CRD refused
// every patch drawn.
func (g *Generator) update(t *rapid.T, at state, slot string) plan.Action {
	for range updateDraws {
		mutable := sampled(t, g.fields, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent.
		patch := nest(mutable.path, value)
		patched := mergePatch(runtime.DeepCopyJSON(at[slot]), patch)
		if g.rules.refusal(patched, at[slot]) == nil {
			at[slot] = patched
			return plan.Action{Do: plan.Update, Slot: slot, Patch: marshal(patch)}
		}
	}
	return plan.Action{Do: plan.Restart}
}

func (g *Generator) delete(at state, slot string) plan.Action {
	at[slot] = nil
	return plan.Action{Do: plan.Delete, Slot: slot}
}

// damage deletes an object of a drawn resource, of a live slot when one is.
func (g *Generator) damage(t *rapid.T, at state) plan.Action {
	return g.damageOf(t, at, sampled(t, g.resources[1:], "resource"))
}

func (g *Generator) damageOf(t *rapid.T, at state, gr gvk.GroupResource) plan.Action {
	a := plan.Action{Do: plan.Damage, Target: &plan.Target{Group: gr.Group, Resource: gr.Resource}}
	if live := at.live(); len(live) > 0 {
		a.Slot = sampled(t, live, "slot")
	}
	return a
}

// traffic is an action that makes the controller send requests during a
// fault's period: a damage of the fault's resource, an update of a live CR
// for a fault on the primary, or else a restart.
func (g *Generator) traffic(t *rapid.T, at state, m plan.Match) plan.Action {
	gr := gvk.GroupResource{Group: m.Group, Resource: m.Resource}
	switch {
	case slices.Contains(g.resources[1:], gr):
		return g.damageOf(t, at, gr)
	case gr == g.resources[0] && len(at.live()) > 0 && len(g.fields) > 0:
		return g.update(t, at, sampled(t, at.live(), "slot"))
	}
	return plan.Action{Do: plan.Restart}
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

// favored is how much more often faults match the primary and the core
// kinds the controller may create, such as Secrets.
const favored = 4

// addRequests adds a request for each verb the RBAC grants on each resource
// and its status, or get and list on a resource it does not name.
func (g *Generator) addRequests() {
	for n, gr := range g.resources {
		kindWeight := 1
		if n == 0 || gr.Group == "" && slices.Contains(g.granted[gr], "create") {
			kindWeight = favored
		}
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

func (g *Generator) fault(t *rapid.T) plan.Fault {
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
	switch uniform(t, 5, "effect") {
	case 0:
		f.Error = &plan.Error{Code: sampled(t, []int{500, 503, 429}, "code")}
	case 1:
		delay := plan.Duration(g.stable / time.Duration(sampled(t, []int{4, 2, 1}, "delay")))
		f.Delay = &delay
	case 2:
		f.Drop = true
	case 3:
		f.DropResponse = true
	default:
		f.KillAfter = true
	}
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
func (g *Generator) cr(t *rapid.T, slot string) *unstructured.Unstructured {
	cr := g.base(slot)
	for _, mutable := range g.fields {
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
		if g.rules.refusal(changed.Object, nil) == nil {
			cr = changed
		}
	}
	return cr
}

// base is the sample as the slot's CR. Each slot after a appends -<slot> to
// the sample's name and to its value at each distinct path.
func (g *Generator) base(slot string) *unstructured.Unstructured {
	cr := g.sample.DeepCopy()
	if slot == slots[0] {
		return cr
	}
	cr.SetName(cr.GetName() + "-" + slot)
	for _, path := range g.distinct {
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
