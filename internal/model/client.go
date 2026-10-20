package model

import (
	"context"
	"encoding/json"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// client reads and writes through a model's listener.
type client struct {
	mapper  meta.RESTMapper
	dynamic dynamic.Interface
	core    kubernetes.Interface
}

func newClient(config *rest.Config, mapper meta.RESTMapper) (*client, error) {
	d, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &client{mapper: mapper, dynamic: d, core: core}, nil
}

func (c *client) scope(gvk schema.GroupVersionKind) (namespaced, served bool) {
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, false
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, true
}

func (c *client) read(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*metav1.ObjectMeta, error) {
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	found, err := c.dynamic.Resource(mapping.Resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &metav1.ObjectMeta{UID: found.GetUID(), DeletionTimestamp: found.GetDeletionTimestamp(), Finalizers: found.GetFinalizers()}, nil
}

func (c *client) apply(ctx context.Context, o op) error {
	if o.finalize != nil {
		var namespace corev1.Namespace
		if err := json.Unmarshal(o.finalize, &namespace); err != nil {
			return err
		}
		namespace.Spec.Finalizers = slices.DeleteFunc(namespace.Spec.Finalizers, func(f corev1.FinalizerName) bool {
			return f == corev1.FinalizerKubernetes
		})
		_, err := c.core.CoreV1().Namespaces().Finalize(ctx, &namespace, metav1.UpdateOptions{})
		return err
	}
	gv, err := schema.ParseGroupVersion(o.apiVersion)
	if err != nil {
		return err
	}
	resource := c.dynamic.Resource(gv.WithResource(o.ref.Resource)).Namespace(o.ref.Namespace)
	if o.delete {
		uid := types.UID(o.uid)
		options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
		if o.foreground {
			policy := metav1.DeletePropagationForeground
			options.PropagationPolicy = &policy
		}
		return resource.Delete(ctx, o.ref.Name, options)
	}
	fields := map[string]any{"uid": o.uid, "resourceVersion": o.rv}
	for key, value := range o.patch {
		fields[key] = value
	}
	patch, err := json.Marshal(map[string]any{"metadata": fields})
	if err != nil {
		return err
	}
	_, err = resource.Patch(ctx, o.ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}
