package main_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

func TestReadyNeedsTheCurrentGenerationInPhaseReady(t *testing.T) {
	for _, path := range append([]string{clusterConfig}, configs...) {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range c.Primaries {
			apiVersion, kind := p.Kind.ToAPIVersionAndKind()
			cr := func(status map[string]any) map[string]any {
				o := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"generation": int64(2)}}
				if status != nil {
					o["status"] = status
				}
				return o
			}
			for name, tc := range map[string]struct {
				cr   map[string]any
				want bool
			}{
				"no status":           {cr(nil), false},
				"an older generation": {cr(map[string]any{"observedGeneration": int64(1), "phase": "Ready"}), false},
				"Provisioning":        {cr(map[string]any{"observedGeneration": int64(2), "phase": "Provisioning"}), false},
				"Deleting":            {cr(map[string]any{"observedGeneration": int64(2), "phase": "Deleting"}), false},
				"Ready":               {cr(map[string]any{"observedGeneration": int64(2), "phase": "Ready"}), true},
			} {
				if ready, err := c.Ready(tc.cr); ready != tc.want || err != nil {
					t.Errorf("%s: %s %s: Ready = %v, %v; want %v.", path, kind, name, ready, err, tc.want)
				}
			}
		}
	}
}

// The README shows cluster/config.yaml's properties. Each fails only where
// the README says, and skips CRs of other kinds.
func TestClusterPropertiesFailWhereTheReadmeSays(t *testing.T) {
	c, err := config.Load(clusterConfig)
	if err != nil {
		t.Fatal(err)
	}
	holds := map[string]func([]any, map[string]map[string][]any) (bool, error){}
	for _, p := range c.Properties {
		program, err := config.CompileProperty(p.Expr)
		if err == nil {
			holds[p.Name], err = program.Bind(nil)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// The Cluster wants two small Volumes. A Volume names its owner's UID.
	cluster := func(status string) string {
		if status != "" {
			status = `,"status":` + status
		}
		return `{"kind":"Cluster","metadata":{"uid":"c","generation":2},"spec":{"volumes":2,"volumeSize":"small"}` + status + `}`
	}
	volume := func(owner, size string) string {
		if owner != "" {
			owner = `"ownerReferences":[{"uid":"` + owner + `"}]`
		}
		return `{"kind":"Volume","metadata":{` + owner + `},"spec":{"size":"` + size + `"}}`
	}
	const ready = `{"phase":"Ready","observedGeneration":2,"readyVolumes":2}`
	small := volume("c", "small")
	for _, tc := range []struct {
		name, cr                                           string
		volumes                                            []string
		volumesMatchSpec, provisioningMeansVolumesNotReady bool
	}{
		{"a Cluster with no status", cluster(""), nil, true, true},
		{"a status with no phase", cluster(`{"observedGeneration":2,"readyVolumes":2}`), nil, true, true},
		{"Provisioning with no Volume ready", cluster(`{"phase":"Provisioning","observedGeneration":2}`), nil, true, true},
		{"Provisioning with one Volume ready", cluster(`{"phase":"Provisioning","observedGeneration":2,"readyVolumes":1}`), []string{small, small}, true, true},
		{"Provisioning with every Volume ready", cluster(`{"phase":"Provisioning","observedGeneration":2,"readyVolumes":2}`), []string{small, small}, true, false},
		{"Provisioning an older generation", cluster(`{"phase":"Provisioning","observedGeneration":1,"readyVolumes":2}`), []string{small, small}, true, true},
		{"Provisioning with no observedGeneration", cluster(`{"phase":"Provisioning","readyVolumes":2}`), []string{small, small}, true, true},
		{"Ready with the Volumes of its spec", cluster(ready), []string{small, small}, true, true},
		{"Ready with a Volume missing", cluster(ready), []string{small}, false, true},
		{"Ready with a large Volume", cluster(ready), []string{small, volume("c", "large")}, false, true},
		{"Ready with a third Volume", cluster(ready), []string{small, small, small}, false, true},
		{"Ready with another Cluster's Volume", cluster(ready), []string{small, volume("d", "small")}, false, true},
		{"Ready with an unowned Volume too", cluster(ready), []string{small, small, volume("", "small")}, true, true},
		{"a Ready Database", `{"kind":"Database","metadata":{"uid":"d","generation":2},"status":{"phase":"Ready","observedGeneration":2}}`, nil, true, true},
		{"a Provisioning Database", `{"kind":"Database","metadata":{"uid":"d","generation":2},"status":{"phase":"Provisioning","observedGeneration":2}}`, nil, true, true},
	} {
		volumes := []any{}
		for _, v := range tc.volumes {
			volumes = append(volumes, decoded(t, v))
		}
		world := map[string]map[string][]any{"kube": {"volumes.storage.thirdparty": volumes}}
		for property, want := range map[string]bool{
			"volumes-match-spec":                   tc.volumesMatchSpec,
			"provisioning-means-volumes-not-ready": tc.provisioningMeansVolumesNotReady,
		} {
			if holds[property] == nil {
				t.Fatalf("%s has no property %s.", clusterConfig, property)
			}
			if got, err := holds[property]([]any{decoded(t, tc.cr)}, world); got != want || err != nil {
				t.Errorf("%s: %s = %v, %v; want %v.", tc.name, property, got, err, want)
			}
		}
	}
}

// decoded is JSON as a property sees it, with float64 numbers.
func decoded(t *testing.T, s string) any {
	t.Helper()
	var o any
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// A Cluster that drops out of Ready and back within a step is Ready at the
// step's end. Only a property judged at every version fails it.
func TestProvisioningMeansVolumesNotReadyFailsADropOutOfReadyWithinAStep(t *testing.T) {
	c, err := config.Load(clusterConfig)
	if err != nil {
		t.Fatal(err)
	}
	cluster := trace.Ref{System: "kube", Group: "toy.reconciler-fuzzer", Resource: "clusters", Namespace: "ns", Name: "cluster"}
	volume := trace.Ref{System: "kube", Group: "storage.thirdparty", Resource: "volumes", Namespace: "ns", Name: "cluster-vol-0"}
	// The Cluster wants one small Volume.
	withPhase := func(phase string) map[string]any {
		body := map[string]any{"kind": "Cluster", "metadata": map[string]any{"name": cluster.Name, "generation": 1},
			"spec": map[string]any{"volumes": 1, "volumeSize": "small"}}
		if phase != "" {
			body["status"] = map[string]any{"phase": phase, "observedGeneration": 1, "readyVolumes": 1}
		}
		return body
	}
	r := tt.NewScript(trace.Kind{Group: cluster.Group, Resource: cluster.Resource}, 1, time.Second)
	r.BodyUIDs, r.Spec.Properties = true, owner.Properties(c)
	r.Begin()
	uid := r.Create(trace.Harness, cluster, withPhase(""))
	r.Act("create", "a", &cluster)
	r.Create(trace.Controller, volume, tt.OwnedBy(map[string]any{"metadata": map[string]any{"name": volume.Name}, "spec": map[string]any{"size": "small"}}, uid, true))
	r.Update(trace.Controller, cluster, withPhase("Ready"))
	r.Update(trace.Controller, cluster, withPhase("Provisioning"))
	r.Update(trace.Controller, cluster, withPhase("Ready")).EndFaults().Settle()
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	const want = "property provisioning-means-volumes-not-ready does not hold"
	if findings := check.Property(ix, spec); len(findings) != 1 || findings[0].Summary != want {
		t.Errorf("findings = %+v, want one that says %s.", findings, want)
	}
}

// The kube role drops a suffix after a Volume's index, so that Unique finds
// two Volumes of one index. Other objects get no role from it.
func TestTheKubeRoleDropsASuffixAfterAVolumesIndex(t *testing.T) {
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Systems) != 1 || c.Systems[0].Name != "kube" {
		t.Fatalf("systems = %+v, want kube alone", c.Systems)
	}
	role, err := config.CompileString(c.Systems[0].Role, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []struct{ kind, name, want string }{
		{"Volume", "cluster-vol-1-ab2cd", "cluster-vol-1"},
		{"Volume", "cluster-vol-0", ""},
		{"Volume", "a-vol-1-b-vol-0", ""},
		{"Secret", "alice-vol-0-ab2cd", ""},
	} {
		if got, err := role(map[string]any{"kind": o.kind, "metadata": map[string]any{"name": o.name}}); got != o.want || err != nil {
			t.Errorf("The role of %s %s is %q, %v; want %q.", o.kind, o.name, got, err, o.want)
		}
	}
}
