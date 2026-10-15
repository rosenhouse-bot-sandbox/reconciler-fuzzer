// Package generate draws the sequences a run executes. Values come from the
// primary CRD's OpenAPI v3 schema, tightened by the target's generate.mutate
// and generate.overlay. Generation starts from the target's sample, so a
// generated CR carries the fields a webhook demands and the schema does not
// describe.
package generate

import (
	"fmt"
	"math/bits"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// defaultMaxOps bounds a sequence that Options does not bound.
const defaultMaxOps = 6

// defaultMaxCRs bounds the CRs a sequence creates for a target that does not.
const defaultMaxCRs = 3

// Options tune generation.
type Options struct {
	// MaxOps bounds the ops a draw makes, counting the create it opens with.
	// Generation adds the settle waits that leave those ops judged, at most
	// two per drawn op, so a sequence holds at most 3*MaxOps.
	// Zero takes the default.
	MaxOps int
}

// Generator draws sequences for one target. New reads the target's CRDs; a
// draw does no I/O.
type Generator struct {
	target    *target.Target
	rules     *crdRules
	fields    []field
	leftAlone []string
	managed   []string
	// faultables are the resource plurals faults may match.
	faultables []string
	// updatable are the fixtures generation may set a string in.
	updatable []target.MutableFixture
	maxOps    int
	maxCRs    int
	// distinct are the paths at which no two CRs hold one value.
	distinct  [][]string
	sequences *rapid.Generator[run.Sequence]
}

// New reads the target's primary CRD and the constraints on generating from
// it. Every error it returns is a configuration error.
func New(t *target.Target, opts Options) (*Generator, error) {
	g, err := build(t, opts)
	if err != nil {
		return nil, fmt.Errorf("generating for the target %s: %w", t.Name, err)
	}
	return g, nil
}

func build(t *target.Target, opts Options) (*Generator, error) {
	crd, err := openAPISchema(t)
	if err != nil {
		return nil, err
	}
	// The API server judges a CR against the CRD as declared, so the rules are
	// read before the overlay changes it.
	rules, err := newCRDRules(crd)
	if err != nil {
		return nil, err
	}
	if err := rules.refusal(t.Sample.Object, nil); err != nil {
		return nil, fmt.Errorf("the CRD refuses the sample: %w", err)
	}
	primary, err := overlaid(crd, t.Generate.Overlay)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err := mutableFields(t, primary)
	if err != nil {
		return nil, err
	}
	fields, leftAlone, err = drawable(t, rules, fields, leftAlone)
	if err != nil {
		return nil, err
	}
	g := &Generator{target: t, rules: rules, fields: fields, leftAlone: leftAlone, maxOps: opts.MaxOps, maxCRs: t.Generate.MaxCRs}
	if g.maxOps < 1 {
		g.maxOps = defaultMaxOps
	}
	if g.maxCRs < 1 {
		g.maxCRs = defaultMaxCRs
	}
	for _, dotted := range t.Generate.Distinct {
		path := strings.Split(dotted, ".")
		if _, found, _ := unstructured.NestedString(t.Sample.Object, path...); !found {
			return nil, fmt.Errorf("generate.distinct %s: the sample holds no string there, and each CR after the first appends to it", dotted)
		}
		g.distinct = append(g.distinct, path)
	}
	for n := 1; n < g.maxCRs; n++ {
		if err := rules.refusal(g.base(n).Object, nil); err != nil {
			return nil, fmt.Errorf("the CRD refuses the CR %s: %w", g.name(n), err)
		}
	}
	for _, gvk := range t.Manages {
		g.managed = append(g.managed, observe.KindName(gvk))
	}
	if !t.Generate.NoFaults {
		g.faultables, err = buildFaultables(t)
		if err != nil {
			return nil, err
		}
	}
	for _, fixture := range t.Generate.Fixtures {
		if len(fixture.Mutate) > 0 {
			g.updatable = append(g.updatable, fixture)
		}
	}
	g.sequences = rapid.Custom(g.sequence)
	return g, nil
}

// drawable keeps the fields the CRD accepts a drawn value of in the sample. A
// refused field that generate.mutate names is a configuration error, and any
// other is left alone.
func drawable(t *target.Target, rules *crdRules, fields []field, leftAlone []string) ([]field, []string, error) {
	var kept []field
	for _, mutable := range fields {
		refused := rules.acceptsADraw(t.Sample, mutable)
		switch {
		case refused == nil:
			kept = append(kept, mutable)
		case len(t.Generate.Mutate) > 0:
			return nil, nil, fmt.Errorf("generate.mutate %s: %w", mutable.dotted, refused)
		default:
			leftAlone = append(leftAlone, leftAloneNote(mutable.dotted, refused))
		}
	}
	slices.Sort(leftAlone)
	return kept, leftAlone, nil
}

// LeftAlone says which spec paths generation never changes, and why. Only a
// target without generate.mutate has any.
func (g *Generator) LeftAlone() []string { return g.leftAlone }

// sequence draws one sequence, which starts by creating the primary CR. It
// leaves Seed zero; Draw, the only way out of this package, records the seed
// it was asked for.
func (g *Generator) sequence(t *rapid.T) run.Sequence {
	var at state
	create := run.Op{Index: 0, Type: run.OpCreate, Obj: g.cr(t, 0, &at), NoSettle: rapid.Bool().Draw(t, "noSettle")}
	ops := []run.Op{create}
	at.advance(create, 0)
	for range g.opsAfterCreate(t) {
		ops = append(ops, g.op(t, len(ops), &at))
	}
	judged := checkpointed(ops)
	judged = g.faulted(t, judged)
	return run.Sequence{Target: g.target.Name, Ops: judged}
}

// opsAfterCreate draws how many ops follow the create: at least one, unless
// MaxOps is 1.
func (g *Generator) opsAfterCreate(t *rapid.T) int {
	if g.maxOps == 1 {
		return 0
	}
	return 1 + uniform(t, g.maxOps-1, "ops")
}

// uniform draws a number below n, each with equal chance. rapid's own integers
// favor small numbers, which suits shrinking by rapid, but run.Shrink
// shrinks a sequence without it.
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

// checkpointed inserts the settle waits that leave the drawn ops judged. A
// restart is wrapped in them: G5 compares the converged state either side of
// a restart, less what another op in between may have changed. A
// deleteManaged follows one too: G7 judges it only once the run has converged,
// and the managed objects are then there to delete. The last op
// takes one because nothing else judges the state the run ends in. A noSettle
// elsewhere is left alone. A deleted fixture comes back before the next op
// that settles, since the target may rightly not be ready without it.
func checkpointed(ops []run.Op) []run.Op {
	judged := make([]run.Op, 0, 3*len(ops))
	settled := false
	emit := func(op run.Op) {
		op.Index = len(judged)
		judged = append(judged, op)
		settled = op.Settles()
	}
	for i, op := range ops {
		if (op.Type == run.OpRestart || op.Type == run.OpDeleteManaged) && !settled {
			emit(run.Op{Type: run.OpSettle})
		}
		emit(op)
		var next run.Op
		last := i == len(ops)-1
		if !last {
			next = ops[i+1]
		}
		switch {
		case op.Type == run.OpRestart && next.Type != run.OpSettle:
			emit(run.Op{Type: run.OpSettle})
		case last && !settled:
			emit(run.Op{Type: run.OpSettle})
		}
	}
	for i := range judged {
		if judged[i].Type == run.OpDeleteFixture {
			judged[i].Until = &run.Until{Op: i + 1 + slices.IndexFunc(judged[i+1:], run.Op.Settles)}
		}
	}
	return judged
}

// Draw returns the sequence the seed produces, recording the seed as the
// sequence's own. One seed always draws the same sequence, so a replay needs
// no rapid.
func (g *Generator) Draw(seed int64) (sequence run.Sequence, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("drawing a sequence for the target %s at seed %d: %v", g.target.Name, seed, recovered)
		}
	}()
	sequence = g.sequences.Example(int(seed))
	sequence.Seed = seed
	return sequence, nil
}

// cr is the n-th CR's base with a subset of the mutable fields changed. A
// change its CRD refuses, or that gives it another CR's distinct value, is
// undone, and the next is judged without it.
func (g *Generator) cr(t *rapid.T, n int, at *state) *unstructured.Unstructured {
	cr := g.base(n)
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
		if g.rules.refusal(changed.Object, nil) == nil && !g.collides(at, n, changed.Object) {
			cr = changed
		}
	}
	return cr
}

// base is the sample as the n-th CR, counting from 0. Each CR after the first
// appends -<n+1> to the sample's name and to its value at each distinct path.
func (g *Generator) base(n int) *unstructured.Unstructured {
	cr := g.target.Sample.DeepCopy()
	if n == 0 {
		return cr
	}
	suffix := fmt.Sprintf("-%d", n+1)
	cr.SetName(g.name(n))
	for _, path := range g.distinct {
		value, _, _ := unstructured.NestedString(cr.Object, path...)
		_ = unstructured.SetNestedField(cr.Object, value+suffix, path...)
	}
	return cr
}

// name is the n-th CR's name.
func (g *Generator) name(n int) string {
	if n == 0 {
		return g.target.Sample.GetName()
	}
	return fmt.Sprintf("%s-%d", g.target.Sample.GetName(), n+1)
}

// updateDraws bounds how often an update is drawn again where the CRD refuses
// it.
const updateDraws = 8

// patch is a merge patch that changes one field of the n-th CR, or nil if the
// CRD refused every one drawn or each gave the CR another's distinct value.
func (g *Generator) patch(t *rapid.T, n int, at *state) map[string]any {
	for range updateDraws {
		mutable := sampled(t, g.fields, "field")
		value, _ := mutable.draw(t)
		// A merge patch removes the field where the draw left it absent
		// (RFC 7386).
		if patch := nest(mutable.path, value); g.admits(at, n, patch) {
			return patch
		}
	}
	return nil
}

// admits reports whether the CRD accepts the patch to the n-th CR, and the
// patch gives it no other CR's distinct value.
func (g *Generator) admits(at *state, n int, patch map[string]any) bool {
	cr := at.crs[n].object
	patched := run.MergePatch(runtime.DeepCopyJSON(cr), patch)
	return g.rules.refusal(patched, cr) == nil && !g.collides(at, n, patched)
}

// draw is one value for the field, or its removal where the schema allows the
// field to be absent.
func (f field) draw(t *rapid.T) (any, bool) {
	if f.optional && rapid.Bool().Draw(t, "remove "+f.dotted) {
		return nil, false
	}
	return f.values.Draw(t, f.dotted), true
}

// state is what the sequence so far leaves the run in, which says what op may
// come next.
type state struct {
	// crs are the CRs the sequence created, in order.
	crs []drawnCR
	// gone are the fixtures deleted since the last op that settles.
	gone []string
}

// drawnCR is a CR as reconciler-fuzzer last wrote it, which it keeps once
// deleted.
type drawnCR struct {
	object map[string]any
	live   bool
}

// live are the positions of the CRs not deleted.
func (at *state) live() []int {
	var live []int
	for n, cr := range at.crs {
		if cr.live {
			live = append(live, n)
		}
	}
	return live
}

// collides reports whether the n-th CR, written as cr, would hold at a
// distinct path another CR's value, or the value another CR's base holds,
// which that CR falls back on.
func (g *Generator) collides(at *state, n int, cr map[string]any) bool {
	var others []map[string]any
	for other := range g.maxCRs {
		if other == n {
			continue
		}
		others = append(others, g.base(other).Object)
		if other < len(at.crs) {
			others = append(others, at.crs[other].object)
		}
	}
	for _, path := range g.distinct {
		value, _, _ := unstructured.NestedFieldNoCopy(cr, path...)
		for _, theirs := range others {
			held, _, _ := unstructured.NestedFieldNoCopy(theirs, path...)
			if reflect.DeepEqual(value, held) {
				return true
			}
		}
	}
	return false
}

// op draws the next op, and the CR it acts on.
func (g *Generator) op(t *rapid.T, index int, at *state) run.Op {
	op := run.Op{Index: index, Type: sampled(t, g.legal(at), "op")}
	n := 0
	switch op.Type {
	case run.OpCreate:
		n = len(at.crs)
		op.Obj = g.cr(t, n, at)
	case run.OpUpdate:
		n = sampled(t, at.live(), "cr")
		if op.Patch = g.patch(t, n, at); op.Patch == nil {
			op.Type = run.OpSettle
		}
	case run.OpDelete:
		n = sampled(t, at.live(), "cr")
	case run.OpRecreate:
		n = uniform(t, len(at.crs), "cr")
		op.Obj = g.cr(t, n, at)
	case run.OpDeleteManaged:
		op.Kind = sampled(t, g.managed, "kind")
		// The first managed object of its kind: generation cannot know how many
		// the run will hold, and a later index would often resolve to nothing
		// and be skipped.
		op.Nth = new(int)
	case run.OpUpdateFixture:
		fixture := sampled(t, g.updatable, "fixture")
		op.Kind, op.Name = observe.KindName(fixture.GVK), fixture.Name
		op.Patch = nest(sampled(t, fixture.Mutate, "path").Keys(), fixtureWords.Draw(t, "value"))
	case run.OpDeleteFixture:
		fixture := sampled(t, g.present(at), "fixture")
		op.Kind, op.Name = observe.KindName(fixture.GVK), fixture.Name
	}
	if op.Type.OnCR() {
		if op.Type != run.OpCreate && n > 0 {
			op.CR = g.name(n)
		}
		op.NoSettle = rapid.Bool().Draw(t, "noSettle")
	}
	at.advance(op, n)
	return op
}

// legal are the ops the state allows.
func (g *Generator) legal(at *state) []run.OpType {
	legal := []run.OpType{run.OpSettle, run.OpRestart}
	live := len(at.live()) > 0
	if live && len(g.fields) > 0 {
		legal = append(legal, run.OpUpdate)
	}
	if len(g.updatable) > 0 {
		legal = append(legal, run.OpUpdateFixture)
	}
	if live && len(g.managed) > 0 {
		legal = append(legal, run.OpDeleteManaged)
	}
	if len(g.present(at)) > 0 {
		legal = append(legal, run.OpDeleteFixture)
	}
	legal = append(legal, run.OpRecreate)
	if live {
		legal = append(legal, run.OpDelete)
	}
	if len(at.crs) < g.maxCRs {
		legal = append(legal, run.OpCreate)
	}
	return legal
}

// present are the fixtures generation may delete and has not.
func (g *Generator) present(at *state) []target.MutableFixture {
	return slices.DeleteFunc(slices.Clone(g.target.Generate.Fixtures), func(fixture target.MutableFixture) bool {
		return slices.Contains(at.gone, observe.KindName(fixture.GVK)+" "+fixture.Name)
	})
}

// fixtureWords are short words whose length is a multiple of four, so that a
// Secret's data reads each as base64.
var fixtureWords = rapid.Custom(func(t *rapid.T) any {
	length := 4 * rapid.IntRange(1, 3).Draw(t, "quads")
	return rapid.StringOfN(wordRunes, length, length, -1).Draw(t, "word")
})

// advance follows the op, which acts on the n-th CR if it is a CR op.
func (at *state) advance(op run.Op, n int) {
	switch op.Type {
	case run.OpCreate:
		at.crs = append(at.crs, drawnCR{object: op.Obj.Object, live: true})
	case run.OpDelete:
		at.crs[n].live = false
	case run.OpRecreate:
		at.crs[n] = drawnCR{object: op.Obj.Object, live: true}
	case run.OpUpdate:
		at.crs[n].object = run.MergePatch(runtime.DeepCopyJSON(at.crs[n].object), op.Patch)
	case run.OpDeleteFixture:
		at.gone = append(at.gone, op.Kind+" "+op.Name)
	}
	if op.Settles() {
		at.gone = nil
	}
}

// nest wraps a value in the objects its path names, which is the merge patch
// that changes that one field.
func nest(path []string, value any) map[string]any {
	nested := value
	for i := len(path) - 1; i >= 0; i-- {
		nested = map[string]any{path[i]: nested}
	}
	return nested.(map[string]any)
}
