package main_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func TestReady(t *testing.T) {
	c, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	widget := func(status map[string]any) map[string]any {
		w := map[string]any{"metadata": map[string]any{"generation": int64(2)}, "spec": map[string]any{"count": int64(3)}}
		if status != nil {
			w["status"] = status
		}
		return w
	}
	for name, tc := range map[string]struct {
		widget map[string]any
		want   bool
	}{
		"no status":            {widget(nil), false},
		"an older generation":  {widget(map[string]any{"observedGeneration": int64(1), "ready": int64(3)}), false},
		"too few children":     {widget(map[string]any{"observedGeneration": int64(2), "ready": int64(2)}), false},
		"all children current": {widget(map[string]any{"observedGeneration": int64(2), "ready": int64(3)}), true},
	} {
		if ready, err := c.Ready(tc.widget); ready != tc.want || err != nil {
			t.Errorf("%s: Ready = %v, %v; want %v.", name, ready, err, tc.want)
		}
	}
}

func TestTheGadgetConfigDependsOnAModelledGadget(t *testing.T) {
	c, err := config.Load("gadget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Launch.Args, "--gadget") {
		t.Errorf("Launch args %v lack --gadget.", c.Launch.Args)
	}
	want := config.Model{
		Kind:       schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Gadget"},
		Stage:      config.ReadyCondition,
		After:      200 * time.Millisecond,
		Dependency: &config.Dependency{Required: true, Track: time.Second},
	}
	if len(c.Models) != 1 || !reflect.DeepEqual(c.Models[0], want) {
		t.Errorf("Models are %+v, want [%+v].", c.Models, want)
	}
	widget := map[string]any{
		"metadata": map[string]any{"generation": int64(1)},
		"spec":     map[string]any{"count": int64(1)},
		"status":   map[string]any{"observedGeneration": int64(1), "ready": int64(1)},
	}
	for gadgetReady, want := range map[any]bool{nil: false, false: false, true: true} {
		status := widget["status"].(map[string]any)
		delete(status, "gadgetReady")
		if gadgetReady != nil {
			status["gadgetReady"] = gadgetReady
		}
		if ready, err := c.Ready(widget); ready != want || err != nil {
			t.Errorf("With gadgetReady %v, Ready = %v, %v; want %v.", gadgetReady, ready, err, want)
		}
	}
}

func TestTheTenantConfigOwnsItsClusterScopedChildrenByName(t *testing.T) {
	c, err := config.Load("tenant/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := (schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Tenant"}); c.Primaries[0].Kind != want {
		t.Errorf("The primary is %v, want %v.", c.Primaries[0].Kind, want)
	}
	var owned []string
	for _, owns := range c.Primaries[0].Owns {
		owned = append(owned, owns.Kind.Kind+" "+owns.Name)
	}
	if want := []string{"ClusterRole 'tenant-' + self.metadata.name", "Namespace 'tenant-' + self.metadata.name"}; !slices.Equal(owned, want) {
		t.Errorf("The Tenant owns %v, want %v.", owned, want)
	}
	if _, set := c.Launch.Env["WATCH_NAMESPACE"]; set {
		t.Error("The Tenant config sets WATCH_NAMESPACE, which would confine the cache.")
	}
	assertReadyOnTheCurrentGeneration(t, c)
}

func TestTheGrantConfigOwnsItsChildrenByLabel(t *testing.T) {
	c, err := config.Load("grant/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if want := []config.System{{Name: "kube", Owner: "self.metadata.labels['toy.reconciler-fuzzer/grant-uid']"}}; !reflect.DeepEqual(c.Systems, want) {
		t.Errorf("Systems are %+v, want %+v.", c.Systems, want)
	}
	if !slices.Contains(c.Launch.Args, "--mirror-namespace=rf-shared") {
		t.Errorf("Launch args %v lack --mirror-namespace=rf-shared.", c.Launch.Args)
	}
	assertReadyOnTheCurrentGeneration(t, c)
}

func TestTheLeaseConfigIsConfigYamlWithALease(t *testing.T) {
	widget, err := config.Load("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(leaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(c.Launch.Args, "--lease=2s") {
		t.Errorf("Launch args %v lack --lease=2s.", c.Launch.Args)
	}
	const leaseTakeover = 3 * time.Second
	if floor := widget.Timeouts.Settle + leaseTakeover; c.Timeouts.Settle < floor {
		t.Errorf("%s allows %v to settle, want at least %v.", leaseConfig, c.Timeouts.Settle, floor)
	}
	c.Launch.Args = slices.DeleteFunc(c.Launch.Args, func(arg string) bool { return arg == "--lease=2s" })
	c.Timeouts.Settle = widget.Timeouts.Settle
	// JSON leaves out the compiled CEL, which reflect.DeepEqual cannot compare.
	got, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.MarshalIndent(widget, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from config.yaml in more than --lease=2s and the settle timeout:\n%s\nconfig.yaml:\n%s", leaseConfig, got, want)
	}
}

func assertReadyOnTheCurrentGeneration(t *testing.T, c *config.Config) {
	t.Helper()
	cr := func(observed int64, ready bool) map[string]any {
		return map[string]any{
			"metadata": map[string]any{"generation": int64(2)},
			"status":   map[string]any{"observedGeneration": observed, "ready": ready},
		}
	}
	for name, tc := range map[string]struct {
		cr   map[string]any
		want bool
	}{
		"no status":           {map[string]any{"metadata": map[string]any{"generation": int64(2)}}, false},
		"an older generation": {cr(1, true), false},
		"not ready":           {cr(2, false), false},
		"ready":               {cr(2, true), true},
	} {
		if ready, err := c.Ready(tc.cr); ready != tc.want || err != nil {
			t.Errorf("%s: Ready = %v, %v; want %v.", name, ready, err, tc.want)
		}
	}
}
