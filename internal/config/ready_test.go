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
                template: {type: object, x-kubernetes-embedded-resource: true, properties: {spec: {type: object}}}
                labels: {type: object, additionalProperties: {type: string}}
                items: {type: array, items: {type: object, properties: {name: {type: string}}}}
                selector: {type: object, properties: {app.kubernetes.io/name: {type: string}}}
                range: {type: object, properties: {"in": {type: integer}}}
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
