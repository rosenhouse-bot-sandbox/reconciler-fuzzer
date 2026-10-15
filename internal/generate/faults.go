package generate

import (
	"math"
	"slices"
	"time"

	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// faultVerbs are the verbs a generated fault may match. An unset verb matches
// all. list and watch are excluded: outside a restart an informer rarely lists
// or starts a watch, so such a fault would fault nothing, and a relist's
// jittered backoff can outrun the owed recovery time.
var faultVerbs = []string{"", "get", "create", "update", "patch", "delete"}

// faultErrors are the HTTP status codes a generated fault may return.
var faultErrors = []int{500, 503, 429}

// faultDelayFractions scale timeouts.stable to produce a generated fault's
// delay, so that the delay is bounded by the settle wait.
var faultDelayFractions = []float64{0.25, 0.5, 1.0}

// buildFaultables returns the resource plurals the generator may fault: the
// primary's, then each managed kind's. A kind whose plural is unknown is
// omitted silently; the runner would refuse it at discovery.
func buildFaultables(t *target.Target) ([]string, error) {
	var plurals []string
	seen := map[string]bool{}
	add := func(plural string) {
		if plural != "" && !seen[plural] {
			seen[plural] = true
			plurals = append(plurals, plural)
		}
	}

	primary, err := crdPlural(t, t.Primary)
	if err != nil {
		return nil, err
	}
	add(primary)

	for _, gvk := range t.Manages {
		if plural, err := crdPlural(t, gvk); err != nil {
			return nil, err
		} else if plural != "" {
			add(plural)
			continue
		}
		add(target.BuiltinPlural(gvk.GroupKind()))
	}
	return plurals, nil
}

// faulted inserts at most one fault into the checkpointed ops. It returns the
// ops unchanged when no eligible start exists or the draw skips the fault.
func (g *Generator) faulted(t *rapid.T, ops []run.Op) []run.Op {
	if len(g.faultables) == 0 {
		return ops
	}
	starts := eligibleStarts(ops)
	if len(starts) == 0 {
		return ops
	}
	if !rapid.Bool().Draw(t, "fault") {
		return ops
	}
	s := rapid.SampledFrom(starts).Draw(t, "faultStart")

	fault := g.drawFault(t)

	// Find the end of the span: the first op at or after s that settles.
	end := s
	for end < len(ops) && !ops[end].Settles() {
		end++
	}
	if end >= len(ops) {
		return ops
	}

	// Insert: [ops before s] [fault op] [span s..end] [inserted settle] [rest].
	out := make([]run.Op, 0, len(ops)+2)
	out = append(out, ops[:s]...)

	faultOp := run.Op{Type: run.OpFault, Fault: &fault}
	out = append(out, faultOp)
	out = append(out, ops[s:end+1]...)
	inserted := run.Op{Type: run.OpSettle}
	out = append(out, inserted)
	if end+1 < len(ops) {
		out = append(out, ops[end+1:]...)
	}

	// Reindex and set the fault's until.op to the inserted settle.
	for i := range out {
		out[i].Index = i
	}
	settleIndex := s + 1 + (end - s + 1)
	out[s].Fault.Until = run.Trigger{Op: &settleIndex}

	// Set deleteFixture untils pointing to the next op that settles.
	for i := range out {
		if out[i].Type == run.OpDeleteFixture {
			rest := out[i+1:]
			if j := findSettles(rest); j >= 0 {
				target := i + 1 + j
				out[i].Until = &run.Until{Op: target}
			}
		}
	}

	return out
}

// unfaultedStarts are the ops no fault starts at. G7 does not judge a
// deleteManaged under a fault.
var unfaultedStarts = []run.OpType{run.OpSettle, run.OpRestart, run.OpDeleteManaged}

// eligibleStarts are the indices where a fault may start: s >= 1 where
// ops[s-1] settles and ops[s] is not one of unfaultedStarts.
func eligibleStarts(ops []run.Op) []int {
	var starts []int
	for s := 1; s < len(ops); s++ {
		if ops[s-1].Settles() && !slices.Contains(unfaultedStarts, ops[s].Type) {
			starts = append(starts, s)
		}
	}
	return starts
}

// drawFault draws a fault specification.
func (g *Generator) drawFault(t *rapid.T) run.Fault {
	resource := rapid.SampledFrom(g.faultables).Draw(t, "faultResource")
	verb := rapid.SampledFrom(faultVerbs).Draw(t, "faultVerb")

	var fraction *float64
	if rapid.Bool().Draw(t, "faultFraction") {
		f := 0.5
		fraction = &f
	}

	action := g.drawAction(t)

	return run.Fault{
		Match:  run.Match{Verb: verb, Resource: resource, Fraction: fraction},
		Action: action,
	}
}

// drawAction draws one fault action: an error code or a delay.
func (g *Generator) drawAction(t *rapid.T) run.Action {
	stable := g.target.Timeouts.Stable
	if stable == 0 {
		stable = target.DefaultTimeouts.Stable
	}
	delays := make([]time.Duration, len(faultDelayFractions))
	for i, f := range faultDelayFractions {
		delays[i] = time.Duration(math.Round(float64(stable)*f/float64(time.Millisecond))) * time.Millisecond
	}

	choices := len(faultErrors) + len(delays)
	pick := rapid.IntRange(0, choices-1).Draw(t, "faultAction")
	if pick < len(faultErrors) {
		return run.Action{Error: faultErrors[pick]}
	}
	return run.Action{Delay: run.Duration(delays[pick-len(faultErrors)])}
}

func findSettles(ops []run.Op) int {
	for i, op := range ops {
		if op.Settles() {
			return i
		}
	}
	return -1
}
