package check

import (
	"fmt"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Crash fails every controller exit the harness did not cause. It gives one
// finding per step.
func Crash(ix *trace.Index) []Finding {
	var order []int
	exits := map[int][]trace.Event{}
	steps := ix.Steps()
	for _, e := range ix.Procs() {
		if e.Proc.Event != trace.Exit || e.Proc.HarnessCaused {
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
		summary := "the controller exited 1 time"
		if n := len(exits[step]); n > 1 {
			summary = fmt.Sprintf("the controller exited %d times", n)
		}
		details := []string{"first exit: " + first.Proc.Exit}
		if first.Proc.StderrTail != "" {
			details = append(details, "output tail:", first.Proc.StderrTail)
		}
		findings = append(findings, Finding{Check: "Crash", Step: step, At: first.At, Summary: summary, Details: details})
	}
	return findings
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
