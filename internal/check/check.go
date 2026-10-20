// Package check judges a trace. Every check is a pure function of the trace
// and the spec.
package check

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Spec is what the checks need from the config.
type Spec struct {
	// Primary is the CR kind, as group and plural resource.
	PrimaryGroup    string
	PrimaryResource string
	Ready           func(obj map[string]any) (bool, error)
	Stable          time.Duration
	Budgets         config.Budgets
	// Retain says whether Clean allows a deleted CR's resources to stay.
	// It is nil when the config sets no primary.retain.
	Retain func(cr map[string]any) (bool, error)
}

// SpecFor builds a Spec from c, or from the trace's Run event when c is nil.
// The primary resource always comes from the Run event.
func SpecFor(c *config.Config, ix *trace.Index) (Spec, error) {
	run, ok := ix.Run()
	if !ok {
		return Spec{}, errors.New("the trace has no run event")
	}
	s := Spec{PrimaryGroup: run.Primary.Group, PrimaryResource: run.Primary.Resource}
	if c != nil {
		if want, _ := meta.UnsafeGuessKindToResource(c.Primary.Kind); want.Group != s.PrimaryGroup || want.Resource != s.PrimaryResource {
			return Spec{}, fmt.Errorf("the config's primary is %s, but the trace's is %s", want.GroupResource(), schema.GroupResource{Group: s.PrimaryGroup, Resource: s.PrimaryResource})
		}
		s.Ready, s.Stable, s.Budgets = c.Ready, c.Timeouts.Stable, c.Budgets
		if c.Primary.Retain != "" {
			s.Retain = c.Retain
		}
		return s, nil
	}
	if run.Ready == "" || run.Stable <= 0 || run.Reads <= 0 || run.Writes <= 0 {
		return Spec{}, errors.New("the trace's run event records no ready, stable and budgets; pass a config")
	}
	ready, err := config.CompileReady(run.Ready)
	if err != nil {
		return Spec{}, fmt.Errorf("the trace's ready: %w", err)
	}
	s.Ready, s.Stable, s.Budgets = ready, run.Stable, config.Budgets{Reads: run.Reads, Writes: run.Writes}
	if run.Retain != "" {
		if s.Retain, err = config.CompileReady(run.Retain); err != nil {
			return Spec{}, fmt.Errorf("the trace's retain: %w", err)
		}
	}
	return s, nil
}

// Finding is one failed check.
type Finding struct {
	Check string `json:"check"`
	// Step counts from 0. It is -1 for a finding outside any step.
	Step int           `json:"step"`
	At   time.Duration `json:"at"`
	// Labels qualify the finding, such as "backoff-bound".
	Labels  []string `json:"labels,omitempty"`
	Summary string   `json:"summary"`
	Details []string `json:"details,omitempty"`
	// Kind is the system, group and resource of the object the finding is
	// about, such as "kube/apps/deployments", or "kube/configmaps" for the
	// core group. It is empty when the finding is about no one kind.
	Kind string `json:"kind,omitempty"`
}

// Signature identifies a finding across replays and shrinking: its check,
// summary and kind. Steps and names change between runs.
func (f Finding) Signature() string {
	return f.Check + "|" + f.Summary + "|" + f.Kind
}

// Report holds the findings and the notes on what the trace could not
// decide.
type Report struct {
	Findings []Finding
	Notes    []string
}

// Judge runs every check.
func Judge(ix *trace.Index, s Spec) Report {
	findings, notes := Converge(ix, s)
	findings = append(findings, Crash(ix)...)
	findings = append(findings, Clean(ix, s)...)
	findings = append(findings, Restore(ix, s)...)
	findings = append(findings, Unique(ix, s)...)
	notes = append(append(actionErrors(ix), unpicked(ix)...), notes...)
	return Report{Findings: findings, Notes: notes}
}

// unpicked notes each fault that altered no controller request while it
// was active.
func unpicked(ix *trace.Index) []string {
	starts := map[[2]int]trace.Event{}
	var notes []string
	for _, e := range ix.Events() {
		f := e.Fault
		if f == nil {
			continue
		}
		key := [2]int{f.Step, f.Index}
		if !f.Ended {
			starts[key] = e
			continue
		}
		if !slices.ContainsFunc(ix.Calls(trace.Controller, starts[key].At, math.MaxInt64), func(c trace.Event) bool {
			return c.At-c.Call.Latency <= e.At && c.Call.Injected != "" && strings.HasPrefix(f.What, c.Call.Injected)
		}) {
			notes = append(notes, fmt.Sprintf("%s, fault %d (%s) picked no request.", stepName(ix, f.Step), f.Index, f.What))
		}
	}
	return notes
}

func actionErrors(ix *trace.Index) []string {
	var notes []string
	for _, step := range ix.Steps() {
		for _, e := range ix.Actions(step.Step) {
			if a := e.Action; a.Error != "" {
				notes = append(notes, fmt.Sprintf("%s, action %d (%s) failed at %v: %s", stepName(ix, a.Step), a.Index, a.Do, e.At, a.Error))
			}
		}
	}
	return notes
}

// stepName names a step, and says which steps the runner appended.
func stepName(ix *trace.Index, step int) string {
	if step < 0 {
		return "Before step 0"
	}
	name := fmt.Sprintf("Step %d", step)
	run, _ := ix.Run()
	switch {
	case run.PlanSteps == 0:
	case step == run.PlanSteps:
		name += " (delete all)"
	case step == run.PlanSteps+1:
		name += " (restart)"
	}
	return name
}

// Markdown renders a report as report.md.
func Markdown(ix *trace.Index, r Report) string {
	var b strings.Builder
	if len(r.Findings) == 0 {
		b.WriteString("No findings.\n")
	} else {
		b.WriteString("# Findings\n")
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "\n## %s: %s\n\n%s, at %v.\n", f.Check, f.Summary, stepName(ix, f.Step), f.At)
		if len(f.Labels) > 0 {
			fmt.Fprintf(&b, "Labels: %s.\n", strings.Join(f.Labels, ", "))
		}
		if len(f.Details) > 0 {
			fmt.Fprintf(&b, "\n```\n%s\n```\n", strings.Join(f.Details, "\n"))
		}
	}
	if len(r.Notes) > 0 {
		b.WriteString("\n# Notes\n\n")
		for _, note := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", note)
		}
	}
	return b.String()
}
