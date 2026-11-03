package kube

import (
	"fmt"
	"io/fs"
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

func TestOnlyAControlPlaneThatCannotRunGetsTheAssetsHint(t *testing.T) {
	missingCRDs := fmt.Errorf("installing CRDs: %w", &fs.PathError{Op: "open", Path: "crds", Err: fs.ErrNotExist})
	for _, err := range []error{missingCRDs, fmt.Errorf("timeout waiting for process kube-apiserver to start")} {
		if hint := assetsHint(err); hint != "" {
			t.Errorf("assetsHint(%v) = %q, want none", err, hint)
		}
	}
}
