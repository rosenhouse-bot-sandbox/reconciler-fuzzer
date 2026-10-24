package main_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

func TestReadyNeedsTheCurrentGenerationInPhaseReady(t *testing.T) {
	for _, path := range configs {
		c, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		cr := func(status map[string]any) map[string]any {
			o := map[string]any{"metadata": map[string]any{"generation": int64(2)}}
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
				t.Errorf("%s: %s: Ready = %v, %v; want %v.", path, name, ready, err, tc.want)
			}
		}
	}
}
