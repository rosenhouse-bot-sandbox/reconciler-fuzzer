package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Execute runs the plan's steps, then step len(p.Steps), which deletes every
// live CR, then step len(p.Steps)+1, which restarts the controller. It
// records Phase, Action and Fault events. It returns an error only when the harness itself fails; a
// controller that does not settle is a finding, not an error.
func (r *Runner) Execute(ctx context.Context, p *plan.Plan) error {
	e := &execution{Runner: r, live: map[string]trace.Ref{}, deleted: map[string]trace.Ref{}, ix: trace.NewIndex(nil)}
	for n, step := range p.Steps {
		err := e.step(ctx, n, step)
		if errors.Is(err, errSlotTaken) {
			break
		}
		if err != nil {
			return err
		}
	}
	n := len(p.Steps)
	var deleteAll plan.Step
	for _, slot := range slices.Sorted(maps.Keys(e.live)) {
		deleteAll.Actions = append(deleteAll.Actions, plan.Action{Do: plan.Delete, Slot: slot})
	}
	if err := e.step(ctx, n, deleteAll); err != nil {
		return err
	}
	return e.step(ctx, n+1, plan.Step{Actions: []plan.Action{{Do: plan.Restart}}})
}

// Quiet waits until the settle condition holds, or for at most the settle
// timeout, so that what the controller makes at startup precedes the first
// CR.
func (r *Runner) Quiet(ctx context.Context) error {
	ix := trace.NewIndex(nil)
	deadline := r.Recorder.Now() + r.Config.Timeouts.Settle
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		now := r.Recorder.Now()
		ix.Append(r.Recorder.EventsSince(ix.Len())...)
		if _, ok := r.FirstSettled(ix, 0, now); ok || now >= deadline {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// errSlotTaken says a create's slot still held its previous CR the settle
// timeout after the create's wait ended the step's faults. The step still
// settles, and then the plan skips to its appended steps.
var errSlotTaken = errors.New("the slot's previous CR is still there")

var errNoMatch = errors.New("no instance matches")

// Deadline is end faults + settle + min(how long the faults lasted,
// backoffCap) + the longest poll interval of the systems the step touched.
// backoffBound says that backoffCap was the smaller.
func Deadline(endFaults, faultsLasted, maxPoll time.Duration, t config.Timeouts) (deadline time.Duration, backoffBound bool) {
	return endFaults + t.Settle + min(faultsLasted, t.BackoffCap) + maxPoll, t.BackoffCap < faultsLasted
}

type execution struct {
	*Runner
	live map[string]trace.Ref
	// deleted holds the last CR deleted from each slot, which a later
	// create of the slot waits out.
	deleted map[string]trace.Ref
	ix      *trace.Index
}

func (e *execution) phase(step int, name trace.PhaseName) {
	e.Recorder.Record(trace.Event{Phase: &trace.Phase{Step: step, Name: name}})
}

func (e *execution) step(ctx context.Context, n int, step plan.Step) error {
	p := &period{execution: e, n: n, step: step, start: time.Now()}
	e.phase(n, trace.FaultPeriod)
	if err := e.Process.Supervise(false); err != nil {
		return err
	}
	err := e.faultPeriod(ctx, p)
	if err != nil && !errors.Is(err, errSlotTaken) {
		p.stopFaults()
		return err
	}
	taken := err
	if err := p.resume(); err != nil {
		return err
	}
	endFaults := e.Recorder.Now()
	deadline, backoffBound := Deadline(endFaults, p.faultsStopped.Sub(p.start), e.maxPoll(step), e.Config.Timeouts)
	e.Recorder.Record(trace.Event{Phase: &trace.Phase{Step: n, Name: trace.EndFaults, Deadline: deadline, BackoffBound: backoffBound}})
	if err := e.settle(ctx, n, deadline); err != nil {
		return err
	}
	return taken
}

// maxPoll is the longest poll interval of the systems the step's faults,
// damage and changeFixture actions name, and of every fake when the step
// changes a CR, since the controller then polls its fakes.
func (e *execution) maxPoll(step plan.Step) time.Duration {
	var systems []string
	for _, f := range step.Faults {
		systems = append(systems, f.System)
	}
	for _, a := range step.Actions {
		switch a.Do {
		case plan.Damage, plan.ChangeFixture:
			systems = append(systems, a.Target.System)
		case plan.Create, plan.Update, plan.Delete:
			for _, s := range e.Config.Fakes() {
				systems = append(systems, s.Name)
			}
		}
	}
	var longest time.Duration
	for _, name := range systems {
		longest = max(longest, e.Config.Poll(name))
	}
	return longest
}

// startedFault is a fault to end, and the model that took it.
type startedFault struct {
	end   func()
	model string
}

// period is step n's fault period, which the EndFaults phase ends. Its
// faults stop once: when a create still waits for its slot after Period, or
// else when the fault period ends.
type period struct {
	*execution
	n             int
	step          plan.Step
	start         time.Time
	started       []startedFault
	faultsStopped time.Time
}

// periodEnd is when Period passes. The fault period may last longer.
func (p *period) periodEnd() time.Time { return p.start.Add(time.Duration(p.step.Period)) }

// stopFaults ends the started faults, once.
func (p *period) stopFaults() {
	if !p.faultsStopped.IsZero() {
		return
	}
	p.faultsStopped = time.Now()
	for i, f := range p.started {
		f.end()
		p.Recorder.Record(faultEvent(p.n, i, p.step.Faults[i], f.model, true))
	}
}

// resume stops the faults and resumes supervision, which restarts a killed
// controller.
func (p *period) resume() error {
	p.stopFaults()
	return p.Process.Supervise(true)
}

// faultPeriod starts the step's faults, applies its actions and waits out
// its period.
func (e *execution) faultPeriod(ctx context.Context, p *period) error {
	for i, f := range p.step.Faults {
		target, model, err := e.target(f)
		if err != nil {
			return fmt.Errorf("steps[%d].faults[%d]: %w", p.n, i, err)
		}
		e.Recorder.Record(faultEvent(p.n, i, f, model, false))
		end, err := target.Fault(f)
		if err != nil {
			return fmt.Errorf("steps[%d].faults[%d]: %w", p.n, i, err)
		}
		p.started = append(p.started, startedFault{end, model})
	}
	for i, a := range p.step.Actions {
		if err := sleepUntil(ctx, p.start.Add(time.Duration(a.At))); err != nil {
			return err
		}
		ref, err := e.act(ctx, a, p)
		event := &trace.Action{Step: p.n, Index: i, Do: string(a.Do), Slot: a.Slot, Ref: ref}
		if err != nil {
			event.Error = err.Error()
		}
		e.Recorder.Record(trace.Event{Action: event})
		if err != nil && !errors.Is(err, errNoMatch) {
			return fmt.Errorf("steps[%d].actions[%d] %s: %w", p.n, i, a.Do, err)
		}
	}
	return sleepUntil(ctx, p.periodEnd())
}

// awaitGone waits for ref, a slot's previous CR, to go. The step's faults or
// a kill may hold it, so once Period passes, awaitGone ends them and waits
// at most the settle timeout more.
func (p *period) awaitGone(ctx context.Context, ref trace.Ref) error {
	if untilPeriodEnd := time.Until(p.periodEnd()); untilPeriodEnd > 0 {
		if timedOut, err := p.waitGone(ctx, ref, untilPeriodEnd); !timedOut {
			return err
		}
	} else if gone, err := p.Cluster.Gone(ctx, ref); gone || err != nil {
		return err
	}
	if err := p.resume(); err != nil {
		return err
	}
	if timedOut, err := p.waitGone(ctx, ref, p.Config.Timeouts.Settle); !timedOut {
		return err
	}
	return p.slotTaken(ref)
}

// waitGone waits up to timeout for ref to go.
func (e *execution) waitGone(ctx context.Context, ref trace.Ref, timeout time.Duration) (timedOut bool, err error) {
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = e.Cluster.WaitGone(wait, ref)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return true, nil
	}
	return false, err
}

// slotTaken names what holds ref, the slot's previous CR: the
// deletionTimestamp and finalizers of the last version the observer
// recorded, unless that version is a deletion.
func (e *execution) slotTaken(ref trace.Ref) error {
	taken := fmt.Errorf("%w after the %v settle timeout", errSlotTaken, e.Config.Timeouts.Settle)
	e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
	var cr struct {
		Metadata struct {
			DeletionTimestamp string   `json:"deletionTimestamp"`
			Finalizers        []string `json:"finalizers"`
		} `json:"metadata"`
	}
	v, found := e.ix.Latest(ref)
	if !found || v.Deleted || json.Unmarshal(v.Body, &cr) != nil {
		return taken
	}
	return fmt.Errorf("%w: deletionTimestamp %s, finalizers %v", taken, cmp.Or(cr.Metadata.DeletionTimestamp, "none"), cr.Metadata.Finalizers)
}

// faultEvent records a fault's start or end. A holdWatch's names its kind.
func faultEvent(n, i int, f plan.Fault, model string, ended bool) trace.Event {
	event := &trace.Fault{Step: n, Index: i, Ended: ended, What: f.String(), System: f.System, Model: model}
	if f.HoldWatch {
		event.Hold = &trace.Ref{System: kube.Name, Group: f.Match.Group, Resource: f.Match.Resource}
	}
	return trace.Event{Fault: event}
}

// target returns the first model that takes a model fault, or the
// controller listener of a request fault's system.
func (e *execution) target(f plan.Fault) (target FaultTarget, model string, err error) {
	if f.ModelFault() {
		for _, m := range e.Models {
			if m.Takes(f) {
				return m, m.Name(), nil
			}
		}
		return nil, "", fmt.Errorf("no model takes %s", f)
	}
	system := f.System
	if system == "" {
		system = kube.Name
	}
	target, known := e.Listeners[system]
	if !known {
		return nil, "", fmt.Errorf("unknown system %q", system)
	}
	return target, "", nil
}

// act applies one action in period p and returns the ref of the object it
// touched.
func (e *execution) act(ctx context.Context, a plan.Action, p *period) (*trace.Ref, error) {
	switch a.Do {
	case plan.Create:
		obj, err := e.object(a)
		if err != nil {
			return nil, err
		}
		if previous, found := e.deleted[a.Slot]; found {
			if err := p.awaitGone(ctx, previous); err != nil {
				return nil, err
			}
			delete(e.deleted, a.Slot)
		}
		ref, err := e.Cluster.Create(ctx, obj)
		if err != nil {
			return nil, err
		}
		e.live[a.Slot] = ref
		return &ref, nil
	case plan.Update:
		ref, err := e.slot(a)
		if err != nil {
			return nil, err
		}
		return &ref, e.Cluster.Patch(ctx, ref, a.Patch)
	case plan.Delete:
		ref, err := e.slot(a)
		if err != nil {
			return nil, err
		}
		delete(e.live, a.Slot)
		e.deleted[a.Slot] = ref
		if err := e.Cluster.Delete(ctx, ref, a.Propagation); !errors.Is(err, system.ErrGone) {
			return &ref, err
		}
		return &ref, nil
	case plan.Restart:
		if err := e.Process.Stop(ctx); err != nil {
			return nil, err
		}
		return nil, e.Process.Start(ctx)
	case plan.Kill:
		return nil, e.Process.Kill(ctx)
	case plan.Damage, plan.ChangeFixture:
		creator := trace.Controller
		if a.Do == plan.ChangeFixture {
			creator = trace.Harness
		}
		e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
		ref, found := e.Select(e.ix, creator, *a.Target, a.Slot, e.Recorder.Now())
		if !found {
			return nil, errNoMatch
		}
		actor, known := e.Actors[ref.System]
		if !known {
			return nil, fmt.Errorf("no system %q takes %s", ref.System, a.Do)
		}
		if a.Patch != nil {
			return &ref, actor.Patch(ctx, ref, a.Patch)
		}
		err := actor.Delete(ctx, ref, "")
		if errors.Is(err, system.ErrGone) {
			return nil, fmt.Errorf("%w: %w", errNoMatch, err)
		}
		return &ref, err
	}
	return nil, fmt.Errorf("unknown do %q", a.Do)
}

func (e *execution) object(a plan.Action) (map[string]any, error) {
	if a.Object == nil {
		return runtime.DeepCopyJSON(e.Sample), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(a.Object, &obj); err != nil {
		return nil, fmt.Errorf("decoding object: %w", err)
	}
	return obj, nil
}

func (e *execution) slot(a plan.Action) (trace.Ref, error) {
	ref, found := e.live[a.Slot]
	if !found {
		return trace.Ref{}, fmt.Errorf("slot %q holds no CR", a.Slot)
	}
	return ref, nil
}

// settle waits until the settle condition holds or the deadline passes. Each
// poll searches the time since the last poll, so it finds the settle time
// Converge finds.
func (e *execution) settle(ctx context.Context, n int, deadline time.Duration) error {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	var from time.Duration
	for first := true; ; first = false {
		now := e.Recorder.Now()
		e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
		if first {
			span, _ := e.ix.Step(n)
			from = span.EndFaults
		}
		if err := e.failed(); err != nil {
			return err
		}
		if _, ok := e.FirstSettled(e.ix, from, min(now, deadline)); ok {
			e.phase(n, trace.Settled)
			if err := sleepUntil(ctx, time.Now().Add(e.Config.Timeouts.Stable)); err != nil {
				return err
			}
			e.phase(n, trace.Confirmed)
			return nil
		}
		if now >= deadline {
			e.phase(n, trace.TimedOut)
			return nil
		}
		from = now
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func sleepUntil(ctx context.Context, t time.Time) error {
	timer := time.NewTimer(time.Until(t))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
