package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// TestADeleteNamesOnlyTheUID matches the garbage collector, whose delete a
// dependent's later write does not refuse.
func TestADeleteNamesOnlyTheUID(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
	}))
	defer server.Close()
	c, err := newClient(&rest.Config{Host: server.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := trace.Ref{System: "kube", Resource: "configmaps", Namespace: "ns", Name: "c"}
	if err := c.apply(context.Background(), op{ref: ref, apiVersion: "v1", uid: "u1", rv: "7", delete: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"uid":"u1"`) || strings.Contains(body, "resourceVersion") {
		t.Errorf("The delete sent %s, want a UID precondition only.", body)
	}
	if err := c.apply(context.Background(), op{ref: ref, apiVersion: "v1", uid: "u1", rv: "7", patch: map[string]any{"finalizers": nil}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"resourceVersion":"7"`) {
		t.Errorf("The patch sent %s, want a resourceVersion precondition.", body)
	}
}
