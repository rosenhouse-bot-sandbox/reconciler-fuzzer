package run

import (
	"fmt"
	"slices"
	"strings"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// Engine is the invariant engine as a Checker: the generic invariants and the
// properties the target declares.
type Engine struct{}

var _ Checker = Engine{}

// Check returns every violation the engine found, in its order of checks, and
// every note. A run ends at the first violation.
func (Engine) Check(in Input) (Findings, error) {
	results, err := Evaluate(in)
	if err != nil {
		return Findings{}, err
	}
	var found Findings
	for _, result := range results {
		for _, violation := range result.Violations {
			found.Violations = append(found.Violations, fromEngine(violation))
		}
		found.Notes = append(found.Notes, result.Notes...)
	}
	return found, nil
}

func fromEngine(violation invariant.Violation) Violation {
	return Violation{
		ID:               violation.ID,
		Statement:        violation.Statement,
		At:               violation.At,
		Evidence:         evidence(violation),
		Requests:         violation.Requests,
		RequestsTotal:    violation.RequestsTotal,
		Versions:         violation.Versions,
		VersionsTotal:    violation.VersionsTotal,
		VersionsOf:       violation.VersionsOf,
		Managed:          violation.Managed,
		ManagedTotal:     violation.ManagedTotal,
		Ready:            violation.Ready,
		Differences:      violation.Differences,
		DifferencesTotal: violation.DifferencesTotal,
		Compared:         violation.Compared,
	}
}

// Evaluate runs every check over the run so far and returns what each one
// found. The bug matrix reads the results; a run reads the violations.
func Evaluate(in Input) ([]invariant.Result, error) {
	return invariant.Evaluate(engineInput(in))
}

func engineInput(in Input) invariant.Input {
	return invariant.Input{
		Target:      in.Target,
		Requests:    in.Requests,
		History:     in.Objects,
		Ops:         engineOps(in.Target, in.Timeline),
		Checkpoints: engineCheckpoints(in.Timeline.Checkpoints),
		Faults:      engineFaults(in.Timeline.Faults),
		Exits:       engineExits(in.Timeline.Exits),
		Teardown:    in.Timeline.Deletion.Start,
		Quiet:       in.Timeline.Quiet.Start,
		Cleaned:     in.Timeline.Cleaned,
		// The deletion window is the last of the run the Observer watched.
		// It is zero until the teardown closes it, and the engine then
		// evaluates at the last checkpoint.
		End: in.Timeline.Deletion.End,
	}
}

// engineOps carries each op's index, which is what a checkpoint names and not
// the op's position in the timeline, the CR a CR op wrote, and the object a
// deleteManaged op deleted: G3 does not credit the target for a cleanup
// reconciler-fuzzer performed.
func engineOps(t *target.Target, timeline Timeline) []invariant.Op {
	ops := make([]invariant.Op, len(timeline.Ops))
	for i, op := range timeline.Ops {
		ops[i] = invariant.Op{Index: op.Op.Index, Type: invariant.OpType(op.Op.Type), Time: op.At, Restored: op.Restored}
		if op.CR != "" {
			ops[i].CR = observe.Key{GVK: t.Primary, Namespace: timeline.Namespace, Name: op.CR}
		}
		if op.Deleted == "" {
			continue
		}
		// The kind resolves: the Runner refuses an op whose kind does not, so
		// no op that deleted an object can carry one.
		gvk, _ := managedKind(t, op.Op.Kind)
		ops[i].Deleted = observe.Key{GVK: gvk, Namespace: timeline.Namespace, Name: op.Deleted}
	}
	return ops
}

func engineCheckpoints(checkpoints []Checkpoint) []invariant.Checkpoint {
	engine := make([]invariant.Checkpoint, len(checkpoints))
	for i, checkpoint := range checkpoints {
		engine[i] = engineCheckpoint(checkpoint)
	}
	return engine
}

func engineCheckpoint(checkpoint Checkpoint) invariant.Checkpoint {
	return invariant.Checkpoint{
		Op: checkpoint.Op, Began: checkpoint.Began, Time: checkpoint.At, Settle: settleResult(checkpoint),
		Stayed: checkpoint.Stayed, Held: checkpoint.Held,
	}
}

// settleResult reads a checkpoint's Converged. The teardown's checkpoint
// follows no settle wait: its Converged says the namespace came clean, which
// is what G3 judges and never an expired wait.
func settleResult(checkpoint Checkpoint) invariant.SettleResult {
	switch {
	case checkpoint.Op == Teardown:
		return invariant.NoSettle
	case checkpoint.Converged:
		return invariant.Converged
	default:
		return invariant.Expired
	}
}

// engineFaults hands over the windows the proxy applied a fault in. A fault
// the proxy applied to no request has no window: it changed nothing about the
// run, so it excuses nothing.
func engineFaults(windows []Window) []invariant.FaultWindow {
	var faults []invariant.FaultWindow
	for _, window := range windows {
		if window.Start.IsZero() {
			continue
		}
		faults = append(faults, invariant.FaultWindow{Start: window.Start, End: window.End})
	}
	return faults
}

func engineExits(exits []Exit) []invariant.Exit {
	engine := make([]invariant.Exit, len(exits))
	for i, exit := range exits {
		engine[i] = invariant.Exit{At: exit.At, Restart: exit.Restart, Why: exit.String()}
	}
	return engine
}

// part writes how much of the evidence the line quotes, which is less than the
// check chose from wherever invariant.MaxEvidence cut it.
func part(shown, total int, noun string) string {
	if shown < total {
		return fmt.Sprintf("%d of %s", shown, count(total, noun))
	}
	return count(shown, noun)
}

// count writes a number of things, in the singular where there is one.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// evidence is what a violation quotes, in one line. The instant it judged is
// a field of its own, and the whole request log and version history stay in
// the run directory.
func evidence(violation invariant.Violation) string {
	var quoted []string
	// The statement names the first object that differs, and how a whole one does.
	differences := violation.Differences
	if i := slices.IndexFunc(differences, invariant.Difference.NamesAField); i >= 0 {
		field := differences[i]
		path := field.Path
		if field.Object != differences[0].Object {
			path += " of the " + field.Object
		}
		clause := fmt.Sprintf("%s was %s, is %s", path, field.Before, field.After)
		if violation.DifferencesTotal > 1 {
			clause += fmt.Sprintf(" (1 of %s)", count(violation.DifferencesTotal, "difference"))
		}
		quoted = append(quoted, clause)
	}
	if requests := violation.Requests; len(requests) > 0 {
		quoted = append(quoted, fmt.Sprintf("%s, the first %s %s %d",
			part(len(requests), violation.RequestsTotal, "request"), requests[0].Verb, requests[0].Path, requests[0].Status))
	}
	if versions := violation.Versions; len(versions) > 0 {
		quoted = append(quoted, fmt.Sprintf("%s, the first %s %s",
			part(len(versions), violation.VersionsTotal, "version"), observe.KindName(versions[0].GVK), versions[0].Name))
	}
	if violation.ManagedTotal != nil {
		quoted = append(quoted, managedClause(*violation.ManagedTotal))
	}
	return strings.Join(quoted, "; ")
}

// managedClause says what the target managed at a violation, over the kinds
// the target declares. The clause names the declaration because a target that
// declares too few kinds managed nothing by that measure while the request log
// shows it creating children.
func managedClause(managed int) string {
	return "the target managed " + count(managed, "object") + " of the kinds it declares"
}
