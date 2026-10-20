// Package tracetest builds traces for tests of code that reads them.
package tracetest

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
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

// Run is the first event of a trace whose primary kind is group and resource.
func Run(group, resource string) trace.Event {
	return trace.Event{Run: &trace.Run{Namespace: "ns", Primary: trace.Ref{System: "kube", Group: group, Resource: resource}}}
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

// EndFaults marks the end of step's fault period.
func EndFaults(step int, deadline time.Duration, backoffBound bool) trace.Event {
	return trace.Event{Phase: &trace.Phase{Step: step, Name: trace.EndFaults, Deadline: deadline, BackoffBound: backoffBound}}
}

// Action is a harness action of step on ref.
func Action(step int, do string, ref trace.Ref) trace.Event {
	return trace.Event{Action: &trace.Action{Step: step, Do: do, Ref: &ref}}
}
