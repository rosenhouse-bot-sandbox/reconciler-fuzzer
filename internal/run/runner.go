package run

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/launch"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// defaultMaxManaged bounds the managed objects in the run namespace. Beyond
// it, the run ends as a harness limit rather than a finding.
const defaultMaxManaged = 500

// teardownMargin is what the teardown gets beyond its own waits, so that a run
// that reached its deadline still takes itself down.
const teardownMargin = 30 * time.Second

// Teardown and Recovery are the Checkpoint.Op of the checkpoints no op opened.
const (
	Teardown = invariant.Teardown
	Recovery = invariant.Recovery
)

// maxTail is how much of the target's output the harness reads to quote what
// it said as it stopped. A panic's stack follows the line it opens with.
const maxTail = 1 << 20

// deletionMargin holds the run namespace open past timeouts.delete, because
// G3's window opens when the Observer recorded the deletion, which is after
// the teardown asked for it.
const deletionMargin = time.Second

// Checker evaluates the invariants and the target's properties at a
// checkpoint. Its error is a configuration error, never a finding.
type Checker interface {
	Check(Input) (Findings, error)
}

// Findings are what the checks made of the run at one checkpoint.
type Findings struct {
	Violations []Violation
	// Notes name what a check could not judge.
	Notes []string
}

// Input is what a check reads: the proxy's request log, the Observer's object
// history, the target's declaration and what the run has done so far.
type Input struct {
	Target   *target.Target
	Requests []proxy.Request
	Objects  *observe.Store
	Timeline Timeline
}

// Violation is one failed invariant or property.
type Violation struct {
	// ID is G1..Gn or P1..Pn.
	ID string
	// Statement is what the check requires and the run broke.
	Statement string
	// At is the instant the check judged, which aligns the evidence below.
	At time.Time
	// Evidence quotes what the run did, in one line.
	Evidence string
	// Requests and Versions are the evidence the violation named, which a
	// report quotes.
	Requests []proxy.Request
	Versions []observe.Version
	// RequestsTotal and VersionsTotal are what the check chose each excerpt
	// from, and VersionsOf names the object the timeline is the history of
	// (pkg/invariant).
	RequestsTotal, VersionsTotal int
	VersionsOf                   string
	// Managed is the state at the violation, one version per object the target
	// managed, and ManagedTotal how many there were. A check that does not
	// quote the state leaves the total nil.
	Managed      []observe.Version
	ManagedTotal *int
	// Ready is what a readiness verdict read of the predicate and the CR.
	Ready *invariant.Readiness
	// Differences are what G5 found changed across a restart, DifferencesTotal
	// how many there were, and Compared names the two states.
	Differences      []invariant.Difference
	DifferencesTotal int
	Compared         string
}

// String is the violation in one line. A message that prints one wants the
// finding, not the evidence behind it.
func (v Violation) String() string {
	if v.Evidence == "" {
		return v.ID + " " + v.Statement
	}
	return fmt.Sprintf("%s %s; %s", v.ID, v.Statement, v.Evidence)
}

// Result is one run's outcome. An error alongside it is a configuration or
// harness error, never a finding.
type Result struct {
	Timeline Timeline
	// Violation is the first violation the run found, or nil. A run ends at
	// its first violation.
	Violation *Violation
	// Notes name what the checks could not judge, as the last checkpoint read
	// the run.
	Notes []string
	// Recorded is the whole run as the checks read it, sampled once the
	// teardown is done. A run that ended early recorded what it reached.
	Recorded Input
}

// Timeline is what the run did, in order.
type Timeline struct {
	// Namespace is private to this run and never reused.
	Namespace   string
	Ops         []AppliedOp
	Checkpoints []Checkpoint
	// Recovery is the settle wait the teardown gave a target still owed time
	// to recover from the faults, or nil if it owed none.
	Recovery *Wait
	// Quiet is the timeouts.stable the teardown waits before it deletes
	// anything, the run's last quiet window. It closes where Deletion opens.
	Quiet Window
	// Deletion is the window G3 judges: it opens when the teardown deletes the
	// primary CR and closes when the namespace is clean or the window expires.
	Deletion Window
	// Cleaned is when the teardown saw the run namespace empty, or zero if it
	// never did.
	Cleaned time.Time
	// Faults are the windows the proxy applied each fault op's fault in, one
	// per fault op. A window with no Start is a fault the proxy applied to no
	// request, which changed nothing and excuses nothing. A window with no End
	// is a fault the proxy still applies or still holds a request of.
	Faults []Window
	// Forced names every object the teardown force-removed a finalizer from.
	// A run that was not abandoned notes each one: G3 judged the deletion
	// window, which closed before any of this.
	Forced []string
	// Exits are the times the target stopped on its own after the first
	// settle wait converged.
	Exits []Exit
}

// Exit is one time the target stopped on its own.
type Exit struct {
	At  time.Time
	Err error
	// Said is what the target wrote as it stopped, or empty.
	Said string
	// Restart is when the launcher starts the target again.
	Restart time.Time
}

func (e Exit) String() string {
	why := "no error"
	if e.Err != nil {
		why = e.Err.Error()
	}
	if e.Said == "" {
		return why
	}
	return fmt.Sprintf("%s after writing %q", why, e.Said)
}

// AppliedOp is one op the Runner applied.
type AppliedOp struct {
	Op Op
	At time.Time
	// CR is the primary CR a CR op wrote.
	CR string
	// Deleted is the object a deleteManaged op deleted.
	Deleted string
	// Restored says reconciler-fuzzer created a fixture it had deleted again,
	// after At and before the op's own change.
	Restored bool
	// Settled is the settle wait that followed the op, or nil if none did.
	Settled *Wait
}

// Wait is one settle wait's outcome.
type Wait struct {
	Window    Window
	Converged bool
}

// checkpoint is where the wait after op ended.
func (w Wait) checkpoint(op int) Checkpoint {
	return Checkpoint{At: w.Window.End, Began: w.Window.Start, Op: op, Converged: w.Converged}
}

// Checkpoint is where the checks ran.
type Checkpoint struct {
	// At is where the settle wait ended, or where the teardown's deletion
	// window closed.
	At time.Time
	// Began is where that settle wait began. The teardown's is zero.
	Began time.Time
	// Op is the op whose settle wait ended here, Recovery, or Teardown.
	Op int
	// Converged is whether that wait converged. At the teardown's checkpoint
	// it is whether the namespace came clean within the deletion window.
	Converged bool
	// Stayed marks a recreate's wait for its old CR to go, which ended with
	// the CR still there.
	Stayed bool
	// Held is whether the proxy held a request of the target's at At, or
	// released one within timeouts.stable before it.
	Held bool
}

// Window is a stretch of a run's time.
type Window struct{ Start, End time.Time }

// Run executes one sequence against a fresh namespace and evaluates
// opts.Check at each checkpoint. The proxy's fault sampling follows the
// sequence's seed, so a replay faults the same requests.
func Run(ctx context.Context, t *target.Target, sequence Sequence, opts Options) (Result, error) {
	if err := validateRun(t, sequence, opts); err != nil {
		return Result{}, fmt.Errorf("running the sequence: %w", err)
	}
	if err := WriteRunSequence(opts.Dir, sequence); err != nil {
		return Result{}, err
	}
	opts.Seed = sequence.Seed
	h, err := Start(ctx, t, opts)
	if err != nil {
		return Result{}, err
	}
	live, err := newLiveRun(h, t)
	if err != nil {
		return Result{}, errors.Join(err, h.Stop(ctx))
	}
	return runSequence(ctx, t, sequence, opts, live)
}

func validateRun(t *target.Target, sequence Sequence, opts Options) error {
	if err := validate(t, opts); err != nil {
		return err
	}
	if opts.Check == nil {
		return errors.New("a checker is required: the Runner evaluates it at each checkpoint")
	}
	if sequence.Target != t.Name {
		return fmt.Errorf("the sequence is for the target %q, and this run's target is %q", sequence.Target, t.Name)
	}
	if err := sequence.Validate(); err != nil {
		return err
	}
	return sequence.checkCRs(t.Sample.GetName())
}

// WriteRunSequence puts the sequence in the run directory, so that a failing
// run carries what it executed.
func WriteRunSequence(dir string, sequence Sequence) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the run directory: %w", err)
	}
	return WriteSequence(filepath.Join(dir, sequenceFile), sequence)
}

// harness is the live run the Runner drives. The unit tier fakes it; liveRun
// implements it over a started Harness.
type harness interface {
	namespace() string
	now() time.Time
	// settle waits for the target to converge, past timeouts.settle while
	// owed returns a later instant. It converges no sooner than floor, and
	// gives up no sooner than timeouts.stable past it.
	settle(ctx context.Context, owed func() time.Time, floor time.Time) (bool, error)
	sleep(ctx context.Context, d time.Duration) error
	restart(ctx context.Context) error
	// addFault has the proxy apply the fault after those it holds.
	addFault(spec proxy.FaultSpec) proxy.FaultID
	removeFault(id proxy.FaultID)
	clearFaults()
	// faultWindow is what the proxy has done with the fault.
	faultWindow(id proxy.FaultID) proxy.FaultWindow
	// held reports, of the target's requests that reached the proxy before
	// then, whether the proxy holds one and when it last released one.
	held(before time.Time) (holding bool, released time.Time)
	// servedResources is what the API server serves now, which includes the
	// CRDs a target installed.
	servedResources() ([]metav1.APIResource, error)
	createCR(ctx context.Context, obj *unstructured.Unstructured) error
	patchCR(ctx context.Context, name string, patch map[string]any) error
	deleteCR(ctx context.Context, name string) error
	// awaitCRGone waits for the CR to go until the instant until returns has
	// passed, and reports whether it went.
	awaitCRGone(ctx context.Context, name string, until func() time.Time) (bool, error)
	// managedObjects names the managed objects of one kind, ordered by
	// creationTimestamp then name.
	managedObjects(gvk schema.GroupVersionKind) []string
	// deleteManaged deletes the object, and reports false where it was
	// already gone.
	deleteManaged(ctx context.Context, gvk schema.GroupVersionKind, name string) (bool, error)
	patchFixture(ctx context.Context, gvk schema.GroupVersionKind, name string, patch map[string]any) error
	deleteFixture(ctx context.Context, gvk schema.GroupVersionKind, name string) error
	createFixture(ctx context.Context, fixture *unstructured.Unstructured) error
	managedCount() int
	// awaitClean waits for the run namespace to empty, and reports whether it
	// did within the window.
	awaitClean(ctx context.Context, within time.Duration) (bool, error)
	// forceFinalizers removes every finalizer left in the run namespace and
	// names what it took them from.
	forceFinalizers(ctx context.Context) ([]string, error)
	empty(ctx context.Context) error
	requests() []proxy.Request
	objects() *observe.Store
	// targetStatus reports whether the target is still running, and why it
	// stopped if it is not.
	targetStatus() launch.Status
	// supervise restarts the target whenever it exits until ctx ends, and
	// records each exit in exits.
	supervise(ctx context.Context)
	exits() []Exit
	stop(ctx context.Context) error
	// unresolvedOwners names each owner the collector could not resolve. It
	// is complete once stop has returned.
	unresolvedOwners() []cluster.Unresolved
}

// runner executes one sequence. It always tears the run down.
type runner struct {
	target   *target.Target
	sequence Sequence
	check    Checker
	h        harness
	limit    int
	dir      string
	now      func() time.Time

	timeline  Timeline
	violation *Violation
	notes     []string
	// skipped names what no check judged: what the run could not do, and what
	// reconciler-fuzzer did to the run namespace itself.
	skipped []string
	failed  bool
	// stopped is whether an op ended the run early with no finding, as a
	// recreate does whose old CR a fault keeps.
	stopped bool
	// converged is where the last settle wait that converged ended. The first
	// shows the target works, so reconciler-fuzzer supervises it from there on.
	converged time.Time

	teardownStart time.Time
	// crs are the CRs the run created, in the order it first created each.
	crs    []string
	faults []heldFault
	// faultOps is the op index of each of Timeline.Faults.
	faultOps []int
	// fixtures are the fixtures as reconciler-fuzzer last wrote them, by kind and
	// name.
	fixtures map[string]*unstructured.Unstructured
	// deleted are the deleteFixture ops whose fixture is still gone.
	deleted []Op
}

// heldFault is a fault op's fault. The Runner holds it to read its window from
// the proxy.
type heldFault struct {
	id proxy.FaultID
	// until is the op index the fault ends at, or nil if only the proxy's own
	// trigger ends it.
	until *int
	// window is the fault's place in Timeline.Faults.
	window int
	// retired is whether the proxy has stopped applying it and released what
	// it held.
	retired bool
}

func runSequence(ctx context.Context, t *target.Target, sequence Sequence, opts Options, h harness) (Result, error) {
	r := &runner{
		target:   t,
		sequence: sequence,
		check:    opts.Check,
		h:        h,
		limit:    opts.maxManaged(),
		dir:      opts.Dir,
		now:      h.now,
		timeline: Timeline{Namespace: h.namespace()},
		fixtures: map[string]*unstructured.Unstructured{},
	}
	for _, fixture := range t.Fixtures {
		r.fixtures[observe.KindName(fixture.GroupVersionKind())+" "+fixture.GetName()] = fixture.DeepCopy()
	}
	failure := r.applyOps(ctx)
	r.failed = failure != nil
	teardown := r.teardown(ctx)
	r.readExits()
	// The run's own notes come before the last checkpoint's.
	notes := slices.Concat(r.exitNotes(), r.forbiddenNotes(), r.skipped, r.notes)
	result := Result{Timeline: r.timeline, Violation: r.violation, Notes: notes, Recorded: r.input()}
	return result, errors.Join(failure, teardown)
}

// Refused is the API server turning away the CR an op wrote. The CRD's schema
// or rules refused it, or an admission webhook did.
type Refused struct {
	Op     Op
	Reason error
}

func (r *Refused) Error() string {
	return fmt.Sprintf("the API server refused op %d (%s): %v", r.Op.Index, r.Op.Type, r.Reason)
}

// refusal is err as a Refused when the API server turned the op's write away
// for what it carried: 422 from validation, and 400 or 403 from an admission
// webhook.
func refusal(op Op, err error) error {
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err) {
		return &Refused{Op: op, Reason: err}
	}
	return err
}

// applyOps applies the sequence in order and stops at the first violation,
// or where an op stopped the run.
func (r *runner) applyOps(ctx context.Context) error {
	for _, op := range r.sequence.Ops {
		if r.violation != nil || r.stopped {
			return nil
		}
		if err := r.applyOp(ctx, op); err != nil {
			if refused := (*Refused)(nil); errors.As(err, &refused) {
				return err
			}
			return fmt.Errorf("op %d (%s): %w", op.Index, op.Type, err)
		}
	}
	return nil
}

// applyOp applies one op and waits for the target's reaction. A target that
// has stopped ends the run here, because the op would otherwise be applied to
// nothing.
func (r *runner) applyOp(ctx context.Context, op Op) error {
	r.expireFaults(op.Index)
	if err := r.awaitRestart(ctx); err != nil {
		return err
	}
	if status := r.h.targetStatus(); !status.Running {
		return r.targetStopped(ctx, status)
	}
	applied := AppliedOp{Op: op, At: r.now()}
	if op.Type.OnCR() {
		applied.CR = op.crName(r.target.Sample.GetName())
	}
	// The op is on the timeline while it acts, so that an exit during it is
	// the op's.
	r.timeline.Ops = append(r.timeline.Ops, applied)
	landed := len(r.timeline.Ops) - 1
	var err error
	if applied.Restored, err = r.restoreFixtures(ctx, op.Index); err == nil {
		applied.Deleted, err = r.apply(ctx, op, applied.CR)
	}
	r.timeline.Ops[landed] = applied
	if stayed := (*crStayed)(nil); errors.As(err, &stayed) {
		return r.judgeStayed(ctx, op, stayed)
	}
	if err != nil {
		return err
	}
	if op.Settles() {
		return r.settle(ctx, op)
	}
	return nil
}

// awaitRestart waits for the target to restart after an exit a fault excused.
func (r *runner) awaitRestart(ctx context.Context) error {
	now := r.now()
	if restart := r.asOf(now).PendingRestart(now); !restart.IsZero() {
		return r.h.sleep(ctx, restart.Sub(now))
	}
	return nil
}

// apply applies the op, a CR op to the CR named cr, and names the managed
// object a deleteManaged op deleted.
func (r *runner) apply(ctx context.Context, op Op, cr string) (string, error) {
	switch op.Type {
	case OpCreate:
		return "", r.create(ctx, op)
	case OpUpdate:
		return "", refusal(op, r.h.patchCR(ctx, cr, op.Patch))
	case OpDelete:
		return "", r.h.deleteCR(ctx, cr)
	case OpRecreate:
		return "", r.recreate(ctx, op, cr)
	case OpRestart:
		return "", r.h.restart(ctx)
	case OpFault:
		if err := r.checkResource(op.Fault.Match.Resource); err != nil {
			return "", err
		}
		r.inject(op)
		return "", nil
	case OpDeleteManaged:
		return r.applyDeleteManaged(ctx, op)
	case OpUpdateFixture, OpDeleteFixture:
		return "", r.applyToFixture(ctx, op)
	case OpSettle:
		return "", nil
	}
	return "", fmt.Errorf("%q is not an op type", op.Type)
}

// applyToFixture changes or deletes a fixture. The fixture is
// reconciler-fuzzer's, so a deleted one is not the target's to recreate.
func (r *runner) applyToFixture(ctx context.Context, op Op) error {
	fixture, declared := r.fixtures[op.fixture()]
	if !declared {
		return fmt.Errorf("the target declares no fixture %s %s", op.Kind, op.Name)
	}
	gvk := fixture.GroupVersionKind()
	if slices.Contains(r.target.ClusterFixtures, gvk) {
		return fmt.Errorf("the fixture %s %s is cluster-scoped, and a sequence cannot mutate what every run shares", observe.KindName(gvk), op.Name)
	}
	if op.Type == OpDeleteFixture {
		r.deleted = append(r.deleted, op)
		return r.h.deleteFixture(ctx, gvk, op.Name)
	}
	if err := r.h.patchFixture(ctx, gvk, op.Name, op.Patch); err != nil {
		return err
	}
	MergePatch(fixture.Object, op.Patch)
	return nil
}

// restoreFixtures creates again, as reconciler-fuzzer last wrote them, the
// fixtures deleted until this op, and reports whether it created any.
func (r *runner) restoreFixtures(ctx context.Context, op int) (bool, error) {
	var gone []Op
	restored := false
	for _, deleted := range r.deleted {
		if deleted.Until.Op > op {
			gone = append(gone, deleted)
			continue
		}
		if err := r.h.createFixture(ctx, r.fixtures[deleted.fixture()]); err != nil {
			return restored, err
		}
		restored = true
	}
	r.deleted = gone
	return restored, nil
}

func (r *runner) create(ctx context.Context, op Op) error {
	if err := r.h.createCR(ctx, op.Obj); err != nil {
		return refusal(op, err)
	}
	if name := op.Obj.GetName(); !slices.Contains(r.crs, name) {
		r.crs = append(r.crs, name)
	}
	return nil
}

// recreate deletes the CR, waits for it to go and creates the op's object. The
// wait lasts timeouts.delete, or longer while the run is owed time or the
// proxy holds a request, as a settle wait does. A CR still there where the
// wait ends is judged there, and the op creates nothing.
func (r *runner) recreate(ctx context.Context, op Op, cr string) error {
	due := r.now().Add(r.target.Timeouts.Delete)
	if err := r.h.deleteCR(ctx, cr); err != nil {
		return err
	}
	wait := Wait{Window: Window{Start: r.now()}}
	gone, err := r.h.awaitCRGone(ctx, cr, func() time.Time {
		deadline := due
		if owed := r.owed(); owed.After(due) {
			deadline = owed
		}
		return heldOpen(r.h.held, deadline, r.now(), r.target.Timeouts.Settle)
	})
	switch {
	case err != nil:
		return err
	case gone:
		return r.create(ctx, op)
	}
	wait.Window.End = r.now()
	return &crStayed{cr: cr, wait: wait}
}

// crStayed is a recreate whose CR was still there where the wait for it to go
// ended.
type crStayed struct {
	cr   string
	wait Wait
}

func (s *crStayed) Error() string {
	return fmt.Sprintf("the CR %s was still there %v after its delete", s.cr, s.wait.Window.End.Sub(s.wait.Window.Start).Round(time.Second))
}

// judgeStayed checkpoints where a recreate's wait for its CR ended. The op
// cannot go on. Where a fault excuses the target, the run stops, and the
// teardown judges the target once it has cleared the fault. A CR that no
// check reports there otherwise is a harness error.
func (r *runner) judgeStayed(ctx context.Context, op Op, stayed *crStayed) error {
	checkpoint := stayed.wait.checkpoint(op.Index)
	checkpoint.Stayed = true
	if err := r.judge(ctx, checkpoint, invariant.Input.Excused); err != nil || r.violation != nil {
		return err
	}
	if !r.asOf(checkpoint.At).Recovering(checkpoint.At) {
		return stayed
	}
	r.stopped = true
	r.skipped = append(r.skipped, fmt.Sprintf("op %d (%s) stopped the run: %v, and a fault excused the target, so the op could not create its CR",
		op.Index, op.Type, stayed))
	return nil
}

// applyDeleteManaged resolves the op's index against the managed objects and
// deletes the one it names, behind the target's back. How many objects the
// target manages is its own doing, so an index that resolves to nothing skips
// the op and is reported as a note. So does an object already gone, which the
// Observer had not yet seen go. A kind the target does not manage is still a
// configuration error: no run of that sequence can resolve it.
func (r *runner) applyDeleteManaged(ctx context.Context, op Op) (string, error) {
	gvk, err := managedKind(r.target, op.Kind)
	if err != nil {
		return "", err
	}
	names := r.h.managedObjects(gvk)
	if *op.Nth >= len(names) {
		r.skipped = append(r.skipped, fmt.Sprintf("op %d (deleteManaged) deleted nothing: index %d resolves to nothing, and the run holds %d managed %s",
			op.Index, *op.Nth, len(names), op.Kind))
		return "", nil
	}
	name := names[*op.Nth]
	switch deleted, err := r.h.deleteManaged(ctx, gvk, name); {
	case err != nil:
		return "", err
	case !deleted:
		r.skipped = append(r.skipped, fmt.Sprintf("op %d (deleteManaged) deleted nothing: index %d resolved to the %s %s, which was gone before reconciler-fuzzer could delete it",
			op.Index, *op.Nth, op.Kind, name))
		return "", nil
	}
	return name, nil
}

// settle waits for the target's reaction and checkpoints where the wait ends.
// Nothing shows what a deleteManaged changed, so the wait after one that
// deleted an object converges no sooner than the target must have recreated
// it.
func (r *runner) settle(ctx context.Context, op Op) error {
	var floor time.Time
	if applied := r.timeline.Ops[len(r.timeline.Ops)-1]; applied.Deleted != "" {
		floor = r.asOf(r.now()).RecreateOwed(applied.At)
	}
	wait, err := r.wait(ctx, floor)
	if err != nil {
		return err
	}
	r.timeline.Ops[len(r.timeline.Ops)-1].Settled = &wait
	return r.judge(ctx, wait.checkpoint(op.Index), invariant.Input.Excused)
}

// wait waits up to timeouts.settle for the target to converge, or longer
// while it is owed time to recover from the faults or to finish a deletion.
// It converges no sooner than floor.
func (r *runner) wait(ctx context.Context, floor time.Time) (Wait, error) {
	wait := Wait{Window: Window{Start: r.now()}}
	converged, err := r.h.settle(ctx, r.owed, floor)
	wait.Window.End, wait.Converged = r.now(), converged
	if converged {
		if r.converged.IsZero() {
			r.h.supervise(ctx)
		}
		r.converged = wait.Window.End
	}
	return wait, err
}

// owed is when a settle wait may give up, as the checks judge it.
func (r *runner) owed() time.Time {
	now := r.now()
	return r.asOf(now).WaitOwed(now)
}

// asOf is what the checks read of the run at t.
func (r *runner) asOf(t time.Time) invariant.Input {
	r.readFaultWindows()
	r.readExits()
	return invariant.Input{
		Target:      r.target,
		Requests:    r.h.requests(),
		History:     r.h.objects(),
		Ops:         engineOps(r.target, r.timeline),
		Checkpoints: engineCheckpoints(r.timeline.Checkpoints),
		Faults:      engineFaults(r.timeline.Faults),
		Exits:       engineExits(r.timeline.Exits),
		End:         t,
	}
}

func (r *runner) readExits() { r.timeline.Exits = r.h.exits() }

// judge checkpoints where a settle wait ended. A wait that expired unexcused
// is a G4 violation, which ends the run.
func (r *runner) judge(ctx context.Context, checkpoint Checkpoint, excused func(invariant.Input, invariant.Checkpoint) bool) error {
	if !checkpoint.Converged {
		// A target that is gone cannot converge, so that is the harness's
		// failure to report, not the target's to answer for.
		if status := r.h.targetStatus(); !status.Running {
			return r.targetStopped(ctx, status)
		}
	}
	expired := !checkpoint.Converged && !excused(r.asOf(checkpoint.At), engineCheckpoint(checkpoint))
	return r.checkpoint(checkpoint, expired)
}

// ErrTargetStopped is in the error of a run whose target is no longer running.
var ErrTargetStopped = errors.New("the target is no longer running")

// targetStopped is the harness error for a target that is no longer running.
// A target that rejects its own flags writes one line and exits, and that line
// is what its reader acts on. A terminal's Ctrl-C stops the target as it ends
// ctx, so a target found stopped once ctx has ended carries ctx's error.
func (r *runner) targetStopped(ctx context.Context, status launch.Status) error {
	log := filepath.Join(r.dir, targetLogFile)
	stopped := ErrTargetStopped
	// The launcher knows no exit where it holds no process at all.
	if status.Exit != nil {
		stopped = fmt.Errorf("%w: %v", ErrTargetStopped, status.Exit)
	}
	said, _ := whyItStopped(log, 0)
	err := fmt.Errorf("%w; its output is in %s", stopped, log)
	if said != "" {
		err = fmt.Errorf("%w; it wrote %q, and the rest of its output is in %s", stopped, said, log)
	}
	switch {
	case strings.Contains(said, "address already in use"):
		err = fmt.Errorf("%w; another process holds that port, perhaps a concurrent run of this target, so give the target a free one in launch.args", err)
	// A supervised target stops only where a restart failed, and a target that
	// requested no resource never saw the CR.
	case len(r.crs) > 0 && r.converged.IsZero() && slices.ContainsFunc(r.h.requests(), namesAResource):
		err = fmt.Errorf("%w; reconciler-fuzzer had created the CR, so the CR may have crashed the target, and %s replays the run",
			err, filepath.Join(r.dir, sequenceFile))
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %w", ctx.Err(), err)
	}
	return err
}

func namesAResource(request proxy.Request) bool { return request.Resource != "" }

// panicked opens the report a Go runtime writes on the way out.
var panicked = []string{"panic: ", "fatal error: "}

var (
	// frameLocation is the file line of a Go stack frame, below its function.
	frameLocation   = regexp.MustCompile(`^\t.+:\d+( \+0x[0-9a-f]+)?$`)
	goroutineHeader = regexp.MustCompile(`^goroutine \d+ .*:$`)
)

// usageHeader opens the usage text that Go's flag package and cobra print
// below a flag error.
var usageHeader = regexp.MustCompile(`^Usage( of .+)?:$`)

// whyItStopped is what the target said as it stopped, in the log past from:
// the line the last panic opens with, or else the line above the last usage
// text, or else the last whole line above any stack trace. It is empty where
// the log holds none, which leaves the reader the file itself. It also returns
// where the log ends.
func whyItStopped(path string, from int64) (string, int64) {
	said, end := tailLines(path, from)
	for _, line := range slices.Backward(said) {
		if slices.ContainsFunc(panicked, func(opener string) bool { return strings.HasPrefix(line, opener) }) {
			return line, end
		}
	}
	if header := lastUsageHeader(said); header >= 0 {
		for _, line := range slices.Backward(said[:header]) {
			if strings.TrimSpace(line) != "" {
				return strings.TrimSpace(line), end
			}
		}
	}
	for i, line := range slices.Backward(said) {
		inTrace := strings.TrimSpace(line) == "" || frameLocation.MatchString(line) || goroutineHeader.MatchString(line) ||
			(i+1 < len(said) && frameLocation.MatchString(said[i+1]))
		if !inTrace {
			return strings.TrimSpace(line), end
		}
	}
	return "", end
}

// lastUsageHeader is the index of the last line that opens a usage text, or -1.
func lastUsageHeader(lines []string) int {
	for i, line := range slices.Backward(lines) {
		if usageHeader.MatchString(line) {
			return i
		}
	}
	return -1
}

// tailLines are the whole lines of the file past from, at most maxTail bytes
// of them, innermost last, and where the file ends. A line longer than
// maxTail has no whole form to quote, and a file reconciler-fuzzer cannot read
// has nothing.
func tailLines(path string, from int64) ([]string, int64) {
	file, err := os.Open(path)
	if err != nil {
		return nil, from
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, from
	}
	start := max(from, info.Size()-maxTail)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil {
		return nil, from
	}
	lines := strings.Split(strings.TrimRight(string(tail), "\n"), "\n")
	// A tail cut short begins mid-line, so its first line is a fragment: klog
	// puts the level and the message at the front.
	if start > from {
		lines = lines[1:]
	}
	return lines, info.Size()
}

// checkpoint evaluates the checks and keeps the first violation. The G4 of a
// wait that expired unexcused comes first.
func (r *runner) checkpoint(checkpoint Checkpoint, expired bool) error {
	if count := r.h.managedCount(); count > r.limit {
		return fmt.Errorf("the run namespace holds %d managed objects, over the harness limit of %d", count, r.limit)
	}
	// The target may still be acting on a request released within
	// timeouts.stable. A wait that converged saw no such release.
	holding, released := r.h.held(checkpoint.At)
	checkpoint.Held = holding || released.After(checkpoint.At.Add(-r.target.Timeouts.Stable))
	r.timeline.Checkpoints = append(r.timeline.Checkpoints, checkpoint)
	r.readExits()
	if expired {
		violation, err := engineInput(r.input()).ExpiredWait(engineCheckpoint(checkpoint))
		if err != nil {
			return err
		}
		r.violate(fromEngine(violation))
	}
	found, err := r.check.Check(r.input())
	if err != nil {
		return fmt.Errorf("evaluating the checks: %w", err)
	}
	r.notes = found.Notes
	for _, violation := range found.Violations {
		r.violate(violation)
		break
	}
	return nil
}

func (r *runner) input() Input {
	return Input{Target: r.target, Requests: r.h.requests(), Objects: r.h.objects(), Timeline: r.timeline}
}

func (r *runner) violate(violation Violation) {
	if r.violation == nil {
		r.violation = &violation
	}
}

// checkResource refuses a fault on a resource the API server does not serve,
// since the proxy records a request's resource as the plural it is served at.
func (r *runner) checkResource(resource string) error {
	if resource == "" {
		return nil
	}
	served, err := r.h.servedResources()
	if err != nil {
		return err
	}
	var meant string
	for _, s := range served {
		if s.Name == resource {
			return nil
		}
		if strings.EqualFold(s.Name, resource) || strings.EqualFold(s.SingularName, resource) {
			meant = s.Name
		}
	}
	if meant != "" {
		return fmt.Errorf("match.resource %q is not a resource the API server serves; did you mean %s?", resource, meant)
	}
	return fmt.Errorf("match.resource %q is not a resource the API server serves; it takes a plural such as configmaps", resource)
}

// inject adds the op's fault to those the proxy applies. Its window opens
// where the proxy first applies it, which may be never.
func (r *runner) inject(op Op) {
	r.timeline.Faults = append(r.timeline.Faults, Window{})
	r.faultOps = append(r.faultOps, op.Index)
	r.faults = append(r.faults, heldFault{
		id:     r.h.addFault(op.Fault.spec()),
		until:  op.Fault.Until.Op,
		window: len(r.timeline.Faults) - 1,
	})
}

// expireFaults removes the faults whose until trigger names this op or an
// earlier one. It drops a fault once its window is closed, so that the window
// holds every request the proxy faulted with it.
func (r *runner) expireFaults(op int) {
	for _, fault := range r.faults {
		if fault.until != nil && *fault.until <= op {
			r.h.removeFault(fault.id)
		}
	}
	r.readFaultWindows()
	r.faults = slices.DeleteFunc(r.faults, func(fault heldFault) bool { return fault.retired })
}

// readFaultWindows writes what the proxy has done with each fault into the
// timeline: a window opens where the proxy first applied the fault and closes
// where the proxy stopped applying it and released what it held. A fault the
// proxy never applied leaves its window unopened, because the run then ran as
// if the fault op were not there.
func (r *runner) readFaultWindows() {
	for i := range r.faults {
		fault := &r.faults[i]
		proxied := r.h.faultWindow(fault.id)
		r.timeline.Faults[fault.window] = Window{Start: proxied.First, End: proxied.Retired}
		fault.retired = !proxied.Retired.IsZero()
	}
}

// clearFaults takes every fault off the proxy, which closes its window. The
// teardown does it before it measures anything.
func (r *runner) clearFaults() {
	r.h.clearFaults()
	r.readFaultWindows()
}

// awaitRecovery waits for a target still owed time to recover from the faults.
func (r *runner) awaitRecovery(ctx context.Context) error {
	if now := r.now(); r.violation != nil || r.failed || !r.asOf(now).Recovering(now) {
		return nil
	}
	wait, err := r.wait(ctx, time.Time{})
	if err == nil {
		r.timeline.Recovery = &wait
		// The faults are cleared, and the wait ran until the time they left
		// the target was up, so no fault excuses it.
		err = r.judge(ctx, wait.checkpoint(Recovery), invariant.Input.DeletionOverdue)
	}
	if err != nil {
		r.failed = true
		return fmt.Errorf("the settle wait after the last fault stopped: %w", err)
	}
	return nil
}

// teardown clears the faults, waits for a target still owed time to recover,
// waits out the last quiet window, deletes the CRs and judges their deletion.
// Then it empties the namespace and stops the target. Every step runs even if
// one fails. Its waits end with the caller's context, which abandons the run:
// the teardown then judges nothing and kills the target at once.
func (r *runner) teardown(ctx context.Context) error {
	r.teardownStart = r.now()
	r.clearFaults()
	for i, window := range r.timeline.Faults {
		if window.Start.IsZero() {
			r.skipped = append(r.skipped, fmt.Sprintf("the proxy applied the fault of op %d to no request", r.faultOps[i]))
		}
	}
	failures := []error{r.awaitRecovery(ctx)}

	down, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.teardownBudget())
	defer cancel()
	r.timeline.Quiet.Start = r.now()
	cut := r.h.sleep(ctx, r.target.Timeouts.Stable)

	// Stamped before the delete, not after it: from here on reconciler-fuzzer is
	// the one changing the namespace, and no invariant window reaches past this
	// instant. The sleep above is the last quiet window, and still the target's to
	// answer for.
	r.timeline.Quiet.End = r.now()
	r.timeline.Deletion.Start = r.timeline.Quiet.End
	for _, cr := range r.crs {
		failures = append(failures, r.h.deleteCR(down, cr))
	}
	clean := false
	if cut == nil {
		clean, cut = r.h.awaitClean(ctx, r.target.Timeouts.Delete+deletionMargin)
	}
	r.timeline.Deletion.End = r.now()
	if clean {
		r.timeline.Cleaned = r.timeline.Deletion.End
	}
	switch {
	case r.violation != nil || r.failed:
	case cut != nil:
		failures = append(failures, fmt.Errorf("the teardown: %w", cut))
	default:
		failures = append(failures, r.teardownCheckpoint(ctx, clean))
	}

	// Deleting first keeps a running target from putting back a finalizer it
	// owns, since the API server refuses one new to an object being deleted.
	failures = append(failures, r.h.empty(down))
	forced, err := r.h.forceFinalizers(down)
	r.timeline.Forced = forced
	if len(forced) > 0 && cut == nil {
		// G3's window closed before this, so nothing forced here was ever
		// credited to the target. What a reader would otherwise miss is that
		// the namespace did not empty on its own. An abandoned run gave it no
		// time to.
		r.skipped = append(r.skipped, fmt.Sprintf("the teardown force-removed the finalizers of %s, so the run namespace did not empty on its own",
			strings.Join(forced, ", ")))
	}
	failures = append(failures, err, r.h.stop(ctx))
	for _, owner := range r.h.unresolvedOwners() {
		r.skipped = append(r.skipped, unresolvedNote(owner))
	}
	return errors.Join(failures...)
}

func (r *runner) exitNotes() []string {
	notes := make([]string, len(r.timeline.Exits))
	for i, exit := range r.timeline.Exits {
		notes[i] = fmt.Sprintf("the target exited during %s with %v", r.during(exit.At), exit)
	}
	return notes
}

// forbiddenNotes returns a note naming each distinct verb+resource the API
// server forbade the target, excluding faults the proxy injected.
func (r *runner) forbiddenNotes() []string {
	type pair struct{ verb, resource string }
	seen := map[pair]bool{}
	var pairs []pair
	for _, req := range r.h.requests() {
		if !req.Forbidden() {
			continue
		}
		resource := req.Resource
		if req.Subresource != "" {
			resource += "/" + req.Subresource
		}
		p := pair{req.Verb, resource}
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.verb + " " + p.resource
	}
	return []string{"the API server forbade the target: " + strings.Join(parts, ", ")}
}

// during names what the run was doing at t: the op it had applied last, or the
// teardown.
func (r *runner) during(t time.Time) string {
	if !t.Before(r.teardownStart) {
		return "the teardown"
	}
	var last AppliedOp
	for _, applied := range r.timeline.Ops {
		if !applied.At.After(t) {
			last = applied
		}
	}
	return fmt.Sprintf("op %d (%s)", last.Op.Index, last.Op.Type)
}

// unresolvedNote says why the collector kept an object: it counts an owner it
// cannot resolve as live.
func unresolvedNote(u cluster.Unresolved) string {
	why := "it does not watch " + observe.KindName(u.OwnerKind)
	if u.Unserved {
		why = "the API server does not serve " + observe.KindName(u.OwnerKind)
	}
	return fmt.Sprintf("reconciler-fuzzer's garbage collector never deletes %s %s, because %s, the kind of its owner %s",
		observe.KindName(u.DependentKind), u.DependentName, why, u.OwnerName)
}

// teardownCheckpoint judges the deletion window, unless the target stopped: a
// target that is gone cleaned nothing up.
func (r *runner) teardownCheckpoint(ctx context.Context, clean bool) error {
	if status := r.h.targetStatus(); !status.Running {
		return fmt.Errorf("the teardown: %w", r.targetStopped(ctx, status))
	}
	return r.checkpoint(Checkpoint{At: r.now(), Op: Teardown, Converged: clean}, false)
}

func (r *runner) teardownBudget() time.Duration {
	return r.target.Timeouts.Stable + r.target.Timeouts.Delete + teardownMargin
}

// spec is the fault in the form the proxy injects.
func (f Fault) spec() proxy.FaultSpec {
	var fraction float64 // The proxy reads 0 as every request.
	if f.Match.Fraction != nil {
		fraction = *f.Match.Fraction
	}
	return proxy.FaultSpec{
		Match: proxy.RequestMatcher{
			Verb:     f.Match.Verb,
			Resource: f.Match.Resource,
			Name:     f.Match.Name,
			Fraction: fraction,
		},
		Action: f.Action.action(),
		Until:  proxy.Trigger{Count: f.Until.Count, For: time.Duration(f.Until.For)},
	}
}

func (a Action) action() proxy.FaultAction {
	switch {
	case a.Error != 0:
		return proxy.Error{Code: a.Error}
	case a.Delay != 0:
		return proxy.Delay{For: time.Duration(a.Delay)}
	default:
		return proxy.Drop{}
	}
}
