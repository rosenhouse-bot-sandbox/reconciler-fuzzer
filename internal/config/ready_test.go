package config_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
)

// widgetConfig writes a config whose primary is toy-widget's Widget, whose
// CRD has a schema, with the entry's keys.
func widgetConfig(t *testing.T, keys string) string {
	t.Helper()
	crds, err := filepath.Abs("../../targets/toy-widget/crds")
	if err != nil {
		t.Fatal(err)
	}
	return writeConfig(t, fmt.Sprintf("launch: {binary: bin/controller}\ncrds: [%s]\nprimary: {kind: toy.reconciler-fuzzer/v1/Widget, sample: sample.yaml, %s}\n", crds, keys))
}

func TestAReadyTheCRDSchemaCannotTypeIsAConfigurationError(t *testing.T) {
	for _, ready := range []string{`self.status.ready == 'yes'`, `self.status.ready > 'a'`, `self.status.state == 1`} {
		if _, err := config.Load(widgetConfig(t, `ready: "`+ready+`"`)); err == nil || !strings.Contains(err.Error(), "primary.ready: ") {
			t.Errorf("ready %s: Load = %v, want an error that names primary.ready", ready, err)
		}
	}
}

func TestEachExpressionOverTheCRTypesAgainstTheCRDSchema(t *testing.T) {
	for keys, want := range map[string]string{
		`ready: "true", retain: "self.spec.count == 'x'"`:                                                  "primary.retain: ",
		`ready: "true", halted: "self.status.gadgetReady > 1"`:                                             "primary.halted: ",
		`ready: "true", owns: [{kind: v1/ConfigMap, name: self.spec.count}]`:                               "primary.owns[0].name: ",
		`ready: "true", owns: [{kind: v1/ConfigMap, name: self.metadata.name, namespace: self.spec.nmae}]`: "primary.owns[0].namespace: ",
	} {
		if _, err := config.Load(widgetConfig(t, keys)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Load = %v, want an error that names %s", keys, err, want)
		}
	}
}

// The harness reads a CR as JSON: any metadata field, and a string in any
// format.
func TestAReadyTypesTheCRAsTheHarnessReadsIt(t *testing.T) {
	for _, ready := range []string{
		`self.metadata.generation == self.status.observedGeneration && self.metadata.labels['a'] == 'b'`,
		`self.status.lastSyncTime != '' && self.status.gadgetReady && self.spec.count > 1`,
		`self.kind == 'Widget' && self.apiVersion.startsWith('toy.')`,
	} {
		if _, err := config.Load(widgetConfig(t, `ready: "`+ready+`"`)); err != nil {
			t.Errorf("ready %s: Load = %v, want it to load", ready, err)
		}
	}
}

func TestAFieldTheCRLacksLeavesItNotReady(t *testing.T) {
	c, err := config.Load(widgetConfig(t, `ready: "self.status.ready == self.spec.count"`))
	if err != nil {
		t.Fatal(err)
	}
	for cr, want := range map[string]bool{`{"spec": {"count": 2}}`: false, `{"spec": {"count": 2}, "status": {"ready": 2}}`: true} {
		var self map[string]any
		if err := json.Unmarshal([]byte(cr), &self); err != nil {
			t.Fatal(err)
		}
		if ready, _ := c.Ready(self); ready != want {
			t.Errorf("Ready(%s) = %v, want %v", cr, ready, want)
		}
	}
}
