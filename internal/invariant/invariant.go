// Package invariant checks one run against the generic invariants, which hold
// for every target, and the target's declared properties. Every check is a
// pure function over the proxy's request log, the Observer's history and the
// target declaration.
package invariant

import (
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// OpType is the kind of one op in a sequence.
type OpType string

const (
	OpCreate        OpType = "create"
	OpUpdate        OpType = "update"
	OpDelete        OpType = "delete"
	OpRecreate      OpType = "recreate"
	OpRestart       OpType = "restart"
	OpFault         OpType = "fault"
	OpSettle        OpType = "settle"
	OpDeleteManaged OpType = "deleteManaged"
	OpUpdateFixture OpType = "updateFixture"
	OpDeleteFixture OpType = "deleteFixture"
)

// changesSpec reports whether the op gives the target a new spec to converge
// on, which is what G4 measures from.
func (k OpType) changesSpec() bool {
	return k == OpCreate || k == OpUpdate || k == OpRecreate
}

// touchesCR reports whether the op changed the primary CR, which opens a new
// stretch of unchanged spec.
func (k OpType) touchesCR() bool { return k.changesSpec() || k == OpDelete }

func (k OpType) onFixture() bool { return k == OpUpdateFixture || k == OpDeleteFixture }

// givesInput reports whether the op gave the target a spec or a fixture to
// converge on.
func (op Op) givesInput() bool {
	return op.Type.changesSpec() || op.Type == OpUpdateFixture || op.Restored
}

// Op is one executed step of the sequence.
type Op struct {
	// Index is the op's place in the sequence, which Checkpoint.Op names.
	Index int
	Type  OpType
	Time  time.Time
	// Deleted is the object a DeleteManaged op removed. Every other op carries
	// the zero Key, and so does a DeleteManaged op that deleted nothing.
	Deleted observe.Key
	// CR is the primary CR a CR op wrote. Every other op carries the zero Key.
	CR observe.Key
	// Restored says reconciler-fuzzer created a fixture it had deleted again, just
	// after the op's Time and before the op's own change.
	Restored bool
}

// changesRun reports whether the op changed the CR, a managed object or a
// fixture.
func (op Op) changesRun() bool {
	return op.Type.touchesCR() || op.Deleted != (observe.Key{}) || op.Type.onFixture() || op.Restored
}

// SettleResult is how the settle wait a checkpoint follows ended.
type SettleResult string

const (
	// NoSettle marks a checkpoint that follows no wait, such as teardown's.
	NoSettle  SettleResult = ""
	Converged SettleResult = "converged"
	Expired   SettleResult = "expired"
)

// Teardown and Recovery are the Op of the checkpoints no op opened. Teardown
// follows the teardown's deletion window. Recovery follows the settle wait the
// teardown gives a target still owed time to recover from the faults.
const (
	Teardown = -1
	Recovery = -2
)

// Checkpoint is an instant at which the checks judge the run.
type Checkpoint struct {
	// Op is the Index of the op the checkpoint follows, Teardown or Recovery.
	Op int
	// Began is when the settle wait that ends here began. The teardown's
	// checkpoint follows no wait and leaves it zero.
	Began  time.Time
	Time   time.Time
	Settle SettleResult
	// Stayed marks a recreate's wait for its old CR to go, which ended with
	// the CR still there. timeouts.delete bounds that wait.
	Stayed bool
	// Held marks a checkpoint where the proxy held a request of the
	// target's, or released one within timeouts.stable, which may still change
	// what the checks read.
	Held bool
}

// FaultWindow is a period in which a fault was active. Every invariant
// ignores what a fault reached into. A zero End means the fault outlived the
// run.
type FaultWindow struct{ Start, End time.Time }

// overlaps reports whether the fault was active anywhere in [from, to]. A
// fault that stopped at from did not reach into the window, which is what
// lets G4 measure from the instant a fault stopped.
func (f FaultWindow) overlaps(from, to time.Time) bool {
	return !f.Start.After(to) && (f.End.IsZero() || f.End.After(from))
}

// Exit is one time the target stopped on its own, and when reconciler-fuzzer
// started it again. Why is its error and what the target wrote as it stopped.
type Exit struct {
	At, Restart time.Time
	Why         string
}

// Input is one run as the checks see it.
type Input struct {
	Target   *target.Target
	Requests []proxy.Request
	// History is what the Observer recorded. The checks hand the target's
	// predicates the objects it holds, which a predicate must not modify.
	History     *observe.Store
	Ops         []Op
	Checkpoints []Checkpoint
	Faults      []FaultWindow
	Exits       []Exit
	// Teardown is when reconciler-fuzzer began emptying the run namespace. What
	// changes after it is reconciler-fuzzer's own doing, so no window reaches past
	// it. G3 judges the deletion it opens.
	Teardown time.Time
	// Quiet is when the teardown began waiting timeouts.stable, the run's last
	// quiet window. The window closes at Teardown.
	Quiet time.Time
	// Cleaned is when reconciler-fuzzer saw the run namespace empty: every managed
	// object gone and the CR with it. It satisfies G3's deletion for every
	// deadline at or after it. Zero means the namespace never emptied.
	Cleaned time.Time
	// End is the instant the engine evaluates at, after which the run is
	// unobserved. A zero End takes the last checkpoint's time.
	End time.Time
}

// Violation is one failure with the evidence a report quotes.
type Violation struct {
	ID        string            `json:"id"`
	Statement string            `json:"statement"`
	At        time.Time         `json:"at"`
	Requests  []proxy.Request   `json:"requests,omitempty"`
	Versions  []observe.Version `json:"versions,omitempty"`
	// RequestsTotal and VersionsTotal are how many entries each excerpt above
	// was chosen from, which a report needs to say what the bound left out.
	// The quotingRequests and quotingVersions methods set each pair together.
	RequestsTotal int `json:"requestsTotal,omitempty"`
	VersionsTotal int `json:"versionsTotal,omitempty"`
	// VersionsOf names the object the timeline is the history of, and is
	// empty where the versions are of several.
	VersionsOf string `json:"versionsOf,omitempty"`
	// Managed is the state at At: one version of each object the target
	// managed, which a check that judges the CR's readiness quotes as a table
	// of its own. ManagedTotal is how many it was chosen from. A check that
	// does not quote the state leaves the total nil, so that a total of zero
	// says the target managed nothing.
	Managed      []observe.Version `json:"managed,omitempty"`
	ManagedTotal *int              `json:"managedTotal,omitempty"`
	// Ready is what a readiness verdict read. Other checks leave it nil.
	Ready *Readiness `json:"ready,omitempty"`
	// Differences are what G5 found changed across a restart, and
	// DifferencesTotal how many there were. Compared names the two states.
	Differences      []Difference `json:"differences,omitempty"`
	DifferencesTotal int          `json:"differencesTotal,omitempty"`
	Compared         string       `json:"compared,omitempty"`
}

// Readiness is the Ready predicate at a verdict and the CR it read.
type Readiness struct {
	// Expr is the target's ready: CEL, or go:<name>.
	Expr string `json:"expr,omitempty"`
	// CR is the name of the CR it read.
	CR string `json:"cr,omitempty"`
	// Error is what evaluating Expr on the CR returned.
	Error string `json:"error,omitempty"`
	// Status is the CR's status, unbounded. A report bounds it.
	Status map[string]any `json:"status,omitempty"`
}

// Difference is one field that differs between two versions of an object,
// with its value in each. Path is empty where one version is absent, whose
// resourceVersion is then empty, or where the target's own equality compared
// the whole object.
type Difference struct {
	Object           string    `json:"object"`
	ResourceVersions [2]string `json:"resourceVersions"`
	Path             string    `json:"path"`
	Before           string    `json:"before"`
	After            string    `json:"after"`
}

// NamesAField says whether Path names what differs, which it does unless the
// difference is in the object as a whole.
func (d Difference) NamesAField() bool { return d.Path != "" }

// whole is a difference in the object as a whole.
func whole(object Difference, before, after string) Difference {
	object.Before, object.After = before, after
	return object
}

// Result is what one check found.
type Result struct {
	ID         string      `json:"id"`
	Violations []Violation `json:"violations,omitempty"`
	// Notes record what the check could not evaluate, which the report prints.
	Notes []string `json:"notes,omitempty"`
}

// Check is one invariant or property. Its error is a configuration error,
// never a finding.
type Check func(Input) (Result, error)

// Generic returns the invariants every target is held to, in ID order.
func Generic() []Check {
	return []Check{BoundedReconciliation, NoChurn, CleanDeletion, Convergence, RestartStable, NoErrorLoop, SelfHealing}
}

// Evaluate runs every generic invariant and every property the target
// declares.
func Evaluate(in Input) ([]Result, error) {
	checks := Generic()
	for _, declared := range in.Target.Properties {
		checks = append(checks, Property(declared))
	}
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		result, err := check(in)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// quotingRequests carries an excerpt of the request log into the violation
// with the number it was chosen from, so that the two cannot disagree.
func (v Violation) quotingRequests(e Excerpt[proxy.Request]) Violation {
	v.Requests, v.RequestsTotal = e.Quoted, e.Total
	return v
}

// quotingVersions does the same for a timeline of object versions.
func (v Violation) quotingVersions(e Excerpt[observe.Version]) Violation {
	v.Versions, v.VersionsTotal, v.VersionsOf = e.Quoted, e.Total, e.Of
	return v
}

// quotingDifferences does the same for what changed across a restart.
func (v Violation) quotingDifferences(e Excerpt[Difference]) Violation {
	v.Differences, v.DifferencesTotal = e.Quoted, e.Total
	return v
}

// quotingManaged does the same for the state at the verdict. It sets the
// total even at zero, which says the target managed nothing.
func (v Violation) quotingManaged(e Excerpt[observe.Version]) Violation {
	v.Managed, v.ManagedTotal = e.Quoted, &e.Total
	return v
}

// violate appends a violation carrying the result's ID.
func (r *Result) violate(v Violation) {
	v.ID = r.ID
	r.Violations = append(r.Violations, v)
}

// note records what the check left unjudged, naming the check, so that a
// reader can tell a skipped check from a passing one.
func (r *Result) note(format string, args ...any) {
	r.Notes = append(r.Notes, r.ID+" is not evaluated "+fmt.Sprintf(format, args...))
}

// timeouts are the target's, with the defaults wherever it declares none.
func (in Input) timeouts() target.Timeouts {
	declared := in.Target.Timeouts
	if declared.Settle <= 0 {
		declared.Settle = target.DefaultTimeouts.Settle
	}
	if declared.Stable <= 0 {
		declared.Stable = target.DefaultTimeouts.Stable
	}
	if declared.Delete <= 0 {
		declared.Delete = target.DefaultTimeouts.Delete
	}
	return declared
}

// errLoop is G6's threshold, thresholds.errloop.
func (in Input) errLoop() int {
	if in.Target.Thresholds.ErrLoop > 0 {
		return in.Target.Thresholds.ErrLoop
	}
	return target.DefaultThresholds.ErrLoop
}

// quietAllowance is thresholds.quiet: the requests G1 and the status writes
// G2 allow in one quiet window.
func (in Input) quietAllowance() int { return max(in.Target.Thresholds.Quiet, 0) }

// end is the instant the run stops being observed.
func (in Input) end() time.Time {
	if !in.End.IsZero() {
		return in.End
	}
	if n := len(in.Checkpoints); n > 0 {
		return in.Checkpoints[n-1].Time
	}
	return time.Time{}
}

// observed reports whether the run was watched through t.
func (in Input) observed(t time.Time) bool {
	end := in.end()
	return !end.IsZero() && !end.Before(t)
}

// faulted reports whether a fault was active anywhere in [from, to].
func (in Input) faulted(from, to time.Time) bool {
	for _, fault := range in.Faults {
		if fault.overlaps(from, to) {
			return true
		}
	}
	return false
}

// versions returns every version the Observer recorded up to the evaluation
// time, oldest first. One informer per kind writes the history, so the
// recorded order can invert; the checks read it as a timeline.
func (in Input) versions() []observe.Version {
	return in.versionsIn(time.Time{}, in.end())
}

// versionsIn returns the versions the Observer recorded in [from, to], oldest
// first.
func (in Input) versionsIn(from, to time.Time) []observe.Version {
	if in.History == nil {
		return nil
	}
	window := in.History.Window(from, to)
	slices.SortStableFunc(window, func(a, b observe.Version) int { return a.Time.Compare(b.Time) })
	return window
}

// opBy returns the last op applied by t.
func (in Input) opBy(t time.Time) Op {
	var last Op
	for _, op := range in.Ops {
		if !op.Time.After(t) {
			last = op
		}
	}
	return last
}

// opBefore returns the last op applied before t.
func (in Input) opBefore(t time.Time) Op {
	var last Op
	for _, op := range in.Ops {
		if op.Time.Before(t) {
			last = op
		}
	}
	return last
}

// op returns the op of this index, which is not its position: an Input may
// carry a subset of the sequence.
func (in Input) op(index int) (Op, bool) {
	for _, op := range in.Ops {
		if op.Index == index {
			return op, true
		}
	}
	return Op{}, false
}
