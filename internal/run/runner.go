package run

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Execute runs the plan's steps, then step len(p.Steps), which deletes each
// slot's live CR, then step len(p.Steps)+1, which restarts the controller.
// It records Phase, Action and Fault events. It returns an error only when
// the harness itself fails; a controller that does not settle is a finding,
// not an error.
func (r *Runner) Execute(ctx context.Context, p *plan.Plan) error {
	e := &execution{
		Runner: r, live: map[string]trace.Ref{}, kinds: map[string]schema.GroupKind{},
		deleted: map[string]deletion{}, fixtures: map[plan.Target]deletion{}, ix: trace.NewIndex(nil),
	}
	for n, step := range p.Steps {
		err := e.step(ctx, n, step)
		if errors.Is(err, errStillTaken) {
			break
		}
		if err != nil {
			return err
		}
	}
	n := len(p.Steps)
	var deleteAll plan.Step
	for _, slot := range e.deleteOrder() {
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

// errStillTaken says that a create's or a createFixture's wait for the
// object its name last held timed out. The step still settles, and then the
// plan skips to its appended steps.
var errStillTaken = errors.New("is still taken")

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
	// kinds holds the kind of each slot's last created CR.
	kinds map[string]schema.GroupKind
	// deleted holds the last deletion from each slot, which a later create
	// of the slot waits out.
	deleted map[string]deletion
	// fixtures holds, by fixtureKey of its target, each fixture that a
	// changeFixture deleted and no createFixture has made again.
	fixtures map[plan.Target]deletion
	ix       *trace.Index
}

// deletion is an object that a delete or changeFixture action removed, and
// when.
type deletion struct {
	ref trace.Ref
	at  time.Time
}

// deleteOrder is the slots that hold a live CR, by Config.DeleteOrder of the
// kind each created, and else by name. A kind DeleteOrder does not list
// comes first.
func (e *execution) deleteOrder() []string {
	order := e.Config.DeleteOrder()
	slots := slices.Sorted(maps.Keys(e.live))
	slices.SortStableFunc(slots, func(a, b string) int {
		return cmp.Compare(slices.Index(order, e.kinds[a]), slices.Index(order, e.kinds[b]))
	})
	return slots
}

// fixtureKey is t with an empty system read as kube, so that a createFixture
// finds the changeFixture of an equal target.
func fixtureKey(t plan.Target) plan.Target {
	t.System = cmp.Or(t.System, kube.Name)
	return t
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
	if err != nil && !errors.Is(err, errStillTaken) {
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
// damage, changeFixture and createFixture actions name, and of every fake
// when the step changes a CR, since the controller then polls its fakes.
func (e *execution) maxPoll(step plan.Step) time.Duration {
	var systems []string
	for _, f := range step.Faults {
		systems = append(systems, f.System)
	}
	for _, a := range step.Actions {
		switch a.Do {
		case plan.Damage, plan.ChangeFixture, plan.CreateFixture:
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
// createFixture still waits for its name after Period has passed, or else
// when the fault period ends.
type stepRun struct {
	*execution
	n             int
	step          plan.Step
	start         time.Time
	started       []startedFault
	faultsStopped time.Time
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

// resume stops the faults and resumes supervision, which restarts a killed
// controller.
func (s *stepRun) resume() error {
	s.stopFaults()
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

// awaitGone waits until d's object, whose name action a needs, no longer
// exists. The step's faults or a kill may hold it. So once Period has passed
// with the object still there, awaitGone ends them. It gives up once the
// allowance has passed since the faults stopped, or since d if that is
// later.
func (s *stepRun) awaitGone(ctx context.Context, a plan.Action, d deletion) error {
	begun := time.Now()
	if begun.Before(s.periodEnd()) {
		if timedOut, err := s.waitGone(ctx, d.ref, s.periodEnd()); !timedOut {
			return err
		}
	}
	if gone, err := s.Cluster.Gone(ctx, d.ref); gone || err != nil {
		return err
	}
	if err := s.resume(); err != nil {
		return err
	}
	from := s.faultsStopped
	if d.at.After(from) {
		from = d.at
	}
	allowance, _ := s.allowance()
	if timedOut, err := s.waitGone(ctx, d.ref, from.Add(allowance)); !timedOut {
		return err
	}
	return s.stillTaken(a, d.ref, time.Since(begun))
}

// waitGone waits until ref names no object, or until the time until.
func (e *execution) waitGone(ctx context.Context, ref trace.Ref, until time.Time) (timedOut bool, err error) {
	wait, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	err = e.Cluster.WaitGone(wait, ref)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return true, nil
	}
	return false, err
}

// stillTaken is the error of action a, a create or a createFixture, that
// waited for ref, the object whose name it needs, and gave up. The error
// describes the last version of ref that the observer recorded, which may be
// of another object with the same name.
func (e *execution) stillTaken(a plan.Action, ref trace.Ref, waited time.Duration) error {
	name, holder := "the slot's name", "the CR"
	if a.Do == plan.CreateFixture {
		kind := schema.GroupResource{Group: ref.Group, Resource: ref.Resource}
		name, holder = fmt.Sprintf("the name of %s %s", kind, path.Join(ref.Namespace, ref.Name)), "the object"
	}
	taken := fmt.Errorf("%s %w after the %s waited %v", name, errStillTaken, a.Do, waited.Round(time.Millisecond))
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
		return fmt.Errorf("%w: %s with that name is not being deleted", taken, holder)
	}
	return fmt.Errorf("%w: %s with that name has deletionTimestamp %s and finalizers %v", taken, holder, cr.Metadata.DeletionTimestamp, cr.Metadata.Finalizers)
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
			if err := s.awaitGone(ctx, a, previous); err != nil {
				return nil, err
			}
			delete(e.deleted, a.Slot)
		}
		kind := (&unstructured.Unstructured{Object: obj}).GroupVersionKind().GroupKind()
		ref, err := e.Cluster.Create(ctx, obj)
		if err != nil {
			return nil, err
		}
		e.live[a.Slot], e.kinds[a.Slot] = ref, kind
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
		e.deleted[a.Slot] = deletion{ref, time.Now()}
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
		deleted := deletion{ref, time.Now()}
		err = actor.Delete(ctx, ref, "")
		if errors.Is(err, system.ErrGone) {
			return nil, fmt.Errorf("%w: %w", owner.ErrNoMatch, err)
		}
		if err == nil && a.Do == plan.ChangeFixture {
			e.fixtures[fixtureKey(*a.Target)] = deleted
		}
		return &ref, err
	case plan.CreateFixture:
		key := fixtureKey(*a.Target)
		deleted, found := e.fixtures[key]
		if !found {
			return nil, fmt.Errorf("%w: no changeFixture deleted it", owner.ErrNoMatch)
		}
		delete(e.fixtures, key)
		if err := s.awaitGone(ctx, a, deleted); err != nil {
			return nil, err
		}
		ref, err := e.Cluster.CreateFixture(ctx, deleted.ref)
		if err != nil {
			return nil, err
		}
		return &ref, nil
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
