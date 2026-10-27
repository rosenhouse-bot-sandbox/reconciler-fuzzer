package trace

import (
	"cmp"
	"math"
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
	// stop is the first stop action that did not fail.
	stop *Event
	// versions holds each object's Version events by kind: a Ref with no
	// namespace or name.
	versions map[Ref]map[Ref][]Event
	// firstRV is the first event, a version or a response, to hold each
	// object's resourceVersion.
	firstRV map[objectRV]Event
	// answers holds the calls whose responses held each resourceVersion.
	answers map[objectRV][]Event
	// byRV holds each object's Version event of each resourceVersion.
	byRV map[objectRV]Event
	// seen holds each caller's Seen events by UID, and opaque its opaque
	// ones.
	seen   map[Caller]map[string][]Event
	opaque map[Caller][]Event
	// watches holds each caller's watch calls; watchEnds the time each
	// stream ended.
	watches   map[Caller][]Event
	watchEnds map[WatchEnd]time.Duration
	// work holds each system's Work events.
	work map[string][]Event
	// holds holds the holdWatch faults, by start.
	holds     []Hold
	holdStart map[[2]int]int
}

// Hold is one holdWatch fault's span. End is the largest Duration while the
// hold lasts.
type Hold struct {
	Kind       Ref
	Step       int
	Index      int
	What       string
	Start, End time.Duration
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
		versions: map[Ref]map[Ref][]Event{},
		firstRV:  map[objectRV]Event{},
		answers:  map[objectRV][]Event{},
		byRV:     map[objectRV]Event{},
		seen:     map[Caller]map[string][]Event{},
		opaque:   map[Caller][]Event{},

		watches:   map[Caller][]Event{},
		watchEnds: map[WatchEnd]time.Duration{},
		work:      map[string][]Event{},
		holdStart: map[[2]int]int{},
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
			ix.addAction(e)
		case e.WatchEnd != nil:
			ix.watchEnds[*e.WatchEnd] = e.At
		case e.Work != nil:
			ix.work[e.Work.System] = append(ix.work[e.Work.System], e)
		case e.Fault != nil && e.Fault.Hold != nil:
			ix.addHold(e)
		case e.Seen != nil && e.Seen.Opaque:
			ix.opaque[e.Seen.Caller] = append(ix.opaque[e.Seen.Caller], e)
		case e.Seen != nil:
			if ix.seen[e.Seen.Caller] == nil {
				ix.seen[e.Seen.Caller] = map[string][]Event{}
			}
			ix.seen[e.Seen.Caller][e.Seen.UID] = append(ix.seen[e.Seen.Caller][e.Seen.UID], e)
		}
	}
}

func (ix *Index) addHold(e Event) {
	f := e.Fault
	key := [2]int{f.Step, f.Index}
	if !f.Ended {
		ix.holdStart[key] = len(ix.holds)
		ix.holds = append(ix.holds, Hold{Kind: *f.Hold, Step: f.Step, Index: f.Index, What: f.What, Start: e.At, End: math.MaxInt64})
	} else if n, found := ix.holdStart[key]; found {
		ix.holds[n].End = e.At
	}
}

// Holds returns every holdWatch span, by start.
func (ix *Index) Holds() []Hold { return ix.holds }

// Released is the end of the latest holdWatch on ref's kind that overlaps
// [from, t], when its held watch events reach the controller. holding says
// a hold is still on at t.
func (ix *Index) Released(ref Ref, from, t time.Duration) (end time.Duration, holding, found bool) {
	for _, h := range ix.holds {
		if h.Kind.Group != ref.Group || h.Kind.Resource != ref.Resource || h.Kind.System != ref.System || h.Start > t || h.End < from {
			continue
		}
		if h.End > t {
			return h.End, true, true
		}
		end, found = max(end, h.End), true
	}
	return end, false, found
}

// WatchedKind reports whether caller had a successful watch of kind's group
// and resource open at some time in [from, to].
func (ix *Index) WatchedKind(caller Caller, kind Ref, from, to time.Duration) bool {
	watches := ix.watches[caller]
	return slices.ContainsFunc(watches[:after(watches, to)], func(e Event) bool {
		c := e.Call
		if c.Ref.System != kind.System || c.Ref.Group != kind.Group || c.Ref.Resource != kind.Resource || c.Status < 200 || c.Status > 299 {
			return false
		}
		end, ended := ix.watchEnds[WatchEnd{Caller: caller, System: c.Ref.System, Stream: c.Stream}]
		return !ended || end >= from
	})
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
	if c.Watch {
		ix.watches[c.Caller] = append(ix.watches[c.Caller], e)
	}
	if c.ResultRV == "" {
		return
	}
	key := objectRV{c.Object(), c.ResultRV}
	if _, seen := ix.firstRV[key]; !seen {
		ix.firstRV[key] = e
	}
	ix.answers[key] = append(ix.answers[key], e)
}

func (ix *Index) addVersion(e Event) {
	v := e.Version
	kind := Ref{System: v.Ref.System, Group: v.Ref.Group, Resource: v.Ref.Resource}
	if ix.versions[kind] == nil {
		ix.versions[kind] = map[Ref][]Event{}
	}
	ix.versions[kind][v.Ref] = append(ix.versions[kind][v.Ref], e)
	key := objectRV{v.Ref, v.RV}
	if _, seen := ix.firstRV[key]; !seen {
		ix.firstRV[key] = e
	}
	if _, seen := ix.byRV[key]; !seen {
		ix.byRV[key] = e
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

// Covers reports whether a request on r reaches o: r names o's kind, and
// o's namespace or none, and o's name or none.
func (r Ref) Covers(o Ref) bool {
	return r.System == o.System && r.Group == o.Group && r.Resource == o.Resource &&
		(r.Namespace == "" || r.Namespace == o.Namespace) && (r.Name == "" || r.Name == o.Name)
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

// Live returns the latest version at t of each kube object of group and
// resource that exists at t, ordered by Ref.
func (ix *Index) Live(group, resource string, t time.Duration) []Version {
	var live []Version
	for _, versions := range ix.versions[Ref{System: "kube", Group: group, Resource: resource}] {
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
	if e, found := ix.WriteCall(v.Ref, v.RV); found {
		return e.Call.Caller
	}
	return Cluster
}

// WriteCall is the first call whose response carried ref and rv.
func (ix *Index) WriteCall(ref Ref, rv string) (Event, bool) {
	answers := ix.Answers(ref, rv)
	if len(answers) == 0 {
		return Event{}, false
	}
	return answers[0], true
}

// Answers are the calls whose responses carried ref and rv, in Seq order.
func (ix *Index) Answers(ref Ref, rv string) []Event { return ix.answers[objectRV{ref, rv}] }

// VersionOf is the version of ref with rv.
func (ix *Index) VersionOf(ref Ref, rv string) (Version, bool) {
	e, found := ix.byRV[objectRV{ref, rv}]
	if !found {
		return Version{}, false
	}
	return *e.Version, true
}

// Latest is the last version of ref that the observer recorded.
func (ix *Index) Latest(ref Ref) (Version, bool) {
	versions := ix.versions[Ref{System: ref.System, Group: ref.Group, Resource: ref.Resource}][ref]
	if len(versions) == 0 {
		return Version{}, false
	}
	return *versions[len(versions)-1].Version, true
}

// Previous is the version of ref, of the same instance, that the observer
// recorded just before ref's version rv.
func (ix *Index) Previous(ref Ref, rv string) (Version, bool) {
	versions := ix.versions[Ref{System: ref.System, Group: ref.Group, Resource: ref.Resource}][ref]
	n := slices.IndexFunc(versions, func(e Event) bool { return e.Version.RV == rv })
	if n <= 0 || versions[n-1].Version.UID != versions[n].Version.UID {
		return Version{}, false
	}
	return *versions[n-1].Version, true
}

// Later reports whether ref's version rv came at or after its version
// base, by the observer's order. found is false when the trace lacks either.
func (ix *Index) Later(ref Ref, rv, base string) (later, found bool) {
	v, foundV := ix.byRV[objectRV{ref, rv}]
	b, foundB := ix.byRV[objectRV{ref, base}]
	if !foundV || !foundB {
		return false, false
	}
	return v.Seq >= b.Seq, true
}

// Seen returns the Seen events by caller of the object with uid, in Seq
// order.
func (ix *Index) Seen(caller Caller, uid string) []Event { return ix.seen[caller][uid] }

// Opaque reports whether a response that caller's listener could not
// decode may have shown caller ref in (from, to]: a get or list that ended
// then, or a watch open then.
func (ix *Index) Opaque(caller Caller, ref Ref, from, to time.Duration) bool {
	opaque := ix.opaque[caller]
	return slices.ContainsFunc(opaque[:after(opaque, to)], func(e Event) bool {
		s := e.Seen
		if !s.Ref.Covers(ref) {
			return false
		}
		if s.Stream == 0 {
			return e.At > from
		}
		end, ended := ix.watchEnds[WatchEnd{Caller: caller, System: s.Ref.System, Stream: s.Stream}]
		return !ended || end > from
	})
}

// Changed reports whether e, a successful mutating call, answered with a
// resourceVersion that no earlier version or response held for its object.
// A no-op write is not a change. A kube deletecollection's object is its
// collection. It answers with the resourceVersion of its list. By default,
// a list reads the latest resourceVersion, which repeats only while nothing
// in the cluster changes. So a repeat counts as no change. The rule assumes
// that the repeat has the earlier call's selector and options. A kube write
// whose response carried no resourceVersion counts as a change. A fake's
// such write changed its object when the observer recorded, within
// ObserveLag of the response, a version that differs from the last before
// the request, or when the trace ends within ObserveLag of the response.
//
// An earlier holder must precede the call's request, at At minus Latency,
// because the observer may see a write's own version before its response
// ends.
func (ix *Index) Changed(e Event) bool {
	c := e.Call
	if c == nil || !Mutating(c.Verb) || c.Status < 200 || c.Status > 299 {
		return false
	}
	start := e.At - c.Latency
	if c.ResultRV == "" && c.Ref.System != "kube" && c.Ref.System != "" {
		return ix.observedChange(c.Object(), start, e.At)
	}
	if c.ResultRV == "" {
		return true
	}
	first, found := ix.firstRV[objectRV{c.Object(), c.ResultRV}]
	return !found || first.Seq >= e.Seq || first.At > start
}

// ObserveLag bounds how long a fake's observer takes to record a write.
const ObserveLag = 200 * time.Millisecond

func (ix *Index) observedChange(ref Ref, start, end time.Duration) bool {
	if len(ix.events) == 0 || ix.events[len(ix.events)-1].At < end+ObserveLag {
		return true
	}
	versions := ix.versions[Ref{System: ref.System, Group: ref.Group, Resource: ref.Resource}][ref]
	n := after(versions, start)
	if n == len(versions) || versions[n].At > end+ObserveLag {
		return false
	}
	next := versions[n].Version
	if n == 0 {
		return !next.Deleted
	}
	last := versions[n-1].Version
	return next.RV != last.RV || next.Deleted != last.Deleted || next.UID != last.UID
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

func (ix *Index) addAction(e Event) {
	a := e.Action
	ix.actions[a.Step] = append(ix.actions[a.Step], e)
	if a.Do == "stop" && a.Error == "" && ix.stop == nil {
		ix.stop = &e
	}
}

// Actions returns the Action events of step in Seq order.
func (ix *Index) Actions(step int) []Event { return ix.actions[step] }

// Stop returns the first stop action that did not fail, when it came by t.
func (ix *Index) Stop(t time.Duration) (Event, bool) {
	if ix.stop == nil || ix.stop.At > t {
		return Event{}, false
	}
	return *ix.stop, true
}

// StopWaitedFor reports whether the stop waited for the CR with uid.
func (ix *Index) StopWaitedFor(uid string) bool {
	return ix.stop != nil && slices.Contains(ix.stop.Action.Waited, uid)
}

// Procs returns every Proc event in Seq order.
func (ix *Index) Procs() []Event { return ix.procs }

// Watching reports whether a successful watch by caller that covers ref
// opened at or before since and is still open at t.
func (ix *Index) Watching(caller Caller, ref Ref, since, t time.Duration) bool {
	watches := ix.watches[caller]
	for _, e := range watches[:after(watches, since)] {
		c := e.Call
		if !c.Ref.Covers(ref) || c.Status < 200 || c.Status > 299 {
			continue
		}
		if end, ended := ix.watchEnds[WatchEnd{Caller: caller, System: c.Ref.System, Stream: c.Stream}]; !ended || end > t {
			return true
		}
	}
	return false
}

// Pending returns, in name order, the systems whose latest Work event at t
// says they have work pending.
func (ix *Index) Pending(t time.Duration) []string {
	var pending []string
	for system, work := range ix.work {
		if n := after(work, t); n > 0 && work[n-1].Work.Pending {
			pending = append(pending, system)
		}
	}
	slices.Sort(pending)
	return pending
}
