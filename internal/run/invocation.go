package run

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// Invocation manages cluster-scoped fixtures that are shared across runs.
// Prepare creates them once; Close deletes them.
type Invocation struct {
	client  dynamic.Interface
	created []clusterFixture
}

type clusterFixture struct {
	resource schema.GroupVersionResource
	name     string
}

// NewInvocation builds an Invocation from the target and a REST config.
// It is a no-op when the target has no cluster-scoped fixtures.
func NewInvocation(config *rest.Config, t *target.Target) (*Invocation, error) {
	if len(t.ClusterFixtures) == 0 {
		return &Invocation{}, nil
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building reconciler-fuzzer's dynamic client for cluster fixtures: %w", err)
	}
	return &Invocation{client: client}, nil
}

// Prepare creates the cluster-scoped fixtures. It must be called after vet.
func (inv *Invocation) Prepare(ctx context.Context, t *target.Target, mapper meta.RESTMapper) error {
	for _, fixture := range t.Fixtures {
		gvk := fixture.GroupVersionKind()
		if !slices.Contains(t.ClusterFixtures, gvk) {
			continue
		}
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("resolving the cluster fixture %s %s: %w", observe.KindName(gvk), fixture.GetName(), err)
		}
		obj := fixture.DeepCopy()
		obj.SetNamespace("")
		if _, err := inv.client.Resource(mapping.Resource).Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating the cluster fixture %s %s: %w", observe.KindName(gvk), fixture.GetName(), err)
		}
		inv.created = append(inv.created, clusterFixture{resource: mapping.Resource, name: fixture.GetName()})
	}
	return nil
}

// Close deletes the cluster-scoped fixtures Prepare created.
func (inv *Invocation) Close(ctx context.Context) error {
	var errs []error
	for _, cf := range inv.created {
		if err := inv.client.Resource(cf.resource).Delete(ctx, cf.name, metav1.DeleteOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("deleting the cluster fixture %s: %w", cf.name, err))
		}
	}
	return errors.Join(errs...)
}
