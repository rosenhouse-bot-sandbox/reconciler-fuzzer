package cluster_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
)

func TestNewRESTMapperReportsADiscoveryThatFailed(t *testing.T) {
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	mapper, err := cluster.NewRESTMapper(unreachable)

	if err == nil {
		t.Fatalf("NewRESTMapper discovered %v against an API server that is not listening.", mapper)
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("NewRESTMapper returned %q, which does not say that discovery failed.", err)
	}
}

func TestServedResourcesLeavesOutSubresources(t *testing.T) {
	discovery := map[string]string{
		"/api":  `{"kind": "APIVersions", "versions": ["v1"]}`,
		"/apis": `{"kind": "APIGroupList", "groups": []}`,
		"/api/v1": `{"kind": "APIResourceList", "groupVersion": "v1", "resources": [
			{"name": "configmaps", "singularName": "configmap", "namespaced": true, "kind": "ConfigMap", "verbs": ["get"]},
			{"name": "configmaps/status", "namespaced": true, "kind": "ConfigMap", "verbs": ["get"]}]}`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, found := discovery[r.URL.Path]
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	served, err := cluster.ServedResources(&rest.Config{Host: server.URL})

	if err != nil {
		t.Fatalf("ServedResources failed: %v", err)
	}
	if len(served) != 1 || served[0].Name != "configmaps" || served[0].Kind != "ConfigMap" {
		t.Errorf("ServedResources returned %+v, want configmaps alone.", served)
	}
}

func TestServedResourcesReportsADiscoveryThatFailed(t *testing.T) {
	if _, err := cluster.ServedResources(&rest.Config{Host: "http://127.0.0.1:1"}); err == nil {
		t.Error("ServedResources discovered resources on an API server that is not listening.")
	}
}

func TestNewRESTMapperReportsAClientItCannotBuild(t *testing.T) {
	absentCA := &rest.Config{Host: "https://127.0.0.1:1"}
	absentCA.TLSClientConfig.CAFile = "/no/such/ca.crt"

	mapper, err := cluster.NewRESTMapper(absentCA)

	if err == nil {
		t.Fatalf("NewRESTMapper built %v from a configuration naming a CA file that is not there.", mapper)
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("NewRESTMapper returned %q, which does not say that it could not build a client.", err)
	}
}
