// Package check judges a trace. Every check is a pure function of the trace
// and the spec.
package check

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Spec is what the checks need from the config.
type Spec struct {
	// Primaries are the listed kinds, in config order.
	Primaries []Primary
	// Primary is the CR kind, as group and plural resource.
	PrimaryGroup    string
	PrimaryResource string
	Ready           func(obj map[string]any) (bool, error)
	Stable          time.Duration
	Budgets         config.Budgets
	// Retain says whether Clean allows a deleted CR's resources to stay.
	// It is nil when the config sets no primary.retain.
	Retain func(cr map[string]any) (bool, error)
	// Models are the stage models, from the config or the Run event.
	Models []Model
	// StatusKinds are the installed CRDs with a status subresource.
	StatusKinds []trace.Kind
	// Owners are owning-CR rules 2 and 3, from the config or the Run event.
	Owners owner.Rules
	// Halted is nil when the config sets no primary.halted.
	Halted func(cr map[string]any) (bool, error)
	// Polls maps each fake to its poll interval.
	Polls map[string]time.Duration
	// Ignore maps a fake to the body fields Restore does not compare.
	Ignore map[string][]string
	// Properties come from the config, or else the Run event.
	Properties []PropertySpec
	// Periodic is nil when the config sets no primary.periodic.
	Periodic *trace.Periodic
}

// Primary is one listed kind as the checks judge its CRs.
type Primary struct {
	Kind  trace.Kind
	Ready func(cr map[string]any) (bool, error)
	// Retain, Halted and Periodic are nil when the entry sets none.
	Retain, Halted func(cr map[string]any) (bool, error)
	Periodic       *trace.Periodic
}

// PropertySpec is a compiled config property.
type PropertySpec struct {
	Name string
	// Always judges it at each version event, and otherwise at each settled
	// step's end.
	Always  bool
	Expr    string
	Program *config.PropertyProgram
}

func compileProperties(properties []trace.Property) ([]PropertySpec, error) {
	var compiled []PropertySpec
	for _, p := range properties {
		program, err := config.CompileProperty(p.Expr)
		if err != nil {
			return nil, fmt.Errorf("property %s: %w", p.Name, err)
		}
		compiled = append(compiled, PropertySpec{Name: p.Name, Always: p.When == string(config.Always), Expr: p.Expr, Program: program})
	}
	return compiled, nil
}

// Model is a stage model's kind as the checks judge it.
type Model struct {
	// System is empty for kube. A fake's Model has an empty Kind, which
	// matches each of its collections.
	System string
	Kind   trace.Kind
	// Ready says whether an object of the kind is ready.
	Ready func(obj map[string]any) (bool, error)
	// Dependency is nil unless Honest judges the kind.
	Dependency *trace.Dependency
}

// SpecFor builds a Spec from c, or from the trace's Run event when c is nil.
// The primary resource and the status kinds always come from the Run event.
func SpecFor(c *config.Config, ix *trace.Index) (Spec, error) {
	run, ok := ix.Run()
	if !ok {
		return Spec{}, errors.New("the trace has no run event")
	}
	s := Spec{PrimaryGroup: run.Primary.Group, PrimaryResource: run.Primary.Resource, StatusKinds: run.StatusKinds}
	if c != nil {
		if want, _ := meta.UnsafeGuessKindToResource(c.Primary.Kind); want.Group != s.PrimaryGroup || want.Resource != s.PrimaryResource {
			return Spec{}, fmt.Errorf("the config's primary is %s, but the trace's is %s", want.GroupResource(), schema.GroupResource{Group: s.PrimaryGroup, Resource: s.PrimaryResource})
		}
		s.Ready, s.Stable, s.Budgets = c.Ready, c.Timeouts.Stable, c.Budgets
		if c.Primary.Retain != "" {
			s.Retain = c.Retain
		}
		if c.Primary.Halted != "" {
			s.Halted = c.Halted
		}
		var err error
		if s.Models, err = configModels(c.Models, run.Models); err != nil {
			return Spec{}, err
		}
		owners, owns, err := owner.Sources(c, recordedResource(run))
		if err != nil {
			return Spec{}, fmt.Errorf("the config's owners: %w", err)
		}
		systems := owner.Systems(c)
		if err := s.addSystems(systems); err != nil {
			return Spec{}, fmt.Errorf("the config's systems: %w", err)
		}
		if s.Owners, err = owner.Compile(owners, []trace.Primary{{Kind: trace.Kind{Group: s.PrimaryGroup, Resource: s.PrimaryResource}, Owns: owns}}, systems); err != nil {
			return Spec{}, fmt.Errorf("the config's owners: %w", err)
		}
		if s.Properties, err = compileProperties(owner.Properties(c)); err != nil {
			return Spec{}, fmt.Errorf("the config's properties: %w", err)
		}
		s.Periodic = owner.Periodic(c)
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
	if run.Halted != "" {
		if s.Halted, err = config.CompileReady(run.Halted); err != nil {
			return Spec{}, fmt.Errorf("the trace's halted: %w", err)
		}
	}
	for _, m := range run.Models {
		ready, err := config.CompileReady(m.Ready)
		if err != nil {
			return Spec{}, fmt.Errorf("the trace's ready of model %s: %w", m.Kind.Kind, err)
		}
		s.Models = append(s.Models, Model{Kind: m.Kind, Ready: ready, Dependency: m.Dependency})
	}
	if err := s.addSystems(run.Systems); err != nil {
		return Spec{}, fmt.Errorf("the trace's systems: %w", err)
	}
	if s.Owners, err = owner.Compile(run.Owners, []trace.Primary{{Kind: trace.Kind{Group: s.PrimaryGroup, Resource: s.PrimaryResource}, Owns: run.Owns}}, run.Systems); err != nil {
		return Spec{}, fmt.Errorf("the trace's owners: %w", err)
	}
	if s.Properties, err = compileProperties(run.Properties); err != nil {
		return Spec{}, fmt.Errorf("the trace's properties: %w", err)
	}
	s.Periodic = run.Periodic
	return s, nil
}

// addSystems sets Polls and Ignore, and adds a Model for each dependency
// fake.
func (s *Spec) addSystems(fakes []trace.System) error {
	s.Polls = map[string]time.Duration{}
	for _, fake := range fakes {
		s.Polls[fake.Name] = fake.Poll
		if len(fake.Ignore) > 0 {
			if s.Ignore == nil {
				s.Ignore = map[string][]string{}
			}
			s.Ignore[fake.Name] = fake.Ignore
		}
		if fake.Dependency == nil {
			continue
		}
		ready, err := config.CompileReady(fake.Ready)
		if err != nil {
			return fmt.Errorf("ready of %s: %w", fake.Name, err)
		}
		s.Models = append(s.Models, Model{System: fake.Name, Ready: ready, Dependency: fake.Dependency})
	}
	return nil
}

// poll is a fake's poll interval, or else Stable.
func (s Spec) poll(system string) time.Duration {
	if p, found := s.Polls[system]; found {
		return p
	}
	return s.Stable
}

// covers reports whether ref is of m's kind: on m's system, and of its
// resource when Kind sets one.
func (m Model) covers(ref trace.Ref) bool {
	return ref.System == cmp.Or(m.System, "kube") && (m.Kind.Resource == "" || m.Kind.Group == ref.Group && m.Kind.Resource == ref.Resource)
}

// modelled reports whether a stage model writes the status of ref's kind.
func (s Spec) modelled(ref trace.Ref) bool {
	return ref.System == "kube" && slices.ContainsFunc(s.Models, func(m Model) bool {
		return m.System == "" && m.Kind.Group == ref.Group && m.Kind.Resource == ref.Resource
	})
}

// recordedResource maps an owns kind to the run event's resource for it, or
// else a guess. It counts every kind as namespaced, since the run refused a
// namespace on a cluster-scoped kind.
func recordedResource(run trace.Run) func(schema.GroupVersionKind) (string, bool, error) {
	return func(gvk schema.GroupVersionKind) (string, bool, error) {
		if i := slices.IndexFunc(run.Owns, func(r trace.Owns) bool { return r.Kind.Group == gvk.Group && r.Kind.Kind == gvk.Kind }); i >= 0 {
			return run.Owns[i].Kind.Resource, true, nil
		}
		plural, _ := meta.UnsafeGuessKindToResource(gvk)
		return plural.Resource, true, nil
	}
}

// configModels takes each model's ready and dependency from the config, and
// its resource from the run event's model of the same kind, or else a guess.
func configModels(models []config.Model, recorded []trace.Model) ([]Model, error) {
	ready, err := config.CompileReady(config.ReadyConditionCEL)
	if err != nil {
		return nil, err
	}
	var judged []Model
	for _, m := range models {
		plural, _ := meta.UnsafeGuessKindToResource(m.Kind)
		kind := trace.Kind{Group: m.Kind.Group, Version: m.Kind.Version, Kind: m.Kind.Kind, Resource: plural.Resource}
		if i := slices.IndexFunc(recorded, func(r trace.Model) bool { return r.Kind.Group == kind.Group && r.Kind.Kind == kind.Kind }); i >= 0 {
			kind = recorded[i].Kind
		}
		var dep *trace.Dependency
		if m.Dependency != nil {
			dep = &trace.Dependency{Required: m.Dependency.Required, Track: m.Dependency.Track}
		}
		judged = append(judged, Model{Kind: kind, Ready: ready, Dependency: dep})
	}
	return judged, nil
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
	findings = append(findings, Honest(ix, s)...)
	findings = append(findings, Backoff(ix, s)...)
	findings = append(findings, Property(ix, s)...)
	notes = append(append(append(actionErrors(ix), unpicked(ix, s)...), notes...), unmapped(ix)...)
	return Report{Findings: findings, Notes: notes}
}

// unmapped notes each distinct request of the controller that its system's
// API map could not place.
func unmapped(ix *trace.Index) []string {
	var notes []string
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		if c := e.Call; c.Unmapped {
			if note := fmt.Sprintf("%s: unmapped request %s %s", c.Ref.System, c.Method, c.Path); !slices.Contains(notes, note) {
				notes = append(notes, note)
			}
		}
	}
	return notes
}

// unpicked notes each request fault that altered no controller request
// while it was active, each stage-model fault whose model held no object,
// and each holdWatch while the controller had no watch of its kind.
func unpicked(ix *trace.Index, s Spec) []string {
	starts := map[[2]int]trace.Event{}
	var notes []string
	for _, h := range ix.Holds() {
		if !ix.WatchedKind(trace.Controller, h.Kind, h.Start, h.End) {
			notes = append(notes, fmt.Sprintf("%s, fault %d (%s) held no watch.", stepName(ix, h.Step), h.Index, h.What))
		}
	}
	for _, e := range ix.Events() {
		f := e.Fault
		if f == nil || f.Hold != nil {
			continue
		}
		key := [2]int{f.Step, f.Index}
		if !f.Ended {
			starts[key] = e
			continue
		}
		switch {
		case f.Model != "" && !s.fake(f.Model) && !held(ix, *f, starts[key].At, e.At):
			notes = append(notes, fmt.Sprintf("%s, fault %d (%s) held no object.", stepName(ix, f.Step), f.Index, f.What))
		case f.Model == "" && !slices.ContainsFunc(ix.Calls(trace.Controller, starts[key].At, math.MaxInt64), func(c trace.Event) bool {
			return c.At-c.Call.Latency <= e.At && c.Call.Ref.System == cmp.Or(f.System, "kube") &&
				c.Call.Injected != "" && strings.HasPrefix(f.What, c.Call.Injected)
		}):
			notes = append(notes, fmt.Sprintf("%s, fault %d (%s) picked no request.", stepName(ix, f.Step), f.Index, f.What))
		}
	}
	return notes
}

// fake reports whether a fault's model is a fake service.
func (s Spec) fake(model string) bool {
	_, found := s.Polls[model]
	return found
}

// held reports whether f's model wrote, in (from, to], a version that f
// holds: a stuck status, or the slowDelete finalizer.
func held(ix *trace.Index, f trace.Fault, from, to time.Duration) bool {
	return slices.ContainsFunc(ix.EventsIn(from, to), func(e trace.Event) bool {
		v := e.Version
		if v == nil || ix.Writer(*v) != trace.Caller(f.Model) {
			return false
		}
		var body struct {
			Metadata struct {
				Finalizers []string `json:"finalizers"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct{ Type, Reason string } `json:"conditions"`
			} `json:"status"`
		}
		json.Unmarshal(v.Body, &body)
		if strings.HasPrefix(f.What, "slowDelete") {
			return slices.Contains(body.Metadata.Finalizers, trace.SlowDeleteFinalizer)
		}
		return slices.ContainsFunc(body.Status.Conditions, func(c struct{ Type, Reason string }) bool {
			return c.Type == "Ready" && c.Reason == trace.StuckReason
		})
	})
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
