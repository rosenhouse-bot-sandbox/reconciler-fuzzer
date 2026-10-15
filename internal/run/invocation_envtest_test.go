//go:build envtest

package run_test

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

func TestInvocationCreatesAndDeletesClusterScopedFixtures(t *testing.T) {
	t.Parallel()
	ingressClassGVK := schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "IngressClass"}
	ingressClassGVR := schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"}
	fixture := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "IngressClass",
		"metadata":   map[string]any{"name": "reconciler-fuzzer-test"},
		"spec":       map[string]any{"controller": "example.com/test"},
	}}
	toy := &target.Target{
		Name:            "toy-widget",
		Primary:         schema.GroupVersionKind{Group: "toy.reconciler-fuzzer", Version: "v1", Kind: "Widget"},
		Fixtures:        []*unstructured.Unstructured{fixture},
		ClusterFixtures: []schema.GroupVersionKind{ingressClassGVK},
	}
	testCluster, err := cluster.Start(cluster.Options{CRDPaths: []string{repoRoot + "/targets/toy-widget/crds/"}})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	defer func() {
		if err := testCluster.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	}()
	config := testCluster.Config()
	mapper, err := cluster.NewRESTMapper(config)
	if err != nil {
		t.Fatalf("Building a REST mapper failed: %v", err)
	}

	inv, err := run.NewInvocation(config, toy)
	if err != nil {
		t.Fatalf("NewInvocation failed: %v", err)
	}

	ctx := t.Context()
	if err := inv.Prepare(ctx, toy, mapper); err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// The IngressClass exists on the cluster.
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("Building a dynamic client failed: %v", err)
	}
	got, err := client.Resource(ingressClassGVR).Get(ctx, "reconciler-fuzzer-test", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("The cluster fixture was not created: %v", err)
	}
	if controller, _, _ := unstructured.NestedString(got.Object, "spec", "controller"); controller != "example.com/test" {
		t.Errorf("The IngressClass has controller %q, want example.com/test.", controller)
	}

	if err := inv.Close(ctx); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// The IngressClass is gone.
	_, err = client.Resource(ingressClassGVR).Get(ctx, "reconciler-fuzzer-test", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("After Close the cluster fixture returned %v, want not-found.", err)
	}
}
