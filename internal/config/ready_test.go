package config_test

import (
	"encoding/json"
	"fmt"
	"os"
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

// The harness reads a CR as JSON, so metadata takes any field and a
// date-time is a string.
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

// decode reads a CR as the harness reads one from a trace.
func decode(t *testing.T, cr string) map[string]any {
	t.Helper()
	var self map[string]any
	if err := json.Unmarshal([]byte(cr), &self); err != nil {
		t.Fatal(err)
	}
	return self
}

func TestAReadyOverAFieldTheCRLacksErrs(t *testing.T) {
	c, err := config.Load(widgetConfig(t, `ready: "self.status.ready == self.spec.count"`))
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := c.Ready(decode(t, `{"spec": {"count": 2}}`)); ready || err == nil || !strings.Contains(err.Error(), "no such key: status") {
		t.Errorf("Ready of a CR without status = %v, %v; want an error that names status", ready, err)
	}
	if ready, err := c.Ready(decode(t, `{"spec": {"count": 2}, "status": {"ready": 2}}`)); !ready || err != nil {
		t.Errorf("Ready of a ready CR = %v, %v; want true", ready, err)
	}
}

// thingCRD is the CRD of toy/v1/Thing, whose schema holds a field of each
// type.
const thingCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: things.toy}
spec:
  group: toy
  names: {kind: Thing, plural: things}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                size: {type: integer}
                ratio: {type: number}
                "on": {type: boolean}
                name: {type: string}
                namespace: {type: string}
                port: {x-kubernetes-int-or-string: true, anyOf: [{type: integer}, {type: string}]}
                extra: {type: object, x-kubernetes-preserve-unknown-fields: true}
                loose: {type: object, x-kubernetes-preserve-unknown-fields: true, properties: {ratio: {type: number}}}
                template: {type: object, x-kubernetes-embedded-resource: true, properties: {spec: {type: object, properties: {ratio: {type: number}}}}}
                labels: {type: object, additionalProperties: {type: string}}
                items: {type: array, items: {type: object, properties: {name: {type: string}}}}
                selector: {type: object, properties: {app.kubernetes.io/name: {type: string}, ratio: {type: number}}}
                range: {type: object, properties: {"in": {type: integer}}}
                ratios: {type: array, items: {type: number}}
                weights: {type: object, additionalProperties: {type: number}}
`

// thingConfig writes a config whose primary is a Thing, with the entry's
// keys, and then rest.
func thingConfig(t *testing.T, keys, rest string) string {
	t.Helper()
	path := writeConfig(t, "launch: {binary: bin/controller}\ncrds: [thing.yaml]\nprimary: {kind: toy/v1/Thing, sample: sample.yaml, "+keys+"}\n"+rest)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "thing.yaml"), []byte(thingCRD), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAReadyTypesEachSchemaType(t *testing.T) {
	for ready, loads := range map[string]bool{
		"self.spec.size > 1 && self.spec.ratio > 0.5 && self.spec.on && self.spec.name == 'a'": true,
		"self.spec.port == 80 || self.spec.port == 'http'":                                     true,
		"self.spec.extra.anything == 1":                                                        true,
		"self.spec.template.metadata.name == 'a'":                                              true,
		"self.spec.labels['app'] == 'web'":                                                     true,
		"self.spec.items.exists(i, i.name == 'a')":                                             true,
		"self.spec.namespace == 'a' && self.kind == 'Thing' && self.apiVersion == 'toy/v1'":    true,
		"self.spec.selector['app.kubernetes.io/name'] == 'web' && self.spec.range['in'] > 1":   true,
		"self.spec.ratio > 1 && self.spec.size < 2.5":                                          true,
		"self.spec.size + 1 > 2":                                                               true,
		"self.spec.ratio + 0.5 > 1.0":                                                          true,
		"self.spec.ratio == 0.5":                                                               true,
		"self.spec.size == 2.5":                                                                false,
		"self.spec.ratio == 1":                                                                 false,
		"self.spec.size == 'a'":                                                                false,
		"self.spec.ratio == 'a'":                                                               false,
		"self.spec.on == 1":                                                                    false,
		"self.spec.name > 1":                                                                   false,
		"self.spec.labels['app'] > 1":                                                          false,
		"self.spec.items.exists(i, i.nmae == 'a')":                                             false,
		"self.kind == 1":                                                                       false,
		"self.spec.size":                                                                       false,
	} {
		if _, err := config.Load(thingConfig(t, `ready: "`+ready+`"`, "")); (err == nil) != loads {
			t.Errorf("ready %s: Load = %v, want it to load: %t", ready, err, loads)
		}
	}
}

func TestAReferenceReadyTypesTheToKind(t *testing.T) {
	for ready, loads := range map[string]bool{"self.spec.size > 1": true, "self.spec.size == 'a'": false} {
		reference := "references: [{from: toy/v1/Thing, to: toy/v1/Thing, name: spec.name, ready: \"" + ready + "\"}]\n"
		_, err := config.Load(thingConfig(t, `ready: "true"`, reference))
		if (err == nil) != loads || err != nil && !strings.Contains(err.Error(), "references[0].ready: ") {
			t.Errorf("ready %s: Load = %v, want it to load: %t", ready, err, loads)
		}
	}
}

// A JSON number decodes to a float64. Ready reads it as the API server
// reads a CR: an int, unless the schema types it number.
func TestAReadyComputesWithTheTypesTheSchemaGives(t *testing.T) {
	cr := `{"spec": {"size": 2, "ratio": 1, "name": "abc", "ratios": [1], "weights": {"a": 1},
		"extra": {"count": 2, "half": 0.5, "big": 1e19, "small": -1e19}, "port": 80,
		"loose": {"ratio": 1, "count": 2}, "template": {"spec": {"ratio": 1}}, "selector": {"ratio": 1}}}`
	for _, ready := range []string{
		"self.spec.size + 1 == 3",
		"self.spec.size % 2 == 0",
		"self.spec.name.substring(0, self.spec.size) == 'ab'",
		"self.spec.ratio + 0.5 == 1.5",
		"self.spec.ratios[0] + 0.5 == 1.5",
		"self.spec.weights['a'] + 0.5 == 1.5",
		"self.spec.extra.count + 1 == 3",
		"self.spec.extra.half + 0.5 == 1.0",
		"self.spec.extra.big > 1e18",
		"self.spec.extra.small == -1e19",
		"self.spec.port + 1 == 81",
		"self.spec.loose.ratio + 0.5 == 1.5",
		"self.spec.loose.count + 1 == 3",
		"self.spec.template.spec.ratio + 0.5 == 1.5",
		"self.spec.selector.ratio + 0.5 == 1.5",
	} {
		c, err := config.Load(thingConfig(t, `ready: "`+ready+`"`, ""))
		if err != nil {
			t.Fatalf("ready %s: %v", ready, err)
		}
		if ok, err := c.Ready(decode(t, cr)); !ok || err != nil {
			t.Errorf("ready %s: Ready = %v, %v; want true", ready, ok, err)
		}
	}
}
