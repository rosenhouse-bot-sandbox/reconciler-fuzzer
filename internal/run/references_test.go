package run

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var passwordSecret = trace.Ref{System: "kube", Resource: "secrets", Namespace: "ns", Name: "pw"}

// recreatePassword deletes the fixture pw in step 0 and creates it again in
// step 1, naming it the second time with its system.
func recreatePassword() *plan.Plan {
	return &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "pw"}}}},
		{Actions: []plan.Action{{Do: plan.CreateFixture, Target: &plan.Target{System: "kube", Resource: "secrets", Name: "pw"}}}},
	}}
}

func TestExecuteCreatesADeletedFixtureAgain(t *testing.T) {
	t.Skip("enabled by the run package")
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"secrets": passwordSecret})
	if err := f.runner.Execute(context.Background(), recreatePassword()); err != nil {
		t.Fatal(err)
	}
	if got, want := f.cluster.all(), []string{"delete pw", "gone pw", "createFixture pw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
	events := f.rec.Events()
	if a, _ := findAction(events, 1, 0); a.Action == nil || a.Action.Error != "" || a.Action.Ref == nil || *a.Action.Ref != passwordSecret {
		t.Errorf("createFixture action = %+v, want one of %+v without an error", a.Action, passwordSecret)
	}
	start, _ := find(events, 1, trace.FaultPeriod)
	endFaults, _ := find(events, 1, trace.EndFaults)
	if want, _ := Deadline(endFaults.At, endFaults.At-start.At, timeouts.Stable, timeouts); (endFaults.Phase.Deadline - want).Abs() > 5*time.Millisecond {
		t.Errorf("deadline = %v, want about %v, which adds kube's poll", endFaults.Phase.Deadline, want)
	}
}

func TestExecuteWaitsForADeletedFixtureToGoBeforeCreatingIt(t *testing.T) {
	t.Skip("enabled by the run package")
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"secrets": passwordSecret})
	f.cluster.waitGone = func(context.Context) error { return nil }
	if err := f.runner.Execute(context.Background(), recreatePassword()); err != nil {
		t.Fatal(err)
	}
	if got, want := f.cluster.all(), []string{"delete pw", "gone pw", "waitGone pw", "createFixture pw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestExecuteEndsThePlanWhenADeletedFixtureStays(t *testing.T) {
	t.Skip("enabled by the run package")
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"secrets": passwordSecret})
	f.cluster.waitGone = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	p := recreatePassword()
	p.Steps = append(p.Steps, plan.Step{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("never")}}})
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, want := f.cluster.all(), []string{"delete pw", "gone pw", "waitGone pw"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
	if a, _ := findAction(f.rec.Events(), 1, 0); a.Action == nil || !strings.Contains(a.Action.Error, errSlotTaken.Error()) {
		t.Errorf("createFixture action = %+v, want the error %q", a.Action, errSlotTaken)
	}
}

func TestExecuteNotesACreateFixtureWhoseChangeFixtureDeletedNothing(t *testing.T) {
	t.Skip("enabled by the run package")
	for name, c := range map[string]struct {
		found     map[string]trace.Ref
		deleteErr error
		want      []string
	}{
		"no instance matched":  {nil, nil, nil},
		"the fixture was gone": {map[string]trace.Ref{"secrets": passwordSecret}, system.ErrGone, []string{"delete pw"}},
	} {
		f := newFixture(t, nil)
		selecting(f, c.found)
		f.cluster.deleteErr = c.deleteErr
		if err := f.runner.Execute(context.Background(), recreatePassword()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.cluster.all(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: cluster calls = %q, want %q", name, got, c.want)
		}
		if a, _ := findAction(f.rec.Events(), 1, 0); a.Action == nil || a.Action.Error == "" || a.Action.Ref != nil {
			t.Errorf("%s: createFixture action = %+v, want an error and no ref", name, a.Action)
		}
	}
}

func TestExecuteDeletesEachReferencingKindBeforeTheKindItNames(t *testing.T) {
	t.Skip("enabled by the run package")
	f := newFixture(t, nil)
	toy := func(kind string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: "toy", Version: "v1", Kind: kind}
	}
	f.runner.Config.Primaries = []config.Primary{{Kind: toy("Cluster")}, {Kind: toy("Database")}}
	f.runner.Config.References = []config.Reference{{From: toy("Database"), To: toy("Cluster"), Name: "spec.clusterRef"}}
	object := func(kind, name string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"apiVersion":"toy/v1","kind":%q,"metadata":{"name":%q}}`, kind, name))
	}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{
		{Do: plan.Create, Slot: "a", Object: object("Database", "d1")},
		{Do: plan.Create, Slot: "b", Object: object("Cluster", "c")},
		{Do: plan.Create, Slot: "c", Object: object("Database", "d2")},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{"create d1", "create c", "create d2", "delete d1", "delete d2", "delete c"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestReferenceKindsAreTheKubeKindsReferencesName(t *testing.T) {
	t.Skip("enabled by the run package")
	secretKind := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	references := []config.Reference{
		{From: sprocketKind, To: gadgetKind, Name: "spec.gadget"},
		{From: sprocketKind, System: "cloud", Collection: "buckets", Name: "spec.bucket"},
		{From: gadgetKind, To: secretKind, Name: "spec.secret"},
	}
	if got, want := referenceKinds(references), []schema.GroupVersionKind{gadgetKind, secretKind}; !reflect.DeepEqual(got, want) {
		t.Errorf("referenceKinds = %v, want %v", got, want)
	}
}

func TestFakeOptionsSeeAllOfAFakeAReferenceNames(t *testing.T) {
	t.Skip("enabled by the run package")
	s := config.System{Name: "cloud", API: "crud", Process: &config.Process{Command: []string{"/bin/crudfake"}}}
	for system, want := range map[string]bool{"cloud": true, "other": false} {
		o, err := fakeOptions(s, []config.Reference{{From: gadgetKind, System: system, Collection: "buckets", Name: "spec.bucket"}})
		if err != nil || o.SeenAll != want {
			t.Errorf("fakeOptions with a reference to %s = %+v, %v; want SeenAll %v", system, o, err, want)
		}
	}
}
