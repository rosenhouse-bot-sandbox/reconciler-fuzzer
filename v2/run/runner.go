package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
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

// errSlotTaken says a create waited the settle timeout for its slot's
// previous CR to go. The step still settles, and then the plan skips to its
// appended steps.
var errSlotTaken = errors.New("the slot's previous CR is still there")

// Deadline is end faults + settle + min(fault period, backoffCap) + the
// longest poll interval of the systems the step touched. backoffBound says
// that backoffCap was the smaller.
func Deadline(endFaults, faultPeriod, maxPoll time.Duration, t config.Timeouts) (deadline time.Duration, backoffBound bool) {
	return endFaults + t.Settle + min(faultPeriod, t.BackoffCap) + maxPoll, t.BackoffCap < faultPeriod
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
	start := time.Now()
	e.phase(n, trace.FaultPeriod)
	if err := e.Process.Supervise(false); err != nil {
		return err
	}
	ends, err := e.faultPeriod(ctx, n, step, start)
	for i, end := range ends {
		end()
		e.Recorder.Record(trace.Event{Fault: &trace.Fault{Step: n, Index: i, Ended: true, What: step.Faults[i].String()}})
	}
	if err != nil && !errors.Is(err, errSlotTaken) {
		return err
	}
	taken := err
	if err := e.Process.Supervise(true); err != nil {
		return err
	}
	endFaults := e.Recorder.Now()
	deadline, backoffBound := Deadline(endFaults, time.Since(start), 0, e.Config.Timeouts)
	e.Recorder.Record(trace.Event{Phase: &trace.Phase{Step: n, Name: trace.EndFaults, Deadline: deadline, BackoffBound: backoffBound}})
	if err := e.settle(ctx, n, deadline); err != nil {
		return err
	}
	return taken
}

// faultPeriod starts the step's faults, applies its actions and waits out
// its period. It returns the end of every fault it started.
func (e *execution) faultPeriod(ctx context.Context, n int, step plan.Step, start time.Time) ([]func(), error) {
	var ends []func()
	for i, f := range step.Faults {
		system := f.System
		if system == "" {
			system = "kube"
		}
		target, known := e.Listeners[system]
		if !known {
			return ends, fmt.Errorf("steps[%d].faults[%d]: unknown system %q", n, i, system)
		}
		end, err := target.Fault(f)
		if err != nil {
			return ends, fmt.Errorf("steps[%d].faults[%d]: %w", n, i, err)
		}
		ends = append(ends, end)
		e.Recorder.Record(trace.Event{Fault: &trace.Fault{Step: n, Index: i, What: f.String()}})
	}
	for i, a := range step.Actions {
		if err := sleepUntil(ctx, start.Add(time.Duration(a.At))); err != nil {
			return ends, err
		}
		ref, err := e.act(ctx, a)
		event := &trace.Action{Step: n, Index: i, Do: string(a.Do), Slot: a.Slot, Ref: ref}
		if err != nil {
			event.Error = err.Error()
		}
		e.Recorder.Record(trace.Event{Action: event})
		if err != nil {
			return ends, fmt.Errorf("steps[%d].actions[%d] %s: %w", n, i, a.Do, err)
		}
	}
	return ends, sleepUntil(ctx, start.Add(time.Duration(step.Period)))
}

// act applies one action and returns the ref of the CR it touched.
func (e *execution) act(ctx context.Context, a plan.Action) (*trace.Ref, error) {
	switch a.Do {
	case plan.Create:
		obj, err := e.object(a)
		if err != nil {
			return nil, err
		}
		if previous, found := e.deleted[a.Slot]; found {
			wait, cancel := context.WithTimeout(ctx, e.Config.Timeouts.Settle)
			err := e.Cluster.WaitGone(wait, previous)
			cancel()
			if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("%w: %w", errSlotTaken, err)
			}
			if err != nil {
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
		return &ref, e.Cluster.Delete(ctx, ref)
	case plan.Restart:
		if err := e.Process.Stop(ctx); err != nil {
			return nil, err
		}
		return nil, e.Process.Start(ctx)
	case plan.Kill:
		return nil, e.Process.Kill(ctx)
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
