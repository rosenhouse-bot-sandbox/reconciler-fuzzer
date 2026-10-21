package main_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crudfake"
)

func bucket(status map[string]any) map[string]any {
	b := map[string]any{"metadata": map[string]any{"generation": int64(2)}, "spec": map[string]any{"region": "eu"}}
	if status != nil {
		b["status"] = status
	}
	return b
}

func TestEachConfigPutsTheCloudBehindCrudfake(t *testing.T) {
	for _, path := range configs {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Launch.Env["CLOUD_ENDPOINT"]; got != "${systems.cloud.url}" {
			t.Errorf("%s: CLOUD_ENDPOINT is %q", path, got)
		}
		fakes := c.Fakes()
		if len(fakes) != 1 {
			t.Fatalf("%s: fakes %+v, want cloud", path, fakes)
		}
		cloud := fakes[0]
		root, _ := filepath.Abs("../..")
		if cloud.Name != "cloud" || cloud.API != "crud" || cloud.Process.Command[0] != filepath.Join(root, "bin/crudfake") ||
			cloud.Owner != "self.labels['k8s-uid']" || cloud.Ready != "self.state == 'READY'" || cloud.Gone != "self.state == 'DELETING'" ||
			cloud.Dependency == nil || !cloud.Dependency.Required || cloud.Poll != time.Second {
			t.Errorf("%s: cloud is %+v", path, cloud)
		}
		if _, err := crudfake.Load(filepath.Join(cloud.Process.Dir, cloud.Process.Command[2])); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestReady(t *testing.T) {
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		bucket map[string]any
		want   bool
	}{
		"no status":           {bucket(nil), false},
		"an older generation": {bucket(map[string]any{"observedGeneration": int64(1), "ready": true}), false},
		"not ready":           {bucket(map[string]any{"observedGeneration": int64(2), "ready": false}), false},
		"ready":               {bucket(map[string]any{"observedGeneration": int64(2), "ready": true}), true},
	} {
		if ready, err := c.Ready(tc.bucket); ready != tc.want || err != nil {
			t.Errorf("%s: Ready = %v, %v; want %v.", name, ready, err, tc.want)
		}
	}
}

func TestTheHaltedConfigHaltsOnCreatePending(t *testing.T) {
	c, err := config.Load(haltedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Launch.Args, "--halt") {
		t.Errorf("Launch args %v lack --halt.", c.Launch.Args)
	}
	condition := func(typ, status string) map[string]any {
		return bucket(map[string]any{"conditions": []any{map[string]any{"type": typ, "status": status}}})
	}
	for name, tc := range map[string]struct {
		bucket map[string]any
		want   bool
	}{
		"no status":           {bucket(nil), false},
		"CreatePending False": {condition("CreatePending", "False"), false},
		"another condition":   {condition("Ready", "True"), false},
		"CreatePending True":  {condition("CreatePending", "True"), true},
	} {
		if halted, err := c.Halted(tc.bucket); halted != tc.want || err != nil {
			t.Errorf("%s: Halted = %v, %v; want %v.", name, halted, err, tc.want)
		}
	}
}
