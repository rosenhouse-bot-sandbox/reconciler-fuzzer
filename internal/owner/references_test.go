package owner_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestReferencesDescribeEachEntry(t *testing.T) {
	secretKind := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	c := &config.Config{References: []config.Reference{
		{From: databaseKind, To: widgetKind, Namespaced: true, Name: "spec.widget", Ready: "self.ok", Required: true},
		{From: databaseKind, To: secretKind, Namespaced: true, Name: "spec.keys[*].name", Namespace: "spec.keys[*].namespace", Ready: "has(self.data)", Track: time.Second},
		{From: widgetKind, System: "cloud", Collection: "buckets", Name: "spec.bucket", Ready: "true", Required: true},
	}}
	got, err := owner.References(c, resolver(map[string]string{"Widget": "widgetries", "Database": "databases", "Secret": "secrets"}))
	if err != nil {
		t.Fatal(err)
	}
	database := trace.Kind{Group: "toy", Version: "v1", Kind: "Database", Resource: "databases"}
	want := []trace.Reference{
		{From: database, To: trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgetries"}, Namespaced: true, Name: "spec.widget", Ready: "self.ok", Required: true},
		{From: database, To: trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}, Namespaced: true, Name: "spec.keys[*].name", Namespace: "spec.keys[*].namespace", Ready: "has(self.data)", Track: time.Second},
		{From: trace.Kind{Group: "toy", Version: "v1", Kind: "Widget", Resource: "widgetries"}, System: "cloud", To: trace.Kind{Resource: "buckets"}, Name: "spec.bucket", Ready: "true", Required: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("References =\n%+v\nwant\n%+v", got, want)
	}
	for missing, resources := range map[string]map[string]string{
		"Secret":   {"Widget": "widgetries", "Database": "databases"},
		"Database": {"Widget": "widgetries", "Secret": "secrets"},
	} {
		if _, err := owner.References(c, resolver(resources)); err == nil || !strings.Contains(err.Error(), missing) {
			t.Errorf("References = %v, want an error that names the %s kind it cannot map.", err, missing)
		}
	}
}
