//go:build envtest

package run_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/invariant"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// toySequence drives the toy through every op the Runner executes in M3.
const toySequence = `{
  "seed": 20260920,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 3}}},
    {"i": 1, "t": "update", "patch": {"spec": {"count": 5}}},
    {"i": 2, "t": "settle"},
    {"i": 3, "t": "restart"},
    {"i": 4, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 5, "t": "delete"}
  ]
}`

// oneOutlivesTheOther deletes one Widget while another lives, and runs past
// the first's deletion deadline before the teardown cleans up.
const oneOutlivesTheOther = `{
  "seed": 20260920,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget-2"}, "spec": {"count": 1}}},
    {"i": 2, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 3, "t": "delete"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "update", "cr": "widget-2", "patch": {"spec": {"count": 2}}},
    {"i": 7, "t": "restart"},
    {"i": 8, "t": "settle"}
  ]
}`

// oneCreate settles, so the run waits for a reaction from a target that has
// stopped.
const oneCreate = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}}
  ]
}`

const createThenDelete = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "delete"}
  ]
}`

const createThenRecreate = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "recreate", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}}
  ]
}`

const createThenDeleteManaged = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0}
  ]
}`

const createThenRestart = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "restart"},
    {"i": 2, "t": "settle"}
  ]
}`

const restartThenDeleteManaged = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "restart"},
    {"i": 2, "t": "settle"},
    {"i": 3, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0}
  ]
}`

const changeThenDeleteTheFixture = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "updateFixture", "kind": "v1/ConfigMap", "name": "widget-config", "patch": {"data": {"label": "blue"}}},
    {"i": 2, "t": "deleteFixture", "kind": "v1/ConfigMap", "name": "widget-config", "until": {"op": 3}},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"}
  ]
}`

const deleteTheFixture = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "deleteFixture", "kind": "v1/ConfigMap", "name": "fixture", "until": {"op": 2}},
    {"i": 2, "t": "settle"}
  ]
}`

const deleteTheFixtureOverASettle = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "deleteFixture", "kind": "v1/ConfigMap", "name": "fixture", "until": {"op": 3}},
    {"i": 2, "t": "settle"},
    {"i": 3, "t": "settle"}
  ]
}`

// recreatingChecker judges as the engine does. At the checkpoint of the op
// that at names, it then creates the fixture ConfigMap, as something other
// than reconciler-fuzzer might.
type recreatingChecker struct {
	configMaps dynamic.NamespaceableResourceInterface
	at         int
}

func (c recreatingChecker) Check(in run.Input) (run.Findings, error) {
	findings, err := run.Engine{}.Check(in)
	if last := in.Timeline.Checkpoints[len(in.Timeline.Checkpoints)-1]; err == nil && last.Op == c.at {
		_, err = c.configMaps.Namespace(in.Timeline.Namespace).Create(context.Background(), fixtureConfigMap(), metav1.CreateOptions{})
	}
	return findings, err
}

const twoWidgets = `{
  "seed": 1,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget"}, "spec": {"count": 1}}},
    {"i": 1, "t": "create", "obj": {"apiVersion": "toy.reconciler-fuzzer/v1", "kind": "Widget", "metadata": {"name": "widget-2"}, "spec": {"count": 1}}}
  ]
}`

// checkpointState is what a check saw when it ran.
type checkpointState struct {
	op          int
	converged   bool
	managed     []string
	managedCRs  []string
	ready       int64
	widgetWatch int
}

// recordingChecker records what each checkpoint was given, and reports fail if
// it is set.
type recordingChecker struct {
	fail        *run.Violation
	checkpoints []checkpointState
}

func (c *recordingChecker) Check(in run.Input) (run.Findings, error) {
	last := in.Timeline.Checkpoints[len(in.Timeline.Checkpoints)-1]
	state := checkpointState{op: last.Op, converged: last.Converged}
	for _, managed := range in.Objects.Managed() {
		state.managed = append(state.managed, managed.Name)
		if managed.GVK == in.Target.Primary {
			state.managedCRs = append(state.managedCRs, managed.Name)
		}
	}
	for _, cr := range in.Objects.Current(in.Target.Primary) {
		state.ready, _, _ = unstructured.NestedInt64(cr.Object.Object, "status", "ready")
	}
	for _, request := range in.Requests {
		if request.Verb == "watch" && request.Resource == "widgets" {
			state.widgetWatch++
		}
	}
	c.checkpoints = append(c.checkpoints, state)
	if c.fail != nil {
		return run.Findings{Violations: []run.Violation{*c.fail}}, nil
	}
	return run.Findings{}, nil
}

func (c *recordingChecker) at(op int) (checkpointState, bool) {
	for _, state := range c.checkpoints {
		if state.op == op {
			return state, true
		}
	}
	return checkpointState{}, false
}

func (c *recordingChecker) ops() []int {
	var ops []int
	for _, state := range c.checkpoints {
		ops = append(ops, state.op)
	}
	return ops
}

// staleObserver judges as the engine does, over an Observer that lags the API
// server: from op 0's checkpoint it holds a managed ConfigMap the API server
// does not, and it sees the ConfigMap go at op 1. The ConfigMap has no
// creationTimestamp, so it sorts first.
type staleObserver struct{}

const vanished = "vanished"

func (staleObserver) Check(in run.Input) (run.Findings, error) {
	gone := &unstructured.Unstructured{}
	gone.SetGroupVersionKind(configMapKind)
	gone.SetNamespace(in.Timeline.Namespace)
	gone.SetName(vanished)
	gone.SetUID("uid-" + vanished)
	gone.SetResourceVersion("1")
	switch last := in.Timeline.Checkpoints[len(in.Timeline.Checkpoints)-1]; last.Op {
	case 0:
		in.Objects.Record(configMapKind, gone, last.At)
	case 1:
		in.Objects.RecordDeletion(configMapKind, gone, in.Timeline.Ops[1].At)
	}
	return run.Engine{}.Check(in)
}

func TestRunner(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	binary := buildToy(t)
	testCluster := startCluster(t, loadTarget(t, binary).CRDs)

	t.Run("drives the toy through a sequence and checkpoints where the design says", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Fixtures = append(toy.Fixtures, fixtureSecret())
		check := &recordingChecker{}
		dir := t.TempDir()

		result, err := run.Run(ctx, toy, readSequence(t, toySequence), run.Options{
			Dir: dir, Config: testCluster.Config(), Check: check,
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %+v, want none: the toy runs without a bug.", result.Violation)
		}
		requireCheckpointsOfSection4(t, check, result)
		requireOpsTookEffect(t, check, result)
		requireNamespaceEmpty(t, ctx, testCluster.Config(), result.Timeline.Namespace)
		if len(result.Timeline.Forced) > 0 {
			t.Errorf("The teardown forced the finalizers off %v, want the toy to clear its own.", result.Timeline.Forced)
		}
	})

	t.Run("stops at the first violation and leaves the evidence in run-1", func(t *testing.T) {
		toy := loadTarget(t, binary)
		failed := run.Violation{ID: "P1", Statement: "status.ready never exceeds the ConfigMaps present"}
		check := &recordingChecker{fail: &failed}
		out, err := run.OpenOutput(t.TempDir(), 20260920, time.Now())
		if err != nil {
			t.Fatalf("Opening the output directory failed: %v", err)
		}

		result, err := run.Run(ctx, toy, readSequence(t, toySequence), run.Options{
			Dir: out.RunDir(1), Config: testCluster.Config(), Check: check,
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != failed.ID {
			t.Fatalf("The run reported %+v, want the violation the check gave it.", result.Violation)
		}
		if len(check.checkpoints) != 1 {
			t.Errorf("The checks ran at %v, want the run to end at the first violation.", check.ops())
		}
		if len(result.Timeline.Ops) != 1 {
			t.Errorf("The run applied %d ops, want it to stop after the first violation.", len(result.Timeline.Ops))
		}
		requireRunFiles(t, out.RunDir(1))
		requireNamespaceEmpty(t, ctx, testCluster.Config(), result.Timeline.Namespace)
	})

	t.Run("changes, deletes and restores the toy's fixture, which stays reconciler-fuzzer's", func(t *testing.T) {
		toy := loadTarget(t, binary)

		result, err := run.Run(ctx, toy, readSequence(t, changeThenDeleteTheFixture), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if len(result.Notes) > 0 {
			t.Errorf("The run noted %q, want every check to judge.", result.Notes)
		}
		store := result.Recorded.Objects
		fixture := observe.Key{GVK: configMapKind, Namespace: result.Timeline.Namespace, Name: "widget-config"}
		var labels []string
		for _, version := range store.History(fixture) {
			label, _, _ := unstructured.NestedString(version.Object.Object, "data", "label")
			if version.Deleted {
				label = "deleted"
			}
			labels = append(labels, label)
		}
		// The teardown deletes it last.
		if want := []string{"red", "blue", "deleted", "blue", "deleted"}; !slices.Equal(labels, want) {
			t.Errorf("The fixture went through %v, want %v.", labels, want)
		}
		if store.IsManaged(fixture) {
			t.Errorf("The fixture reconciler-fuzzer restored counts as the target's.")
		}
		child := store.HistoryOf(configMapKind, "widget-0")
		if label, _, _ := unstructured.NestedString(child[len(child)-1].Object.Object, "data", "label"); label != "blue" {
			t.Errorf("The toy's child last carried the label %q, want the fixture's blue.", label)
		}
	})

	// b10.json scales the toy down right after a restart, before anything
	// settles, so reconciler-fuzzer's update lies between the states G5 compares.
	t.Run("passes the toy without a bug on b10.json and notes the restart", func(t *testing.T) {
		toy := loadTarget(t, binary)
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b10.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if !slices.ContainsFunc(result.Notes, func(note string) bool {
			return strings.HasPrefix(note, "G5") && strings.Contains(note, "for op 1 (restart): op 2 (update) ran")
		}) {
			t.Errorf("The run noted %q, want G5 to say the update of op 2 kept it from judging the restart of op 1.", result.Notes)
		}
	})

	// The children of the Widget that lives stay past the other's deadline, and
	// belong to it.
	t.Run("passes the toy without a bug where one Widget outlives another", func(t *testing.T) {
		toy := loadTarget(t, binary)

		result, err := run.Run(ctx, toy, readSequence(t, oneOutlivesTheOther), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if len(result.Notes) > 0 {
			t.Errorf("The run noted %q, want every check to judge.", result.Notes)
		}
		deleted := result.Timeline.Ops[3].At
		if passed := result.Timeline.Deletion.Start.Sub(deleted); passed <= toy.Timeouts.Delete {
			t.Errorf("The teardown began %v after the first Widget's delete, so G3 never judged its deadline %v on.",
				passed, toy.Timeouts.Delete)
		}
	})

	t.Run("never counts a CR it created as managed, where the target manages its primary kind", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Manages = append(toy.Manages, toy.Primary)
		check := &recordingChecker{}

		_, err := run.Run(ctx, toy, readSequence(t, twoWidgets), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: check,
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if _, checked := check.at(1); !checked {
			t.Fatalf("The checks ran after the ops %v, want op 1, which creates widget-2.", check.ops())
		}
		for _, state := range check.checkpoints {
			if len(state.managedCRs) > 0 {
				t.Errorf("After op %d the run counted the Widgets %v as managed, want none: reconciler-fuzzer created them.",
					state.op, state.managedCRs)
			}
		}
	})

	// B8 never recreates the child reconciler-fuzzer deleted, and the toy's P1 is
	// not what catches it.
	t.Run("fails G7 where b8.json deletes a child B8 never recreates", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, "--bug=8")
		toy.Properties = nil
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b8.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != "G7" {
			t.Fatalf("The run reported %v, want G7.", result.Violation)
		}
		if want := "the v1/ConfigMap widget-0 that op 1 (deleteManaged) deleted never came back"; !strings.HasPrefix(result.Violation.Statement, want) {
			t.Errorf("G7 says %q, want it to begin %q.", result.Violation.Statement, want)
		}
	})

	// A target can delete the object a deleteManaged op resolved to before
	// reconciler-fuzzer deletes it. The op then deleted nothing, so G7 has nothing
	// to require back.
	t.Run("notes a deleteManaged whose object was gone", func(t *testing.T) {
		toy := loadTarget(t, binary)

		result, err := run.Run(ctx, toy, readSequence(t, createThenDeleteManaged), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: staleObserver{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: reconciler-fuzzer deleted nothing.", result.Violation)
		}
		want := "op 1 (deleteManaged) deleted nothing: index 0 resolved to the v1/ConfigMap " + vanished + ", which was gone before reconciler-fuzzer could delete it"
		if !slices.Equal(result.Notes, []string{want}) {
			t.Errorf("The run noted %q, want %q.", result.Notes, want)
		}
	})

	t.Run("passes the toy without a bug on b8.json", func(t *testing.T) {
		toy := loadTarget(t, binary)
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b8.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if len(result.Notes) > 0 {
			t.Errorf("The run noted %q, want every check to judge.", result.Notes)
		}
	})

	// B8 never recreates the child reconciler-fuzzer deleted until a restart does.
	// P1 and G7 would end the run before the restart, so the target declares no
	// property and leaves ConfigMaps deleted.
	t.Run("names what the restart of b8.json brought back", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Launch.Args = append(toy.Launch.Args, "--bug=8")
		toy.Properties = nil
		toy.NotRecreated = toy.Manages
		sequence, err := run.ReadSequence(repoRoot + "/targets/toy-widget/sequences/b8.json")
		if err != nil {
			t.Fatalf("Reading the sequence failed: %v", err)
		}

		result, err := run.Run(ctx, toy, sequence, run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != "G5" {
			t.Fatalf("The run reported %v, want G5.", result.Violation)
		}
		got := result.Violation.Differences
		if len(got) != 1 {
			t.Fatalf("G5 quoted %+v, want the ConfigMap the restart brought back.", got)
		}
		want := invariant.Difference{
			Object: "v1/ConfigMap widget-0", ResourceVersions: [2]string{"", got[0].ResourceVersions[1]},
			Before: "(absent)", After: "(present)",
		}
		if got[0] != want || want.ResourceVersions[1] == "" {
			t.Errorf("G5 quoted %+v, want %+v with the resourceVersion the restart created.", got[0], want)
		}
		if want := "the state converged after op 1 (deleteManaged) and the one after op 3 (settle)"; result.Violation.Compared != want {
			t.Errorf("G5 says it compared %q, want %q.", result.Violation.Compared, want)
		}
	})

	// A deletionTimestamp holds whole seconds, so a 4s delay holds the
	// finalizer 3s to 4s past the delete.
	t.Run("passes a delete whose cleanup outlasts T_settle", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Timeouts = target.Timeouts{Settle: 2 * time.Second, Stable: time.Second, Delete: 6 * time.Second}
		toy.Launch.Args = append(toy.Launch.Args, "--cleanup-delay=4s")

		result, err := run.Run(ctx, toy, readSequence(t, createThenDelete), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy cleaned up within T_delete.", result.Violation)
		}
		if wait := result.Timeline.Ops[1].Settled; wait == nil || !wait.Converged || wait.Window.End.Sub(wait.Window.Start) <= toy.Timeouts.Settle {
			t.Errorf("The delete's settle wait was %+v, want one that converged after T_settle of %v.", wait, toy.Timeouts.Settle)
		}
	})

	t.Run("passes a recreate whose cleanup outlasts T_settle", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Timeouts = target.Timeouts{Settle: 2 * time.Second, Stable: time.Second, Delete: 6 * time.Second}
		toy.Launch.Args = append(toy.Launch.Args, "--cleanup-delay=4s")

		result, err := run.Run(ctx, toy, readSequence(t, createThenRecreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy cleaned up within T_delete.", result.Violation)
		}
		if wait := result.Timeline.Ops[1].Settled; wait == nil || !wait.Converged {
			t.Errorf("The recreate's settle wait was %+v, want one that converged.", wait)
		}
	})

	for op, sequence := range map[string]string{"delete": createThenDelete, "recreate": createThenRecreate} {
		t.Run("fails G3 on a finalizer that never clears, under a "+op, func(t *testing.T) {
			toy := loadTarget(t, binary)
			toy.Timeouts = target.Timeouts{Settle: 2 * time.Second, Stable: time.Second, Delete: 4 * time.Second}
			toy.Launch.Args = append(toy.Launch.Args, "--bug=13")

			result, err := run.Run(ctx, toy, readSequence(t, sequence), run.Options{
				Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
			})

			if err != nil {
				t.Fatalf("The run failed: %v", err)
			}
			if result.Violation == nil || result.Violation.ID != "G3" {
				t.Fatalf("The run reported %v, want G3.", result.Violation)
			}
			if want := "the CR widget still carried the finalizers [widget.reconciler-fuzzer/cleanup] 4s (timeouts.delete) after its deletion"; result.Violation.Statement != want {
				t.Errorf("G3 says %q, want %q.", result.Violation.Statement, want)
			}
			if want := "toy.reconciler-fuzzer/v1/Widget widget"; result.Violation.VersionsOf != want || len(result.Violation.Versions) == 0 {
				t.Errorf("G3 quotes %d versions of %q, want the history of %s.", len(result.Violation.Versions), result.Violation.VersionsOf, want)
			}
		})
	}

	// The target stands in for one that owns the fixture's finalizer, such as
	// external-secrets on its SecretStore.
	t.Run("forces a fixture's finalizer off, though the target puts it back, and notes it", func(t *testing.T) {
		const hold = "example.com/hold"
		toy := loadTarget(t, binary)
		held := fixtureSecret()
		held.SetFinalizers([]string{hold})
		toy.Fixtures = append(toy.Fixtures, held)
		client, err := dynamic.NewForConfig(testCluster.Config())
		if err != nil {
			t.Fatalf("Building a client failed: %v", err)
		}
		secrets := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"})
		putBackFinalizer(t, secrets, fixtureName, hold)

		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: &recordingChecker{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if want := []string{"v1/Secret " + fixtureName}; !slices.Equal(result.Timeline.Forced, want) {
			t.Errorf("The teardown forced the finalizers off %v, want %v.", result.Timeline.Forced, want)
		}
		if want := "the teardown force-removed the finalizers of v1/Secret " + fixtureName; !slices.ContainsFunc(result.Notes, func(note string) bool {
			return strings.HasPrefix(note, want)
		}) {
			t.Errorf("The run carried the notes %q, want one saying %q.", result.Notes, want)
		}
		left, err := secrets.Namespace(result.Timeline.Namespace).Get(ctx, fixtureName, metav1.GetOptions{})
		switch {
		case err == nil:
			t.Errorf("The fixture outlived the run with the finalizers %v, want it gone.", left.GetFinalizers())
		case !apierrors.IsNotFound(err):
			t.Errorf("Reading the fixture after the run returned %v, want NotFound.", err)
		}
	})

	// A target may hold a fixture's deletion with a finalizer of its own, as
	// external-secrets does its SecretStore's.
	t.Run("restores a deleted fixture once a finalizer lets it go", func(t *testing.T) {
		toy := loadTarget(t, binary)
		held := fixtureConfigMap()
		held.SetFinalizers([]string{"example.com/hold"})
		toy.Fixtures = append(toy.Fixtures, held)
		client, err := dynamic.NewForConfig(testCluster.Config())
		if err != nil {
			t.Fatalf("Building a client failed: %v", err)
		}
		releaseOnDelete(t, client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}), fixtureName, time.Second)

		result, err := run.Run(ctx, toy, readSequence(t, deleteTheFixture), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
	})

	t.Run("ends the run where a finalizer holds a deleted fixture past T_delete", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Timeouts.Delete = 2 * time.Second
		held := fixtureConfigMap()
		held.SetFinalizers([]string{"example.com/hold"})
		toy.Fixtures = append(toy.Fixtures, held)

		_, err := run.Run(ctx, toy, readSequence(t, deleteTheFixture), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		want := "op 1 (deleteFixture): the fixture v1/ConfigMap fixture was still there 2s (timeouts.delete) after reconciler-fuzzer deleted it, held by the finalizers [example.com/hold]"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("The run returned %v, want an error saying %q.", err, want)
		}
	})

	t.Run("ends the run where something creates a deleted fixture again", func(t *testing.T) {
		toy := loadTarget(t, binary)
		toy.Fixtures = append(toy.Fixtures, fixtureConfigMap())
		client, err := dynamic.NewForConfig(testCluster.Config())
		if err != nil {
			t.Fatalf("Building a client failed: %v", err)
		}
		check := recreatingChecker{configMaps: client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}), at: 2}

		_, err = run.Run(ctx, toy, readSequence(t, deleteTheFixtureOverASettle), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: check,
		})

		want := "op 3 (settle): something created the fixture v1/ConfigMap fixture again after reconciler-fuzzer deleted it"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("The run returned %v, want an error saying %q.", err, want)
		}
	})

	t.Run("notes an owner the collector cannot resolve", func(t *testing.T) {
		toy := loadTarget(t, binary)
		ownedBySecret := fixtureConfigMap()
		ownedBySecret.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "v1", Kind: "Secret", Name: "absent", UID: "8a1d0f2c-5b3e-4d7a-9c6f-1e2b3c4d5e6f",
		}})
		toy.Fixtures = append(toy.Fixtures, ownedBySecret)

		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: &recordingChecker{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		want := "reconciler-fuzzer's garbage collector never deletes v1/ConfigMap " + fixtureName +
			", because it does not watch v1/Secret, the kind of its owner absent"
		if !slices.Equal(result.Notes, []string{want}) {
			t.Errorf("The run carried the notes %q, want %q.", result.Notes, want)
		}
	})

	// Only its requests show reconciler-fuzzer that a restarted target is back,
	// and the wait gives it T_settle from there.
	t.Run("passes a target that takes a while to come back from a restart", func(t *testing.T) {
		const delay = 3500 * time.Millisecond
		toy := loadTarget(t, restartsAfter(t, binary, fmt.Sprintf("sleep %v", delay.Seconds())))
		toy.Timeouts = target.Timeouts{Settle: 5 * time.Second, Stable: 2 * time.Second, Delete: 10 * time.Second}

		result, err := run.Run(ctx, toy, readSequence(t, restartThenDeleteManaged), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation != nil {
			t.Errorf("The run reported %s, want none: the toy runs without a bug.", result.Violation)
		}
		if len(result.Notes) > 0 {
			t.Errorf("The run noted %q, want every check to judge.", result.Notes)
		}
		if wait := result.Timeline.Ops[2].Settled; wait == nil || !wait.Converged || wait.Window.End.Sub(wait.Window.Start) < delay+toy.Timeouts.Stable {
			t.Errorf("The settle wait after the restart was %+v, want one that converged once the toy had been back for stable, after %v.",
				wait, delay+toy.Timeouts.Stable)
		}
	})

	t.Run("fails G4 on a target that never comes back from a restart", func(t *testing.T) {
		toy := loadTarget(t, restartsAfter(t, binary, "exec sleep 600"))
		toy.Timeouts = target.Timeouts{Settle: 3 * time.Second, Stable: time.Second, Delete: 2 * time.Second}

		result, err := run.Run(ctx, toy, readSequence(t, createThenRestart), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: run.Engine{},
		})

		if err != nil {
			t.Fatalf("The run failed: %v", err)
		}
		if result.Violation == nil || result.Violation.ID != "G4" {
			t.Fatalf("The run reported %v, want G4.", result.Violation)
		}
		const want = "the settle wait after op 2 (settle) expired with no fault active"
		const why = "but the target had requested no resource outside leader election since op 1 (restart)"
		if statement := result.Violation.Statement; !strings.HasPrefix(statement, want) || !strings.Contains(statement, why) {
			t.Errorf("G4 says %q, want it to begin %q and say %q.", statement, want, why)
		}
	})

	// A target that dies mid-run takes the run with it, and no wait outlives
	// it (DESIGN.md §5.5).
	t.Run("ends the settle wait where the target stopped", func(t *testing.T) {
		const ranFor = 2 * time.Second
		toy := loadTarget(t, diesAfter(t, ranFor, "toy-widget: bind: address already in use"))
		// A target that is gone never converges, so a wait that ignored its
		// exit would take all of T_settle.
		toy.Timeouts.Settle = 30 * time.Second
		toy.Timeouts.Stable = time.Second
		toy.Timeouts.Delete = time.Second

		result, err := run.Run(ctx, toy, readSequence(t, oneCreate), run.Options{
			Dir: t.TempDir(), Config: testCluster.Config(), Check: &recordingChecker{},
		})

		if err == nil {
			t.Fatal("The run reported no error although the target had stopped.")
		}
		for _, want := range []string{"op 0 (create)", "no longer running", "bind: address already in use"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("The run reported %q, which does not mention %q.", err, want)
			}
		}
		if result.Violation != nil {
			t.Errorf("The run reported %+v against the target; a target that stopped is the harness's failure.", result.Violation)
		}
		if len(result.Timeline.Ops) != 1 || result.Timeline.Ops[0].Settled == nil {
			t.Fatalf("The run recorded %+v, want the create and the wait that followed it.", result.Timeline.Ops)
		}
		window := result.Timeline.Ops[0].Settled.Window
		if waited := window.End.Sub(window.Start); waited > ranFor+5*time.Second {
			t.Errorf("The settle wait took %v, want it to end where the target stopped, %v in.", waited, ranFor)
		}
	})
}

// diesAfter is a target that runs for d, says why it is stopping and exits.
func diesAfter(t *testing.T, d time.Duration, says string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dies-after")
	script := fmt.Sprintf("#!/bin/sh\nsleep %v\necho %q >&2\nexit 1\n", d.Seconds(), says)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// restartsAfter is the toy behind a script that runs then before every start
// but the first.
func restartsAfter(t *testing.T, toy, then string) string {
	t.Helper()
	dir := t.TempDir()
	path, started := filepath.Join(dir, "restarts-after"), filepath.Join(dir, "started")
	script := fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then %s; fi\n: > %q\nexec %q \"$@\"\n", started, then, started, toy)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// putBackFinalizer adds the finalizer back to every live object of that name
// that lacks it, until the test ends.
func putBackFinalizer(t *testing.T, resource dynamic.NamespaceableResourceInterface, name, finalizer string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := resource.Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		t.Fatalf("Watching for %s failed: %v", name, err)
	}
	patch := fmt.Appendf(nil, `{"metadata":{"finalizers":[%q]}}`, finalizer)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range watcher.ResultChan() {
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok || object.GetDeletionTimestamp() != nil || slices.Contains(object.GetFinalizers(), finalizer) {
				continue
			}
			// The API server refuses a finalizer new to an object being deleted.
			_, _ = resource.Namespace(object.GetNamespace()).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		}
	}()
	t.Cleanup(func() {
		cancel()
		watcher.Stop()
		<-done
	})
}

// releaseOnDelete takes the finalizers off the object of that name, in any
// namespace, once it has been under deletion for the delay.
func releaseOnDelete(t *testing.T, resource dynamic.NamespaceableResourceInterface, name string, delay time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := resource.Watch(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + name})
	if err != nil {
		t.Fatalf("Watching for %s failed: %v", name, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range watcher.ResultChan() {
			object, ok := event.Object.(*unstructured.Unstructured)
			if !ok || object.GetDeletionTimestamp() == nil || len(object.GetFinalizers()) == 0 {
				continue
			}
			time.Sleep(delay)
			_, _ = resource.Namespace(object.GetNamespace()).Patch(ctx, name, types.MergePatchType,
				[]byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		}
	}()
	t.Cleanup(func() {
		cancel()
		watcher.Stop()
		<-done
	})
}

// requireCheckpointsOfSection4 asserts a checkpoint where each settle wait
// ended and one after the teardown deletion window. The restart of op 3 is
// checked at the settle that follows it.
func requireCheckpointsOfSection4(t *testing.T, check *recordingChecker, result run.Result) {
	t.Helper()
	want := []int{0, 1, 2, 4, 5, run.Teardown}
	if got := check.ops(); !slices.Equal(got, want) {
		t.Errorf("The checks ran after the ops %v, want %v.", got, want)
	}
	if got := checkpointOps(result.Timeline); !slices.Equal(got, want) {
		t.Errorf("The timeline holds checkpoints at %v, want %v.", got, want)
	}
	for _, state := range check.checkpoints {
		if !state.converged {
			t.Errorf("The checkpoint after op %d reports no convergence.", state.op)
		}
	}
}

// requireOpsTookEffect asserts what the cluster did: the update scaled the
// children, the restart started a second process, the deleteManaged took the
// first child and the delete emptied the namespace.
func requireOpsTookEffect(t *testing.T, check *recordingChecker, result run.Result) {
	t.Helper()
	created, _ := check.at(0)
	if len(created.managed) != 3 || created.ready != 3 {
		t.Errorf("After the create the run held the ConfigMaps %v and status.ready %d, want 3 of each.",
			created.managed, created.ready)
	}
	updated, _ := check.at(1)
	if len(updated.managed) != 5 || updated.ready != 5 {
		t.Errorf("After the update to count 5 the run held the ConfigMaps %v and status.ready %d, want 5 of each.",
			updated.managed, updated.ready)
	}
	restarted, _ := check.at(4)
	if before, _ := check.at(2); restarted.widgetWatch <= before.widgetWatch {
		t.Errorf("The target watched widgets %d times before the restart and %d after, want the restart to open another.",
			before.widgetWatch, restarted.widgetWatch)
	}
	if want := "widget-0"; result.Timeline.Ops[4].Deleted != want {
		t.Errorf("The deleteManaged op deleted %q, want the first ConfigMap by creationTimestamp then name, %q.",
			result.Timeline.Ops[4].Deleted, want)
	}
	if len(restarted.managed) != 5 {
		t.Errorf("After the deleted child the run held the ConfigMaps %v, want the target to have made it again.",
			restarted.managed)
	}
	deleted, _ := check.at(5)
	if len(deleted.managed) != 0 {
		t.Errorf("After the delete the run still held the ConfigMaps %v.", deleted.managed)
	}
}

// requireNamespaceEmpty asserts what the teardown left: a namespace holding
// none of the run's ConfigMaps and Secrets. envtest never finishes terminating
// one, so its contents stay readable (DESIGN.md §5.8).
func requireNamespaceEmpty(t *testing.T, ctx context.Context, config *rest.Config, namespace string) {
	t.Helper()
	if namespace == "" {
		t.Fatal("The run recorded no namespace.")
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("Building a client failed: %v", err)
	}
	for _, resource := range []string{"configmaps", "secrets"} {
		remaining, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: resource}).
			Namespace(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Listing the %s the run left failed: %v", resource, err)
		}
		for _, object := range remaining.Items {
			if object.GetDeletionTimestamp() == nil {
				t.Errorf("The run namespace still holds the %s %s.", resource, object.GetName())
			}
		}
	}
}

// requireRunFiles asserts the run directory of DESIGN.md §11.
func requireRunFiles(t *testing.T, dir string) {
	t.Helper()
	if filepath.Base(dir) != "run-1" {
		t.Errorf("The failing run wrote to %s, want run-1.", dir)
	}
	for _, name := range []string{"sequence.json", "requests.jsonl", "objects.jsonl", "target.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("The failing run wrote no %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("The failing run's %s is empty.", name)
		}
	}
}

func readSequence(t *testing.T, text string) run.Sequence {
	t.Helper()
	sequence, err := run.UnmarshalSequence([]byte(text))
	if err != nil {
		t.Fatalf("Reading the sequence failed: %v", err)
	}
	return sequence
}

func checkpointOps(timeline run.Timeline) []int {
	var ops []int
	for _, checkpoint := range timeline.Checkpoints {
		ops = append(ops, checkpoint.Op)
	}
	return ops
}
