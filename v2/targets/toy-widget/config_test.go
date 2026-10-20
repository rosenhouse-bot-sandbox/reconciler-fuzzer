package toywidget_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/config"
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
