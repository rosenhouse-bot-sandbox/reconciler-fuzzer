package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var timeouts = config.Timeouts{Settle: 300 * time.Millisecond, Stable: 50 * time.Millisecond, BackoffCap: time.Second}

// calls logs what the fakes were asked to do, in order.
type calls struct {
	mu  sync.Mutex
	log []string
	at  []time.Time
}

func (c *calls) add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, fmt.Sprintf(format, args...))
	c.at = append(c.at, time.Now())
}

func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.log)
}

func (c *calls) when(entry string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at[slices.Index(c.log, entry)]
}

// whenAfter is when entry was first logged after the first other.
func (c *calls) whenAfter(entry, other string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := slices.Index(c.log, other) + 1
	return c.at[from+slices.Index(c.log[from:], entry)]
}

type fakeProcess struct{ *calls }

func (p fakeProcess) Start(context.Context) error { p.add("start"); return nil }
func (p fakeProcess) Stop(context.Context) error  { p.add("stop"); return nil }
func (p fakeProcess) Kill(context.Context) error  { p.add("kill"); return nil }
func (p fakeProcess) Supervise(on bool) error     { p.add("supervise(%v)", on); return nil }

type fakeCluster struct {
	*calls
	createErr error
	created   []map[string]any
	// waitGone, when set, makes deleted CRs linger: Gone reports them there,
	// and WaitGone answers with waitGone.
	waitGone  func(ctx context.Context) error
	goneErr   error
	deleteErr error
}

func (c *fakeCluster) Create(_ context.Context, obj map[string]any) (trace.Ref, error) {
	name := obj["metadata"].(map[string]any)["name"].(string)
	c.add("create %s", name)
	c.created = append(c.created, obj)
	obj["mutated"] = true
	return trace.Ref{System: "kube", Resource: "widgets", Namespace: "ns", Name: name}, c.createErr
}

func (c *fakeCluster) Patch(_ context.Context, ref trace.Ref, patch []byte) error {
	c.add("patch %s %s", ref.Name, patch)
	return nil
}

func (c *fakeCluster) Delete(_ context.Context, ref trace.Ref, propagation plan.Propagation) error {
	if propagation == "" {
		c.add("delete %s", ref.Name)
	} else {
		c.add("delete %s %s", ref.Name, propagation)
	}
	return c.deleteErr
}

func (c *fakeCluster) Gone(_ context.Context, ref trace.Ref) (bool, error) {
	c.add("gone %s", ref.Name)
	return c.waitGone == nil && c.goneErr == nil, c.goneErr
}

func (c *fakeCluster) WaitGone(ctx context.Context, ref trace.Ref) error {
	c.add("waitGone %s", ref.Name)
	if c.waitGone != nil {
		return c.waitGone(ctx)
	}
	return nil
}

type fakeTarget struct {
	*calls
	// onEnd runs when a fault ends.
	onEnd func()
}

func (f fakeTarget) Fault(fault plan.Fault) (func(), error) {
	f.add("fault %s", fault.Match.Resource)
	return func() {
		f.add("end %s", fault.Match.Resource)
		if f.onEnd != nil {
			f.onEnd()
		}
	}, nil
}

type fixture struct {
	runner  *Runner
	rec     *trace.Recorder
	process *calls
	cluster *fakeCluster
	faults  *calls
}

func newFixture(t *testing.T, firstSettled FirstSettled) *fixture {
	t.Helper()
	rec, err := trace.NewRecorder(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	if firstSettled == nil {
		firstSettled = func(_ *trace.Index, from, _ time.Duration) (time.Duration, bool) { return from, true }
	}
	f := &fixture{rec: rec, process: &calls{}, cluster: &fakeCluster{calls: &calls{}}, faults: &calls{}}
	f.runner = &Runner{
		Config:       &config.Config{Timeouts: timeouts},
		Recorder:     rec,
		Cluster:      f.cluster,
		Process:      fakeProcess{f.process},
		Listeners:    map[string]FaultTarget{"kube": fakeTarget{calls: f.faults}},
		Actors:       map[string]Actor{"kube": f.cluster},
		FirstSettled: firstSettled,
		Sample:       map[string]any{"kind": "Widget", "metadata": map[string]any{"name": "sample"}},
	}
	return f
}

func widget(name string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"kind":"Widget","metadata":{"name":%q}}`, name))
}

func summarize(events []trace.Event) []string {
	var out []string
	for _, e := range events {
		switch {
		case e.Phase != nil:
			out = append(out, fmt.Sprintf("phase %d %s", e.Phase.Step, e.Phase.Name))
		case e.Action != nil:
			ref := ""
			if e.Action.Ref != nil {
				ref = " " + e.Action.Ref.Name
			}
			out = append(out, fmt.Sprintf("action %d.%d %s %s%s", e.Action.Step, e.Action.Index, e.Action.Do, e.Action.Slot, ref))
		case e.Fault != nil:
			state := "start"
			if e.Fault.Ended {
				state = "end"
			}
			out = append(out, fmt.Sprintf("fault %d.%d %s", e.Fault.Step, e.Fault.Index, state))
		}
	}
	return out
}

func find(events []trace.Event, step int, name trace.PhaseName) (trace.Event, bool) {
	for _, e := range events {
		if e.Phase != nil && e.Phase.Step == step && e.Phase.Name == name {
			return e, true
		}
	}
	return trace.Event{}, false
}

func findFaultEnd(events []trace.Event) (trace.Event, bool) {
	for _, e := range events {
		if e.Fault != nil && e.Fault.Ended {
			return e, true
		}
	}
	return trace.Event{}, false
}

func findAction(events []trace.Event, step, index int) (trace.Event, bool) {
	for _, e := range events {
		if e.Action != nil && e.Action.Step == step && e.Action.Index == index {
			return e, true
		}
	}
	return trace.Event{}, false
}

func TestExecuteRecordsStepsInOrder(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}}},
		{
			Faults:  []plan.Fault{{Match: plan.Match{Resource: "configmaps"}, Drop: true}},
			Actions: []plan.Action{{Do: plan.Update, Slot: "a", Patch: json.RawMessage(`{"spec":{"count":2}}`)}},
		},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"phase 0 faultPeriod", "action 0.0 create a w", "phase 0 endFaults", "phase 0 settled", "phase 0 confirmed",
		"phase 1 faultPeriod", "fault 1.0 start", "action 1.0 update a w", "fault 1.0 end", "phase 1 endFaults", "phase 1 settled", "phase 1 confirmed",
		"phase 2 faultPeriod", "action 2.0 delete a w", "phase 2 endFaults", "phase 2 settled", "phase 2 confirmed",
		"phase 3 faultPeriod", "action 3.0 restart ", "phase 3 endFaults", "phase 3 settled", "phase 3 confirmed",
	}
	if got := summarize(f.rec.Events()); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantCluster := []string{"create w", `patch w {"spec":{"count":2}}`, "delete w"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, wantCluster) {
		t.Errorf("cluster calls = %q, want %q", got, wantCluster)
	}
	wantProcess := []string{
		"supervise(false)", "supervise(true)", "supervise(false)", "supervise(true)", "supervise(false)", "supervise(true)",
		"supervise(false)", "stop", "start", "supervise(true)",
	}
	if got := f.process.all(); !reflect.DeepEqual(got, wantProcess) {
		t.Errorf("process calls = %q, want %q", got, wantProcess)
	}
	events := f.rec.Events()
	settled, _ := find(events, 0, trace.Settled)
	confirmed, _ := find(events, 0, trace.Confirmed)
	if gap := confirmed.At - settled.At; gap < timeouts.Stable {
		t.Errorf("confirmed %v after settled, want at least %v", gap, timeouts.Stable)
	}
	start, _ := find(events, 0, trace.FaultPeriod)
	endFaults, _ := find(events, 0, trace.EndFaults)
	if want, _ := Deadline(endFaults.At, endFaults.At-start.At, 0, timeouts); (endFaults.Phase.Deadline - want).Abs() > 5*time.Millisecond {
		t.Errorf("deadline = %v, want about %v", endFaults.Phase.Deadline, want)
	}
}

func TestExecuteCreatesFromSample(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a"}}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := f.cluster.all()[:3]; !reflect.DeepEqual(got, []string{"create sample", "delete sample", "gone sample"}) {
		t.Errorf("cluster calls = %q", got)
	}
	if _, mutated := f.runner.Sample["mutated"]; mutated || f.cluster.created[1]["mutated"] != true {
		t.Error("each create must get its own copy of the sample")
	}
}

func TestExecuteWaitsForADeletedSlotBeforeCreating(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("first")}}},
		{Actions: []plan.Action{{Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a", Object: widget("second")}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{"create first", "delete first", "gone first", "create second", "delete second"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestExecuteEndsThePlanWhenADeletedSlotStaysTaken(t *testing.T) {
	f := newFixture(t, nil)
	recordTheDeletion := sync.OnceFunc(func() {
		f.rec.Record(trace.Event{Version: &trace.Version{
			Ref: trace.Ref{System: "kube", Resource: "widgets", Namespace: "ns", Name: "first"}, UID: "u1", RV: "2",
			Body: json.RawMessage(`{"metadata":{"name":"first","deletionTimestamp":"2026-10-24T09:06:40Z","finalizers":["toy/cleanup","toy/audit"]}}`),
		}})
	})
	f.cluster.waitGone = func(ctx context.Context) error {
		recordTheDeletion()
		<-ctx.Done()
		return fmt.Errorf(`Get "http://127.0.0.1:36123/widgets/first": %w`, ctx.Err())
	}
	const period = 100 * time.Millisecond
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("first")}}},
		{Period: plan.Duration(period), Faults: holdWidgets, Actions: []plan.Action{
			{Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a", Object: widget("second")}, {Do: plan.Restart},
		}},
		{Actions: []plan.Action{{Do: plan.Create, Slot: "b", Object: widget("never")}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	events := f.rec.Events()
	want := []string{
		"phase 0 faultPeriod", "action 0.0 create a first", "phase 0 endFaults", "phase 0 settled", "phase 0 confirmed",
		"phase 1 faultPeriod", "fault 1.0 start", "action 1.0 delete a first", "fault 1.0 end", "action 1.1 create a",
		"phase 1 endFaults", "phase 1 settled", "phase 1 confirmed",
		"phase 3 faultPeriod", "phase 3 endFaults", "phase 3 settled", "phase 3 confirmed",
		"phase 4 faultPeriod", "action 4.0 restart ", "phase 4 endFaults", "phase 4 settled", "phase 4 confirmed",
	}
	if got := summarize(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	faultEnd, _ := findFaultEnd(events)
	create, _ := findAction(events, 1, 1)
	allowed, _ := Deadline(0, faultLasted(t, events), f.runner.Config.Poll("kube"), timeouts)
	if waited := create.At - faultEnd.At; waited < allowed-20*time.Millisecond || waited > allowed+100*time.Millisecond {
		t.Errorf("the create waited %v after the fault ended, want the %v that Deadline allows", waited, allowed)
	}
	note := regexp.MustCompile(`^the slot's name is still taken (\S+) after the step's faults ended: ` +
		`its previous CR has deletionTimestamp 2026-10-24T09:06:40Z and finalizers \[toy/cleanup toy/audit\]$`)
	if m := note.FindStringSubmatch(create.Action.Error); m == nil {
		t.Errorf("action error = %q, want a match of %s", create.Action.Error, note)
	} else if named, _ := time.ParseDuration(m[1]); (named - allowed).Abs() > 20*time.Millisecond {
		t.Errorf("the note names %s, want about %v", m[1], allowed)
	}
}

func actionErrors(events []trace.Event) []string {
	var errs []string
	for _, e := range events {
		if e.Action != nil && e.Action.Error != "" {
			errs = append(errs, e.Action.Error)
		}
	}
	return errs
}

// faultHoldsDeletedCRs keeps a deleted CR until a kube fault has ended and
// lingers has passed.
func faultHoldsDeletedCRs(f *fixture, lingers time.Duration) {
	ended := make(chan struct{})
	f.runner.Listeners["kube"] = fakeTarget{calls: f.faults, onEnd: func() { close(ended) }}
	f.cluster.waitGone = func(ctx context.Context) error {
		// Like kube.WaitGone's first GET, a wait past its deadline fails.
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ended:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-time.After(lingers):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// recreateUnder deletes and creates slot a, after actions before, in a
// step with period and faults. The next step updates slot a.
func recreateUnder(period time.Duration, faults []plan.Fault, before ...plan.Action) *plan.Plan {
	return &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("first")}}},
		{Period: plan.Duration(period), Faults: faults, Actions: append(before,
			plan.Action{Do: plan.Delete, Slot: "a"}, plan.Action{Do: plan.Create, Slot: "a", Object: widget("second")})},
		{Actions: []plan.Action{{Do: plan.Update, Slot: "a", Patch: json.RawMessage(`{"spec":{"count":2}}`)}}},
	}}
}

var holdWidgets = []plan.Fault{{Match: plan.Match{Resource: "widgets"}, HoldWatch: true}}

// recreated is what Execute records for a recreateUnder plan whose step 1
// records step1 after its faultPeriod phase.
func recreated(step1 ...string) []string {
	return slices.Concat(
		[]string{"phase 0 faultPeriod", "action 0.0 create a first", "phase 0 endFaults", "phase 0 settled", "phase 0 confirmed", "phase 1 faultPeriod"},
		step1,
		[]string{
			"phase 1 endFaults", "phase 1 settled", "phase 1 confirmed",
			"phase 2 faultPeriod", "action 2.0 update a second", "phase 2 endFaults", "phase 2 settled", "phase 2 confirmed",
			"phase 3 faultPeriod", "action 3.0 delete a second", "phase 3 endFaults", "phase 3 settled", "phase 3 confirmed",
			"phase 4 faultPeriod", "action 4.0 restart ", "phase 4 endFaults", "phase 4 settled", "phase 4 confirmed",
		},
	)
}

// faultLasted is how long after step 1's start its first fault ended.
func faultLasted(t *testing.T, events []trace.Event) time.Duration {
	t.Helper()
	start, _ := find(events, 1, trace.FaultPeriod)
	end, found := findFaultEnd(events)
	if !found {
		t.Fatal("no fault ended")
	}
	return end.At - start.At
}

func TestExecuteEndsTheFaultsAtThePeriodWhileACreateWaits(t *testing.T) {
	f := newFixture(t, nil)
	faultHoldsDeletedCRs(f, 0)
	const period = 100 * time.Millisecond
	if err := f.runner.Execute(context.Background(), recreateUnder(period, holdWidgets)); err != nil {
		t.Fatal(err)
	}
	want := recreated("fault 1.0 start", "action 1.0 delete a first", "fault 1.0 end", "action 1.1 create a second")
	if got := summarize(f.rec.Events()); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if lasted := faultLasted(t, f.rec.Events()); lasted < period-5*time.Millisecond || lasted >= timeouts.Settle {
		t.Errorf("the fault lasted %v, want the period %v", lasted, period)
	}
}

func TestExecuteKeepsTheFaultsForAPeriodLongerThanTheSettleTimeout(t *testing.T) {
	f := newFixture(t, nil)
	faultHoldsDeletedCRs(f, 0)
	period := 2 * timeouts.Settle
	if err := f.runner.Execute(context.Background(), recreateUnder(period, holdWidgets)); err != nil {
		t.Fatal(err)
	}
	if errs := actionErrors(f.rec.Events()); len(errs) != 0 {
		t.Errorf("action errors = %q, want none", errs)
	}
	if lasted := faultLasted(t, f.rec.Events()); lasted < period-5*time.Millisecond {
		t.Errorf("the fault lasted %v, want the period %v", lasted, period)
	}
}

func TestExecuteKeepsTheFaultsWhenTheSlotFreesWithinThePeriod(t *testing.T) {
	f := newFixture(t, nil)
	f.cluster.waitGone = func(ctx context.Context) error {
		select {
		case <-time.After(20 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	const period = 200 * time.Millisecond
	if err := f.runner.Execute(context.Background(), recreateUnder(period, holdWidgets)); err != nil {
		t.Fatal(err)
	}
	want := recreated("fault 1.0 start", "action 1.0 delete a first", "action 1.1 create a second", "fault 1.0 end")
	if got := summarize(f.rec.Events()); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if lasted := faultLasted(t, f.rec.Events()); lasted < period-5*time.Millisecond {
		t.Errorf("the fault lasted %v, want the period %v", lasted, period)
	}
}

func TestExecuteEndsTheFaultsForACreateAfterThePeriodIntoAHeldSlot(t *testing.T) {
	f := newFixture(t, nil)
	faultHoldsDeletedCRs(f, 0)
	p := recreateUnder(50*time.Millisecond, holdWidgets)
	p.Steps[1].Actions[1].At = plan.Duration(150 * time.Millisecond)
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := recreated("fault 1.0 start", "action 1.0 delete a first", "fault 1.0 end", "action 1.1 create a second")
	if got := summarize(f.rec.Events()); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecuteKeepsTheFaultsForACreateAfterThePeriodIntoAFreeSlot(t *testing.T) {
	f := newFixture(t, nil)
	p := recreateUnder(50*time.Millisecond, holdWidgets)
	p.Steps[1].Actions[1].At = plan.Duration(150 * time.Millisecond)
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := recreated("fault 1.0 start", "action 1.0 delete a first", "action 1.1 create a second", "fault 1.0 end")
	if got := summarize(f.rec.Events()); !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecuteRestartsAKilledControllerWhileACreateWaits(t *testing.T) {
	for _, c := range []struct {
		name       string
		period, at time.Duration
	}{
		{"create within the period", 100 * time.Millisecond, 0},
		{"create after the period", 50 * time.Millisecond, 150 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.cluster.waitGone = func(ctx context.Context) error {
				for {
					if log := f.process.all(); log[len(log)-1] == "supervise(true)" {
						return nil
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(5 * time.Millisecond):
					}
				}
			}
			p := recreateUnder(c.period, nil, plan.Action{Do: plan.Kill})
			p.Steps[1].Actions[2].At = plan.Duration(c.at)
			if err := f.runner.Execute(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			if errs := actionErrors(f.rec.Events()); len(errs) != 0 {
				t.Fatalf("action errors = %q, want none", errs)
			}
			restarted := f.process.whenAfter("supervise(true)", "kill")
			if held := restarted.Sub(f.process.when("kill")); held < max(c.period, c.at)-10*time.Millisecond {
				t.Errorf("supervision resumed %v after the kill, want %v", held, max(c.period, c.at))
			}
			if created := f.cluster.when("create second"); restarted.After(created) {
				t.Errorf("supervision resumed %v after the create", restarted.Sub(created))
			}
			if got := f.cluster.all(); !slices.Contains(got, `patch second {"spec":{"count":2}}`) {
				t.Errorf("cluster calls = %q, want the next step's update", got)
			}
		})
	}
}

func TestExecuteBoundsTheBackoffByHowLongTheFaultsLasted(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Config.Timeouts.BackoffCap = 350 * time.Millisecond
	faultHoldsDeletedCRs(f, 200*time.Millisecond)
	p := recreateUnder(50*time.Millisecond, holdWidgets)
	p.Steps[1].Actions[1].At = plan.Duration(250 * time.Millisecond)
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	events := f.rec.Events()
	lasted := faultLasted(t, events)
	if lasted < 245*time.Millisecond {
		t.Fatalf("the fault lasted %v, want it to outlast the period until the create", lasted)
	}
	endFaults, _ := find(events, 1, trace.EndFaults)
	want, wantBound := Deadline(endFaults.At, lasted, f.runner.Config.Poll("kube"), f.runner.Config.Timeouts)
	if got := endFaults.Phase; (got.Deadline-want).Abs() > 20*time.Millisecond || got.BackoffBound != wantBound {
		t.Errorf("deadline = %v, backoff-bound %v, want about %v, %v", got.Deadline, got.BackoffBound, want, wantBound)
	}
}

func TestSlotTakenNamesWhatTheObserverLastSawHoldTheCR(t *testing.T) {
	ref := trace.Ref{System: "kube", Resource: "widgets", Namespace: "ns", Name: "first"}
	held := trace.Event{Version: &trace.Version{Ref: ref, UID: "u1", RV: "2",
		Body: json.RawMessage(`{"metadata":{"deletionTimestamp":"2026-10-24T09:06:40Z","finalizers":["toy/cleanup"]}}`)}}
	gone := trace.Event{Version: &trace.Version{Ref: ref, UID: "u1", RV: "3", Deleted: true,
		Body: json.RawMessage(`{"metadata":{"deletionTimestamp":"2026-10-24T09:06:40Z"}}`)}}
	replaced := trace.Event{Version: &trace.Version{Ref: ref, UID: "u2", RV: "4",
		Body: json.RawMessage(`{"metadata":{"finalizers":["toy/cleanup"]}}`)}}
	undecodable := trace.Event{Version: &trace.Version{Ref: ref, UID: "u1", RV: "2", Body: json.RawMessage(`[]`)}}
	const taken = "the slot's name is still taken 1.5s after the step's faults ended"
	for _, c := range []struct {
		name   string
		events []trace.Event
		want   string
	}{
		{"held", []trace.Event{held}, taken + ": its previous CR has deletionTimestamp 2026-10-24T09:06:40Z and finalizers [toy/cleanup]"},
		{"replaced", []trace.Event{held, gone, replaced}, taken + ": a CR that is not being deleted has it"},
		{"gone since", []trace.Event{held, gone}, taken},
		{"unrecorded", nil, taken},
		{"undecodable", []trace.Event{undecodable}, taken},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			for _, e := range c.events {
				f.rec.Record(e)
			}
			e := &execution{Runner: f.runner, ix: trace.NewIndex(nil)}
			if got := e.slotTaken(ref, 1500*time.Millisecond+400*time.Microsecond).Error(); got != c.want {
				t.Errorf("slotTaken = %q, want %q", got, c.want)
			}
		})
	}
}

func TestExecuteReturnsACancelDuringTheSlotWait(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	f.cluster.waitGone = func(wait context.Context) error {
		cancel()
		<-wait.Done()
		return wait.Err()
	}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a"}}}}}
	if err := f.runner.Execute(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute = %v, want the cancellation", err)
	}
}

func TestExecuteReturnsTheRunsDeadlineDuringTheSlotWait(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeouts.Settle/3)
	defer cancel()
	f.cluster.waitGone = func(wait context.Context) error {
		<-wait.Done()
		return wait.Err()
	}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a"}}}}}
	if err := f.runner.Execute(ctx, p); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute = %v, want the deadline", err)
	}
	if got, want := actionErrors(f.rec.Events()), []string{context.DeadlineExceeded.Error()}; !reflect.DeepEqual(got, want) {
		t.Errorf("action errors = %q, want %q", got, want)
	}
}

func TestExecuteReturnsAGoneError(t *testing.T) {
	f := newFixture(t, nil)
	f.cluster.goneErr = errors.New("forbidden")
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a"}}}}}
	if err := f.runner.Execute(context.Background(), p); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("Execute = %v, want Gone's error", err)
	}
}

func TestExecuteReturnsAWaitGoneError(t *testing.T) {
	f := newFixture(t, nil)
	f.cluster.waitGone = func(context.Context) error { return errors.New("forbidden") }
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a"}, {Do: plan.Delete, Slot: "a"}, {Do: plan.Create, Slot: "a"}}}}}
	if err := f.runner.Execute(context.Background(), p); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("Execute = %v, want the wait's error", err)
	}
}

func TestExecutePassesADeletesPropagationAndDeletesAtTheEndByDefault(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{
		{Do: plan.Create, Slot: "a", Object: widget("foreground")},
		{Do: plan.Create, Slot: "b", Object: widget("kept")},
		{Do: plan.Delete, Slot: "a", Propagation: plan.Foreground},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{"create foreground", "create kept", "delete foreground Foreground", "delete kept"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestExecuteDeletesOnlyLiveSlotsAtTheEnd(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{
		{Do: plan.Create, Slot: "a", Object: widget("gone")},
		{Do: plan.Create, Slot: "b", Object: widget("kept")},
		{Do: plan.Delete, Slot: "a"},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{"create gone", "create kept", "delete gone", "delete kept"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestExecuteKeepsAKilledControllerDown(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Period: plan.Duration(100 * time.Millisecond), Actions: []plan.Action{{Do: plan.Kill}}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got := f.process.all()
	if !reflect.DeepEqual(got[:3], []string{"supervise(false)", "kill", "supervise(true)"}) {
		t.Fatalf("process calls = %q", got)
	}
	if held := f.process.when("supervise(true)").Sub(f.process.when("kill")); held < 90*time.Millisecond {
		t.Errorf("supervision resumed %v after the kill, want the rest of the period", held)
	}
}

func TestExecuteHonoursOffsetsAndPeriod(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{
		Period: plan.Duration(300 * time.Millisecond),
		Actions: []plan.Action{
			{Do: plan.Create, Slot: "a", Object: widget("early")},
			{At: plan.Duration(150 * time.Millisecond), Do: plan.Create, Slot: "b", Object: widget("late")},
			{At: plan.Duration(10 * time.Millisecond), Do: plan.Create, Slot: "c", Object: widget("passed")},
		},
	}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if gap := f.cluster.when("create late").Sub(f.cluster.when("create early")); gap < 140*time.Millisecond || gap > 250*time.Millisecond {
		t.Errorf("late create ran %v after the early one, want about 150ms", gap)
	}
	if gap := f.cluster.when("create passed").Sub(f.cluster.when("create late")); gap > 50*time.Millisecond {
		t.Errorf("an action whose offset passed waited %v", gap)
	}
	events := f.rec.Events()
	start, _ := find(events, 0, trace.FaultPeriod)
	end, _ := find(events, 0, trace.EndFaults)
	if period := end.At - start.At; period < 300*time.Millisecond {
		t.Errorf("fault period lasted %v, want 300ms", period)
	}
}

func TestExecuteEndsFaultsBeforeEndFaults(t *testing.T) {
	f := newFixture(t, nil)
	var recordedEndFaults []bool
	f.runner.Listeners["kube"] = fakeTarget{calls: f.faults, onEnd: func() {
		_, found := find(f.rec.Events(), 0, trace.EndFaults)
		recordedEndFaults = append(recordedEndFaults, found)
	}}
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{
		{Match: plan.Match{Resource: "configmaps"}, Drop: true},
		{System: "kube", Match: plan.Match{Resource: "widgets"}, Drop: true},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recordedEndFaults, []bool{false, false}) {
		t.Errorf("endFaults recorded before faults ended: %v", recordedEndFaults)
	}
	if got := f.faults.all(); !reflect.DeepEqual(got, []string{"fault configmaps", "fault widgets", "end configmaps", "end widgets"}) {
		t.Errorf("fault calls = %q", got)
	}
}

func TestExecuteTimesOutAndContinues(t *testing.T) {
	f := newFixture(t, func(*trace.Index, time.Duration, time.Duration) (time.Duration, bool) { return 0, false })
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	events := f.rec.Events()
	for step := range 3 {
		endFaults, _ := find(events, step, trace.EndFaults)
		timedOut, found := find(events, step, trace.TimedOut)
		if !found {
			t.Fatalf("step %d did not time out", step)
		}
		if late := timedOut.At - endFaults.Phase.Deadline; late < 0 || late > 3*PollInterval {
			t.Errorf("step %d timed out %v after its deadline", step, late)
		}
		if _, found := find(events, step, trace.Settled); found {
			t.Errorf("step %d settled", step)
		}
	}
}

func TestExecuteSearchesEachPollsWindow(t *testing.T) {
	type window struct{ from, to time.Duration }
	var polls []window
	var indexed []int
	f := newFixture(t, nil)
	f.runner.FirstSettled = func(ix *trace.Index, from, to time.Duration) (time.Duration, bool) {
		polls = append(polls, window{from, to})
		indexed = append(indexed, ix.Len())
		if recorded := len(f.rec.Events()); ix.Len() > recorded {
			t.Errorf("the index holds %d events of %d recorded", ix.Len(), recorded)
		}
		return from, len(polls) == 3
	}
	f.runner.Recorder.Record(trace.Event{Run: &trace.Run{Namespace: "ns"}})
	if err := f.runner.Execute(context.Background(), &plan.Plan{}); err != nil {
		t.Fatal(err)
	}
	if indexed[0] < 3 {
		t.Errorf("the first poll indexed %d events, want the run event and the step's phases", indexed[0])
	}
	events := f.rec.Events()
	endFaults, _ := find(events, 0, trace.EndFaults)
	if polls[0].from != endFaults.At {
		t.Errorf("the first poll searched from %v, want endFaults %v", polls[0].from, endFaults.At)
	}
	for i := 1; i < 3; i++ {
		if polls[i].from != polls[i-1].to || polls[i].to <= polls[i].from {
			t.Errorf("poll %d searched %v, after %v", i, polls[i], polls[i-1])
		}
	}
	settled, found := find(events, 0, trace.Settled)
	if !found || settled.At-endFaults.At < 2*PollInterval-5*time.Millisecond {
		t.Errorf("settled %v after endFaults, want two polls", settled.At-endFaults.At)
	}
}

func TestExecuteSearchesNoFurtherThanTheDeadline(t *testing.T) {
	var last time.Duration
	f := newFixture(t, func(_ *trace.Index, _, to time.Duration) (time.Duration, bool) {
		last = to
		return 0, false
	})
	if err := f.runner.Execute(context.Background(), &plan.Plan{}); err != nil {
		t.Fatal(err)
	}
	endFaults, _ := find(f.rec.Events(), 1, trace.EndFaults)
	if last != endFaults.Phase.Deadline {
		t.Errorf("the last poll searched to %v, want the deadline %v", last, endFaults.Phase.Deadline)
	}
}

func TestExecuteReturnsHarnessErrors(t *testing.T) {
	f := newFixture(t, nil)
	f.cluster.createErr = errors.New("boom")
	p := &plan.Plan{Steps: []plan.Step{{
		Faults:  []plan.Fault{{Match: plan.Match{Resource: "configmaps"}, Drop: true}},
		Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}},
	}}}
	err := f.runner.Execute(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Execute = %v, want the create error", err)
	}
	if got := f.faults.all(); !reflect.DeepEqual(got, []string{"fault configmaps", "end configmaps"}) {
		t.Errorf("fault calls = %q, want the fault ended", got)
	}
	var actionErr string
	for _, e := range f.rec.Events() {
		if e.Action != nil {
			actionErr = e.Action.Error
		}
	}
	if actionErr != "boom" {
		t.Errorf("action error = %q, want boom", actionErr)
	}
}

func TestExecuteRejectsAnUnknownSystem(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{
		{Match: plan.Match{Resource: "configmaps"}, Drop: true},
		{System: "vault", Drop: true},
	}}}}
	err := f.runner.Execute(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("Execute = %v, want an unknown system error", err)
	}
	if got := f.faults.all(); !reflect.DeepEqual(got, []string{"fault configmaps", "end configmaps"}) {
		t.Errorf("fault calls = %q, want the started fault ended", got)
	}
}

func TestDeadline(t *testing.T) {
	cases := []struct {
		name                          string
		endFaults, faultsLasted, poll time.Duration
		want                          time.Duration
		wantBackoffBound              bool
	}{
		{"no faults", 10 * time.Second, 0, 0, 10*time.Second + 300*time.Millisecond, false},
		{"short faults", 10 * time.Second, 400 * time.Millisecond, 0, 10*time.Second + 700*time.Millisecond, false},
		{"faults as long as the cap", 0, time.Second, 0, 1300 * time.Millisecond, false},
		{"cap bounds", 0, 5 * time.Second, 0, 1300 * time.Millisecond, true},
		{"poll adds", 0, 0, 2 * time.Second, 2300 * time.Millisecond, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, backoffBound := Deadline(c.endFaults, c.faultsLasted, c.poll, timeouts)
			if got != c.want || backoffBound != c.wantBackoffBound {
				t.Errorf("Deadline = %v, %v, want %v, %v", got, backoffBound, c.want, c.wantBackoffBound)
			}
		})
	}
}

// selection is what a fake Select was asked.
type selection struct {
	creator trace.Caller
	target  plan.Target
	slot    string
	// indexed says the index held every event recorded before the call.
	indexed bool
}

func selecting(f *fixture, found map[string]trace.Ref) *[]selection {
	var asked []selection
	f.runner.Select = func(ix *trace.Index, creator trace.Caller, target plan.Target, slot string, t time.Duration) (trace.Ref, bool) {
		asked = append(asked, selection{creator, target, slot, ix.Len() == len(f.rec.Events())})
		ref, ok := found[target.Resource]
		return ref, ok
	}
	return &asked
}

func TestExecuteDamagesAndChangesFixtures(t *testing.T) {
	f := newFixture(t, nil)
	cm := trace.Ref{System: "kube", Resource: "configmaps", Namespace: "ns", Name: "child"}
	secret := trace.Ref{System: "kube", Resource: "secrets", Namespace: "ns", Name: "fixture"}
	asked := selecting(f, map[string]trace.Ref{"configmaps": cm, "secrets": secret})
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}}},
		{Actions: []plan.Action{
			{Do: plan.Damage, Slot: "a", Target: &plan.Target{Resource: "configmaps", Role: "child"}},
			{Do: plan.Damage, Target: &plan.Target{Resource: "configmaps"}, Patch: json.RawMessage(`{"data":{}}`)},
			{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "fixture"}, Patch: json.RawMessage(`{"data":null}`)},
			{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "fixture"}},
		}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []selection{
		{trace.Controller, plan.Target{Resource: "configmaps", Role: "child"}, "a", true},
		{trace.Controller, plan.Target{Resource: "configmaps"}, "", true},
		{trace.Harness, plan.Target{Resource: "secrets", Name: "fixture"}, "", true},
		{trace.Harness, plan.Target{Resource: "secrets", Name: "fixture"}, "", true},
	}
	if !reflect.DeepEqual(*asked, want) {
		t.Errorf("Select asked %+v, want %+v", *asked, want)
	}
	wantCluster := []string{"create w", "delete child", `patch child {"data":{}}`, `patch fixture {"data":null}`, "delete fixture", "delete w"}
	if got := f.cluster.all(); !reflect.DeepEqual(got, wantCluster) {
		t.Errorf("cluster calls = %q, want %q", got, wantCluster)
	}
	var refs []trace.Ref
	for _, e := range f.rec.Events() {
		if a := e.Action; a != nil && a.Step == 1 {
			refs = append(refs, *a.Ref)
		}
	}
	if !reflect.DeepEqual(refs, []trace.Ref{cm, cm, secret, secret}) {
		t.Errorf("action refs = %+v", refs)
	}
}

func TestExecuteGoesOnAfterAnUnmatchedTarget(t *testing.T) {
	f := newFixture(t, nil)
	selecting(f, nil)
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{Resource: "configmaps"}}, {Do: plan.Create, Slot: "a", Object: widget("w")}}},
		{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "s"}}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, e := range f.rec.Events() {
		if a := e.Action; a != nil {
			errs = append(errs, fmt.Sprintf("%d.%d %s %q", a.Step, a.Index, a.Do, a.Error))
		}
	}
	want := []string{`0.0 damage "no instance matches"`, `0.1 create ""`, `1.0 changeFixture "no instance matches"`, `2.0 delete ""`, `3.0 restart ""`}
	if !reflect.DeepEqual(errs, want) {
		t.Errorf("actions = %q, want %q", errs, want)
	}
}

func TestExecuteAddsThePollIntervalToAChangingStepsDeadline(t *testing.T) {
	f := newFixture(t, nil)
	selecting(f, nil)
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{Resource: "configmaps"}}}},
		{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "s"}}}},
		{Actions: []plan.Action{{Do: plan.Restart}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	events := f.rec.Events()
	for step, poll := range []time.Duration{timeouts.Stable, timeouts.Stable, 0} {
		start, _ := find(events, step, trace.FaultPeriod)
		endFaults, _ := find(events, step, trace.EndFaults)
		want, _ := Deadline(endFaults.At, endFaults.At-start.At, poll, timeouts)
		if got := endFaults.Phase.Deadline; (got - want).Abs() > 5*time.Millisecond {
			t.Errorf("step %d deadline = %v, want about %v", step, got, want)
		}
	}
}

func TestExecuteRecordsADamageOfAGoneObjectAsUnmatched(t *testing.T) {
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"configmaps": {System: "kube", Resource: "configmaps", Namespace: "ns", Name: "child"}})
	f.cluster.deleteErr = system.ErrGone
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}, {Do: plan.Delete, Slot: "a"}}},
		{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{Resource: "configmaps"}}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range f.rec.Events() {
		if a := e.Action; a != nil && a.Do != "restart" {
			got = append(got, fmt.Sprintf("%s ref=%v errored=%v", a.Do, a.Ref != nil, a.Error != ""))
		}
	}
	want := []string{"create ref=true errored=false", "delete ref=true errored=false", "damage ref=false errored=true"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actions = %q, want %q", got, want)
	}
}

func TestQuietWaitsUntilTheControllerSettles(t *testing.T) {
	var polls int
	f := newFixture(t, func(ix *trace.Index, from, to time.Duration) (time.Duration, bool) {
		polls++
		return to, polls == 3
	})
	start := time.Now()
	if err := f.runner.Quiet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if polls != 3 || time.Since(start) < 2*PollInterval {
		t.Errorf("Quiet returned after %d polls and %v, want 3 polls", polls, time.Since(start))
	}
	if events := f.rec.Events(); len(events) != 0 {
		t.Errorf("Quiet recorded %d events, want none", len(events))
	}
}

func TestQuietGivesUpAfterTheSettleTimeout(t *testing.T) {
	f := newFixture(t, func(*trace.Index, time.Duration, time.Duration) (time.Duration, bool) { return 0, false })
	start := time.Now()
	if err := f.runner.Quiet(context.Background()); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited < timeouts.Settle || waited > timeouts.Settle+3*PollInterval {
		t.Errorf("Quiet waited %v, want the settle timeout %v", waited, timeouts.Settle)
	}
}

type fakeModel struct {
	fakeTarget
	name, resource string
}

func (m fakeModel) Name() string { return m.name }

func (m fakeModel) Takes(f plan.Fault) bool { return f.ModelFault() && f.Match.Resource == m.resource }

func TestExecuteGivesAModelFaultToTheFirstModelThatTakesIt(t *testing.T) {
	f := newFixture(t, nil)
	models := &calls{}
	f.runner.Models = []ModelTarget{
		fakeModel{fakeTarget{calls: models}, "model:sprockets", "sprockets"},
		fakeModel{fakeTarget{calls: models}, "model:gadgets", "gadgets"},
		fakeModel{fakeTarget{calls: &calls{}}, "model:gadgets-again", "gadgets"},
	}
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{
		{Match: plan.Match{Resource: "gadgets"}, Stuck: true},
		{Match: plan.Match{Resource: "gadgets"}, Drop: true},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := models.all(); !reflect.DeepEqual(got, []string{"fault gadgets", "end gadgets"}) {
		t.Errorf("model calls = %q", got)
	}
	if got := f.faults.all(); !reflect.DeepEqual(got, []string{"fault gadgets", "end gadgets"}) {
		t.Errorf("listener calls = %q, want the request fault", got)
	}
	var got []string
	for _, e := range f.rec.Events() {
		if e.Fault != nil {
			got = append(got, fmt.Sprintf("%d %v %q", e.Fault.Index, e.Fault.Ended, e.Fault.Model))
		}
	}
	want := []string{`0 false "model:gadgets"`, `1 false ""`, `0 true "model:gadgets"`, `1 true ""`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fault events = %q, want %q", got, want)
	}
}

func TestExecuteRejectsAModelFaultNoModelTakes(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{{Match: plan.Match{Resource: "gadgets"}, SlowDelete: true}}}}}
	err := f.runner.Execute(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "slowDelete") {
		t.Fatalf("Execute = %v, want an error naming the fault", err)
	}
}

type writingModel struct {
	fakeModel
	rec *trace.Recorder
}

func (m writingModel) Fault(f plan.Fault) (func(), error) {
	m.rec.Record(trace.Event{Work: &trace.Work{System: m.name, Pending: true}})
	return m.fakeModel.Fault(f)
}

func TestExecuteRecordsAFaultsStartBeforeItsModelWrites(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Models = []ModelTarget{writingModel{fakeModel{fakeTarget{calls: &calls{}}, "model:gadgets", "gadgets"}, f.rec}}
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{{Match: plan.Match{Resource: "gadgets"}, Stuck: true}}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range f.rec.Events() {
		switch {
		case e.Fault != nil && !e.Fault.Ended:
			got = append(got, "fault")
		case e.Work != nil:
			got = append(got, "write")
		}
	}
	if !reflect.DeepEqual(got, []string{"fault", "write"}) {
		t.Errorf("events = %q, want the fault's start first", got)
	}
}

// fakeActor logs the damage a fake service takes.
type fakeActor struct{ *calls }

func (a fakeActor) Patch(_ context.Context, ref trace.Ref, patch []byte) error {
	a.add("patch %s %s", ref.Name, patch)
	return nil
}

func (a fakeActor) Delete(_ context.Context, ref trace.Ref, _ plan.Propagation) error {
	a.add("delete %s", ref.Name)
	return nil
}

func TestExecuteRoutesDamageToTheTargetsSystem(t *testing.T) {
	f := newFixture(t, nil)
	cloud := &calls{}
	f.runner.Actors["cloud"] = fakeActor{cloud}
	bucket := trace.Ref{System: "cloud", Resource: "buckets", Name: "buckets-1"}
	selecting(f, map[string]trace.Ref{"": bucket, "secrets": {System: "kube", Resource: "secrets", Name: "fixture"}})
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{
		{Do: plan.Damage, Target: &plan.Target{System: "cloud"}},
		{Do: plan.Damage, Target: &plan.Target{System: "cloud"}, Patch: json.RawMessage(`{"size":2}`)},
		{Do: plan.ChangeFixture, Target: &plan.Target{Resource: "secrets", Name: "fixture"}},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, want := cloud.all(), []string{"delete buckets-1", `patch buckets-1 {"size":2}`}; !reflect.DeepEqual(got, want) {
		t.Errorf("cloud calls = %q, want %q", got, want)
	}
	if got, want := f.cluster.all(), []string{"delete fixture"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster calls = %q, want %q", got, want)
	}
}

func TestExecuteRefusesDamageOnASystemWithNoActor(t *testing.T) {
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"": {System: "vault", Resource: "secrets", Name: "s"}})
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{System: "vault"}}}}}}
	if err := f.runner.Execute(context.Background(), p); err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("Execute = %v, want an error naming vault", err)
	}
}

func TestExecuteGivesTheDeadlineTheLongestPollOfTheStepsSystems(t *testing.T) {
	f := newFixture(t, nil)
	const cloudPoll = 200 * time.Millisecond
	f.runner.Config.Systems = []config.System{{Name: "cloud", Process: &config.Process{}, Poll: cloudPoll}}
	f.runner.Listeners["cloud"] = fakeTarget{calls: &calls{}}
	f.runner.Actors["cloud"] = fakeActor{&calls{}}
	f.runner.Models = []ModelTarget{fakeModel{fakeTarget{calls: &calls{}}, "cloud", ""}}
	selecting(f, map[string]trace.Ref{"": {System: "cloud", Resource: "buckets", Name: "b"}, "configmaps": {System: "kube", Resource: "configmaps", Name: "c"}})
	p := &plan.Plan{Steps: []plan.Step{
		{Actions: []plan.Action{{Do: plan.Restart}}},
		{Faults: []plan.Fault{{Match: plan.Match{Resource: "configmaps"}, Drop: true}}},
		{Faults: []plan.Fault{{System: "cloud", Drop: true}}},
		{Faults: []plan.Fault{{System: "cloud", Stuck: true}}},
		{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{System: "cloud"}}}},
		{Actions: []plan.Action{{Do: plan.Damage, Target: &plan.Target{Resource: "configmaps"}}}},
		{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: &plan.Target{System: "cloud", Name: "b"}}}},
		{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}}},
	}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	events := f.rec.Events()
	for step, poll := range []time.Duration{0, timeouts.Stable, cloudPoll, cloudPoll, cloudPoll, timeouts.Stable, cloudPoll, cloudPoll, cloudPoll} {
		start, _ := find(events, step, trace.FaultPeriod)
		endFaults, _ := find(events, step, trace.EndFaults)
		want, _ := Deadline(endFaults.At, endFaults.At-start.At, poll, timeouts)
		if got := endFaults.Phase.Deadline; (got - want).Abs() > 5*time.Millisecond {
			t.Errorf("step %d deadline = %v, want about %v", step, got, want)
		}
	}
}

func TestExecuteStopsWhenASystemFailsMidSettle(t *testing.T) {
	f := newFixture(t, func(*trace.Index, time.Duration, time.Duration) (time.Duration, bool) { return 0, false })
	failAt := time.Now().Add(100 * time.Millisecond)
	f.runner.Failed = func() error {
		if time.Now().After(failAt) {
			return errors.New("fake cloud exited")
		}
		return nil
	}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a", Object: widget("w")}}}}}
	err := f.runner.Execute(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "fake cloud exited") {
		t.Fatalf("Execute = %v, want the system's error", err)
	}
	if _, found := find(f.rec.Events(), 0, trace.TimedOut); found {
		t.Error("step 0 waited out its deadline")
	}
}

func TestExecuteRecordsEachFaultsSystem(t *testing.T) {
	f := newFixture(t, nil)
	f.runner.Listeners["cloud"] = fakeTarget{calls: &calls{}}
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{{System: "cloud", Drop: true}}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range f.rec.Events() {
		if e.Fault != nil {
			n++
			if e.Fault.System != "cloud" {
				t.Errorf("fault event %+v, want system cloud", *e.Fault)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d fault events, want 2", n)
	}
}

func TestExecuteRecordsTheKindAHoldWatchHolds(t *testing.T) {
	f := newFixture(t, nil)
	p := &plan.Plan{Steps: []plan.Step{{Faults: []plan.Fault{
		{HoldWatch: true, Match: plan.Match{Group: "cert-manager.io", Resource: "certificates"}},
		{Drop: true, Match: plan.Match{Resource: "secrets"}},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, want := f.faults.all()[:2], []string{"fault certificates", "fault secrets"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kube listener calls = %q, want %q", got, want)
	}
	hold := trace.Ref{System: "kube", Group: "cert-manager.io", Resource: "certificates"}
	var holds []*trace.Ref
	for _, e := range f.rec.Events() {
		if e.Fault != nil {
			holds = append(holds, e.Fault.Hold)
		}
	}
	if want := []*trace.Ref{&hold, nil, &hold, nil}; !reflect.DeepEqual(holds, want) {
		t.Errorf("fault holds = %v, want %v", holds, want)
	}
}

func TestExecuteRoutesAChangeFixtureToItsTargetsSystem(t *testing.T) {
	f := newFixture(t, nil)
	vault := &calls{}
	f.runner.Actors["vault"] = fakeActor{vault}
	asked := selecting(f, map[string]trace.Ref{"": {System: "vault", Resource: "secret", Name: "app"}})
	target := plan.Target{System: "vault", Name: "app"}
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{
		{Do: plan.ChangeFixture, Target: &target, Patch: json.RawMessage(`{"data":{"k":"v2"}}`)},
		{Do: plan.ChangeFixture, Target: &target},
	}}}}
	if err := f.runner.Execute(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got, want := vault.all(), []string{`patch app {"data":{"k":"v2"}}`, "delete app"}; !reflect.DeepEqual(got, want) {
		t.Errorf("vault calls = %q, want %q", got, want)
	}
	if got := f.cluster.all(); len(got) != 0 {
		t.Errorf("cluster calls = %q, want none", got)
	}
	for _, s := range *asked {
		if s.creator != trace.Harness {
			t.Errorf("Select asked for creator %s, want the harness", s.creator)
		}
	}
}

func TestExecuteRefusesAChangeFixtureOnASystemWithNoActor(t *testing.T) {
	f := newFixture(t, nil)
	selecting(f, map[string]trace.Ref{"": {System: "vault", Resource: "secret", Name: "app"}})
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.ChangeFixture, Target: &plan.Target{System: "vault", Name: "app"}}}}}}
	if err := f.runner.Execute(context.Background(), p); err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("Execute = %v, want an error naming vault", err)
	}
}
