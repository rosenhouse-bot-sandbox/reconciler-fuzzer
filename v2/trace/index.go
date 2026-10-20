package trace

import (
	"cmp"
	"slices"
	"sort"
	"time"
)

// Index answers the questions checks ask of a trace. A query at time t sees
// the events with At <= t.
type Index struct {
	events []Event
	run    *Run
	steps  map[int]*StepSpan
	calls  map[Caller][]Event
	procs  []Event
	// actions holds the Action events of each step.
	actions map[int][]Event
	// versions holds each object's Version events by kind.
	versions map[groupResource]map[Ref][]Event
	// firstRV is the first event, a version or a response, to hold each
	// object's resourceVersion.
	firstRV map[objectRV]Event
	// writer is the caller of the first response to hold each
	// resourceVersion.
	writer map[objectRV]Caller
}

type objectRV struct {
	ref Ref
	rv  string
}

// NewIndex indexes events, which must be in Seq order.
func NewIndex(events []Event) *Index {
	ix := &Index{
		steps:    map[int]*StepSpan{},
		calls:    map[Caller][]Event{},
		actions:  map[int][]Event{},
		versions: map[groupResource]map[Ref][]Event{},
		firstRV:  map[objectRV]Event{},
		writer:   map[objectRV]Caller{},
	}
	ix.Append(events...)
	return ix
}

// Append indexes events recorded after those already indexed, so a live
// settle wait need not rebuild the index.
func (ix *Index) Append(events ...Event) {
	for _, e := range events {
		ix.events = append(ix.events, e)
		switch {
		case e.Run != nil:
			if ix.run == nil {
				ix.run = e.Run
			}
		case e.Phase != nil:
			ix.addPhase(e)
		case e.Call != nil:
			ix.addCall(e)
		case e.Version != nil:
			ix.addVersion(e)
		case e.Proc != nil:
			ix.procs = append(ix.procs, e)
		case e.Action != nil:
			ix.actions[e.Action.Step] = append(ix.actions[e.Action.Step], e)
		}
	}
}

func (ix *Index) addPhase(e Event) {
	p := e.Phase
	span := ix.steps[p.Step]
	if span == nil {
		span = &StepSpan{Step: p.Step, Reached: map[PhaseName]bool{}}
		ix.steps[p.Step] = span
	}
	span.Reached[p.Name] = true
	switch p.Name {
	case FaultPeriod:
		span.Start = e.At
	case EndFaults:
		span.EndFaults = e.At
		span.Deadline = p.Deadline
		span.BackoffBound = p.BackoffBound
	case Settled:
		span.WaitEnd = e.At
		span.Settled = true
	case TimedOut:
		span.WaitEnd = e.At
	case Confirmed:
		span.ConfirmEnd = e.At
	}
}

func (ix *Index) addCall(e Event) {
	c := e.Call
	ix.calls[c.Caller] = append(ix.calls[c.Caller], e)
	if c.ResultRV == "" {
		return
	}
	key := objectRV{c.Object(), c.ResultRV}
	if _, seen := ix.firstRV[key]; !seen {
		ix.firstRV[key] = e
	}
	if _, seen := ix.writer[key]; !seen {
		ix.writer[key] = c.Caller
	}
}

func (ix *Index) addVersion(e Event) {
	v := e.Version
	kind := groupResource{v.Ref.Group, v.Ref.Resource}
	if ix.versions[kind] == nil {
		ix.versions[kind] = map[Ref][]Event{}
	}
	ix.versions[kind][v.Ref] = append(ix.versions[kind][v.Ref], e)
	key := objectRV{v.Ref, v.RV}
	if _, seen := ix.firstRV[key]; !seen {
		ix.firstRV[key] = e
	}
}

// Object is the object a call acted on. A create names it only in the
// response.
func (c Call) Object() Ref {
	ref := c.Ref
	if ref.Name == "" {
		ref.Name = c.ResultName
	}
	return ref
}

// Len is the number of events indexed.
func (ix *Index) Len() int { return len(ix.events) }

// Events returns the indexed events in Seq order.
func (ix *Index) Events() []Event { return ix.events }

// EventsIn returns the events with from < At <= to.
func (ix *Index) EventsIn(from, to time.Duration) []Event {
	end := after(ix.events, to)
	return ix.events[after(ix.events, from):end:end]
}

// Run returns the trace's Run event, or false if it has none.
func (ix *Index) Run() (Run, bool) {
	if ix.run == nil {
		return Run{}, false
	}
	return *ix.run, true
}

// StepSpan collects the phase times of one step. A phase the trace has not
// reached yet is zero, and Reached says which phases it has.
type StepSpan struct {
	Step         int
	Start        time.Duration
	EndFaults    time.Duration
	Deadline     time.Duration
	BackoffBound bool
	// WaitEnd is when the settle wait ended, by Settled or TimedOut.
	WaitEnd    time.Duration
	Settled    bool
	ConfirmEnd time.Duration
	Reached    map[PhaseName]bool
}

// Steps returns one span per step, in step order.
func (ix *Index) Steps() []StepSpan {
	spans := make([]StepSpan, 0, len(ix.steps))
	for _, span := range ix.steps {
		spans = append(spans, *span)
	}
	slices.SortFunc(spans, func(a, b StepSpan) int { return cmp.Compare(a.Step, b.Step) })
	return spans
}

// Step returns the span of step n, or false when the trace has no phase of it.
func (ix *Index) Step(n int) (StepSpan, bool) {
	span, found := ix.steps[n]
	if !found {
		return StepSpan{}, false
	}
	return *span, true
}

// Calls returns the Call events of caller with from < At <= to.
func (ix *Index) Calls(caller Caller, from, to time.Duration) []Event {
	calls := ix.calls[caller]
	end := after(calls, to)
	return calls[after(calls, from):end:end]
}

// after is the position of the first event later than t.
func after(events []Event, t time.Duration) int {
	return sort.Search(len(events), func(i int) bool { return events[i].At > t })
}

// Live returns the latest version at t of each object of group and resource
// that exists at t, ordered by Ref.
func (ix *Index) Live(group, resource string, t time.Duration) []Version {
	var live []Version
	for _, versions := range ix.versions[groupResource{group, resource}] {
		n := after(versions, t)
		if n == 0 || versions[n-1].Version.Deleted {
			continue
		}
		live = append(live, *versions[n-1].Version)
	}
	slices.SortFunc(live, func(a, b Version) int { return compareRefs(a.Ref, b.Ref) })
	return live
}

func compareRefs(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.System, b.System), cmp.Compare(a.Group, b.Group), cmp.Compare(a.Resource, b.Resource),
		cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
}

// Writer returns the caller whose response carried v's Ref and RV, or
// Cluster when none did.
func (ix *Index) Writer(v Version) Caller {
	if caller, found := ix.writer[objectRV{v.Ref, v.RV}]; found {
		return caller
	}
	return Cluster
}

// Changed reports whether e, a successful mutating call, produced a
// resourceVersion that no earlier version or response held for its object.
// A no-op write is not a change. A successful mutating call whose response
// carried no resourceVersion counts as a change.
//
// An earlier holder must precede the call's request, at At minus Latency,
// because the observer may see a write's own version before its response
// ends.
func (ix *Index) Changed(e Event) bool {
	c := e.Call
	if c == nil || !Mutating(c.Verb) || c.Status < 200 || c.Status > 299 {
		return false
	}
	if c.ResultRV == "" {
		return true
	}
	first, found := ix.firstRV[objectRV{c.Object(), c.ResultRV}]
	return !found || first.Seq >= e.Seq || first.At > e.At-c.Latency
}

// Mutating reports whether verb writes.
func Mutating(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}

// Up reports whether the controller runs at t, and since when.
func (ix *Index) Up(t time.Duration) (since time.Duration, up bool) {
	for _, e := range slices.Backward(ix.procs[:after(ix.procs, t)]) {
		switch e.Proc.Event {
		case Exit:
			return 0, false
		case Start:
			return e.At, true
		}
	}
	return 0, false
}

// Actions returns the Action events of step in Seq order.
func (ix *Index) Actions(step int) []Event { return ix.actions[step] }

// Procs returns every Proc event in Seq order.
func (ix *Index) Procs() []Event { return ix.procs }
