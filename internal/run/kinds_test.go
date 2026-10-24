package run

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
)

func TestRunRefusesAPlanThatCreatesAnUnlistedKind(t *testing.T) {
	t.Skip("enabled by the toy-fleet package")
	p := &plan.Plan{Steps: []plan.Step{{Actions: []plan.Action{{Do: plan.Create, Slot: "a",
		Object: json.RawMessage(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "settings"}}`)}}}}}
	_, err := Run(t.Context(), Options{ConfigPath: "../../targets/toy-widget/config.yaml", Plan: p, Out: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("Run = %v, want a refusal that names ConfigMap.", err)
	}
}
