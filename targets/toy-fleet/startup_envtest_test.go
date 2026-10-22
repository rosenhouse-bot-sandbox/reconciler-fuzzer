//go:build envtest

package main_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// A controller that never reaches the API server is a setup error, like a
// missing binary: the run stops early and exits 2.
func TestAControllerThatNeverStartsFailsTheRunFast(t *testing.T) {
	for bug, tail := range map[int]string{
		70: "no kind Cluster is registered in the scheme",
		71: "localhost:8080",
	} {
		t.Run(fmt.Sprintf("B%d", bug), func(t *testing.T) {
			t.Parallel()
			o := run.Options{ConfigPath: "config.yaml", PlanPath: planPath("config.yaml", "b0"), Out: t.TempDir(),
				LaunchArgs: []string{fmt.Sprintf("--bug=%d", bug)}}
			start := time.Now()
			_, err := run.Run(t.Context(), o)
			if err == nil || !strings.Contains(err.Error(), "the controller never started") || !strings.Contains(err.Error(), tail) {
				t.Errorf("err = %v, want one saying the controller never started and quoting %q", err, tail)
			}
			if took := time.Since(start); took > 30*time.Second {
				t.Errorf("The run took %v, want under 30s.", took)
			}
		})
	}
}
