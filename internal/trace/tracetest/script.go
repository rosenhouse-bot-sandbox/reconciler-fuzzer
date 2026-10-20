package tracetest

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Script builds the trace of a run step by step on a clock that each call
// moves on by Tick. The controller starts at 0 and lists the primary kind.
type Script struct {
	Timeline
	// Spec is the trace's run event. A CR is ready when status.ready is
	// true.
	Spec   *trace.Run
	Now    time.Duration
	Step   int
	n      int
	uids   map[trace.Ref]string
	bodies map[trace.Ref]map[string]any
	index  map[int]int
}

// Tick is how far each call moves the clock.
const Tick = 10 * time.Millisecond

// NewScript starts a run of a plan of planSteps steps on primary.
func NewScript(primary trace.Ref, planSteps int, stable time.Duration) *Script {
	primary.Namespace, primary.Name = "", ""
	r := &Script{Step: -1, uids: map[trace.Ref]string{}, bodies: map[trace.Ref]map[string]any{}, index: map[int]int{}}
	r.Spec = &trace.Run{Namespace: "ns", Primary: primary, PlanSteps: planSteps,
		Ready: "has(self.status) && self.status.ready == true", Stable: stable, Reads: 2, Writes: 4}
	r.At(0, trace.Event{Run: r.Spec}, Start(1))
	primary.Namespace = "ns"
	r.Call(trace.Controller, "list", primary, 200)
	return r
}

// Tick moves the clock on by d.
func (r *Script) Tick(d time.Duration) *Script {
	r.Now += d
	return r
}

// Add adds events now and moves the clock on by Tick.
func (r *Script) Add(events ...trace.Event) *Script {
	r.At(r.Now, events...)
	r.Now += Tick
	return r
}

func (r *Script) next() string {
	r.n++
	return fmt.Sprint(r.n)
}

// Begin starts the next step's fault period.
func (r *Script) Begin() *Script {
	r.Step++
	return r.Add(Phase(r.Step, trace.FaultPeriod))
}

// EndFaults ends the step's faults, with a deadline 30s on.
func (r *Script) EndFaults() *Script {
	return r.Add(EndFaults(r.Step, r.Now+30*time.Second, false))
}

// Settle marks the step settled one stable from now and confirmed one
// stable later, and moves the clock past both.
func (r *Script) Settle() *Script {
	r.Now += r.Spec.Stable
	r.Add(Phase(r.Step, trace.Settled))
	r.Now += r.Spec.Stable - Tick
	return r.Add(Phase(r.Step, trace.Confirmed))
}

// TimeOut marks the step timed out at its deadline.
func (r *Script) TimeOut() *Script {
	r.Now += 30 * time.Second
	return r.Add(Phase(r.Step, trace.TimedOut))
}

// Act records a harness action of the step.
func (r *Script) Act(do, slot string, ref *trace.Ref) *Script {
	e := trace.Event{Action: &trace.Action{Step: r.Step, Index: r.index[r.Step], Do: do, Slot: slot, Ref: ref}}
	r.index[r.Step]++
	return r.Add(e)
}

// Create has caller create ref with body, and returns its uid.
func (r *Script) Create(caller trace.Caller, ref trace.Ref, body map[string]any) string {
	uid, rv := "u"+r.next(), r.next()
	r.uids[ref], r.bodies[ref] = uid, body
	r.Add(Create(caller, ref, uid, rv), Version(ref, uid, rv, body))
	return uid
}

// CreateInFlight has caller send a create of ref now, whose response ends
// latency later, as inFlight records it.
func (r *Script) CreateInFlight(caller trace.Caller, ref trace.Ref, body map[string]any, latency time.Duration) string {
	uid, rv := "u"+r.next(), r.next()
	r.uids[ref], r.bodies[ref] = uid, body
	r.inFlight(Create(caller, ref, uid, rv), Version(ref, uid, rv, body), latency)
	return uid
}

// UpdateInFlight has caller send an update of ref now, whose response ends
// latency later, as inFlight records it.
func (r *Script) UpdateInFlight(caller trace.Caller, ref trace.Ref, body map[string]any, latency time.Duration) *Script {
	rv := r.next()
	r.bodies[ref] = body
	r.inFlight(Call(caller, "update", ref, 200, rv), Version(ref, r.uids[ref], rv, body), latency)
	return r
}

// inFlight records a call sent now, whose version the observer records
// latency/2 later and whose response ends latency later.
func (r *Script) inFlight(call, version trace.Event, latency time.Duration) {
	call.Call.Latency = latency
	r.At(r.Now+latency/2, version)
	r.At(r.Now+latency, call)
	r.Now += Tick
}

// Update has caller write body to ref.
func (r *Script) Update(caller trace.Caller, ref trace.Ref, body map[string]any) *Script {
	rv := r.next()
	r.bodies[ref] = body
	return r.Add(Call(caller, "update", ref, 200, rv), Version(ref, r.uids[ref], rv, body))
}

// NoOp has caller write ref without changing it.
func (r *Script) NoOp(caller trace.Caller, ref trace.Ref) *Script {
	return r.Add(Call(caller, "update", ref, 200, r.lastRV(ref)))
}

func (r *Script) lastRV(ref trace.Ref) string {
	for i := len(r.events) - 1; i >= 0; i-- {
		if v := r.events[i].Version; v != nil && v.Ref == ref {
			return v.RV
		}
	}
	return ""
}

// Delete has caller delete ref.
func (r *Script) Delete(caller trace.Caller, ref trace.Ref) *Script {
	rv := r.next()
	return r.Add(Call(caller, "delete", ref, 200, rv), Deleted(ref, r.uids[ref], rv, r.bodies[ref]))
}

// Call has caller make a request on ref that changes nothing.
func (r *Script) Call(caller trace.Caller, verb string, ref trace.Ref, status int) *Script {
	return r.Add(Call(caller, verb, ref, status, ""))
}

// UID is the uid of ref's latest instance.
func (r *Script) UID(ref trace.Ref) string { return r.uids[ref] }

// CreateCR has the harness create a CR in slot that is not ready yet.
func (r *Script) CreateCR(slot string, ref trace.Ref) string {
	uid := r.Create(trace.Harness, ref, CR(ref.Name, false))
	r.Act("create", slot, &ref)
	return uid
}

// Ready has the controller make the CR at ref ready.
func (r *Script) Ready(ref trace.Ref) *Script {
	return r.Update(trace.Controller, ref, CR(ref.Name, true))
}

// DeleteCR has the harness delete the CR in slot.
func (r *Script) DeleteCR(slot string, ref trace.Ref) *Script {
	return r.Delete(trace.Harness, ref).Act("delete", slot, &ref)
}

// Restart has the harness restart the controller, which lists the primary
// kind again.
func (r *Script) Restart() *Script {
	r.n++
	primary := r.Spec.Primary
	primary.Namespace = "ns"
	return r.Add(Exit("signal: terminated", true, ""), Start(r.n)).Act("restart", "", nil).
		Call(trace.Controller, "list", primary, 200)
}

// CR is a body named name whose status.ready is ready.
func CR(name string, ready bool) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"ready": ready}}
}

// JSONL is the events as trace.jsonl holds them.
func (tl *Timeline) JSONL() []byte {
	var out []byte
	for _, e := range tl.Events() {
		line, err := json.Marshal(e)
		if err != nil {
			panic(err)
		}
		out = append(append(out, line...), '\n')
	}
	return out
}
