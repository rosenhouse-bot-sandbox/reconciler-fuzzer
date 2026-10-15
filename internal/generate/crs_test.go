package generate

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// written is a CR as reconciler-fuzzer last wrote it.
type written struct {
	object map[string]any
	live   bool
}

// follow replays a sequence's CR ops by the name of the CR each acts on, and
// calls visit after each op with every CR the sequence has written so far.
func follow(sequence run.Sequence, sample string, visit func(op run.Op, name string, crs map[string]*written)) {
	crs := map[string]*written{}
	for _, op := range sequence.Ops {
		name := orSample(op.CR, sample)
		switch op.Type {
		case run.OpCreate:
			name = op.Obj.GetName()
			crs[name] = &written{object: op.Obj.Object, live: true}
		case run.OpRecreate:
			crs[name] = &written{object: op.Obj.Object, live: true}
		case run.OpUpdate:
			crs[name].object = merge(crs[name].object, op.Patch)
		case run.OpDelete:
			crs[name].live = false
		}
		visit(op, name, crs)
	}
}

func TestSequencesCreateUpToThreeCRsNamedAfterTheSample(t *testing.T) {
	loaded := loadTarget(t, toyTarget)
	g := newGenerator(t, loaded, Options{})
	most := 0
	for seed := int64(1); seed <= 100; seed++ {
		var created []string
		for _, op := range sequenceAt(t, g, seed).Ops {
			if op.Type == run.OpCreate {
				created = append(created, op.Obj.GetName())
			}
		}
		want := []string{"widget", "widget-2", "widget-3"}
		if len(created) > len(want) || !slices.Equal(created, want[:len(created)]) {
			t.Fatalf("Seed %d creates the CRs %v, want the first of %v.", seed, created, want)
		}
		most = max(most, len(created))
	}
	if most != 3 {
		t.Errorf("No seed from 1 to 100 creates more than %d CRs, want some to create 3.", most)
	}
}

func TestATargetBoundsTheCRsASequenceCreates(t *testing.T) {
	for _, maxCRs := range []int{1, 2} {
		t.Run(fmt.Sprint(maxCRs), func(t *testing.T) {
			loaded := loadTarget(t, toyTarget)
			loaded.Generate.MaxCRs = maxCRs
			g := newGenerator(t, loaded, Options{})
			most := 0
			for seed := int64(1); seed <= 100; seed++ {
				creates := 0
				for _, op := range sequenceAt(t, g, seed).Ops {
					if op.Type == run.OpCreate {
						creates++
					}
				}
				most = max(most, creates)
			}
			if most != maxCRs {
				t.Errorf("Seeds 1 to 100 create at most %d CRs, want %d.", most, maxCRs)
			}
		})
	}
}

func TestEveryOpActsOnACRTheSequenceCreated(t *testing.T) {
	for _, testCase := range targets {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			rapid.Check(t, func(rt *rapid.T) {
				crs := map[string]*written{}
				for _, op := range g.sequence(rt).Ops {
					name := orSample(op.CR, loaded.Sample.GetName())
					cr, created := crs[name]
					switch {
					case op.Type == run.OpCreate && crs[op.Obj.GetName()] != nil:
						rt.Fatalf("Op %d creates the CR %s a second time.", op.Index, op.Obj.GetName())
					case op.Type == run.OpCreate:
						crs[op.Obj.GetName()] = &written{live: true}
					case op.Type == run.OpRecreate && (!created || op.Obj.GetName() != name):
						rt.Fatalf("Op %d recreates the CR %s as %s, and the sequence created %v.", op.Index, name, op.Obj.GetName(), crs)
					case op.Type == run.OpRecreate:
						cr.live = true
					case (op.Type == run.OpUpdate || op.Type == run.OpDelete) && (!created || !cr.live):
						rt.Fatalf("Op %d is a %s of %s, which is not live.", op.Index, op.Type, name)
					case op.Type == run.OpDelete:
						cr.live = false
					case op.Type == run.OpDeleteManaged && !slices.ContainsFunc(values(crs), func(cr *written) bool { return cr.live }):
						rt.Fatalf("Op %d deletes a managed object while no CR is live.", op.Index)
					}
				}
			})
		})
	}
}

func orSample(name, sample string) string {
	if name == "" {
		return sample
	}
	return name
}

func values(crs map[string]*written) []*written {
	var all []*written
	for _, cr := range crs {
		all = append(all, cr)
	}
	return all
}

// No two CRs of a sequence hold one value at a distinct path, whichever CR
// reconciler-fuzzer wrote last and whether or not it is live.
func TestNoTwoCRsShareADistinctValue(t *testing.T) {
	for _, testCase := range []struct {
		path     string
		distinct []string
	}{
		{certManagerTarget, []string{"spec", "secretName"}},
		{externalSecretsTarget, []string{"spec", "target", "name"}},
	} {
		t.Run(testCase.path, func(t *testing.T) {
			loaded := loadTarget(t, testCase.path)
			g := newGenerator(t, loaded, Options{})
			several := 0
			// Without the check, seed 713 gives two ExternalSecrets one target.
			for seed := int64(1); seed <= 3000; seed++ {
				follow(sequenceAt(t, g, seed), loaded.Sample.GetName(), func(op run.Op, _ string, crs map[string]*written) {
					held := map[string]string{}
					for name, cr := range crs {
						value, found, _ := unstructured.NestedString(cr.object, testCase.distinct...)
						if other, taken := held[value]; found && taken {
							t.Fatalf("After op %d of seed %d, the CRs %s and %s both hold %q at %s.",
								op.Index, seed, other, name, value, strings.Join(testCase.distinct, "."))
						}
						held[value] = name
					}
					if len(crs) > 1 {
						several++
					}
				})
			}
			if several == 0 {
				t.Error("No sequence created a second CR.")
			}
		})
	}
}

// Each CR after the first takes the sample's value with its own suffix.
func TestACRAfterTheFirstTakesTheSamplesDistinctValueWithASuffix(t *testing.T) {
	g := newGenerator(t, loadTarget(t, certManagerTarget), Options{})
	rapid.Check(t, func(rt *rapid.T) {
		for _, op := range g.sequence(rt).Ops {
			if op.Obj == nil {
				continue
			}
			name := op.Obj.GetName()
			secret, _, _ := unstructured.NestedString(op.Obj.Object, "spec", "secretName")
			if want := "example-tls" + strings.TrimPrefix(name, "example"); secret != want {
				rt.Fatalf("Op %d writes the Certificate %s with the secretName %s, want %s.", op.Index, name, secret, want)
			}
		}
	})
}

// fixedField is a field whose every draw is the value given.
func fixedField(dotted string, value any) field {
	return field{path: strings.Split(dotted, "."), dotted: dotted, values: rapid.Just(value).AsAny()}
}

// twoGadgets is the state of a sequence that created two gadgets, the first
// holding the sample's spec.left and the second its own.
func twoGadgets(g *Generator) *state {
	return &state{crs: []drawnCR{{object: g.base(0).Object, live: true}, {object: g.base(1).Object, live: true}}}
}

func TestADrawThatRepeatsAnotherCRsDistinctValueIsUndone(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Distinct = []string{"spec.left"}
	g := newGenerator(t, loaded, Options{})
	g.fields = []field{fixedField("spec.left", "one")}
	at := twoGadgets(g)
	rapid.Check(t, func(rt *rapid.T) {
		cr := g.cr(rt, 1, at)
		if left, _, _ := unstructured.NestedString(cr.Object, "spec", "left"); left != "one-2" {
			rt.Fatalf("The second gadget holds the spec.left %q, want its own one-2.", left)
		}
	})
}

func TestAnUpdateThatRepeatsAnotherCRsDistinctValueIsDrawnAgain(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Distinct = []string{"spec.left"}
	g := newGenerator(t, loaded, Options{})
	g.fields = []field{fixedField("spec.left", "one")}
	at := twoGadgets(g)

	patch := rapid.Custom(func(t *rapid.T) map[string]any { return g.patch(t, 1, at) }).Example(0)

	if patch != nil {
		t.Errorf("An update of the second gadget patched %v, which gives it the first's spec.left.", patch)
	}
}

// Each CR's own value is the one it falls back on, so no other CR takes it.
func TestADrawNeverTakesTheValueAnotherCRFallsBackOn(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Distinct = []string{"spec.left"}
	g := newGenerator(t, loaded, Options{})
	g.fields = []field{fixedField("spec.left", "one-3")}
	var alone state
	rapid.Check(t, func(rt *rapid.T) {
		cr := g.cr(rt, 0, &alone)
		if left, _, _ := unstructured.NestedString(cr.Object, "spec", "left"); left != "one" {
			rt.Fatalf("The first gadget holds the spec.left %q, which the third falls back on.", left)
		}
	})
	at := &state{crs: []drawnCR{{object: g.base(0).Object, live: true}}}
	if patch := rapid.Custom(func(t *rapid.T) map[string]any { return g.patch(t, 0, at) }).Example(0); patch != nil {
		t.Errorf("An update of the first gadget patched %v, which the third falls back on.", patch)
	}
}

func TestADistinctPathMustHoldAStringInTheSample(t *testing.T) {
	for _, dotted := range []string{"spec.count", "spec.nothing"} {
		t.Run(dotted, func(t *testing.T) {
			loaded := loadTarget(t, toyTarget)
			loaded.Generate.Distinct = []string{dotted}

			_, err := New(loaded, Options{})

			if err == nil || !strings.Contains(err.Error(), "generate.distinct "+dotted+": the sample holds no string there") {
				t.Errorf("New returned %v, want an error naming generate.distinct %s.", err, dotted)
			}
		})
	}
}

func TestTheCRDMustAcceptEveryCRASequenceMayCreate(t *testing.T) {
	loaded := loadTarget(t, rulesTarget)
	loaded.Generate.Distinct = []string{"spec.mode"}

	_, err := New(loaded, Options{})

	if err == nil || !strings.Contains(err.Error(), "the CRD refuses the CR gadget-2") || !strings.Contains(err.Error(), "fast-2") {
		t.Errorf("New returned %v, want the CRD's refusal of gadget-2 and its mode fast-2.", err)
	}
}

// sequenceAt is the sequence the seed draws.
func sequenceAt(t *testing.T, g *Generator, seed int64) run.Sequence {
	t.Helper()
	sequence, err := g.Draw(seed)
	if err != nil {
		t.Fatalf("Draw(%d) failed: %v.", seed, err)
	}
	return sequence
}
