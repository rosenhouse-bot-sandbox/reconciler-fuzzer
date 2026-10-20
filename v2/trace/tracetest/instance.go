package tracetest

import (
	"cmp"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Object is a body named name.
func Object(name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}}
}

// Generated is a body that the server named name from prefix.
func Generated(prefix, name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "generateName": prefix}}
}

// OwnedBy adds an ownerReference to the owner with uid, and returns body.
func OwnedBy(body map[string]any, uid string, controller bool) map[string]any {
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		body["metadata"] = metadata
	}
	refs, _ := metadata["ownerReferences"].([]any)
	metadata["ownerReferences"] = append(refs, map[string]any{"uid": uid, "controller": controller})
	return body
}

// Create is a successful create by caller of ref, to which the server gave
// uid and rv.
func Create(caller trace.Caller, ref trace.Ref, uid, rv string) trace.Event {
	collection := ref
	collection.Name = ""
	return trace.Event{Call: &trace.Call{Caller: caller, Verb: "create", Ref: collection, Status: 201,
		ResultUID: uid, ResultRV: rv, ResultName: ref.Name}}
}

// SlotAction is a harness action of step on the CR of slot.
func SlotAction(step int, do, slot string, ref trace.Ref) trace.Event {
	e := Action(step, do, ref)
	e.Action.Slot = slot
	return e
}

// Timeline collects events at any time and orders them by time, keeping the
// order of events added at one time.
type Timeline struct{ events []trace.Event }

// At adds events at t.
func (tl *Timeline) At(t time.Duration, events ...trace.Event) *Timeline {
	for _, e := range events {
		e.At = t
		tl.events = append(tl.events, e)
	}
	return tl
}

// Events returns the events in time order, numbered from 1.
func (tl *Timeline) Events() []trace.Event {
	events := slices.Clone(tl.events)
	slices.SortStableFunc(events, func(a, b trace.Event) int { return cmp.Compare(a.At, b.At) })
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	return events
}

// Index indexes the events in time order.
func (tl *Timeline) Index() *trace.Index { return trace.NewIndex(tl.Events()) }
