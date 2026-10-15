//go:build envtest

package generate

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kinds "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/run"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// The API server itself judges what generation draws, with no controller
// running and nothing else in the namespace.
func TestTheAPIServerAcceptsEveryGeneratedCROp(t *testing.T) {
	const seeds = 200
	ctx := t.Context()
	var loaded []*target.Target
	var crds []string
	for _, path := range []string{rulesTarget, toyTarget, certManagerTarget, externalSecretsTarget} {
		declared := loadTarget(t, path)
		loaded = append(loaded, declared)
		crds = append(crds, declared.CRDs...)
	}
	// Without generate, the generator walks every spec path the schema says
	// enough about.
	for _, path := range []string{certManagerTarget, externalSecretsTarget} {
		unconstrained := loadTarget(t, path)
		unconstrained.Name += "-unconstrained"
		unconstrained.Generate = target.GenerateSpec{}
		loaded = append(loaded, unconstrained)
	}
	testCluster, err := cluster.Start(cluster.Options{CRDPaths: crds})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := testCluster.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	})
	client, err := dynamic.NewForConfig(testCluster.Config())
	if err != nil {
		t.Fatalf("Building the dynamic client failed: %v", err)
	}
	namespaces, err := kubernetes.NewForConfig(testCluster.Config())
	if err != nil {
		t.Fatalf("Building the core client failed: %v", err)
	}

	for _, declared := range loaded {
		t.Run(declared.Name, func(t *testing.T) {
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "generated-" + declared.Name}}
			if _, err := namespaces.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Creating the namespace failed: %v", err)
			}
			crs := client.Resource(resourceOf(t, declared)).Namespace(namespace.Name)
			g := newGenerator(t, declared, Options{})
			writes := 0
			for seed := int64(1); seed <= seeds; seed++ {
				sequence, err := g.Draw(seed)
				if err != nil {
					t.Fatalf("Draw(%d) failed: %v", seed, err)
				}
				for _, op := range sequence.Ops {
					written, err := apply(ctx, crs, orSample(op.CR, declared.Sample.GetName()), op)
					if err != nil {
						t.Errorf("The API server refused op %d (%s) of seed %d: %v", op.Index, op.Type, seed, err)
						break
					}
					writes += written
				}
				if err := crs.DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
					t.Fatalf("Deleting the CRs after seed %d failed: %v", seed, err)
				}
			}
			if writes < seeds {
				t.Errorf("%d seeds wrote the CR %d times, so they judged little.", seeds, writes)
			}
		})
	}
}

// apply writes a CR op as the Runner does, and counts the writes.
func apply(ctx context.Context, crs dynamic.ResourceInterface, name string, op run.Op) (int, error) {
	switch op.Type {
	case run.OpCreate:
		_, err := crs.Create(ctx, op.Obj, metav1.CreateOptions{})
		return 1, err
	case run.OpRecreate:
		if err := crs.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return 0, err
		}
		_, err := crs.Create(ctx, op.Obj, metav1.CreateOptions{})
		return 1, err
	case run.OpUpdate:
		patch, err := json.Marshal(op.Patch)
		if err != nil {
			return 0, err
		}
		_, err = crs.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		return 1, err
	case run.OpDelete:
		return 0, crs.Delete(ctx, name, metav1.DeleteOptions{})
	}
	return 0, nil
}

// resourceOf reads the primary CR's resource from its CRD.
func resourceOf(t *testing.T, declared *target.Target) kinds.GroupVersionResource {
	t.Helper()
	documents, err := target.ReadCRDs(declared.CRDs)
	if err != nil {
		t.Fatalf("Reading the CRDs of %s failed: %v", declared.Name, err)
	}
	for _, document := range documents {
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		if spec["group"] == declared.Primary.Group && names["kind"] == declared.Primary.Kind {
			plural, _ := names["plural"].(string)
			return declared.Primary.GroupVersion().WithResource(plural)
		}
	}
	t.Fatalf("The CRDs of %s describe no %s.", declared.Name, declared.Primary.Kind)
	return kinds.GroupVersionResource{}
}
