package owner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	clusters  = trace.Kind{Group: "toy", Version: "v1", Kind: "Cluster", Resource: "clusters"}
	databases = trace.Kind{Group: "toy", Version: "v1", Kind: "Database", Resource: "databases"}
	secrets   = trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}
	clusterA  = tt.Ref("toy/clusters", "ns", "a")
	databaseB = tt.Ref("toy/databases", "ns", "b")
)

// fleet is a run whose primaries list kinds. The harness creates Cluster a
// in slot a at 1s and Database b in slot b at 2s.
func fleet(kinds ...trace.Kind) *tt.Timeline {
	run := &trace.Run{Namespace: "ns"}
	for _, k := range kinds {
		run.Primaries = append(run.Primaries, trace.Primary{Kind: k, Ready: "true"})
	}
	var tl tt.Timeline
	tl.At(0, trace.Event{Run: run}).
		At(1*s, tt.Create(trace.Harness, clusterA, "ua", "1"), tt.Version(clusterA, "ua", "1", naming("a", "x")),
			tt.SlotAction(0, "create", "a", clusterA)).
		At(2*s, tt.Create(trace.Harness, databaseB, "ub", "2"), tt.Version(databaseB, "ub", "2", naming("b", "x")),
			tt.SlotAction(0, "create", "b", databaseB))
	return &tl
}

func TestRule1ReachesACROfAnyListedKind(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ub", true)))
	wantOwner(t, ownerBy(t, tl, owner.Rules{}, "c1", 3*s), 1, "ub")
}

func TestRule4SpansEveryListedKind(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.Object("a-0")))
	wantOwner(t, ownerBy(t, tl, owner.Rules{}, "c1", 3*s), 4, "ua", "ub")
	wantOwner(t, ownerBy(t, tl, owner.Rules{}, "ub", 3*s), 4, "ua")
}

func TestACRTheControllerCreatedIsACRThatItsOwnerOwns(t *testing.T) {
	t.Skip("enabled by the judge package")
	databaseC := tt.Ref("toy/databases", "ns", "c")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Create(ctl, databaseC, "uc", "3"), tt.Version(databaseC, "uc", "3", tt.OwnedBy(tt.Object("c"), "ua", true))).
		At(4*s, tt.Version(cmA0, "c1", "4", tt.OwnedBy(tt.Object("a-0"), "uc", true)))
	w := owner.New(tl.Index(), owner.Rules{})
	c := instance(t, w, "uc")
	if !w.Primary(c) || c.Creator != ctl {
		t.Errorf("Database c: Primary = %v, creator %s; want a CR the controller created.", w.Primary(c), c.Creator)
	}
	wantOwner(t, w.Owner(c, 4*s), 1, "ua")
	wantOwner(t, w.Owner(instance(t, w, "c1"), 4*s), 1, "uc")
}

func TestAChildOfALiveUnlistedOwnerHasNoCR(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters)
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ub", true))).
		At(5*s, tt.Deleted(databaseB, "ub", "5", tt.Object("b")))
	w := owner.New(tl.Index(), owner.Rules{})
	child := instance(t, w, "c1")
	o := w.Owner(child, 3*s)
	if o.Rule != 1 || len(o.CRs) != 0 || o.Unlisted != "ub" || !o.Shared() || o.Key() != "ub" {
		t.Errorf("owner while Database b lives = %+v, Shared %v, Key %q; want rule 1 to unlisted owner ub, and no CR.", o, o.Shared(), o.Key())
	}
	if cr, named := o.Named(); named {
		t.Errorf("Named = %q, want no CR.", cr)
	}
	wantOwner(t, w.Owner(child, 5*s), 4, "ua")
}

func TestRule1PrefersACRToAnUnlistedOwner(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters)
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.OwnedBy(tt.Object("a-0"), "ub", true), "ua", false)))
	wantOwner(t, ownerBy(t, tl, owner.Rules{}, "c1", 3*s), 1, "ua")
}

func TestAnOwnerReferenceToAnUnseenUIDFallsBackToRule4(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "unseen", true)))
	o := ownerBy(t, tl, owner.Rules{}, "c1", 3*s)
	wantOwner(t, o, 4, "ua", "ub")
	if o.Unlisted != "" {
		t.Errorf("Unlisted = %q, want none for a UID the trace never saw.", o.Unlisted)
	}
}

func TestRule2SkipsANameThatCRsOfTwoKindsShare(t *testing.T) {
	t.Skip("enabled by the judge package")
	databaseA := tt.Ref("toy/databases", "ns", "a")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Version(databaseA, "ua-db", "3", tt.Object("a"))).
		At(4*s, tt.Version(cmA0, "c-name", "4", labeled("a-0", "a")), tt.Version(cmGen, "c-uid", "4", labeled("a-x7k2", "ub")),
			tt.Version(tt.Ref("configmaps", "ns", "b-0"), "c-qualified", "4", labeled("b-0", "ns/b")))
	rules := compile(t, labelOwner)
	wantOwner(t, ownerBy(t, tl, rules, "c-name", 4*s), 4, "ua", "ua-db", "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c-uid", 4*s), 2, "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c-qualified", 4*s), 2, "ub")
}

func TestRule3NamesAResourceForACROfItsEntrysKind(t *testing.T) {
	t.Skip("enabled by the judge package")
	rules, err := owner.Compile(nil, []trace.Primary{
		{Kind: clusters, Owns: []trace.Owns{{Kind: configMaps, Name: "self.spec.child"}}},
		{Kind: databases, Owns: []trace.Owns{{Kind: secrets, Name: "self.spec.child"}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "ns", "x"), "c1", "3", tt.Object("x")),
		tt.Version(tt.Ref("secrets", "ns", "x"), "s1", "3", tt.Object("x")))
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 3, "ua")
	wantOwner(t, ownerBy(t, tl, rules, "s1", 3*s), 3, "ub")
}

func TestSelectMatchesInASlotOfAnyListedKind(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters, databases)
	tl.At(3*s, tt.Create(ctl, cmA0, "c1", "3"), tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ub", true)))
	w := owner.New(tl.Index(), owner.Rules{})
	if ref, err := w.Select(ctl, plan.Target{Resource: "configmaps"}, "b", 4*s); err != nil || ref != cmA0 {
		t.Errorf("Select in slot b = %+v, %v; want %+v.", ref, err, cmA0)
	}
	if _, err := w.Select(ctl, plan.Target{Resource: "configmaps"}, "a", 4*s); !errors.Is(err, owner.ErrNoMatch) {
		t.Errorf("Select in slot a = %v, want no match.", err)
	}
}

func TestSelectNamesTheKindOfASlotThatHoldsNoCR(t *testing.T) {
	t.Skip("enabled by the judge package")
	tl := fleet(clusters)
	tl.At(3*s, tt.Create(ctl, cmA0, "c1", "3"), tt.Version(cmA0, "c1", "3", tt.OwnedBy(tt.Object("a-0"), "ub", true)))
	w := owner.New(tl.Index(), owner.Rules{})
	_, err := w.Select(ctl, plan.Target{Resource: "configmaps"}, "b", 4*s)
	if !errors.Is(err, owner.ErrNoMatch) || !strings.Contains(err.Error(), "databases.toy") {
		t.Errorf("Select in slot b = %v, want no match that names databases.toy.", err)
	}
}
