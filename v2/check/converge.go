package check

import (
	"fmt"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Converge fails a step whose settle condition does not hold by its
// deadline, or no longer holds at the end of the confirm window. A step the
// trace ends too early to decide gets a note instead.
func Converge(ix *trace.Index, s Spec) (findings []Finding, notes []string) {
	steps := ix.Steps()
	for i, step := range steps {
		if !step.Reached[trace.EndFaults] {
			continue
		}
		f := Finding{Check: "Converge", Step: step.Step}
		if step.BackoffBound {
			f.Labels = []string{"backoff-bound"}
		}
		to := step.Deadline
		if step.Reached[trace.Confirmed] && step.ConfirmEnd < to {
			to = step.ConfirmEnd
		}
		end := recordedEnd(ix, steps, i)
		settled, ok := FirstSettled(ix, step.EndFaults, min(to, end), s)
		confirm := settled + s.Stable
		switch {
		case !ok && end < to:
			notes = append(notes, fmt.Sprintf("%s is not judged: the trace ends at %v, before its deadline %v.", stepName(ix, step.Step), end, to))
			continue
		case !ok:
			f.At, f.Summary, f.Details = to, "did not settle", Settled(ix, to, s)
		case confirm > end:
			notes = append(notes, fmt.Sprintf("%s is not judged: it settled at %v, but the trace ends at %v, before its confirm window ends.", stepName(ix, step.Step), settled, end))
			continue
		default:
			unmet := Settled(ix, confirm, s)
			if len(unmet) == 0 {
				continue
			}
			f.At, f.Summary, f.Details = confirm, "did not stay settled", unmet
		}
		findings = append(findings, f)
	}
	return findings, notes
}

// recordedEnd is the last time the trace records for steps[i]: its confirm,
// the next step's start, or the trace's last event.
func recordedEnd(ix *trace.Index, steps []trace.StepSpan, i int) time.Duration {
	if steps[i].Reached[trace.Confirmed] {
		return steps[i].ConfirmEnd
	}
	if i+1 < len(steps) && steps[i+1].Reached[trace.FaultPeriod] {
		return steps[i+1].Start
	}
	events := ix.Events()
	return events[len(events)-1].At
}
