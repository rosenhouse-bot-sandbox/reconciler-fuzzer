package check

import (
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Crash fails every controller exit the harness did not cause, except one
// within leaseGrace of a fault on a Lease request: leader election exits by
// design. With AllowExitBeforeWatch, it also skips an exit of a process that
// opened no watch of a listed kind when a later process opened one: the
// controller failed at its start and then recovered. It gives one finding per
// step.
func Crash(ix *trace.Index, s Spec) []Finding {
	var order []int
	exits := map[int][]trace.Event{}
	steps := ix.Steps()
	var started time.Duration
	for _, e := range ix.Procs() {
		if e.Proc.Event == trace.Start {
			started = e.At
		}
		if e.Proc.Event != trace.Exit || e.Proc.HarnessCaused || leaseFaulted(ix, e.At) ||
			s.AllowExitBeforeWatch && !s.watchedListed(ix, started, e.At) && s.watchedListed(ix, e.At, math.MaxInt64) {
			continue
		}
		step := stepAt(steps, e.At)
		if exits[step] == nil {
			order = append(order, step)
		}
		exits[step] = append(exits[step], e)
	}
	var findings []Finding
	for _, step := range order {
		first := exits[step][0]
		summary := "the controller exited " + times(len(exits[step]))
		details := []string{"first exit: " + first.Proc.Exit}
		if first.Proc.StderrTail != "" {
			details = append(details, "output tail:", first.Proc.StderrTail)
		}
		findings = append(findings, Finding{Check: "Crash", Step: step, At: first.At, Summary: summary, Details: details})
	}
	return findings
}

// watchedListed reports whether the controller opened a watch of a listed
// kind between from and to.
func (s Spec) watchedListed(ix *trace.Index, from, to time.Duration) bool {
	return slices.ContainsFunc(ix.Calls(trace.Controller, from, to), func(e trace.Event) bool {
		c := e.Call
		return c.Watch && c.Status >= 200 && c.Status <= 299 && s.listed(c.Ref)
	})
}

const leaseGrace = time.Second

func leaseFaulted(ix *trace.Index, exit time.Duration) bool {
	for _, c := range ix.Calls(trace.Controller, exit-leaseGrace-1, exit) {
		if c.Call.Injected != "" && c.Call.Ref.Group == "coordination.k8s.io" && c.Call.Ref.Resource == "leases" {
			return true
		}
	}
	return false
}

// stepAt is the step whose span holds t: from its start to the next step's
// start. It is -1 before the first step.
func stepAt(steps []trace.StepSpan, t time.Duration) int {
	step := -1
	for _, span := range steps {
		if span.Reached[trace.FaultPeriod] && span.Start <= t {
			step = span.Step
		}
	}
	return step
}
