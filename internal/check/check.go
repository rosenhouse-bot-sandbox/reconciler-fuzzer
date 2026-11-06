// Package check judges a trace. Every check is a pure function of the trace
// and the spec.
package check

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
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
	Stable    time.Duration
	Budgets   config.Budgets
	// Models are the stage models, from the config or the Run event.
	Models []Model
	// StatusKinds are the installed CRDs with a status subresource.
	StatusKinds []trace.Kind
	// Owners are owning-CR rules 2 and 3, from the config or the Run event.
	Owners owner.Rules
	// Polls maps each fake to its poll interval.
	Polls map[string]time.Duration
	// Ignore maps a fake to the body fields Restore does not compare.
	Ignore map[string][]string
	// Properties come from the config, or else the Run event.
	Properties []PropertySpec
	// References come from the config, or else the Run event.
	References []Reference
	// NotRestored are the kinds Restore lets stay gone under a damaged
	// root, from the config or else the Run event.
	NotRestored []trace.Kind
	// AllowExitBeforeWatch says Crash skips an exit of a process that opened
	// no watch of a listed kind when a later process opened one, from the
	// config or else the Run event.
	AllowExitBeforeWatch bool
}

// Reference is a config reference as Honest and Converge judge it.
type Reference struct {
	From trace.Kind
	// To names the to kind by a ref with no namespace or name.
	To         trace.Ref
	Namespaced bool
	// Name and Namespace are field paths in the From CR.
	Name, Namespace string
	Ready           func(obj map[string]any) (bool, error)
	Required        bool
	Track           time.Duration
	// Block says the To object's deletion waits for the From CRs that name
	// it (onDelete: block).
	Block bool
}

// Primary is one listed kind as the checks judge its CRs.
type Primary struct {
	// Key names the entry in the config, as "primary" or "primaries[1]".
	Key   string
	Kind  trace.Kind
	Ready func(cr map[string]any) (bool, error)
	// Retain says whether Clean and Teardown allow a deleted CR's resources
	// to stay.
	// Retain, Halted and Periodic are nil when the entry sets none.
	Retain, Halted func(cr map[string]any) (bool, error)
	Periodic       *trace.Periodic
}

// ready reports whether p's ready accepts cr.
func (p Primary) ready(cr map[string]any) bool { return satisfies(p.Ready, cr) }

// primary is the entry of ref's kind, if ref names a CR.
func (s Spec) primary(ref trace.Ref) (Primary, bool) {
	i := slices.IndexFunc(s.Primaries, func(p Primary) bool { return ref.OfKind(p.Kind) })
	if i < 0 {
		return Primary{}, false
	}
	return s.Primaries[i], true
}

// listed reports whether ref names a CR.
func (s Spec) listed(ref trace.Ref) bool {
	_, listed := s.primary(ref)
	return listed
}

// compilePrimaries compiles each entry's CEL. An entry that names no key
// is "primary" if it is the only one, and else "primaries[i]".
func compilePrimaries(entries []trace.Primary) ([]Primary, error) {
	var compiled []Primary
	for i, e := range entries {
		p := Primary{Key: e.Key, Kind: e.Kind, Periodic: e.Periodic}
		switch {
		case p.Key != "":
		case len(entries) == 1:
			p.Key = "primary"
		default:
			p.Key = fmt.Sprintf("primaries[%d]", i)
		}
		name := groupResource(e.Kind)
		var err error
		if p.Ready, err = config.CompileReady(e.Ready, e.Schema); err != nil {
			return nil, fmt.Errorf("ready of %s: %w", name, err)
		}
		if e.Retain != "" {
			if p.Retain, err = config.CompileReady(e.Retain, e.Schema); err != nil {
				return nil, fmt.Errorf("retain of %s: %w", name, err)
			}
		}
		if e.Halted != "" {
			if p.Halted, err = config.CompileReady(e.Halted, e.Schema); err != nil {
				return nil, fmt.Errorf("halted of %s: %w", name, err)
			}
		}
		compiled = append(compiled, p)
	}
	return compiled, nil
}

// groupResource names k as resource.group.
func groupResource(k trace.Kind) string {
	return schema.GroupResource{Group: k.Group, Resource: k.Resource}.String()
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
// The status kinds always come from the Run event, and c must list the Run
// event's kinds. The Run event's references must name each To kind of c's
// references, and name each From kind of c's block references or have one
// from it with onDelete: block, so that the trace holds seens of them.
func SpecFor(c *config.Config, ix *trace.Index) (Spec, error) {
	run, ok := ix.Run()
	if !ok {
		return Spec{}, errors.New("the trace has no run event")
	}
	if len(run.Primaries) == 0 {
		return Spec{}, errors.New("the trace's run event lists no primaries, so an older reconciler-fuzzer recorded it; run its plan.json again")
	}
	s := Spec{StatusKinds: run.StatusKinds}
	if c != nil {
		owners, primaries, err := owner.Sources(c, recordedResource(run))
		if err != nil {
			return Spec{}, fmt.Errorf("the config: %w", err)
		}
		if want, got := listedKinds(primaries), listedKinds(run.Primaries); !slices.Equal(want, got) {
			return Spec{}, fmt.Errorf("the config's primaries list %s, but the trace's list %s", strings.Join(want, ", "), strings.Join(got, ", "))
		}
		if s.Primaries, err = compilePrimaries(primaries); err != nil {
			return Spec{}, fmt.Errorf("the config's primaries: %w", err)
		}
		s.Stable, s.Budgets = c.Timeouts.Stable, c.Budgets
		if s.Models, err = configModels(c.Models, run.Models); err != nil {
			return Spec{}, err
		}
		systems := owner.Systems(c)
		if err := s.addSystems(systems); err != nil {
			return Spec{}, fmt.Errorf("the config's systems: %w", err)
		}
		if s.Owners, err = owner.Compile(owners, primaries, systems); err != nil {
			return Spec{}, fmt.Errorf("the config's owners: %w", err)
		}
		if s.Properties, err = compileProperties(owner.Properties(c)); err != nil {
			return Spec{}, fmt.Errorf("the config's properties: %w", err)
		}
		if s.References, err = configReferences(c, run); err != nil {
			return Spec{}, err
		}
		s.NotRestored = c.NotRestored
		s.AllowExitBeforeWatch = c.Launch.AllowExitBeforeWatch
		return s, nil
	}
	if run.Stable <= 0 || run.Reads <= 0 || run.Writes <= 0 {
		return Spec{}, errors.New("the trace's run event records no stable and budgets; pass a config")
	}
	var err error
	if s.Primaries, err = compilePrimaries(run.Primaries); err != nil {
		return Spec{}, fmt.Errorf("the trace's primaries: %w", err)
	}
	s.Stable, s.Budgets = run.Stable, config.Budgets{Reads: run.Reads, Writes: run.Writes}
	for _, m := range run.Models {
		ready, err := config.CompileReady(m.Ready, nil)
		if err != nil {
			return Spec{}, fmt.Errorf("the trace's ready of model %s: %w", m.Kind.Kind, err)
		}
		s.Models = append(s.Models, Model{Kind: m.Kind, Ready: ready, Dependency: m.Dependency})
	}
	if err := s.addSystems(run.Systems); err != nil {
		return Spec{}, fmt.Errorf("the trace's systems: %w", err)
	}
	if s.Owners, err = owner.Compile(run.Owners, run.Primaries, run.Systems); err != nil {
		return Spec{}, fmt.Errorf("the trace's owners: %w", err)
	}
	if s.Properties, err = compileProperties(run.Properties); err != nil {
		return Spec{}, fmt.Errorf("the trace's properties: %w", err)
	}
	if s.References, err = compileReferences(run.References); err != nil {
		return Spec{}, fmt.Errorf("the trace's references: %w", err)
	}
	s.NotRestored = run.NotRestored
	s.AllowExitBeforeWatch = run.AllowExitBeforeWatch
	return s, nil
}

// configReferences compiles c's references. The run event's references must
// name each kube To kind and each fake To system, and name each From kind of
// a block reference or have one from it with onDelete: block, so that the
// trace holds seens of them.
func configReferences(c *config.Config, run trace.Run) ([]Reference, error) {
	for i, r := range c.References {
		if !slices.ContainsFunc(run.References, func(recorded trace.Reference) bool {
			return recorded.System == r.System && recorded.To.Group == r.To.Group && recorded.To.Kind == r.To.Kind
		}) {
			to := r.System + ":" + r.Collection
			if r.System == "" {
				to = kindName(trace.Kind{Group: r.To.Group, Version: r.To.Version, Kind: r.To.Kind})
			}
			return nil, fmt.Errorf("the config's references[%d] names %s, but no reference of the trace's run event does, so the trace holds no seens of it", i, to)
		}
		if r.Block && !slices.ContainsFunc(run.References, func(recorded trace.Reference) bool {
			return recorded.To.Group == r.From.Group && recorded.To.Kind == r.From.Kind ||
				recorded.Block && recorded.From.Group == r.From.Group && recorded.From.Kind == r.From.Kind
		}) {
			from := kindName(trace.Kind{Group: r.From.Group, Version: r.From.Version, Kind: r.From.Kind})
			return nil, fmt.Errorf("the config's references[%d] from %s has onDelete: block, but no reference of the trace's run event names that kind, and none from it has onDelete: block, so the trace holds no seens of it", i, from)
		}
	}
	references, err := owner.References(c, recordedResource(run))
	if err != nil {
		return nil, fmt.Errorf("the config: %w", err)
	}
	compiled, err := compileReferences(references)
	if err != nil {
		return nil, fmt.Errorf("the config's references: %w", err)
	}
	return compiled, nil
}

// compileReferences compiles each reference's ready.
func compileReferences(references []trace.Reference) ([]Reference, error) {
	var compiled []Reference
	for i, r := range references {
		ready, err := config.CompileReady(r.Ready, r.Schema)
		if err != nil {
			return nil, fmt.Errorf("ready of references[%d]: %w", i, err)
		}
		compiled = append(compiled, Reference{From: r.From, To: r.ToRef(), Namespaced: r.Namespaced, Name: r.Name, Namespace: r.Namespace,
			Ready: ready, Required: r.Required, Track: r.Track, Block: r.Block})
	}
	return compiled, nil
}

// listedKinds names each entry's kind as resource.group, in order.
func listedKinds(primaries []trace.Primary) []string {
	var kinds []string
	for _, p := range primaries {
		kinds = append(kinds, groupResource(p.Kind))
	}
	return kinds
}

// addSystems sets Polls and Ignore from the fakes among systems, and adds a
// Model for each dependency fake.
func (s *Spec) addSystems(systems []trace.System) error {
	s.Polls = map[string]time.Duration{}
	for _, system := range systems {
		if system.Name == "kube" {
			continue
		}
		s.Polls[system.Name] = system.Poll
		if len(system.Ignore) > 0 {
			if s.Ignore == nil {
				s.Ignore = map[string][]string{}
			}
			s.Ignore[system.Name] = system.Ignore
		}
		if system.Dependency == nil {
			continue
		}
		ready, err := config.CompileReady(system.Ready, nil)
		if err != nil {
			return fmt.Errorf("ready of %s: %w", system.Name, err)
		}
		s.Models = append(s.Models, Model{System: system.Name, Ready: ready, Dependency: system.Dependency})
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

// longestPoll is the longest of Stable and every fake's poll interval.
func (s Spec) longestPoll() time.Duration {
	longest := s.Stable
	for _, p := range s.Polls {
		longest = max(longest, p)
	}
	return longest
}

// covers reports whether ref is of m's kind: on m's system, and of its
// resource when Kind sets one.
func (m Model) covers(ref trace.Ref) bool {
	return ref.System == cmp.Or(m.System, "kube") && (m.Kind.Resource == "" || m.Kind.Group == ref.Group && m.Kind.Resource == ref.Resource)
}

// periodicPaths are the periodic paths of ref's kind, if ref names a CR.
func (s Spec) periodicPaths(ref trace.Ref) []string {
	if p, listed := s.primary(ref); listed && p.Periodic != nil {
		return p.Periodic.Paths
	}
	return nil
}

// modelled reports whether a stage model writes the status of ref's kind.
func (s Spec) modelled(ref trace.Ref) bool {
	return ref.System == "kube" && slices.ContainsFunc(s.Models, func(m Model) bool {
		return m.System == "" && m.Kind.Group == ref.Group && m.Kind.Resource == ref.Resource
	})
}

// recordedResource maps a listed, owns or reference kind to the run event's
// resource for it, or else a guess. It counts every kind as namespaced, since
// the run refused a namespace on a cluster-scoped kind.
func recordedResource(run trace.Run) func(schema.GroupVersionKind) (string, bool, error) {
	var recorded []trace.Kind
	for _, p := range run.Primaries {
		recorded = append(recorded, p.Kind)
		for _, o := range p.Owns {
			recorded = append(recorded, o.Kind)
		}
	}
	for _, r := range run.References {
		recorded = append(recorded, r.To)
	}
	return func(gvk schema.GroupVersionKind) (string, bool, error) {
		if i := slices.IndexFunc(recorded, func(k trace.Kind) bool { return k.Group == gvk.Group && k.Kind == gvk.Kind }); i >= 0 {
			return recorded[i].Resource, true, nil
		}
		plural, _ := meta.UnsafeGuessKindToResource(gvk)
		return plural.Resource, true, nil
	}
}

// configModels takes each model's ready and dependency from the config, and
// its resource from the run event's model of the same kind, or else a guess.
func configModels(models []config.Model, recorded []trace.Model) ([]Model, error) {
	ready, err := config.CompileReady(config.ReadyConditionCEL, nil)
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
// decide. EndedPlan holds the notes of the actions whose error skipped the
// plan's later steps.
type Report struct {
	Findings  []Finding
	Notes     []string
	EndedPlan []string
}

// Judge runs every check.
func Judge(ix *trace.Index, s Spec) Report {
	findings, notes := Converge(ix, s)
	findings = append(findings, Crash(ix, s)...)
	findings = append(findings, Clean(ix, s)...)
	findings = append(findings, Restore(ix, s)...)
	findings = append(findings, Unique(ix, s)...)
	findings = append(findings, Honest(ix, s)...)
	findings = append(findings, Ordered(ix, s)...)
	findings = append(findings, Teardown(ix, s)...)
	findings = append(findings, Backoff(ix, s)...)
	findings = append(findings, Finalizers(ix, s)...)
	findings = append(findings, Property(ix, s)...)
	failed, ended := actionErrors(ix)
	notes = slices.Concat(failed, killedStops(ix), unpicked(ix, s), notes, unmapped(ix), forbidden(ix), failedRoles(ix, s))
	return Report{Findings: findings, Notes: notes, EndedPlan: ended}
}

// failedRoles notes each kind whose role CEL failed on every resource the
// controller created, with the first error. A broken role guarded by kind
// fails only on the kinds it describes and yields "" on the rest, so one note
// per system would miss it.
func failedRoles(ix *trace.Index, s Spec) []string {
	w := owner.New(ix, s.Owners)
	failed, worked := map[trace.Ref]error{}, map[trace.Ref]bool{}
	var kinds []trace.Ref
	for _, i := range w.All() {
		if !controllerResource(i) {
			continue
		}
		k := i.Ref
		k.Namespace, k.Name = "", ""
		switch err := w.RoleError(i); {
		case err == nil:
			worked[k] = true
		case failed[k] == nil:
			failed[k] = err
			kinds = append(kinds, k)
		}
	}
	var notes []string
	for _, k := range kinds {
		if !worked[k] {
			notes = append(notes, fmt.Sprintf("%s: the role failed on every resource of this kind the controller created: %v", kind(k), failed[k]))
		}
	}
	return notes
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

// deniedResource and deniedPath match the authorizer's words in a 403's
// message. Admission, such as a namespace being deleted, a quota or a
// webhook, says something else.
var (
	deniedResource = regexp.MustCompile(`cannot (\S+) resource "([^"/]*)(?:/([^"]*))?" in API group "([^"]*)" (?:in the namespace "([^"]*)"|at the cluster scope)`)
	deniedPath     = regexp.MustCompile(`cannot (\S+) path "([^"]*)"`)
)

// deniedGrant is in the RBAC API's 403 to a write of a role or binding that
// grants permissions the writer lacks.
const deniedGrant = "is attempting to grant RBAC permissions not currently held"

// denial says what RBAC forbids in a 403 answer to c, such as "watch on
// volumes.storage.thirdparty in namespace ns", or "" for another 403.
func denial(c trace.Call) string {
	if m := deniedResource.FindStringSubmatch(c.Forbidden); m != nil {
		return describeRequest(m[1], m[4], m[2], m[3], m[5])
	}
	if m := deniedPath.FindStringSubmatch(c.Forbidden); m != nil {
		return fmt.Sprintf("%s on path %s", m[1], m[2])
	}
	if strings.Contains(c.Forbidden, deniedGrant) {
		return describeRequest(c.Verb, c.Ref.Group, c.Ref.Resource, "", c.Ref.Namespace) + ", which grants permissions the controller lacks"
	}
	return ""
}

// describeRequest names a request by its verb, resource or subresource,
// and scope.
func describeRequest(verb, group, resource, subresource, namespace string) string {
	on := schema.GroupResource{Group: group, Resource: resource}.String()
	if subresource != "" {
		on = "the " + subresource + " subresource of " + on
	}
	scope := "at the cluster scope"
	if namespace != "" {
		scope = "in namespace " + namespace
	}
	return fmt.Sprintf("%s on %s %s", verb, on, scope)
}

// forbidden notes how many times RBAC denied the controller each request it
// names.
func forbidden(ix *trace.Index) []string {
	var order []string
	counts := map[string]int{}
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		what := denial(*e.Call)
		if what == "" {
			continue
		}
		if counts[what] == 0 {
			order = append(order, what)
		}
		counts[what]++
	}
	notes := make([]string, len(order))
	for i, what := range order {
		notes[i] = fmt.Sprintf("kube: launch.rbac forbids the controller's %s, %s", what, times(counts[what]))
	}
	return notes
}

// times says n times, as "1 time" or "2 times".
func times(n int) string {
	if n == 1 {
		return "1 time"
	}
	return fmt.Sprintf("%d times", n)
}

// unpicked notes each request fault that altered no controller request
// while it was active, each stage-model fault whose model held no object,
// and each holdWatch while the controller had no watch of its kind.
func unpicked(ix *trace.Index, s Spec) []string {
	starts := map[[2]int]trace.Event{}
	var notes []string
	for _, h := range ix.Holds() {
		switch {
		case ix.WatchedKind(trace.Controller, h.Kind, h.Start, h.End):
		case ix.WatchedKind(trace.Controller, h.Kind, h.End, nextStart(ix, h.Step)):
			// A longer hold could have held the watch that the controller
			// opened later in the hold's step.
			notes = append(notes, faultNote(ix, h.Step, h.Index, h.What, "held no watch", h.Start, h.End))
		default:
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
		start := starts[key].At
		switch {
		case f.Model != "" && !s.fake(f.Model) && !held(ix, *f, start, e.At):
			notes = append(notes, faultNote(ix, f.Step, f.Index, f.What, "held no object", start, e.At))
		case f.Model == "" && !slices.ContainsFunc(ix.Calls(trace.Controller, start, math.MaxInt64), func(c trace.Event) bool {
			return c.At-c.Call.Latency <= e.At && c.Call.Ref.System == cmp.Or(f.System, "kube") &&
				c.Call.Injected != "" && strings.HasPrefix(f.What, c.Call.Injected)
		}):
			notes = append(notes, faultNote(ix, f.Step, f.Index, f.What, "picked no request", start, e.At))
		}
	}
	return notes
}

// nextStart is when the step after step started, or math.MaxInt64 when no
// later step started.
func nextStart(ix *trace.Index, step int) time.Duration {
	for _, span := range ix.Steps() {
		if span.Step > step {
			return span.Start
		}
	}
	return math.MaxInt64
}

// faultNote says that fault index of step did nothing while on from start
// to end. A step without a period ends its faults by the time its last
// action returns. When the controller still runs then, a period would keep
// the fault on, so the note says how long the fault lasted.
func faultNote(ix *trace.Index, step, index int, what, nothing string, start, end time.Duration) string {
	note := fmt.Sprintf("%s, fault %d (%s) %s", stepName(ix, step), index, what, nothing)
	span, _ := ix.Step(step)
	if _, up := ix.Up(end); up && span.Period != nil && *span.Period == 0 {
		return fmt.Sprintf("%s: it lasted %v because the step has no period. Set one to keep faults on after the last action returns.", note, (end - start).Round(time.Millisecond))
	}
	return note + "."
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
		if strings.HasPrefix(f.What, "slowDelete") {
			return slices.Contains(finalizers(*v), trace.SlowDeleteFinalizer)
		}
		var body struct {
			Status struct {
				Conditions []struct{ Type, Reason string } `json:"conditions"`
			} `json:"status"`
		}
		json.Unmarshal(v.Body, &body)
		return slices.ContainsFunc(body.Status.Conditions, func(c struct{ Type, Reason string }) bool {
			return c.Type == "Ready" && c.Reason == trace.StuckReason
		})
	})
}

// actionErrors notes each failed action, and returns the notes of those
// that ended the plan as well.
func actionErrors(ix *trace.Index) (notes, ended []string) {
	for _, step := range ix.Steps() {
		for _, e := range ix.Actions(step.Step) {
			a := e.Action
			if a.Error == "" {
				continue
			}
			note := fmt.Sprintf("%s, action %d (%s) failed at %v: %s", stepName(ix, a.Step), a.Index, a.Do, e.At, a.Error)
			notes = append(notes, note)
			if a.EndedPlan {
				ended = append(ended, note)
			}
		}
	}
	return notes, ended
}

// killedStops notes each stop of the controller that SIGKILL ended once
// launch.gracePeriod passed.
func killedStops(ix *trace.Index) []string {
	var notes []string
	steps := ix.Steps()
	for _, e := range ix.Procs() {
		if !e.Proc.GracePeriodPassed {
			continue
		}
		notes = append(notes, fmt.Sprintf("%s, at %v: the controller did not exit within launch.gracePeriod of SIGTERM, so the harness sent SIGKILL.", stopLabel(ix, steps, e.At), e.At))
	}
	return notes
}

// stopLabel names the step of a stop at t. Only the run's final stop can
// follow the last step's fault period.
func stopLabel(ix *trace.Index, steps []trace.StepSpan, t time.Duration) string {
	if n := len(steps); n > 0 && steps[n-1].Reached[trace.EndFaults] && t > steps[n-1].EndFaults {
		return "After the last step"
	}
	return stepName(ix, stepAt(steps, t))
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

// Markdown renders a report as report.md. replay is a command that runs
// the plan again, or empty.
func Markdown(ix *trace.Index, r Report, replay string) string {
	var b strings.Builder
	if len(r.Findings) == 0 {
		b.WriteString("No findings.\n")
	} else {
		b.WriteString("# Findings\n")
	}
	stepFaults := faults(ix)
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "\n## %s: %s\n\n%s, at %v.\n", f.Check, f.Summary, stepName(ix, f.Step), f.At.Round(time.Millisecond))
		if done := actions(ix, f.Step); len(done) > 0 {
			fmt.Fprintf(&b, "Actions: %s.\n", strings.Join(done, ", "))
		}
		if on := stepFaults[f.Step]; len(on) > 0 {
			fmt.Fprintf(&b, "Faults: %s.\n", strings.Join(on, ", "))
		}
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
	if len(r.Findings) > 0 {
		b.WriteString("\n# Next steps\n\n")
		if replay != "" {
			fmt.Fprintf(&b, "Replay the plan:\n\n```\n%s\n```\n\n", replay)
		}
		b.WriteString("[docs/failures.md](https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/blob/main/docs/failures.md) says what each finding means.\n")
	}
	return b.String()
}

// faults describes each step's faults, and its period when it has one.
func faults(ix *trace.Index) map[int][]string {
	on := map[int][]string{}
	for _, e := range ix.Events() {
		if f := e.Fault; f != nil && !f.Ended {
			on[f.Step] = append(on[f.Step], f.What)
		}
	}
	for step := range on {
		if span, _ := ix.Step(step); span.Period != nil && *span.Period > 0 {
			on[step] = append(on[step], "period "+span.Period.String())
		}
	}
	return on
}

// actions describes the harness's actions in step, such as "create slot a",
// "damage configmaps ns/widget-0" and "restart (upgrade)". An object action
// names its object, and any other action its slot.
func actions(ix *trace.Index, step int) []string {
	var done []string
	for _, e := range ix.Actions(step) {
		a := e.Action
		what := a.Do
		switch {
		case slices.Contains(objectActions, a.Do) && a.Ref != nil:
			what += " " + describe(*a.Ref)
		case a.Slot != "":
			what += " slot " + a.Slot
		}
		if a.Upgrade {
			what += " (upgrade)"
		}
		if a.Error != "" {
			what += " (failed)"
		}
		done = append(done, what)
	}
	return done
}
