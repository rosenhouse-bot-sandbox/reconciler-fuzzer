package check_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	clusterRoles  = trace.Kind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Resource: "clusterroles"}
	clusterRoleA  = tt.Ref("rbac.authorization.k8s.io/clusterroles", "", "a-role")
	elsewhereA    = tt.Ref("configmaps", "other", "a-0")
	ownerLabel    = map[string]string{"kube": "self.metadata.labels.owner"}
	namesItsRole  = trace.Owns{Kind: clusterRoles, Name: "self.metadata.name + '-role'"}
	roleOwnsEntry = []trace.Owns{namesItsRole}
)

func labeledBy(name, owner string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "labels": map[string]any{"owner": owner}}}
}

// leakAfterDeletingA creates slots a and b, then deletes a. The controller
// gives a a ClusterRole that no ownerReference ties to it and a configmap in
// another namespace labeled with a's namespace/name, and leaves both.
func leakAfterDeletingA() *tt.Script {
	r := newRun(2)
	r.Spec.Owners, r.Spec.Primaries[0].Owns = ownerLabel, roleOwnsEntry
	r.Begin()
	r.CreateCR("a", widgetA)
	r.CreateCR("b", widgetB)
	r.Create(ctl, clusterRoleA, tt.Object("a-role"))
	r.Create(ctl, elsewhereA, labeledBy("a-0", "ns/a"))
	r.Ready(widgetA).Ready(widgetB)
	r.EndFaults().Settle()
	return r.Begin().DeleteCR("a", widgetA).EndFaults().Settle()
}

func TestCleanFailsALeakedChildThatRules2And3Name(t *testing.T) {
	findings := judgeWith(t, leakAfterDeletingA(), check.Clean)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want two", findings)
	}
	for _, want := range []struct{ kind, detail string }{
		{"kube/rbac.authorization.k8s.io/clusterroles", "clusterroles.rbac.authorization.k8s.io a-role (rule 3, slot a)"},
		{"kube/configmaps", "configmaps other/a-0 (rule 2, slot a)"},
	} {
		requireFinding(t, filterKind(findings, want.kind), "Clean", 1, "left resources behind", want.kind, want.detail)
	}
}

func filterKind(findings []check.Finding, kind string) []check.Finding {
	var out []check.Finding
	for _, f := range findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func TestSpecForWithoutAConfigCompilesTheRunEventsOwners(t *testing.T) {
	r := leakAfterDeletingA()
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	requireOwner(t, ix, spec, r.UID(clusterRoleA), 3, r.UID(widgetA))
	requireOwner(t, ix, spec, r.UID(elsewhereA), 2, r.UID(widgetA))

	r.Spec.Primaries[0].Owns = []trace.Owns{{Kind: clusterRoles, Name: "self.("}}
	if _, err := check.SpecFor(nil, r.Index()); err == nil {
		t.Fatal("SpecFor accepts owns that do not compile")
	}
}

func TestSpecForTakesOwnersFromTheConfig(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.Systems = []config.System{{Name: "kube", Owner: "self.metadata.labels.owner"}}
	c.Primaries[0].Owns = []config.Owns{
		{Kind: schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}, Name: "self.metadata.name + '-role'"},
		{Kind: schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"}, Name: "self.metadata.name", Namespace: "'other'"},
	}
	recorded := gadgetKind
	recorded.Resource = "gadgetries"
	gadget := tt.Ref("toy.reconciler-fuzzer/gadgetries", "other", "a")
	widget := tt.Ref("toy.reconciler-fuzzer/widgets", "ns", "a")
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.Owners = map[string]string{"kube": "self.nothing"}
	run.Run.Primaries[0].Owns = []trace.Owns{{Kind: recorded, Name: "'unused'"}}
	var tl tt.Timeline
	tl.At(0, run).
		At(1*s, tt.Version(widget, "ua", "1", tt.Object("a"))).
		At(2*s, tt.Version(clusterRoleA, "u-role", "2", tt.Object("a-role")),
			tt.Version(gadget, "u-gadget", "2", tt.Object("a")),
			tt.Version(elsewhereA, "u-cm", "2", labeledBy("a-0", "a")))
	ix := tl.Index()
	spec, err := check.SpecFor(c, ix)
	if err != nil {
		t.Fatal(err)
	}
	requireOwner(t, ix, spec, "u-role", 3, "ua")
	requireOwner(t, ix, spec, "u-gadget", 3, "ua")
	requireOwner(t, ix, spec, "u-cm", 2, "ua")
}

func requireOwner(t *testing.T, ix *trace.Index, spec check.Spec, uid string, rule int, cr string) {
	t.Helper()
	w := owner.New(ix, spec.Owners)
	i, _ := w.Instance(uid)
	if got := w.Owner(i, i.Created); got.Rule != rule || len(got.CRs) != 1 || got.CRs[0] != cr {
		t.Errorf("owner of %s = %+v, want rule %d %s", uid, got, rule, cr)
	}
}

type namedDependency struct {
	owners map[string]string
	owns   []trace.Owns
	ref    trace.Ref
	body   func(r *tt.Script, ready bool) map[string]any
}

// namedDependencies are Gadgets that rules 2 and 3 tie to slot a's CR.
var namedDependencies = map[string]namedDependency{
	"rule 2": {owners: ownerLabel, ref: gadgetA, body: func(r *tt.Script, ready bool) map[string]any {
		body := tt.ReadyCondition("a", 1, ready)
		body["metadata"].(map[string]any)["labels"] = map[string]any{"owner": r.UID(widgetA)}
		return body
	}},
	"rule 3 in another namespace": {
		owns: []trace.Owns{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"}, Name: "self.metadata.name", Namespace: "'other'"}},
		ref:  tt.Ref("toy/gadgets", "other", "a"),
		body: func(r *tt.Script, ready bool) map[string]any { return tt.ReadyCondition("a", 1, ready) },
	},
}

// namedRun creates slot a's CR and its Gadget, which the model makes ready.
func namedRun(tc namedDependency, dep trace.Dependency) *tt.Script {
	r := newRun(1)
	r.Spec.Owners, r.Spec.Primaries[0].Owns = tc.owners, tc.owns
	r.Spec.Models = []trace.Model{{Kind: trace.Kind{Group: "toy", Version: "v1", Kind: "Gadget", Resource: "gadgets"},
		Ready: config.ReadyConditionCEL, Dependency: &dep}}
	r.Begin()
	r.CreateCR("a", widgetA)
	r.Create(ctl, tc.ref, tc.body(r, false))
	return r.Update(gadgetModel, tc.ref, tc.body(r, true))
}

func TestHonestJudgesDependenciesThatRules2And3Name(t *testing.T) {
	for name, tc := range namedDependencies {
		t.Run(name, func(t *testing.T) {
			for _, seen := range []bool{true, false} {
				r := namedRun(tc, trace.Dependency{Required: true})
				if seen {
					r.See(ctl, tc.ref)
				}
				findings := honest(t, r.Ready(widgetA))
				if seen {
					requireNone(t, findings)
				} else {
					requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/gadgets")
				}
			}
		})
	}
}

func TestHonestTracksDependenciesThatRules2And3Name(t *testing.T) {
	for name, tc := range namedDependencies {
		t.Run(name, func(t *testing.T) {
			r := namedRun(tc, trace.Dependency{Track: 500 * ms})
			r.See(ctl, tc.ref).Ready(widgetA)
			r.Update(gadgetModel, tc.ref, tc.body(r, false)).See(ctl, tc.ref)
			requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "kube/toy/gadgets", "slot a")
		})
	}
}

func TestBackoffRunDoesNotBreakOnAnotherCRsGenerationByRule2(t *testing.T) {
	cr := func(ref trace.Ref, generation int) trace.Event {
		return tt.Version(ref, "u"+ref.Name, "v", map[string]any{"metadata": map[string]any{"name": ref.Name, "generation": generation}})
	}
	tl := retries(every(10*ms, 21), func(n int) trace.Event {
		switch n {
		case 1:
			return tt.Version(gadgetA, "ug", "g", labeledBy("a", "ns/a"))
		case 2:
			return cr(widgetA, 1)
		case 3:
			return cr(widgetB, 1)
		case 15:
			return cr(widgetB, 2)
		}
		return tt.Start(1)
	})
	judged := spec
	rules, err := owner.Compile(ownerLabel, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	judged.Owners = rules
	requireFinding(t, check.Backoff(tl.Index(), judged), "Backoff", 0, tightLoop, "kube/toy/gadgets")
}

func TestRestoreKeepsAGroupWhoseResourceChangesRule(t *testing.T) {
	r := newRun(2)
	r.Spec.Primaries[0].Owns = []trace.Owns{{Kind: trace.Kind{Version: "v1", Kind: "ConfigMap", Resource: "configmaps"}, Name: "self.metadata.name + '-0'"}}
	r.Begin()
	withChild(r, "a", false)
	r.EndFaults().Settle()
	r.Begin().Restart()
	r.Update(ctl, childA, tt.OwnedBy(tt.Object("a-0"), r.UID(widgetA), true))
	r.EndFaults().Settle()
	requireFinding(t, restore(t, r), "Restore", 1, "changed resources", "kube/configmaps", "configmaps ns/a-0 (rule 3, slot a): metadata.ownerReferences")
}
