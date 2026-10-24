package check

import (
	"fmt"
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
		end := recordedEnd(ix, steps, i)
		settled, ok := FirstSettled(ix, step.EndFaults, min(to, end), s)
		confirm := settled + s.Stable
		switch {
		case !ok && end < to:
			notes = append(notes, fmt.Sprintf("%s is not judged: the trace ends at %v, before its deadline %v.", stepName(ix, step.Step), end, to))
			continue
		case !ok:
			f.At = to
			f.describe("did not settle", unmetClauses(ix, to, s))
		case confirm > end:
			notes = append(notes, fmt.Sprintf("%s is not judged: it settled at %v, but the trace ends at %v, before its confirm window ends.", stepName(ix, step.Step), settled, end))
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
	return findings, notes
}

// builtinStatusKinds are built-in kinds whose status another controller
// writes, each with the literal fields that a model of it can write. A stuck
// Pod stays Running, as one that fails its readiness probe does. A stuck Job
// keeps succeeded, because the API server refuses to lower it. A model
// cannot track a Deployment's or StatefulSet's observedGeneration, so
// those kinds are left out.
var builtinStatusKinds = map[trace.Kind]string{
	{Group: "batch", Version: "v1", Kind: "Job", Resource: "jobs"}:                     "status: {succeeded: 1}",
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
		if written[k] || s.modelled(trace.Ref{System: "kube", Group: k.Group, Resource: k.Resource}) || k.Group == s.PrimaryGroup && k.Resource == s.PrimaryResource || !slices.ContainsFunc(ix.Live(k.Group, k.Resource, t), noStatus) {
			continue
		}
		gvk := k.Version + "/" + k.Kind
		if k.Group != "" {
			gvk = k.Group + "/" + gvk
		}
		entry := fmt.Sprintf("{kind: %s, stages: readyCondition, after: 200ms}", gvk)
		if fields, builtin := builtinStatusKinds[k]; builtin {
			entry = fmt.Sprintf("{kind: %s, stages: readyCondition, after: 200ms, %s}, with status holding the fields the controller reads", gvk, fields)
		}
		lines = append(lines, fmt.Sprintf("The controller reads %s, whose status nothing models. If another controller sets it, add to models: - %s", gvk, entry))
	}
	slices.Sort(lines)
	return lines
}

func noStatus(v trace.Version) bool {
	status, _ := decode(v.Body)["status"].(map[string]any)
	return len(status) == 0
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

// describe names the first unmet clause, and its object's kind, so that
// findings of different clauses have different signatures.
func (f *Finding) describe(summary string, unmets []unmet) {
	f.Summary = fmt.Sprintf("%s (clause %d)", summary, unmets[0].clause)
	f.Kind = unmets[0].kind
	for _, u := range unmets {
		f.Details = append(f.Details, u.text)
	}
}
