package generate

import (
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// Baseline creates the sample, deletes the first object of each managed kind,
// and then changes each mutable field once. Drawn sequences may miss a kind or
// a field.
func (g *Generator) Baseline() (sequence run.Sequence, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("building the baseline for the target %s: %v", g.target.Name, recovered)
		}
	}()
	return rapid.Custom(g.baseline).Example(0), nil
}

func (g *Generator) baseline(t *rapid.T) run.Sequence {
	sample := g.base(0)
	at := state{crs: []drawnCR{{object: sample.Object, live: true}}}
	ops := []run.Op{{Type: run.OpCreate, Obj: sample}}
	for _, kind := range g.managed {
		ops = append(ops, run.Op{Type: run.OpDeleteManaged, Kind: kind, Nth: new(int)})
	}
	for _, mutable := range g.fields {
		if patch := g.change(t, mutable, &at); patch != nil {
			update := run.Op{Type: run.OpUpdate, Patch: patch}
			at.advance(update, 0)
			ops = append(ops, update)
		}
	}
	for i := range ops {
		ops[i].Index = i
	}
	return run.Sequence{Target: g.target.Name, Ops: ops}
}

// change is a merge patch that sets the field of the first CR to a value it
// does not hold, or nil if no draw both changes it and is admitted.
func (g *Generator) change(t *rapid.T, mutable field, at *state) map[string]any {
	held, _, _ := unstructured.NestedFieldNoCopy(at.crs[0].object, mutable.path...)
	for range updateDraws {
		value := mutable.values.Draw(t, mutable.dotted)
		if reflect.DeepEqual(value, held) {
			continue
		}
		if patch := nest(mutable.path, value); g.admits(at, 0, patch) {
			return patch
		}
	}
	return nil
}
