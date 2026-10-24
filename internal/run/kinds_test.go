package run

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestRunRefusesAPlanThatCreatesAnUnlistedKind(t *testing.T) {
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a",
		Object: json.RawMessage(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "settings"}}`)}}}}}
	out := filepath.Join(t.TempDir(), "out")
	_, err := Run(t.Context(), Options{ConfigPath: "../../targets/toy-widget/config.yaml", Plan: p, Out: out})
	if err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("Run = %v, want a refusal that names ConfigMap.", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Run made %s before it refused the plan: %v", out, err)
	}
}
