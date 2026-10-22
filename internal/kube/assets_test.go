package kube

import (
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestStartWithoutTheControlPlaneNamesKubebuilderAssets(t *testing.T) {
	t.Setenv("KUBEBUILDER_ASSETS", t.TempDir())
	_, err := New(Options{}).Start(t.Context(), &system.Env{Dir: t.TempDir(), Record: func(trace.Event) {}})
	if err == nil || !strings.Contains(err.Error(), "KUBEBUILDER_ASSETS") || !strings.Contains(err.Error(), "setup-envtest") {
		t.Errorf("Start = %v, want an error naming KUBEBUILDER_ASSETS and setup-envtest.", err)
	}
}
