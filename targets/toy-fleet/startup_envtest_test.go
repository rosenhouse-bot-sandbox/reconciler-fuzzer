//go:build envtest

package main_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
)

// A controller that makes no successful request before step 0 fails the run
// early, like a missing binary, and the error quotes its output.
func TestAControllerThatNeverStartsFailsTheRunFast(t *testing.T) {
	for name, c := range map[string]struct{ arg, tail string }{
		"B170":     {"--bug=170", "no kind is registered for the type v1.Database"},
		"B171":     {"--bug=171", "localhost:8080"},
		"bad flag": {"--no-such-flag", "flag provided but not defined: -no-such-flag"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o := run.Options{ConfigPath: "config.yaml", PlanPath: planPath("config.yaml", "b0"), Out: t.TempDir(), LaunchArgs: []string{c.arg}}
			start := time.Now()
			_, err := run.Run(t.Context(), o)
			if err == nil || !strings.Contains(err.Error(), "the controller never started") || !strings.Contains(err.Error(), c.tail) {
				t.Errorf("err = %v, want one saying the controller never started and quoting %q", err, c.tail)
			}
			if took := time.Since(start); took > 30*time.Second {
				t.Errorf("The run took %v, want under 30s.", took)
			}
		})
	}
}
