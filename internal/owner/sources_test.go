package owner_test

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var clusterRoleKind = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}

func resolveTo(resource string, namespaced bool, err error) func(schema.GroupVersionKind) (string, bool, error) {
	return func(schema.GroupVersionKind) (string, bool, error) { return resource, namespaced, err }
}

func TestSourcesTakeTheCELAndResolveEachKind(t *testing.T) {
	c := &config.Config{
		Systems: []config.System{{Name: "kube", Owner: "self.metadata.labels.owner"}, {Name: "s3"}},
		Primary: config.Primary{Owns: []config.Owns{{Kind: clusterRoleKind, Name: "self.metadata.name", Namespace: "'other'"}}},
	}
	owners, owns, err := owner.Sources(c, resolveTo("clusterroleries", true, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owners, map[string]string{"kube": "self.metadata.labels.owner"}) {
		t.Errorf("owners = %v", owners)
	}
	want := []trace.Owns{{Kind: trace.Kind{Group: clusterRoleKind.Group, Version: "v1", Kind: "ClusterRole", Resource: "clusterroleries"},
		Name: "self.metadata.name", Namespace: "'other'"}}
	if !reflect.DeepEqual(owns, want) {
		t.Errorf("owns = %+v", owns)
	}
}

func TestSourcesRefuseAnUnknownKindAndANamespaceOnAClusterScopedKind(t *testing.T) {
	c := &config.Config{Primary: config.Primary{Owns: []config.Owns{{Kind: clusterRoleKind, Name: "'x'", Namespace: "'other'"}}}}
	if _, _, err := owner.Sources(c, resolveTo("", false, errors.New("no kind"))); err == nil {
		t.Error("an unknown owns kind is no error")
	}
	if _, _, err := owner.Sources(c, resolveTo("clusterroles", false, nil)); err == nil {
		t.Error("a namespace on a cluster-scoped owns kind is no error")
	}
	c.Primary.Owns[0].Namespace = ""
	if _, _, err := owner.Sources(c, resolveTo("clusterroles", false, nil)); err != nil {
		t.Error(err)
	}
}
