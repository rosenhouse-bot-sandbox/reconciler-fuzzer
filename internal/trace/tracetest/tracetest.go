// Package tracetest builds traces for tests of code that reads them.
package tracetest

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Builder appends events with increasing Seq at a clock the test moves.
type Builder struct {
	now    time.Duration
	events []trace.Event
}

// At moves the clock to t.
func (b *Builder) At(t time.Duration) *Builder {
	b.now = t
	return b
}

// Add appends each event at the current time.
func (b *Builder) Add(events ...trace.Event) *Builder {
	for _, e := range events {
		e.Seq = uint64(len(b.events) + 1)
		e.At = b.now
		b.events = append(b.events, e)
	}
	return b
}

// Events returns what the builder holds.
func (b *Builder) Events() []trace.Event { return b.events }

// Index indexes what the builder holds.
func (b *Builder) Index() *trace.Index { return trace.NewIndex(b.events) }

// Ref names a kube object. Resource is "group/resource", or "resource" for
// the core group.
func Ref(resource, namespace, name string) trace.Ref {
	group, plural, found := strings.Cut(resource, "/")
	if !found {
		group, plural = "", resource
	}
	return trace.Ref{System: "kube", Group: group, Resource: plural, Namespace: namespace, Name: name}
}

// Run is the first event of a trace whose one primaries entry is the kind of
// group and resource.
func Run(group, resource string) trace.Event {
	return trace.Event{Run: &trace.Run{Namespace: "ns", Primaries: []trace.Primary{{Kind: trace.Kind{Group: group, Resource: resource}}}}}
}

// Call is a request by caller on ref.
func Call(caller trace.Caller, verb string, ref trace.Ref, status int, resultRV string) trace.Event {
	return trace.Event{Call: &trace.Call{Caller: caller, Verb: verb, Ref: ref, Status: status, ResultRV: resultRV}}
}

// Version is an observed state of ref.
func Version(ref trace.Ref, uid, rv string, body map[string]any) trace.Event {
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return trace.Event{Version: &trace.Version{Ref: ref, UID: uid, RV: rv, Body: raw}}
}

// Deleted is the observed delete of ref.
func Deleted(ref trace.Ref, uid, rv string, body map[string]any) trace.Event {
	e := Version(ref, uid, rv, body)
	e.Version.Deleted = true
	return e
}

// Start is a controller start.
func Start(pid int) trace.Event {
	return trace.Event{Proc: &trace.Proc{Event: trace.Start, PID: pid}}
}

// Exit is a controller exit.
func Exit(status string, harnessCaused bool, tail string) trace.Event {
	return trace.Event{Proc: &trace.Proc{Event: trace.Exit, Exit: status, HarnessCaused: harnessCaused, StderrTail: tail}}
}

// Phase marks that step entered phase name.
func Phase(step int, name trace.PhaseName) trace.Event {
	return trace.Event{Phase: &trace.Phase{Step: step, Name: name}}
}

// FaultPeriod starts the fault period of step, whose period is period.
func FaultPeriod(step int, period time.Duration) trace.Event {
	return trace.Event{Phase: &trace.Phase{Step: step, Name: trace.FaultPeriod, Period: period}}
}

// EndFaults marks the end of step's fault period.
func EndFaults(step int, deadline time.Duration, backoffBound bool) trace.Event {
	return trace.Event{Phase: &trace.Phase{Step: step, Name: trace.EndFaults, Deadline: deadline, BackoffBound: backoffBound}}
}

// Action is a harness action of step on ref.
func Action(step int, do string, ref trace.Ref) trace.Event {
	return trace.Event{Action: &trace.Action{Step: step, Do: do, Ref: &ref}}
}

// Seen is a response that showed caller version rv of ref, whose uid is uid.
func Seen(caller trace.Caller, ref trace.Ref, uid, rv string) trace.Event {
	return trace.Event{Seen: &trace.Seen{Caller: caller, Ref: ref, UID: uid, RV: rv}}
}

// ReadyCondition is a body named name of generation whose Ready condition
// is True or False for that generation.
func ReadyCondition(name string, generation int, ready bool) map[string]any {
	status := "False"
	if ready {
		status = "True"
	}
	return map[string]any{
		"metadata": map[string]any{"name": name, "generation": generation},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": status, "observedGeneration": generation}}},
	}
}

// Fault is the start, or with ended the end, of fault index of step.
func Fault(step, index int, ended bool, what string) trace.Event {
	return trace.Event{Fault: &trace.Fault{Step: step, Index: index, Ended: ended, What: what}}
}

// ModelFault is Fault taken by model.
func ModelFault(step, index int, ended bool, what, model string) trace.Event {
	e := Fault(step, index, ended, what)
	e.Fault.Model = model
	return e
}

// HoldFault is Fault of a holdWatch on the kind of hold.
func HoldFault(step, index int, ended bool, hold trace.Ref) trace.Event {
	e := Fault(step, index, ended, "holdWatch")
	e.Fault.Hold = &hold
	return e
}
