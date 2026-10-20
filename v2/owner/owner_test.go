package owner_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace/tracetest"
)

const s = time.Second

var (
	widgetA = tt.Ref("toy/widgets", "ns", "a")
	widgetB = tt.Ref("toy/widgets", "ns", "b")
	cmA0    = tt.Ref("configmaps", "ns", "a-0")
	cmGen   = tt.Ref("configmaps", "ns", "a-x7k2")
	rs      = tt.Ref("apps/replicasets", "ns", "r")
	pod     = tt.Ref("pods", "ns", "p")
	ctl     = trace.Controller
)

// twoWidgets creates widget a at 1s and widget b at 2s. The harness creates
// both, a in slot a and b in slot b.
func twoWidgets() *tt.Timeline {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets")).
		At(1*s, tt.Create(trace.Harness, widgetA, "ua", "1"), tt.Version(widgetA, "ua", "1", tt.Object("a")),
			tt.SlotAction(0, "create", "a", widgetA)).
		At(2*s, tt.Create(trace.Harness, widgetB, "ub", "2"), tt.Version(widgetB, "ub", "2", tt.Object("b")),
			tt.SlotAction(0, "create", "b", widgetB))
	return &tl
}

func instance(t *testing.T, w *owner.World, uid string) owner.Instance {
	t.Helper()
	i, found := w.Instance(uid)
	if !found {
		t.Fatalf("no instance %s", uid)
	}
	return i
}

func TestInstanceLivesFromFirstVersionToDelete(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.Object("a-0"))).
		At(4*s, tt.Version(cmA0, "c1", "4", tt.Object("a-0"))).
		At(5*s, tt.Deleted(cmA0, "c1", "5", tt.Object("a-0"))).
		At(6*s, tt.Version(cmA0, "c2", "6", tt.Object("a-0")))
	w := owner.New(tl.Index())
	first, second := instance(t, w, "c1"), instance(t, w, "c2")
	if first.Ref != cmA0 || first.Created != 3*s || first.Gone != 5*s || second.Created != 6*s || second.Gone != 0 {
		t.Fatalf("instances = %+v, %+v", first, second)
	}
	if first.Live(5*s) || !first.Live(4*s) || first.Live(2*s) || !second.Live(time.Hour) {
		t.Fatal("Live is wrong")
	}
	var uids []string
	for _, i := range w.Instances(6 * s) {
		uids = append(uids, i.UID)
	}
	if !slices.Equal(uids, []string{"c2", "ua", "ub"}) {
		t.Fatalf("instances at 6s = %q", uids)
	}
}

func TestBodyIsTheLatestVersionAtT(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", map[string]any{"data": "old"})).
		At(4*s, tt.Version(cmA0, "c1", "4", map[string]any{"data": "new"}))
	w := owner.New(tl.Index())
	i := instance(t, w, "c1")
	if got := string(w.Body(i, 3500*time.Millisecond)); got != `{"data":"old"}` {
		t.Fatalf("body = %s", got)
	}
	if got := string(w.Body(i, 4*s)); got != `{"data":"new"}` {
		t.Fatalf("body = %s", got)
	}
	if w.Body(i, 2*s) != nil {
		t.Fatal("body before creation")
	}
}

func TestCreatorIsTheCallerWhoseCreateCarriedTheUID(t *testing.T) {
	failed := tt.Create(ctl, pod, "u-pod", "")
	failed.Call.Status = 409
	unknown := tt.Create(ctl, cmGen, "u-gen", "4")
	unknown.Call.Injected = trace.DropResponse
	other := tt.Create(ctl, rs, "u-other", "5")
	tl := twoWidgets()
	tl.At(3*s, tt.Create(ctl, cmA0, "c1", "3"), tt.Version(cmA0, "c1", "3", tt.Object("a-0"))).
		At(4*s, unknown, tt.Version(cmGen, "u-gen", "4", tt.Generated("a-", "a-x7k2"))).
		At(5*s, other, failed, tt.Version(rs, "u-rs", "5", tt.Object("r")), tt.Version(pod, "u-pod", "6", tt.Object("p")))
	w := owner.New(tl.Index())
	for uid, want := range map[string]trace.Caller{"c1": ctl, "u-gen": ctl, "ua": trace.Harness, "u-rs": trace.Cluster, "u-pod": trace.Cluster} {
		if got := instance(t, w, uid).Creator; got != want {
			t.Errorf("creator of %s = %s, want %s", uid, got, want)
		}
	}
}

func TestCreatorOfAnApplyIsItsCaller(t *testing.T) {
	apply := tt.Create(ctl, cmA0, "c1", "3")
	apply.Call.Verb = "patch"
	update := tt.Create(ctl, cmGen, "c2", "4")
	update.Call.Verb, update.Call.Status = "patch", 200
	tl := twoWidgets()
	tl.At(3*s, apply, tt.Version(cmA0, "c1", "3", tt.Object("a-0")), update, tt.Version(cmGen, "c2", "4", tt.Generated("a-", "a-x7k2")))
	w := owner.New(tl.Index())
	if got := instance(t, w, "c1").Creator; got != ctl {
		t.Errorf("creator after a 201 patch = %s", got)
	}
	if got := instance(t, w, "c2").Creator; got != trace.Cluster {
		t.Errorf("creator after a 200 patch = %s", got)
	}
}

func TestCreatorNeedsTheCreateToNameTheObject(t *testing.T) {
	elsewhere := tt.Create(ctl, tt.Ref("configmaps", "other", "a-0"), "c1", "3")
	tl := twoWidgets()
	tl.At(3*s, elsewhere, tt.Version(cmA0, "c1", "3", tt.Object("a-0")))
	if got := instance(t, owner.New(tl.Index()), "c1").Creator; got != trace.Cluster {
		t.Fatalf("creator = %s", got)
	}
}

func TestRoleIsGenerateNameOrName(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.Object("a-0")), tt.Version(cmGen, "c2", "4", tt.Generated("a-", "a-x7k2")))
	w := owner.New(tl.Index())
	if got := instance(t, w, "c1").Role; got != "a-0" {
		t.Errorf("named role = %q", got)
	}
	if got := instance(t, w, "c2").Role; got != "a-" {
		t.Errorf("generated role = %q", got)
	}
}

func ownerAt(t *testing.T, tl *tt.Timeline, uid string, at time.Duration) owner.Owner {
	t.Helper()
	w := owner.New(tl.Index())
	return w.Owner(instance(t, w, uid), at)
}

func TestOwnerRule1FollowsOwnerReferencesToACR(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ub", true)))
	if got := ownerAt(t, tl, "c1", 3*s); got.Rule != 1 || !slices.Equal(got.CRs, []string{"ub"}) {
		t.Fatalf("owner = %+v", got)
	}
}

func TestOwnerRule1FollowsAChainThroughAGoneInstance(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(rs, "u-rs", "3", tt.OwnedBy(tt.Object("r"), "ua", true))).
		At(4*s, tt.Version(pod, "u-pod", "4", tt.OwnedBy(tt.Object("p"), "u-rs", true))).
		At(5*s, tt.Deleted(rs, "u-rs", "5", tt.OwnedBy(tt.Object("r"), "ua", true)))
	if got := ownerAt(t, tl, "u-pod", 6*s); got.Rule != 1 || !slices.Equal(got.CRs, []string{"ua"}) {
		t.Fatalf("owner = %+v", got)
	}
}

func TestOwnerRule1FollowsTheControllerReferenceFirst(t *testing.T) {
	body := tt.OwnedBy(tt.OwnedBy(tt.Object("a-0"), "ua", false), "ub", true)
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", body))
	if got := ownerAt(t, tl, "c1", 3*s); !slices.Equal(got.CRs, []string{"ub"}) {
		t.Fatalf("owner = %+v", got)
	}
}

func TestOwnerRule1ReadsTheBodyAtT(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ua", true))).
		At(4*s, tt.Version(cmA0, "c1", "4", tt.OwnedBy(tt.Object("a-0"), "ub", true)))
	if got := ownerAt(t, tl, "c1", 3*s); !slices.Equal(got.CRs, []string{"ua"}) {
		t.Fatalf("owner at 3s = %+v", got)
	}
	if got := ownerAt(t, tl, "c1", 4*s); !slices.Equal(got.CRs, []string{"ub"}) {
		t.Fatalf("owner at 4s = %+v", got)
	}
}

func TestOwnerRule4IsTheCRsLiveAtCreation(t *testing.T) {
	tl := twoWidgets()
	tl.At(500*time.Millisecond, tt.Version(pod, "u-early", "0", tt.Object("p"))).
		At(1500*time.Millisecond, tt.Version(rs, "u-one", "3", tt.Object("r"))).
		At(3*s, tt.Version(cmA0, "u-both", "4", tt.OwnedBy(tt.Object("a-0"), "u-unknown", true))).
		At(4*s, tt.Deleted(widgetA, "ua", "5", tt.Object("a")))
	for uid, want := range map[string][]string{"u-early": nil, "u-one": {"ua"}, "u-both": {"ua", "ub"}} {
		if got := ownerAt(t, tl, uid, 5*s); got.Rule != 4 || !slices.Equal(got.CRs, want) {
			t.Errorf("owner of %s = %+v, want rule 4 %q", uid, got, want)
		}
	}
}

func TestOwnerSurvivesAnOwnerReferenceCycle(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(rs, "u-rs", "3", tt.OwnedBy(tt.Object("r"), "u-pod", true)),
		tt.Version(pod, "u-pod", "3", tt.OwnedBy(tt.Object("p"), "u-rs", true)))
	if got := ownerAt(t, tl, "u-pod", 3*s); got.Rule != 4 {
		t.Fatalf("owner = %+v", got)
	}
}

func TestOwnerKeyNamesTheRuleAndCRs(t *testing.T) {
	keys := map[string]bool{}
	for _, o := range []owner.Owner{{Rule: 1, CRs: []string{"ua"}}, {Rule: 4, CRs: []string{"ua"}}, {Rule: 4, CRs: []string{"ua", "ub"}}, {Rule: 4}} {
		keys[o.Key()] = true
	}
	if len(keys) != 4 || (owner.Owner{Rule: 4}).Shared() == false || (owner.Owner{Rule: 4, CRs: []string{"ua"}}).Shared() {
		t.Fatalf("keys = %v", keys)
	}
}

func TestSlotIsTheSlotWhoseCreateMadeTheCR(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Deleted(widgetA, "ua", "3", tt.Object("a"))).
		At(4*s, tt.Version(widgetA, "ua2", "4", tt.Object("a")), tt.SlotAction(1, "create", "c", widgetA))
	w := owner.New(tl.Index())
	for uid, want := range map[string]string{"ua": "a", "ub": "b", "ua2": "c", "unknown": ""} {
		if got := w.Slot(uid); got != want {
			t.Errorf("slot of %s = %q, want %q", uid, got, want)
		}
	}
}

// children gives widget a two configmaps and widget b one, all created by
// the controller, and the harness a fixture.
func children() *tt.Timeline {
	tl := twoWidgets()
	fixture := tt.Ref("configmaps", "ns", "fixture")
	tl.At(500*time.Millisecond, tt.Create(trace.Harness, fixture, "u-fix", "0"), tt.Version(fixture, "u-fix", "0", tt.Object("fixture")))
	for i, child := range []struct {
		name, uid, owner string
	}{{"a-0", "c-a0", "ua"}, {"a-1", "c-a1", "ua"}, {"b-0", "c-b0", "ub"}} {
		ref := tt.Ref("configmaps", "ns", child.name)
		rv := string(rune('a' + i))
		tl.At(3*s+time.Duration(i)*s, tt.Create(ctl, ref, child.uid, rv), tt.Version(ref, child.uid, rv, tt.OwnedBy(tt.Object(child.name), child.owner, true)))
	}
	return tl
}

func TestSelectPicksTheNthMatchInCreationOrder(t *testing.T) {
	w := owner.New(children().Index())
	cases := []struct {
		creator trace.Caller
		target  plan.Target
		slot    string
		want    string
	}{
		{ctl, plan.Target{Resource: "configmaps"}, "", "a-0"},
		{ctl, plan.Target{Resource: "configmaps", Nth: 3}, "", "b-0"},
		{ctl, plan.Target{Resource: "configmaps"}, "b", "b-0"},
		{ctl, plan.Target{Resource: "configmaps", Nth: 2}, "a", "a-1"},
		{ctl, plan.Target{Resource: "configmaps", Role: "a-1"}, "", "a-1"},
		{ctl, plan.Target{System: "kube", Resource: "configmaps", Name: "b-0"}, "", "b-0"},
		{trace.Harness, plan.Target{Resource: "configmaps"}, "", "fixture"},
		{ctl, plan.Target{Resource: "configmaps", Nth: 2}, "b", ""},
		{ctl, plan.Target{Resource: "configmaps", Group: "apps"}, "", ""},
		{ctl, plan.Target{System: "vault", Resource: "configmaps"}, "", ""},
		{ctl, plan.Target{Resource: "configmaps"}, "z", ""},
	}
	for _, c := range cases {
		ref, found := w.Select(c.creator, c.target, c.slot, 10*s)
		if found != (c.want != "") || ref.Name != c.want {
			t.Errorf("Select(%s, %+v, %q) = %+v, %v; want %q", c.creator, c.target, c.slot, ref, found, c.want)
		}
	}
}

func TestSelectSkipsGoneInstancesAndTheSlotsGoneCR(t *testing.T) {
	tl := children()
	tl.At(7*s, tt.Deleted(tt.Ref("configmaps", "ns", "a-0"), "c-a0", "x", tt.Object("a-0"))).
		At(8*s, tt.Deleted(widgetB, "ub", "y", tt.Object("b")))
	w := owner.New(tl.Index())
	if ref, _ := w.Select(ctl, plan.Target{Resource: "configmaps"}, "", 7*s); ref.Name != "a-1" {
		t.Errorf("Select = %+v, want a-1", ref)
	}
	if ref, found := w.Select(ctl, plan.Target{Resource: "configmaps"}, "b", 8*s); found {
		t.Errorf("Select in slot b after its CR went = %+v", ref)
	}
}
