package check

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
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
		end, ends := recordedEnd(ix, steps, i)
		settled, ok := FirstSettled(ix, step.EndFaults, min(to, end), s)
		confirm := settled + s.Stable
		switch {
		case !ok && end < to:
			notes = append(notes, fmt.Sprintf("%s is not judged: %s at %v, before its deadline %v.", stepName(ix, step.Step), ends, end, to))
			continue
		case !ok:
			f.At = to
			f.describe("did not settle", unmetClauses(ix, to, s))
		case confirm > end:
			notes = append(notes, fmt.Sprintf("%s is not judged: it settled at %v and had to stay settled until %v, but %s at %v.", stepName(ix, step.Step), settled, confirm, ends, end))
			continue
		default:
			unmets := unmetClauses(ix, confirm, s)
			if len(unmets) == 0 {
				continue
			}
			f.At = confirm
			f.describe("did not stay settled", unmets)
		}
		f.Details = append(f.Details, unmodelledKinds(ix, f.At, s)...)
		findings = append(findings, f)
	}
	return findings, append(notes, unwaitedDeletions(ix, s)...)
}

// unwaitedDeletions notes each CR still being deleted when the trace ends
// that the stop did not wait for. No controller is left to finalize it.
func unwaitedDeletions(ix *trace.Index, s Spec) []string {
	if _, stopped := ix.Stop(math.MaxInt64); !stopped {
		return nil
	}
	var notes []string
	for _, p := range s.Primaries {
		for _, cr := range ix.Live(p.Kind.Group, p.Kind.Resource, math.MaxInt64) {
			if deleting(cr.Body) && !ix.StopWaitedFor(cr.UID) {
				notes = append(notes, describe(cr.Ref)+" is not judged: the stop did not wait for it, and it is still being deleted when the trace ends.")
			}
		}
	}
	return notes
}

// builtinStatusKinds are built-in kinds whose status another controller
// writes, each with the status keys of the models entry that report.md
// suggests. A stuck Pod stays Running, as one that fails its readiness probe
// does. A stuck PVC is Pending. A stuck Job shows one active Pod and no
// succeeded one. The API server refuses to lower a done Job's succeeded, so
// that Job stays done. A model cannot track a Deployment's or StatefulSet's
// observedGeneration, so those kinds are left out.
var builtinStatusKinds = map[trace.Kind]string{
	{Group: "batch", Version: "v1", Kind: "Job", Resource: "jobs"}:                     "status: {succeeded: 1}, stuckStatus: {active: 1}",
	{Version: "v1", Kind: "Pod", Resource: "pods"}:                                     "status: {phase: Running}",
	{Version: "v1", Kind: "PersistentVolumeClaim", Resource: "persistentvolumeclaims"}: "status: {phase: Bound}, stuckStatus: {phase: Pending}",
}

// unmodelledKinds names each status kind that the controller read or
// watched by t, that nothing models, whose status the controller never
// wrote, and that has a live instance with no status at t.
func unmodelledKinds(ix *trace.Index, t time.Duration, s Spec) []string {
	kinds := slices.Clone(s.StatusKinds)
	for k := range builtinStatusKinds {
		kinds = append(kinds, k)
	}
	read, written := map[trace.Kind]bool{}, map[trace.Kind]bool{}
	for _, e := range ix.Calls(trace.Controller, -1, t) {
		c := e.Call
		i := slices.IndexFunc(kinds, func(k trace.Kind) bool {
			return c.Ref.System == "kube" && k.Group == c.Ref.Group && k.Resource == c.Ref.Resource
		})
		if i < 0 {
			continue
		}
		k := kinds[i]
		switch {
		case trace.Mutating(c.Verb) && c.Subresource == "status":
			written[k] = true
		case c.Verb == "get" || c.Verb == "list" || c.Verb == "watch":
			read[k] = true
		}
	}
	var lines []string
	for k := range read {
		ref := trace.Ref{System: "kube", Group: k.Group, Resource: k.Resource}
		if written[k] || s.modelled(ref) || s.listed(ref) || !slices.ContainsFunc(ix.Live(k.Group, k.Resource, t), noStatus) {
			continue
		}
		line := fmt.Sprintf("The controller reads %s, whose status nothing models. If another controller sets it, add to models: - %s", kindName(k), suggestedModel(k))
		if _, builtin := builtinStatusKinds[k]; builtin {
			line += ", and set its status fields to the ones the controller reads"
		}
		lines = append(lines, line)
	}
	slices.Sort(lines)
	return lines
}

// suggestedModel returns the models entry that report.md suggests for k.
func suggestedModel(k trace.Kind) string {
	entry := "{kind: " + kindName(k) + ", stages: readyCondition, after: 200ms"
	if keys, builtin := builtinStatusKinds[k]; builtin {
		entry += ", " + keys
	}
	return entry + "}"
}

// kindName names k as a config does, such as batch/v1/Job.
func kindName(k trace.Kind) string {
	if k.Group == "" {
		return k.Version + "/" + k.Kind
	}
	return k.Group + "/" + k.Version + "/" + k.Kind
}

func noStatus(v trace.Version) bool {
	status, _ := decode(v.Body)["status"].(map[string]any)
	return len(status) == 0
}

// recordedEnd is the last time the trace records for steps[i], and what
// happens then: its confirm window ends, the next step starts, or the trace
// ends.
func recordedEnd(ix *trace.Index, steps []trace.StepSpan, i int) (time.Duration, string) {
	if steps[i].Reached[trace.Confirmed] {
		return steps[i].ConfirmEnd, "its confirm window ends"
	}
	if i+1 < len(steps) && steps[i+1].Reached[trace.FaultPeriod] {
		return steps[i+1].Start, "the next step starts"
	}
	events := ix.Events()
	return events[len(events)-1].At, "the trace ends"
}

// describe names the first unmet clause, and its object's kind, so that
// findings of different clauses have different signatures.
func (f *Finding) describe(summary string, unmets []unmet) {
	f.Summary = fmt.Sprintf("%s (clause %d)", summary, unmets[0].clause)
	f.Kind = unmets[0].kind
	for _, u := range unmets {
		f.Details = append(f.Details, u.text)
	}
}
