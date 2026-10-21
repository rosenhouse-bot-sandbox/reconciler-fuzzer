package main_test

import (
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
