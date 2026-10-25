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

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Execute runs the plan's steps. Unless a stop action ran, it then runs step
// len(p.Steps), which deletes each slot's live CR, and step len(p.Steps)+1,
// which restarts the controller. It records Phase, Action and Fault events.
// It returns an error only when the harness itself fails; a controller that
// does not settle is a finding, not an error.
func (r *Runner) Execute(ctx context.Context, p *plan.Plan) error {
	e := &execution{Runner: r, live: map[string]trace.Ref{}, deleted: map[string]deletion{}, ix: trace.NewIndex(nil)}
	for n, step := range p.Steps {
		err := e.step(ctx, n, step)
		if errors.Is(err, errSlotTaken) {
			break
		}
		if err != nil {
			return err
		}
	}
	if e.stopped {
		return nil
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

// errSlotTaken says that a create's wait for its slot's previous CR timed
// out. The step still settles, and then the plan skips to the steps that
// Execute appends.
var errSlotTaken = errors.New("the slot's name is still taken")

// Deadline is end faults + settle + min(faultsLasted, backoffCap) + the
// longest poll interval of the systems the step touched. faultsLasted is the
// time from the step's start until its faults ended. backoffBound says that
// backoffCap was the smaller.
func Deadline(endFaults, faultsLasted, maxPoll time.Duration, t config.Timeouts) (deadline time.Duration, backoffBound bool) {
	return endFaults + t.Settle + min(faultsLasted, t.BackoffCap) + maxPoll, t.BackoffCap < faultsLasted
}

type execution struct {
	*Runner
	live map[string]trace.Ref
	// deleted holds the last deletion from each slot, which a later create
	// of the slot or a stop waits out.
	deleted map[string]deletion
	ix      *trace.Index
	// stopped says that a stop action stopped the controller for good.
	stopped bool
}

// deletion is a CR that a delete action removed from its slot, and when.
type deletion struct {
	ref trace.Ref
	uid string
	at  time.Time
}

func (e *execution) phase(step int, name trace.PhaseName) {
	e.Recorder.Record(trace.Event{Phase: &trace.Phase{Step: step, Name: name}})
}

func (e *execution) step(ctx context.Context, n int, step plan.Step) error {
	s := &stepRun{execution: e, n: n, step: step, start: time.Now()}
	e.phase(n, trace.FaultPeriod)
	if err := e.Process.Supervise(false); err != nil {
		return err
	}
	err := s.faultPeriod(ctx)
	if err != nil && !errors.Is(err, errSlotTaken) {
		s.stopFaults()
		return err
	}
	taken := err
	if err := s.resume(); err != nil {
		return err
	}
	allowance, backoffBound := s.allowance()
	deadline := e.Recorder.Now() + allowance
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

// stepRun is step n while it runs. Its faults stop once: when a create or a
// stop still waits for a deleted CR after Period has passed, or else when
// the fault period ends.
type stepRun struct {
	*execution
	n             int
	step          plan.Step
	start         time.Time
	started       []startedFault
	faultsStopped time.Time
	waited        []string
}

// periodEnd is when Period has passed. The fault period may last longer.
func (s *stepRun) periodEnd() time.Time { return s.start.Add(time.Duration(s.step.Period)) }

// stopFaults ends the started faults, once.
func (s *stepRun) stopFaults() {
	if !s.faultsStopped.IsZero() {
		return
	}
	for i, f := range s.started {
		f.end()
		s.Recorder.Record(faultEvent(s.n, i, s.step.Faults[i], f.model, true))
	}
	s.faultsStopped = time.Now()
}

// resume stops the faults and, unless a stop action ran, resumes
// supervision, which restarts a killed controller.
func (s *stepRun) resume() error {
	s.stopFaults()
	if s.stopped {
		return nil
	}
	return s.Process.Supervise(true)
}

// allowance is how long the step may take to settle once its faults have
// stopped: its Deadline less end faults. bounded says that backoffCap
// bounded it.
func (s *stepRun) allowance() (allowance time.Duration, bounded bool) {
	return Deadline(0, s.faultsStopped.Sub(s.start), s.maxPoll(s.step), s.Config.Timeouts)
}

// faultPeriod starts the step's faults, applies its actions and waits until
// Period has passed.
func (s *stepRun) faultPeriod(ctx context.Context) error {
	for i, f := range s.step.Faults {
		target, model, err := s.target(f)
		if err != nil {
			return fmt.Errorf("steps[%d].faults[%d]: %w", s.n, i, err)
		}
		s.Recorder.Record(faultEvent(s.n, i, f, model, false))
		end, err := target.Fault(f)
		if err != nil {
			return fmt.Errorf("steps[%d].faults[%d]: %w", s.n, i, err)
		}
		s.started = append(s.started, startedFault{end, model})
	}
	for i, a := range s.step.Actions {
		if err := sleepUntil(ctx, s.start.Add(time.Duration(a.At))); err != nil {
			return err
		}
		ref, err := s.execution.act(ctx, a, s)
		event := &trace.Action{Step: s.n, Index: i, Do: string(a.Do), Slot: a.Slot, Ref: ref}
		if a.Do == plan.Stop {
			event.Waited = s.waited
		}
		if err != nil {
			event.Error = err.Error()
		}
		s.Recorder.Record(trace.Event{Action: event})
		if err != nil && !errors.Is(err, owner.ErrNoMatch) {
			return fmt.Errorf("steps[%d].actions[%d] %s: %w", s.n, i, a.Do, err)
		}
	}
	return sleepUntil(ctx, s.periodEnd())
}

// awaitGone waits until no object holds the name of d's CR, the slot's
// previous one, and fails with errSlotTaken when it gives up.
func (s *stepRun) awaitGone(ctx context.Context, d deletion) error {
	begun := time.Now()
	if timedOut, err := s.outwait(ctx, d.ref, "", d.at); !timedOut {
		return err
	}
	return s.slotTaken(d.ref, time.Since(begun))
}

// outwait waits until Gone holds for ref and uid. The step's faults or a
// kill may hold the object. So once Period has passed with it still there,
// outwait ends them. It gives up once the allowance has passed since the
// faults stopped, or since deleted if that is later.
func (s *stepRun) outwait(ctx context.Context, ref trace.Ref, uid string, deleted time.Time) (timedOut bool, err error) {
	if time.Now().Before(s.periodEnd()) {
		if timedOut, err := s.waitGone(ctx, ref, uid, s.periodEnd()); !timedOut {
			return false, err
		}
	}
	if gone, err := s.Cluster.Gone(ctx, ref, uid); gone || err != nil {
		return false, err
	}
	if err := s.resume(); err != nil {
		return false, err
	}
	from := s.faultsStopped
	if deleted.After(from) {
		from = deleted
	}
	allowance, _ := s.allowance()
	return s.waitGone(ctx, ref, uid, from.Add(allowance))
}

// stop waits until the CRs that deletes removed are gone, or until each
// wait gives up, and then stops the controller for good. A CR is gone once
// its name is free or another CR holds it. Converge judges a CR still
// there.
func (s *stepRun) stop(ctx context.Context) error {
	for _, slot := range slices.Sorted(maps.Keys(s.deleted)) {
		d := s.deleted[slot]
		if _, err := s.outwait(ctx, d.ref, d.uid, d.at); err != nil {
			return err
		}
		s.waited = append(s.waited, d.uid)
	}
	if err := s.Process.Supervise(false); err != nil {
		return err
	}
	s.stopped = true
	return s.Process.Stop(ctx)
}

// waitGone waits until Gone holds for ref and uid, or until the time until.
func (e *execution) waitGone(ctx context.Context, ref trace.Ref, uid string, until time.Time) (timedOut bool, err error) {
	wait, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	err = e.Cluster.WaitGone(wait, ref, uid)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return true, nil
	}
	return false, err
}

// slotTaken is the error of a create that waited for ref, the slot's
// previous CR, and gave up. The error describes the last version of ref that
// the observer recorded, which may be of another CR with the same name.
func (e *execution) slotTaken(ref trace.Ref, waited time.Duration) error {
	taken := fmt.Errorf("%w after the create waited %v", errSlotTaken, waited.Round(time.Millisecond))
	e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
	var cr struct {
		Metadata struct {
			DeletionTimestamp string   `json:"deletionTimestamp"`
			Finalizers        []string `json:"finalizers"`
		} `json:"metadata"`
	}
	v, found := e.ix.Latest(ref)
	switch {
	case !found || v.Deleted || json.Unmarshal(v.Body, &cr) != nil:
		return taken
	case cr.Metadata.DeletionTimestamp == "":
		return fmt.Errorf("%w: the CR with that name is not being deleted", taken)
	}
	return fmt.Errorf("%w: the CR with that name has deletionTimestamp %s and finalizers %v", taken, cr.Metadata.DeletionTimestamp, cr.Metadata.Finalizers)
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

// act applies one action of step run s and returns the ref of the object it
// touched.
func (e *execution) act(ctx context.Context, a plan.Action, s *stepRun) (*trace.Ref, error) {
	switch a.Do {
	case plan.Create:
		obj, err := e.object(a)
		if err != nil {
			return nil, err
		}
		if previous, found := e.deleted[a.Slot]; found {
			if err := s.awaitGone(ctx, previous); err != nil {
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
		// The create returned once the observer recorded the slot's CR, so
		// the latest version is of it.
		e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
		cr, _ := e.ix.Latest(ref)
		delete(e.live, a.Slot)
		at := time.Now()
		if err := e.Cluster.Delete(ctx, ref, a.Propagation); err != nil && !errors.Is(err, system.ErrGone) {
			return &ref, err
		}
		e.deleted[a.Slot] = deletion{ref, cr.UID, at}
		return &ref, nil
	case plan.Restart:
		if err := e.Process.Stop(ctx); err != nil {
			return nil, err
		}
		return nil, e.Process.Start(ctx)
	case plan.Kill:
		return nil, e.Process.Kill(ctx)
	case plan.Stop:
		return nil, s.stop(ctx)
	case plan.Damage, plan.ChangeFixture:
		creator := trace.Controller
		if a.Do == plan.ChangeFixture {
			creator = trace.Harness
		}
		e.ix.Append(e.Recorder.EventsSince(e.ix.Len())...)
		ref, err := e.Select(e.ix, creator, *a.Target, a.Slot, e.Recorder.Now())
		if err != nil {
			return nil, err
		}
		actor, known := e.Actors[ref.System]
		if !known {
			return nil, fmt.Errorf("no system %q takes %s", ref.System, a.Do)
		}
		if a.Patch != nil {
			return &ref, actor.Patch(ctx, ref, a.Patch)
		}
		err = actor.Delete(ctx, ref, "")
		if errors.Is(err, system.ErrGone) {
			return nil, fmt.Errorf("%w: %w", owner.ErrNoMatch, err)
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
