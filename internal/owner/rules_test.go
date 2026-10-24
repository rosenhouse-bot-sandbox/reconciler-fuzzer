package owner_test

import (
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	widgets      = trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgets"}
	configMaps   = trace.Kind{Version: "v1", Kind: "ConfigMap", Resource: "configmaps"}
	clusterRoles = trace.Kind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Resource: "clusterroles"}
	labelOwner   = map[string]string{"kube": "self.metadata.labels.owner"}
)

func compile(t *testing.T, owners map[string]string, owns ...trace.Owns) owner.Rules {
	t.Helper()
	rules, err := owner.Compile(owners, []trace.Primary{{Kind: widgets, Owns: owns}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func ownerBy(t *testing.T, tl *tt.Timeline, rules owner.Rules, uid string, at time.Duration) owner.Owner {
	t.Helper()
	w := owner.New(tl.Index(), rules)
	return w.Owner(instance(t, w, uid), at)
}

func wantOwner(t *testing.T, got owner.Owner, rule int, crs ...string) {
	t.Helper()
	if got.Rule != rule || !slices.Equal(got.CRs, crs) {
		t.Errorf("owner = %+v, want rule %d %q", got, rule, crs)
	}
}

func labeled(name string, owner any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"owner": owner}}}
}

// naming is a CR named name whose spec.child names its resource.
func naming(name, child string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}, "spec": map[string]any{"child": child}}
}

func TestCompileRefusesBadCEL(t *testing.T) {
	if _, err := owner.Compile(map[string]string{"kube": "self.("}, nil, nil); err == nil {
		t.Error("a bad owner compiles")
	}
	if _, err := owner.Compile(nil, []trace.Primary{{Kind: widgets, Owns: []trace.Owns{{Kind: configMaps, Name: "1 + 2"}}}}, nil); err == nil {
		t.Error("a bad name compiles")
	}
	if _, err := owner.Compile(nil, []trace.Primary{{Kind: widgets, Owns: []trace.Owns{{Kind: configMaps, Name: "'x'", Namespace: "self.("}}}}, nil); err == nil {
		t.Error("a bad namespace compiles")
	}
}

func TestRule2NamesTheCRByUID(t *testing.T) {
	elsewhere := tt.Ref("configmaps", "other", "x")
	cluster := tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "x")
	tl := twoWidgets()
	tl.At(3*s, tt.Version(elsewhere, "c1", "3", labeled("x", "ub")), tt.Version(cluster, "c2", "3", labeled("x", "ua")))
	rules := compile(t, labelOwner)
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 2, "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c2", 3*s), 2, "ua")
}

func TestRule2NamesTheCRByNamespaceAndName(t *testing.T) {
	cluster := tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "x")
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cluster, "c1", "3", labeled("x", "ns/b")))
	wantOwner(t, ownerBy(t, tl, compile(t, labelOwner), "c1", 3*s), 2, "ub")
}

func TestRule2NamespaceAndNameBeatsTheResourcesNamespace(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(tt.Ref("toy/widgets", "other", "a"), "ua-other", "3", tt.Object("a"))).
		At(4*s, tt.Version(cmA0, "c1", "4", labeled("a-0", "other/a")))
	wantOwner(t, ownerBy(t, tl, compile(t, labelOwner), "c1", 4*s), 2, "ua-other")
}

func TestRule2NamesTheLatestCRCreatedByT(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", labeled("a-0", "ns/a")), tt.Deleted(widgetA, "ua", "3", tt.Object("a"))).
		At(4*s, tt.Version(widgetA, "ua2", "4", tt.Object("a")))
	rules := compile(t, labelOwner)
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 2, "ua")
	wantOwner(t, ownerBy(t, tl, rules, "c1", 4*s), 2, "ua2")
}

func TestRule2BareNamePrefersTheResourcesNamespace(t *testing.T) {
	otherA := tt.Ref("toy/widgets", "other", "a")
	elsewhere := tt.Ref("configmaps", "third", "x")
	tl := twoWidgets()
	tl.At(3*s, tt.Version(otherA, "ua-other", "3", tt.Object("a"))).
		At(4*s, tt.Version(cmA0, "c1", "4", labeled("a-0", "a")), tt.Version(elsewhere, "c2", "4", labeled("x", "b")))
	rules := compile(t, labelOwner)
	wantOwner(t, ownerBy(t, tl, rules, "c1", 4*s), 2, "ua")
	wantOwner(t, ownerBy(t, tl, rules, "c2", 4*s), 2, "ub")
}

func TestRule2SkipsABareNameOfCRsInTwoOtherNamespaces(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(tt.Ref("toy/widgets", "other", "a"), "ua-other", "3", tt.Object("a"))).
		At(4*s, tt.Version(tt.Ref("configmaps", "third", "x"), "c1", "4", labeled("x", "a"))).
		At(5*s, tt.Version(tt.Ref("toy/widgets", "third", "a"), "ua-third", "5", tt.Object("a")))
	rules := compile(t, labelOwner)
	wantOwner(t, ownerBy(t, tl, rules, "c1", 4*s), 4, "ua", "ua-other", "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c1", 5*s), 2, "ua-third")
}

func TestRule2SkipsTheUIDOfANonCR(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", tt.Object("a-0")),
		tt.Version(tt.Ref("configmaps", "ns", "x"), "c2", "3", labeled("x", "c1")))
	wantOwner(t, ownerBy(t, tl, compile(t, labelOwner), "c2", 3*s), 4, "ua", "ub")
}

func TestRule2SkipsWhatNamesNoCR(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c-none", "3", tt.Object("a-0")),
		tt.Version(tt.Ref("configmaps", "ns", "n"), "c-number", "3", labeled("n", 7)),
		tt.Version(tt.Ref("configmaps", "ns", "e"), "c-empty", "3", labeled("e", "")),
		tt.Version(tt.Ref("configmaps", "ns", "m"), "c-nomatch", "3", labeled("m", "ns/zzz")))
	rules := compile(t, labelOwner)
	for _, uid := range []string{"c-none", "c-number", "c-empty", "c-nomatch"} {
		wantOwner(t, ownerBy(t, tl, rules, uid, 3*s), 4, "ua", "ub")
	}
}

func TestRule2AppliesOnlyToItsSystem(t *testing.T) {
	tl := twoWidgets()
	tl.At(3*s, tt.Version(cmA0, "c1", "3", labeled("a-0", "ua")))
	wantOwner(t, ownerBy(t, tl, compile(t, map[string]string{"s3": "self.metadata.labels.owner"}), "c1", 3*s), 4, "ua", "ub")
}

// namingWidgets creates widget a naming child x at 1s and widget b naming
// child y at 2s.
func namingWidgets() *tt.Timeline {
	var tl tt.Timeline
	tl.At(0, tt.Run("toy", "widgets")).
		At(1*s, tt.Version(widgetA, "ua", "1", naming("a", "x"))).
		At(2*s, tt.Version(widgetB, "ub", "2", naming("b", "y")))
	return &tl
}

func TestRule3NamesAResourceInTheCRsNamespace(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "ns", "y"), "c1", "3", tt.Object("y")),
		tt.Version(tt.Ref("configmaps", "other", "y"), "c2", "3", tt.Object("y")),
		tt.Version(tt.Ref("secrets", "ns", "y"), "c3", "3", tt.Object("y")),
		tt.Version(tt.Ref("example.com/configmaps", "ns", "y"), "c4", "3", tt.Object("y")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 3, "ub")
	for _, uid := range []string{"c2", "c3", "c4"} {
		wantOwner(t, ownerBy(t, tl, rules, uid, 3*s), 4, "ua", "ub")
	}
}

func TestRule3NamesAResourceInTheDeclaredNamespace(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "rf-shared", "y"), "c1", "3", tt.Object("y")),
		tt.Version(tt.Ref("configmaps", "ns", "y"), "c2", "3", tt.Object("y")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child", Namespace: "'rf-shared'"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 3, "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c2", 3*s), 4, "ua", "ub")
}

func TestRule3NamesAClusterScopedResource(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "x"), "c1", "3", tt.Object("x")))
	rules := compile(t, nil, trace.Owns{Kind: clusterRoles, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 3, "ua")
}

func TestRule3StillNamesAGoneCRsResource(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "ns", "x"), "c1", "3", tt.Object("x"))).
		At(4*s, tt.Deleted(widgetA, "ua", "4", naming("a", "x")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 5*s), 3, "ua")
}

func TestRule3ReadsTheCRsBodyAtT(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "ns", "z"), "c1", "3", tt.Object("z"))).
		At(4*s, tt.Version(widgetA, "ua", "4", naming("a", "z")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 4, "ua", "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c1", 4*s), 3, "ua")
}

func TestRule3PrefersALiveCRThenTheLatest(t *testing.T) {
	widgetC := tt.Ref("toy/widgets", "ns", "c")
	child := tt.Ref("configmaps", "ns", "x")
	tl := namingWidgets()
	tl.At(3*s, tt.Version(widgetC, "uc", "3", naming("c", "x")), tt.Version(child, "c1", "3", tt.Object("x"))).
		At(5*s, tt.Deleted(widgetC, "uc", "5", naming("c", "x"))).
		At(6*s, tt.Deleted(widgetA, "ua", "6", naming("a", "x")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 4*s), 3, "uc")
	wantOwner(t, ownerBy(t, tl, rules, "c1", 5*s), 3, "ua")
	wantOwner(t, ownerBy(t, tl, rules, "c1", 6*s), 3, "uc")
}

func TestRule3SkipsACRWhoseCELFails(t *testing.T) {
	tl := namingWidgets()
	tl.At(3*s, tt.Version(widgetA, "ua", "3", tt.Object("a")), tt.Version(tt.Ref("configmaps", "ns", "x"), "c1", "3", tt.Object("x")))
	rules := compile(t, nil, trace.Owns{Kind: configMaps, Name: "self.spec.child"},
		trace.Owns{Kind: configMaps, Name: "'x'", Namespace: "self.spec.namespace"})
	wantOwner(t, ownerBy(t, tl, rules, "c1", 3*s), 4, "ua", "ub")
}

func TestRulesApplyInOrder(t *testing.T) {
	both := labeled("x", "ub")
	tl := namingWidgets()
	tl.At(3*s, tt.Version(tt.Ref("configmaps", "ns", "x"), "c-ref", "3", tt.OwnedBy(labeled("x", "ub"), "ua", true))).
		At(4*s, tt.Deleted(tt.Ref("configmaps", "ns", "x"), "c-ref", "4", both)).
		At(5*s, tt.Version(tt.Ref("configmaps", "ns", "x"), "c-label", "5", both)).
		At(6*s, tt.Version(tt.Ref("configmaps", "ns", "y"), "c-named", "6", tt.Object("y")))
	rules := compile(t, labelOwner, trace.Owns{Kind: configMaps, Name: "self.spec.child"})
	wantOwner(t, ownerBy(t, tl, rules, "c-ref", 3*s), 1, "ua")
	wantOwner(t, ownerBy(t, tl, rules, "c-label", 5*s), 2, "ub")
	wantOwner(t, ownerBy(t, tl, rules, "c-named", 6*s), 3, "ub")
}

func TestNamedIsTheCROfRules1To3(t *testing.T) {
	for rule := 1; rule <= 3; rule++ {
		if cr, named := (owner.Owner{Rule: rule, CRs: []string{"ua"}}).Named(); !named || cr != "ua" {
			t.Errorf("rule %d: Named = %q, %v", rule, cr, named)
		}
	}
	if _, named := (owner.Owner{Rule: 4, CRs: []string{"ua"}}).Named(); named {
		t.Error("rule 4 names a CR")
	}
}
