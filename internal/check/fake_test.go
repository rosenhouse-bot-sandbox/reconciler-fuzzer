package check_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	bucketA     = trace.Ref{System: "cloud", Resource: "buckets", Name: "b1"}
	cloudSystem = trace.System{Name: "cloud", API: "crud", Ready: "self.state == 'READY'", Gone: "self.state == 'DELETING'",
		Ignore: []string{"version", "etag"}, Poll: 500 * ms, Dependency: &trace.Dependency{Required: true}}
	tagOwner = map[string]string{"cloud": "self.tags.cr"}
)

func bucketBody(state, cr string) map[string]any {
	return map[string]any{"id": "b1", "state": state, "tags": map[string]any{"cr": cr}}
}

func TestSpecForReadsFakesFromTheRunEvent(t *testing.T) {
	run := tt.Run("toy", "widgets")
	run.Run.Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "self.ok", 4*s, 3, 5
	run.Run.Halted = "has(self.halted)"
	run.Run.Systems = []trace.System{cloudSystem, {Name: "other", API: "crud", Role: "self.role", Poll: 3 * s}}
	var b tt.Builder
	got, err := check.SpecFor(nil, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := got.Halted(map[string]any{"halted": 1}); !ok || err != nil {
		t.Errorf("Halted = %v, %v", ok, err)
	}
	if want := map[string]time.Duration{"cloud": 500 * ms, "other": 3 * s}; !reflect.DeepEqual(got.Polls, want) {
		t.Errorf("polls = %v", got.Polls)
	}
	if want := map[string][]string{"cloud": {"version", "etag"}}; !reflect.DeepEqual(got.Ignore, want) {
		t.Errorf("ignore = %v", got.Ignore)
	}
	if len(got.Models) != 1 || got.Models[0].System != "cloud" || got.Models[0].Kind != (trace.Kind{}) || !got.Models[0].Dependency.Required {
		t.Fatalf("models = %+v", got.Models)
	}
	if ok, err := got.Models[0].Ready(map[string]any{"state": "READY"}); !ok || err != nil {
		t.Errorf("model Ready = %v, %v", ok, err)
	}
	if got.Owners.Gone["cloud"] == nil || got.Owners.Roles["other"] == nil {
		t.Errorf("owners = %+v", got.Owners)
	}

	run.Run.Halted = ""
	b = tt.Builder{}
	if got, err := check.SpecFor(nil, b.Add(run).Index()); err != nil || got.Halted != nil {
		t.Errorf("no halted: set %v, %v", got.Halted != nil, err)
	}
	for name, mistake := range map[string]func(r *trace.Run){
		"halted": func(r *trace.Run) { r.Halted = "self.(" },
		"ready":  func(r *trace.Run) { r.Systems[0].Ready = "self.(" },
		"gone":   func(r *trace.Run) { r.Systems[0].Gone = "'x'" },
	} {
		bad := *run.Run
		bad.Systems = slices.Clone(run.Run.Systems)
		mistake(&bad)
		b = tt.Builder{}
		if _, err := check.SpecFor(nil, b.Add(trace.Event{Run: &bad}).Index()); err == nil {
			t.Errorf("a bad %s is no error", name)
		}
	}
}

// bucketConfig is the toy-widget config with halted and a fake service.
func bucketConfig(t *testing.T) *config.Config {
	t.Helper()
	widget, err := filepath.Abs("../../targets/toy-widget")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `launch: {binary: ` + widget + `/bin}
crds: [` + widget + `/crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: ` + widget + `/widget.yaml
  ready: "true"
  halted: has(self.halted)
timeouts: {stable: 2s}
systems:
- {name: cloud, api: crud, process: {command: [x]}, ready: self.state == 'READY', poll: 700ms, dependency: {track: 1s}}
- {name: other, api: crud, process: {command: [x]}}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSpecForTakesFakesFromTheConfig(t *testing.T) {
	c := bucketConfig(t)
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	recorded := cloudSystem
	recorded.Ready, recorded.Poll = "false", time.Hour
	run.Run.Systems = []trace.System{recorded}
	var b tt.Builder
	got, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := got.Halted(map[string]any{"halted": 1}); !ok || err != nil {
		t.Errorf("Halted = %v, %v", ok, err)
	}
	if want := map[string]time.Duration{"cloud": 700 * ms, "other": 2 * s}; !reflect.DeepEqual(got.Polls, want) {
		t.Errorf("polls = %v", got.Polls)
	}
	if want := map[string][]string{"cloud": {"version"}, "other": {"version"}}; !reflect.DeepEqual(got.Ignore, want) {
		t.Errorf("ignore = %v", got.Ignore)
	}
	if len(got.Models) != 1 || got.Models[0].System != "cloud" || *got.Models[0].Dependency != (trace.Dependency{Required: true, Track: s}) {
		t.Fatalf("models = %+v", got.Models)
	}
	if ok, err := got.Models[0].Ready(map[string]any{"state": "READY"}); !ok || err != nil {
		t.Errorf("model Ready is not the config's: %v, %v", ok, err)
	}
	c.Primary.Halted = ""
	if got, err := check.SpecFor(c, b.Index()); err != nil || got.Halted != nil {
		t.Errorf("no halted: set %v, %v", got.Halted != nil, err)
	}
}

// haltedSpec is spec with a halted CEL and owning-CR rule 2 on cloud.
func haltedSpec(t *testing.T) check.Spec {
	t.Helper()
	sp := spec
	sp.Halted = func(cr map[string]any) (bool, error) {
		status, _ := cr["status"].(map[string]any)
		return status["halted"] == true, nil
	}
	var err error
	if sp.Owners, err = owner.Compile(tagOwner, nil, nil); err != nil {
		t.Fatal(err)
	}
	return sp
}

func TestClause1CountsAHaltedCRAfterAnUnknownOutcome(t *testing.T) {
	halted := map[string]any{"status": map[string]any{"ready": false, "halted": true}}
	unknown := func(e trace.Event) trace.Event {
		e.Call.Injected = trace.DropResponse
		return e
	}
	bucket := func(cr string) trace.Event { return tt.Version(bucketA, "ubk", "7", bucketBody("CREATING", cr)) }
	for name, c := range map[string]struct {
		at     time.Duration
		events []trace.Event
		done   bool
	}{
		"an unknown write to the CR":        {1500 * ms, []trace.Event{unknown(tt.Call(ctl, "update", widgetB, 200, ""))}, true},
		"an unknown create the CR owns":     {1500 * ms, []trace.Event{unknown(tt.Create(ctl, bucketA, "ubk", "7")), bucket("ub")}, true},
		"an unknown call after t":           {6 * s, []trace.Event{unknown(tt.Call(ctl, "update", widgetB, 200, ""))}, false},
		"a known write to the CR":           {1500 * ms, []trace.Event{tt.Call(ctl, "update", widgetB, 200, "")}, false},
		"an unknown write to another CR":    {1500 * ms, []trace.Event{unknown(tt.Call(ctl, "update", widgetA, 200, ""))}, false},
		"an unknown create another CR owns": {1500 * ms, []trace.Event{unknown(tt.Create(ctl, bucketA, "ubk", "7")), bucket("ua")}, false},
		"an unknown create no rule names":   {1500 * ms, []trace.Event{unknown(tt.Create(ctl, bucketA, "ubk", "7")), bucket("")}, false},
		"an unknown create of no instance":  {1500 * ms, []trace.Event{unknown(tt.Create(ctl, bucketA, "ubk", "7"))}, false},
		"no unknown call":                   {1500 * ms, nil, false},
	} {
		b := settledAt3s()
		b.At(1 * s).Add(tt.Version(widgetB, "ub", "2", halted)).At(c.at).Add(c.events...)
		unmet := check.Settled(b.Index(), 5*s, haltedSpec(t))
		if notReady := slices.ContainsFunc(unmet, func(u string) bool { return strings.Contains(u, "ns/b is not ready") }); notReady == c.done {
			t.Errorf("%s: unmet = %q", name, unmet)
		}
	}
	var alone tt.Builder
	alone.Add(tt.Run("toy", "widgets"), tt.Start(1)).
		At(1*s).Add(tt.Version(widgetB, "ub", "2", halted)).
		At(1500*ms).Add(unknown(tt.Create(ctl, bucketA, "ubk", "7")), bucket(""))
	if unmet := check.Settled(alone.Index(), 5*s, haltedSpec(t)); slices.ContainsFunc(unmet, func(u string) bool { return strings.Contains(u, "ns/b is not ready") }) {
		t.Errorf("an unknown create while only the CR lived: unmet = %q", unmet)
	}
	b := settledAt3s()
	b.At(1 * s).Add(tt.Version(widgetB, "ub", "2", ready(false))).At(1500 * ms).Add(unknown(tt.Call(ctl, "update", widgetB, 200, "")))
	if unmet := check.Settled(b.Index(), 5*s, haltedSpec(t)); !slices.ContainsFunc(unmet, func(u string) bool { return strings.Contains(u, "ns/b is not ready") }) {
		t.Errorf("a CR not halted: unmet = %q", unmet)
	}
}

// fakeDamagedAt1200ms is damagedAt1200ms on a bucket.
func fakeDamagedAt1200ms(refs ...trace.Ref) *tt.Builder {
	b := settledAt3s()
	b.At(1 * s).Add(tt.Phase(0, trace.FaultPeriod))
	for _, ref := range refs {
		b.At(1200 * ms).Add(tt.Action(0, "damage", ref))
	}
	b.At(1300 * ms).Add(tt.EndFaults(0, 20*s, false))
	return b
}

func TestClause5LapsesAfterTheLongestPollOfTheChangedSystems(t *testing.T) {
	sp := spec
	sp.Polls = map[string]time.Duration{"cloud": 4 * s, "fast": s, "slow": 8 * s}
	for name, c := range map[string]struct {
		refs  []trace.Ref
		lapse time.Duration
	}{
		"a bucket":          {[]trace.Ref{bucketA}, 7300 * ms},
		"a configmap":       {[]trace.Ref{cm}, 4300 * ms},
		"a fast bucket":     {[]trace.Ref{{System: "fast", Resource: "buckets", Name: "b1"}}, 2800 * ms},
		"both":              {[]trace.Ref{cm, bucketA}, 7300 * ms},
		"an unknown system": {[]trace.Ref{{System: "nosuch", Resource: "x", Name: "y"}}, 4300 * ms},
	} {
		ix := fakeDamagedAt1200ms(c.refs...).Index()
		if unmet := check.Settled(ix, c.lapse-1, sp); len(unmet) == 0 {
			t.Errorf("%s: unmet before the lapse = %q", name, unmet)
		}
		if got, ok := check.FirstSettled(ix, 1300*ms, 20*s, sp); !ok || got != c.lapse {
			t.Errorf("%s: FirstSettled = %v, %v; want %v", name, got, ok, c.lapse)
		}
	}
}

func TestClause5LooksAtABucket(t *testing.T) {
	b := fakeDamagedAt1200ms(bucketA)
	b.At(1500 * ms).Add(tt.Call(ctl, "get", bucketA, 404, ""))
	if unmet := check.Settled(b.Index(), 3500*ms, spec); len(unmet) != 0 {
		t.Errorf("unmet = %q", unmet)
	}
}

// bucketRun starts step 0, in which the harness creates slot a and the
// controller creates its bucket in cloud, which becomes READY after
// creating runs.
func bucketRun(dep trace.Dependency, creating ...func(r *tt.Script)) *tt.Script {
	r := newRun(1)
	cloud := cloudSystem
	cloud.Dependency = &dep
	r.Spec.Systems, r.Spec.Owners = []trace.System{cloud}, tagOwner
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Create(ctl, bucketA, bucketBody("CREATING", r.UID(widgetA)))
	for _, f := range creating {
		f(r)
	}
	r.Add(tt.Version(bucketA, r.UID(bucketA), "50", bucketBody("READY", r.UID(widgetA))))
	return r
}

func TestHonestJudgesADependencyFake(t *testing.T) {
	r := bucketRun(trace.Dependency{Required: true})
	r.See(ctl, bucketA).Ready(widgetA)
	requireNone(t, honest(t, r))

	r = bucketRun(trace.Dependency{Required: true}, func(r *tt.Script) { r.See(ctl, bucketA) })
	r.Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "cloud/buckets", "slot a", "cloud buckets b1 seen not ready")
}

func TestHonestCountsOnlyTheFakesOwnSystem(t *testing.T) {
	r := newRun(1)
	r.Spec.Systems, r.Spec.Owners = []trace.System{cloudSystem}, map[string]string{"cloud": "self.tags.cr", "other": "self.tags.cr"}
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Create(ctl, trace.Ref{System: "other", Resource: "buckets", Name: "b1"}, bucketBody("CREATING", r.UID(widgetA)))
	r.Ready(widgetA)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/toy/widgets")
}

func TestHonestTracksADependencyFake(t *testing.T) {
	r := bucketRun(trace.Dependency{Required: true, Track: s})
	r.See(ctl, bucketA).Ready(widgetA)
	r.Add(tt.Version(bucketA, r.UID(bucketA), "60", bucketBody("FAILED", r.UID(widgetA)))).See(ctl, bucketA)
	r.Tick(2 * s)
	requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "cloud/buckets", "buckets b1 seen not ready")
}

func TestModelsOfAFakeModelNoKubeKind(t *testing.T) {
	sp := spec
	sp.StatusKinds = []trace.Kind{{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}}
	sp.Models = []check.Model{{System: "cloud", Kind: sp.StatusKinds[0]}}
	if lines := unmodelled(t, sp, bare(gadgetA), tt.Call(ctl, "get", gadgetA, 200, "")); len(lines) != 1 {
		t.Errorf("Converge: lines = %q, want one", lines)
	}

	r := twoCRs()
	r.Spec.Models = nil
	r.Begin().Restart()
	r.Update(ctl, childA, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": "a-0"}, "status": "new"}, r.UID(widgetA), true))
	r.EndFaults().Settle()
	ix := r.Index()
	sp, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	sp.Models = []check.Model{{System: "cloud", Kind: trace.Kind{Resource: "configmaps"}}}
	requireFinding(t, check.Restore(ix, sp), "Restore", 1, "changed resources", "kube/configmaps", "status")
}

func TestRestoreIgnoresAFakesIgnoreFields(t *testing.T) {
	for name, c := range map[string]struct {
		change map[string]any
		want   string
	}{
		"version and etag": {map[string]any{"version": 2, "etag": "x"}, ""},
		"a field":          {map[string]any{"size": 2}, "size"},
		"a status":         {map[string]any{"status": 2}, "status"},
	} {
		r := newRun(2)
		r.Spec.Systems, r.Spec.Owners = []trace.System{cloudSystem}, tagOwner
		r.Spec.Models = []trace.Model{{Kind: trace.Kind{Version: "v1", Kind: "Bucket", Resource: "buckets"}, Ready: "true"}}
		r.Begin()
		r.CreateCR("a", widgetA)
		body := bucketBody("READY", r.UID(widgetA))
		body["version"], body["etag"], body["size"] = 1, "e", 1
		r.Create(ctl, bucketA, body)
		r.Ready(widgetA).EndFaults().Settle()
		r.Begin().Restart()
		changed := bucketBody("READY", r.UID(widgetA))
		changed["version"], changed["etag"], changed["size"] = 1, "e", 1
		for k, v := range c.change {
			changed[k] = v
		}
		r.Update(ctl, bucketA, changed)
		r.EndFaults().Settle()
		findings := restore(t, r)
		if c.want == "" {
			if len(findings) != 0 {
				t.Errorf("%s: findings = %+v", name, findings)
			}
			continue
		}
		requireFinding(t, findings, "Restore", 1, "changed resources", "cloud/buckets", c.want)
	}
}

func unmapped(caller trace.Caller, method, path string) trace.Event {
	return trace.Event{Call: &trace.Call{Caller: caller, Method: method, Path: path, Verb: strings.ToLower(method),
		Ref: trace.Ref{System: "cloud"}, Status: 404, Unmapped: true}}
}

func TestJudgeNotesEachDistinctUnmappedRequest(t *testing.T) {
	b := cleanRun()
	b.At(3*s).Add(unmapped(ctl, "GET", "/healthz"), unmapped(ctl, "GET", "/healthz"), unmapped(ctl, "POST", "/healthz"),
		unmapped(trace.Harness, "GET", "/_fake/resources"))
	var notes []string
	for _, note := range check.Judge(b.Index(), spec).Notes {
		if strings.Contains(note, "unmapped") {
			notes = append(notes, note)
		}
	}
	if want := []string{"cloud: unmapped request GET /healthz", "cloud: unmapped request POST /healthz"}; !slices.Equal(notes, want) {
		t.Errorf("notes = %q, want %q", notes, want)
	}
}

func TestJudgeNotesNoHeldObjectForAFakesModelFault(t *testing.T) {
	sp := spec
	sp.Polls = map[string]time.Duration{"cloud": s}
	for model, noted := range map[string]bool{"cloud": false, "model:gadgets.toy": true} {
		b := createStep()
		b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), tt.ModelFault(1, 0, false, "stuck", model)).
			At(3 * s).Add(tt.ModelFault(1, 0, true, "stuck", model))
		if got := slices.Contains(check.Judge(b.Index(), sp).Notes, "Step 1, fault 0 (stuck) held no object."); got != noted {
			t.Errorf("%s: noted = %v", model, got)
		}
	}
}

func TestJudgeNotesAFakeRequestFaultThatPickedNoRequest(t *testing.T) {
	const what = "error(503) on system=cloud verb=create"
	picked := tt.Call(ctl, "create", trace.Ref{System: "cloud", Resource: "buckets"}, 503, "")
	picked.Call.Injected = "error(503)"
	onKube := tt.Call(ctl, "create", trace.Ref{System: "kube", Resource: "configmaps"}, 503, "")
	onKube.Call.Injected = "error(503)"
	cloudFault := func(ended bool) trace.Event {
		e := tt.Fault(1, 0, ended, what)
		e.Fault.System = "cloud"
		return e
	}
	for name, c := range map[string]struct {
		call  trace.Event
		noted bool
	}{
		"picked":         {picked, false},
		"not picked":     {tt.Call(ctl, "get", bucketA, 200, ""), true},
		"picked on kube": {onKube, true},
	} {
		b := createStep()
		b.At(2*s).Add(tt.Phase(1, trace.FaultPeriod), cloudFault(false)).
			At(2100 * ms).Add(c.call).
			At(3 * s).Add(cloudFault(true))
		if got := slices.Contains(check.Judge(b.Index(), spec).Notes, "Step 1, fault 0 ("+what+") picked no request."); got != c.noted {
			t.Errorf("%s: noted = %v", name, got)
		}
	}
}

func TestHonestDropsABucketTheControllerLastSawGone(t *testing.T) {
	r := bucketRun(trace.Dependency{Required: true})
	r.See(ctl, bucketA).Ready(widgetA)
	cr := r.UID(widgetA)
	r.Add(tt.Version(bucketA, r.UID(bucketA), "60", bucketBody("DELETING", cr))).See(ctl, bucketA)
	r.Update(ctl, widgetA, tt.CR(widgetA.Name, false))
	r.Add(tt.Deleted(bucketA, r.UID(bucketA), "70", bucketBody("DELETING", cr)))
	bucketB := trace.Ref{System: "cloud", Resource: "buckets", Name: "b2"}
	r.Create(ctl, bucketB, bucketBody("CREATING", cr))
	r.Add(tt.Version(bucketB, r.UID(bucketB), "80", bucketBody("READY", cr))).See(ctl, bucketB).Ready(widgetA)
	requireNone(t, honest(t, r))
}

func TestClause7CountsEachSystemsBudgetOverItsPoll(t *testing.T) {
	sp := spec
	sp.Polls = map[string]time.Duration{"cloud": s}
	b := settledAt3s()
	b.At(1500*ms).Add(tt.Call(ctl, "get", bucketA, 200, ""), tt.Call(ctl, "get", bucketA, 200, ""), tt.Call(ctl, "get", bucketA, 200, ""))
	b.At(2*s).Add(tt.Call(ctl, "get", widgetA, 200, ""), tt.Call(ctl, "get", widgetA, 200, ""))
	b.At(2500*ms).Add(tt.Call(ctl, "get", bucketA, 200, ""), tt.Call(ctl, "get", bucketA, 200, ""))
	if unmet := check.Settled(b.Index(), 3*s, sp); len(unmet) != 0 {
		t.Fatalf("two reads of each system in its poll: unmet = %q", unmet)
	}
	b.Add(tt.Call(ctl, "get", bucketA, 200, ""))
	want := "the controller made 3 gets and lists of cloud in (2s, 3s], over the budget of 2"
	if unmet := check.Settled(b.Index(), 3*s, sp); !slices.Contains(unmet, want) {
		t.Fatalf("unmet = %q, want %q", unmet, want)
	}
}

func TestRestoreAllowsAnEmulatorsNewVersionOfTheSameData(t *testing.T) {
	for api, c := range map[string]struct {
		ref           trace.Ref
		before, after func(data string) map[string]any
	}{
		"vault": {
			trace.Ref{System: "cloud", Resource: "secret", Name: "app"},
			func(data string) map[string]any {
				return map[string]any{"path": "app", "current_version": 1, "versions": map[string]any{"1": map[string]any{}},
					"data": map[string]any{"k": data}, "created_time": "t1", "updated_time": "t1"}
			},
			func(data string) map[string]any {
				return map[string]any{"path": "app", "current_version": 2, "versions": map[string]any{"1": map[string]any{}, "2": map[string]any{}},
					"data": map[string]any{"k": data}, "created_time": "t1", "updated_time": "t2"}
			},
		},
		"s3": {
			trace.Ref{System: "cloud", Resource: "objects", Namespace: "b", Name: "k"},
			func(data string) map[string]any {
				return map[string]any{"bucket": "b", "key": "k", "etag": data, "versionId": "v1", "lastModified": "t1"}
			},
			func(data string) map[string]any {
				return map[string]any{"bucket": "b", "key": "k", "etag": data, "versionId": "v2", "lastModified": "t2"}
			},
		},
	} {
		for _, data := range []string{"same", "other"} {
			r := newRun(2)
			r.Spec.Systems = []trace.System{{Name: "cloud", API: api, Ignore: config.APIMaps[api].Ignore}}
			r.Begin()
			r.CreateCR("a", widgetA)
			r.Create(ctl, c.ref, c.before("same"))
			r.Ready(widgetA).EndFaults().Settle()
			r.Begin().Restart()
			r.Update(ctl, c.ref, c.after(data))
			r.EndFaults().Settle()
			findings := restore(t, r)
			if data == "same" && len(findings) != 0 {
				t.Errorf("%s: a new version of the same data: findings = %+v", api, findings)
			}
			if data == "other" && len(findings) != 1 {
				t.Errorf("%s: other data: findings = %+v, want changed resources", api, findings)
			}
		}
	}
}
