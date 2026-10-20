// Package check judges a trace. Every check is a pure function of the trace
// and the spec.
package check

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Spec is what the checks need from the config.
type Spec struct {
	// Primary is the CR kind, as group and plural resource.
	PrimaryGroup    string
	PrimaryResource string
	Ready           func(obj map[string]any) (bool, error)
	Stable          time.Duration
	Budgets         config.Budgets
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
		s.Ready, s.Stable, s.Budgets = c.Ready, c.Timeouts.Stable, c.Budgets
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
	return Report{Findings: append(findings, Crash(ix)...), Notes: append(actionErrors(ix), notes...)}
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
