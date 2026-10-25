package generate

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	gvk "k8s.io/apimachinery/pkg/runtime/schema"
	"pgregory.net/rapid"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

const (
	referencesConfig = "testdata/references/config.yaml"
	linksConfig      = "testdata/references/links.yaml"
)

// clusterNames are the names of a Cluster in each slot.
var clusterNames = []string{"cluster", "cluster-b", "cluster-c"}

// replay calls visit with each action of p and what the slots hold before
// it.
func replay(t *testing.T, p *plan.Plan, visit func(a plan.Action, at state)) {
	t.Helper()
	at := state{}
	for _, step := range p.Steps {
		for _, a := range step.Actions {
			visit(a, at)
			switch a.Do {
			case plan.Create:
				at[a.Slot] = decode(t, a.Object)
			case plan.Update:
				at[a.Slot] = mergePatch(at[a.Slot], decode(t, a.Patch))
			case plan.Delete:
				at[a.Slot] = nil
			}
		}
	}
}

func kindName(cr map[string]any) string {
	kind, _ := cr["kind"].(string)
	return kind
}

func clusterRef(cr map[string]any) string {
	spec, _ := cr["spec"].(map[string]any)
	ref, _ := spec["clusterRef"].(string)
	return ref
}

func liveClusters(at state) []string {
	var names []string
	for _, slot := range at.live() {
		if kindName(at[slot]) == "Cluster" {
			names = append(names, at[slot]["metadata"].(map[string]any)["name"].(string))
		}
	}
	return names
}

func TestCreatesAndUpdatesWriteEachReferenceWithACandidate(t *testing.T) {
	counts := map[string]int{}
	for _, p := range plans(t, referencesConfig, 50) {
		replay(t, p, func(a plan.Action, at state) {
			var spec map[string]any
			switch {
			case a.Do == plan.Create && kindName(decode(t, a.Object)) == "Database":
				spec, _ = decode(t, a.Object)["spec"].(map[string]any)
			case a.Do == plan.Update && kindName(at[a.Slot]) == "Database":
				spec, _ = decode(t, a.Patch)["spec"].(map[string]any)
			default:
				return
			}
			if ref, set := spec["clusterRef"]; set {
				name, _ := ref.(string)
				switch {
				case !slices.Contains(clusterNames, name):
					t.Errorf("Seed %d's %s of slot %s writes clusterRef %v, want one of %v.", p.Seed, a.Do, a.Slot, ref, clusterNames)
				case slices.Contains(liveClusters(at), name):
					counts[string(a.Do)+" naming a live Cluster"]++
				default:
					counts[string(a.Do)+" naming no live Cluster"]++
				}
			}
			if secretRef, _ := spec["passwordSecretRef"].(map[string]any); secretRef != nil {
				switch name := secretRef["name"]; name {
				case "admin-password", "missing":
					counts["passwordSecretRef "+name.(string)]++
				default:
					t.Errorf("Seed %d's %s of slot %s writes passwordSecretRef.name %v, want admin-password or missing.", p.Seed, a.Do, a.Slot, name)
				}
			}
		})
	}
	for _, want := range []string{"create naming a live Cluster", "create naming no live Cluster", "update naming no live Cluster", "passwordSecretRef admin-password", "passwordSecretRef missing"} {
		if counts[want] == 0 {
			t.Errorf("50 seeds draw %v, want a %s.", counts, want)
		}
	}
}

func TestAReferencedFixtureIsDeletedAndCreatedAgain(t *testing.T) {
	secret := plan.Target{Resource: "secrets", Name: "admin-password"}
	counts := map[plan.Do]int{}
	for _, p := range plans(t, referencesConfig, 100) {
		deleted := false
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if a.Do != plan.ChangeFixture && a.Do != plan.CreateFixture {
					continue
				}
				switch {
				case *a.Target != secret || a.Patch != nil:
					t.Errorf("Seed %d draws %s of %+v with patch %s, want only a delete or create of the Secret.", p.Seed, a.Do, *a.Target, a.Patch)
				case a.Do == plan.ChangeFixture && deleted:
					t.Errorf("Seed %d deletes the Secret it already deleted.", p.Seed)
				}
				deleted = a.Do == plan.ChangeFixture
				counts[a.Do]++
			}
		}
	}
	if counts[plan.ChangeFixture] == 0 || counts[plan.CreateFixture] == 0 {
		t.Errorf("100 seeds draw %v, want deletes and creates of the Secret.", counts)
	}
}

func TestDeletesComeInBothOrders(t *testing.T) {
	var toFirst, fromFirst int
	for _, p := range plans(t, referencesConfig, 200) {
		replay(t, p, func(a plan.Action, at state) {
			if a.Do != plan.Delete {
				return
			}
			switch cr := at[a.Slot]; kindName(cr) {
			case "Cluster":
				name := cr["metadata"].(map[string]any)["name"]
				if slices.ContainsFunc(at.live(), func(slot string) bool { return clusterRef(at[slot]) == name }) {
					toFirst++
				}
			case "Database":
				if slices.Contains(liveClusters(at), clusterRef(cr)) {
					fromFirst++
				}
			}
		})
	}
	if toFirst == 0 || fromFirst == 0 {
		t.Errorf("200 seeds delete %d Clusters that a live Database names, and %d Databases that name a live Cluster; want some of each.", toFirst, fromFirst)
	}
}

// written is the kind and spec that a create or update writes.
func written(t *testing.T, a plan.Action, at state) (string, map[string]any) {
	t.Helper()
	var kind string
	var object map[string]any
	switch a.Do {
	case plan.Create:
		object = decode(t, a.Object)
		kind = kindName(object)
	case plan.Update:
		object = decode(t, a.Patch)
		kind = kindName(at[a.Slot])
	}
	spec, _ := object["spec"].(map[string]any)
	return kind, spec
}

// TestAWriteNamesALiveCandidateHalfTheTime draws writes of a Database while
// slot b holds a Cluster. Half the writes name a live Cluster and half any
// of three, so 2 in 3 name cluster-b.
func TestAWriteNamesALiveCandidateHalfTheTime(t *testing.T) {
	g := newGenerator(t, loadConfig(t, referencesConfig))
	cluster, database := g.kinds[0], g.kinds[1]
	at := state{"a": database.base("a").Object, "b": cluster.base("b").Object}
	writes, live := map[plan.Do]int{}, map[plan.Do]int{}
	var secretWrites, missingWrites int
	for seed := range 600 {
		for _, a := range example(t, seed, func(rt *rapid.T) []plan.Action {
			return []plan.Action{g.createOf(rt, maps.Clone(at), "c", database), g.update(rt, maps.Clone(at), "a")}
		}) {
			_, spec := written(t, a, at)
			if ref, set := spec["clusterRef"]; set {
				writes[a.Do]++
				if ref == "cluster-b" {
					live[a.Do]++
				}
			}
			if secretRef, _ := spec["passwordSecretRef"].(map[string]any); secretRef != nil {
				secretWrites++
				if secretRef["name"] == missing {
					missingWrites++
				}
			}
		}
	}
	for _, do := range []plan.Do{plan.Create, plan.Update} {
		if live[do]*6 < writes[do]*3 || live[do]*6 > writes[do]*5 {
			t.Errorf("%d of %d %ss name the live Cluster, want 1/2 to 5/6.", live[do], writes[do], do)
		}
	}
	if missingWrites*8 < secretWrites || missingWrites*8 > secretWrites*3 {
		t.Errorf("%d of %d passwordSecretRef writes are missing, want 1/8 to 3/8.", missingWrites, secretWrites)
	}
}

// TestWritesNameEachListItemAFakeAndAFixture draws Links. Their references
// name each item of spec.clusters, a fake's bucket, Secrets, a ConfigMap,
// a ServiceAccount, which no fixture holds, and spec.short, which no
// candidate fits.
func TestWritesNameEachListItemAFakeAndAFixture(t *testing.T) {
	secrets := []string{"admin-password", "other-password"}
	counts := map[string]int{}
	for _, p := range plans(t, linksConfig, 200) {
		if err := p.Validate(); err != nil {
			t.Fatalf("Seed %d: %v", p.Seed, err)
		}
		replay(t, p, func(a plan.Action, at state) {
			kind, spec := written(t, a, at)
			if kind != "Link" {
				return
			}
			clusters, _ := spec["clusters"].([]any)
			for _, item := range clusters {
				if name, _ := item.(map[string]any)["name"].(string); !slices.Contains(clusterNames, name) {
					t.Errorf("Seed %d's %s writes clusters %v, want each item to name a Cluster.", p.Seed, a.Do, clusters)
				}
			}
			counts[string(a.Do)+" of clusters"] += min(1, len(clusters))
			if a.Do == plan.Create && len(clusters) > 1 {
				counts["create of 2 or more clusters"]++
			}
			for field, names := range map[string][]string{"bucket": {"shared"}, "secretName": secrets, "backupSecretName": secrets, "configName": {"admin-password"}} {
				switch value, set := spec[field]; {
				case !set:
				case value == nil && a.Do == plan.Update:
					counts["update removing "+field]++
				case slices.Contains(names, value.(string)):
					counts[field+" "+value.(string)]++
				default:
					t.Errorf("Seed %d's %s writes %s %v, want one of %v.", p.Seed, a.Do, field, value, names)
				}
			}
			if short, _ := spec["short"].(string); len(short) > 2 || a.Do == plan.Update && short != "" {
				t.Errorf("Seed %d's %s writes short %q, which no candidate fits, want no update and at most 2 characters.", p.Seed, a.Do, short)
			}
			if account, set := spec["accountName"]; set {
				t.Errorf("Seed %d's %s writes accountName %v, which no candidate fits, want none.", p.Seed, a.Do, account)
			}
		})
	}
	for _, want := range []string{"create of clusters", "update of clusters", "create of 2 or more clusters", "bucket shared",
		"secretName admin-password", "secretName other-password", "backupSecretName other-password", "update removing secretName", "configName admin-password"} {
		if counts[want] == 0 {
			t.Errorf("200 seeds draw %v, want a %s.", counts, want)
		}
	}
}

// TestAWriteNamesALiveFixtureHalfTheTime draws Links after the plan deleted
// admin-password. Half the writes name the live other-password and half
// either Secret, so 3 in 4 name it.
func TestAWriteNamesALiveFixtureHalfTheTime(t *testing.T) {
	g := newGenerator(t, loadConfig(t, linksConfig))
	at := state{gone(plan.Target{Resource: "secrets", Name: "admin-password"}): nil}
	live := 0
	for seed := range 200 {
		create := example(t, seed, func(rt *rapid.T) plan.Action { return g.createOf(rt, maps.Clone(at), "a", g.kinds[1]) })
		if _, spec := written(t, create, at); spec["secretName"] == "other-password" {
			live++
		}
	}
	if live*8 < 200*5 || live*8 > 200*7 {
		t.Errorf("%d of 200 creates name the live Secret, want 5/8 to 7/8.", live)
	}
}

func TestReferencedFixturesAreTheKubeFixturesOfToKinds(t *testing.T) {
	g := newGenerator(t, loadConfig(t, linksConfig))
	want := []plan.Target{{Resource: "secrets", Name: "admin-password"}, {Resource: "secrets", Name: "other-password"}, {Resource: "configmaps", Name: "admin-password"}}
	if !reflect.DeepEqual(g.referenced, want) {
		t.Errorf("referenced = %+v, want %+v, each once.", g.referenced, want)
	}
}

// TestACoreNamespaceIsACandidateButNotReferenced adds references to a
// Namespace fixture and to a fixture of another group's Namespace kind.
func TestACoreNamespaceIsACandidateButNotReferenced(t *testing.T) {
	cfg := loadConfig(t, linksConfig)
	for _, c := range []struct{ apiVersion, name string }{{"v1", "spec.namespace"}, {"example.com/v1", "spec.otherNamespace"}} {
		fixture := filepath.Join(t.TempDir(), "namespace.yaml")
		if err := os.WriteFile(fixture, []byte("{apiVersion: "+c.apiVersion+", kind: Namespace, metadata: {name: team}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.Fixtures = append(cfg.Fixtures, fixture)
		cfg.References = append(cfg.References, config.Reference{From: cfg.Primaries[1].Kind, To: gvk.FromAPIVersionAndKind(c.apiVersion, "Namespace"), Name: c.name, Ready: "true", Required: true})
	}
	g := newGenerator(t, cfg)
	link := g.kinds[1]
	if fixtures := link.references[len(link.references)-2].fixtures; !reflect.DeepEqual(fixtures, []plan.Target{{Resource: "namespaces", Name: "team"}}) {
		t.Errorf("The Namespace reference's fixtures = %+v, want the Namespace team.", fixtures)
	}
	want := []plan.Target{{Resource: "secrets", Name: "admin-password"}, {Resource: "secrets", Name: "other-password"}, {Resource: "configmaps", Name: "admin-password"}, {Group: "example.com", Resource: "namespaces", Name: "team"}}
	if !reflect.DeepEqual(g.referenced, want) {
		t.Errorf("referenced = %+v, want %+v, without the Namespace.", g.referenced, want)
	}
}

// TestAFixtureChangesOnlyWhileItLives draws Links' plans, which patch the
// Secret admin-password and delete each referenced fixture.
func TestAFixtureChangesOnlyWhileItLives(t *testing.T) {
	counts := map[string]int{}
	for _, p := range plans(t, linksConfig, 100) {
		deleted := map[plan.Target]bool{}
		for _, step := range p.Steps {
			for _, a := range step.Actions {
				if a.Do != plan.ChangeFixture && a.Do != plan.CreateFixture {
					continue
				}
				target := *a.Target
				fixture := target.Resource + " " + target.Name
				switch {
				case a.Do == plan.CreateFixture:
					counts["create"]++
					delete(deleted, target)
				case deleted[target]:
					t.Errorf("Seed %d changes %s after deleting it.", p.Seed, fixture)
				case a.Patch == nil:
					counts["delete "+fixture]++
					deleted[target] = true
				default:
					counts["patch "+fixture]++
				}
			}
		}
	}
	for _, want := range []string{"patch secrets admin-password", "delete secrets admin-password", "delete secrets other-password", "delete configmaps admin-password", "create"} {
		if counts[want] == 0 {
			t.Errorf("100 seeds draw %v, want a %s.", counts, want)
		}
	}
}

func TestUnderIsWhatFollowsAPathAtOrAboveTheName(t *testing.T) {
	r := &reference{path: []string{"spec", "keys", each, "name"}}
	for _, c := range []struct {
		path, rest []string
		holds      bool
	}{
		{nil, r.path, true},
		{[]string{"spec", "keys"}, []string{each, "name"}, true},
		{[]string{"spec", "keys", each, "name"}, []string{}, true},
		{[]string{"spec", "other"}, nil, false},
		{[]string{"spec", "keys", each, "name", "deeper"}, nil, false},
	} {
		if rest, holds := r.under(c.path); holds != c.holds || !slices.Equal(rest, c.rest) {
			t.Errorf("under(%v) = %v, %t, want %v, %t.", c.path, rest, holds, c.rest, c.holds)
		}
	}
}

func TestWriteNamesWritesUnderEachParentTheValueHolds(t *testing.T) {
	drawn := func() (string, bool) { return "new", true }
	none := func() (string, bool) { return "", false }
	for _, c := range []struct {
		value, want any
		path        []string
		draw        func() (string, bool)
	}{
		{"old", "new", nil, drawn},
		{"old", "old", nil, none},
		{map[string]any{"ref": map[string]any{}}, map[string]any{"ref": map[string]any{"name": "new"}}, []string{"ref", "name"}, drawn},
		{map[string]any{}, map[string]any{}, []string{"ref", "name"}, drawn},
		{map[string]any{"ref": "x"}, map[string]any{"ref": "x"}, []string{"ref", "name"}, drawn},
		{[]any{map[string]any{"name": "old"}, "x"}, []any{map[string]any{"name": "new"}, "x"}, []string{each, "name"}, drawn},
	} {
		if got := writeNames(runtime.DeepCopyJSONValue(c.value), c.path, c.draw); !reflect.DeepEqual(got, c.want) {
			t.Errorf("writeNames(%v, %v) = %v, want %v.", c.value, c.path, got, c.want)
		}
	}
}
