package main_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
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

// The kube role drops a suffix after a Volume's index, so that Unique finds
// two Volumes of one index. Other names are their own role.
func TestTheKubeRoleDropsASuffixAfterAVolumesIndex(t *testing.T) {
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Systems) != 1 || c.Systems[0].Name != "kube" {
		t.Fatalf("systems = %+v, want kube alone", c.Systems)
	}
	role, err := config.CompileString(c.Systems[0].Role)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"cluster-vol-0":       "cluster-vol-0",
		"cluster-vol-1-ab2cd": "cluster-vol-1",
		"a-vol-1-b-vol-0":     "a-vol-1-b-vol-0",
		"db-db":               "db-db",
		"alice-credentials":   "alice-credentials",
	} {
		if got, err := role(map[string]any{"metadata": map[string]any{"name": name}}); got != want || err != nil {
			t.Errorf("The role of %s is %q, %v; want %q.", name, got, err, want)
		}
	}
}
