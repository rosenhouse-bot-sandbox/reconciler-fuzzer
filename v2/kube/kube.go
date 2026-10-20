// Package kube is the Kubernetes system: an envtest API server with a
// per-run namespace, an RBAC identity for the controller, one listener per
// caller, and an observer that records every object version.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/interpose"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Name is the system's name in refs, plans and ${systems.kube.*}.
const Name = "kube"

// ErrUnsupported answers a fault the system cannot start.
var ErrUnsupported = errors.New("unsupported fault")

// Options configure the system. Paths are absolute.
type Options struct {
	CRDs []string
	// RBAC holds Role and ClusterRole manifests. Roles bind in the run
	// namespace and ClusterRoles cluster-wide, to the controller's
	// ServiceAccount.
	RBAC []string
	// Fixtures are objects the harness creates in the run namespace before
	// the controller starts.
	Fixtures []string
	// Watch lists kinds the observer watches from the start. It adds any
	// other kind a listener sees a request for.
	Watch []schema.GroupVersionKind
	// Warm holds objects that Start creates with a server-side dry run as
	// admin. The API server's first create of a new kind can take seconds,
	// which would otherwise lengthen the first fault period.
	Warm []map[string]any
}

// Kube runs one envtest control plane for one run.
type Kube struct {
	o Options

	env       *envtest.Environment
	admin     *rest.Config
	namespace string
	mapper    meta.ResettableRESTMapper
	harness   dynamic.Interface
	observer  *observer
	listeners []*interpose.Listener

	stopOnce sync.Once
	stopErr  error
}

var _ system.System = (*Kube)(nil)
var _ system.Stopper = (*Kube)(nil)

func New(o Options) *Kube { return &Kube{o: o} }

func (k *Kube) Name() string { return Name }

// Start brings up envtest with the CRDs, creates the namespace and identity,
// opens the controller and harness listeners, starts the observer and
// creates the fixtures through the harness listener. Endpoints.Vars holds
// kubeconfig, a file that points at the controller listener and names the
// namespace, namespace, and url, the controller listener's address.
func (k *Kube) Start(ctx context.Context, env *system.Env) (system.Endpoints, error) {
	endpoints, err := k.start(ctx, env)
	if err != nil {
		return system.Endpoints{}, errors.Join(err, k.Stop(ctx))
	}
	return endpoints, nil
}

func (k *Kube) start(ctx context.Context, env *system.Env) (system.Endpoints, error) {
	roles, clusterRoles, err := readRBAC(k.o.RBAC)
	if err != nil {
		return system.Endpoints{}, err
	}
	fixtures, err := readObjects(k.o.Fixtures)
	if err != nil {
		return system.Endpoints{}, err
	}

	existing := false
	k.env = &envtest.Environment{CRDDirectoryPaths: k.o.CRDs, UseExistingCluster: &existing, ErrorIfCRDPathMissing: true}
	// Only kube-controller-manager removes the finalizers these add. Append
	// keeps envtest's own entry, which disables ServiceAccount.
	apiServer := k.env.ControlPlane.GetAPIServer().Configure()
	apiServer.Append("disable-admission-plugins", "StorageObjectInUseProtection")
	apiServer.Set("enable-garbage-collector", "false")
	admin, err := k.env.Start()
	if err != nil {
		k.env = nil
		return system.Endpoints{}, fmt.Errorf("starting envtest: %w", err)
	}
	admin.QPS, admin.Burst = -1, 0
	k.admin = admin

	clientset, err := kubernetes.NewForConfig(admin)
	if err != nil {
		return system.Endpoints{}, err
	}
	k.namespace = newNamespaceName()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: k.namespace}}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, namespace, metav1.CreateOptions{}); err != nil {
		return system.Endpoints{}, fmt.Errorf("creating namespace %s: %w", k.namespace, err)
	}
	identity, err := createIdentity(ctx, clientset, admin, k.namespace, roles, clusterRoles)
	if err != nil {
		return system.Endpoints{}, err
	}

	cached := memory.NewMemCacheClient(clientset.Discovery())
	k.mapper = restmapper.NewDeferredDiscoveryRESTMapper(cached)
	direct, err := dynamic.NewForConfig(admin)
	if err != nil {
		return system.Endpoints{}, err
	}
	observerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	k.observer = &observer{
		ctx: observerCtx, cancel: cancel, client: direct, mapper: k.mapper, discovery: cached, record: env.Record,
		kinds: map[schema.GroupResource]schema.GroupVersionResource{}, seen: map[seenKey]bool{}, gone: map[trace.Ref]bool{},
	}

	upstream, _, err := rest.DefaultServerUrlFor(admin)
	if err != nil {
		return system.Endpoints{}, err
	}
	controller, err := k.listen(trace.Controller, upstream, identity, env.Record)
	if err != nil {
		return system.Endpoints{}, err
	}
	harness, err := k.listen(trace.Harness, upstream, admin, env.Record)
	if err != nil {
		return system.Endpoints{}, err
	}
	k.harness, err = dynamic.NewForConfig(&rest.Config{Host: harness.URL(), QPS: -1})
	if err != nil {
		return system.Endpoints{}, err
	}
	kubeconfig := filepath.Join(env.Dir, "kubeconfig")
	if err := writeKubeconfig(kubeconfig, controller.URL(), k.namespace); err != nil {
		return system.Endpoints{}, err
	}

	kinds := slices.Clone(k.o.Watch)
	for _, fixture := range fixtures {
		kinds = append(kinds, fixture.GroupVersionKind())
	}
	for _, gvk := range kinds {
		mapping, err := k.mapping(gvk)
		if err != nil {
			return system.Endpoints{}, err
		}
		if err := k.observer.watch(mapping.Resource.GroupResource()); err != nil {
			return system.Endpoints{}, err
		}
	}
	// A kind the RBAC names but the server lacks is watched once a listener
	// sees a request for it.
	for _, gr := range ruleKinds(roles, clusterRoles) {
		k.observer.watch(gr)
	}
	for _, fixture := range fixtures {
		if _, err := k.Create(ctx, fixture.Object); err != nil {
			return system.Endpoints{}, fmt.Errorf("creating fixture %s %s: %w", fixture.GetKind(), fixture.GetName(), err)
		}
	}
	for _, obj := range k.o.Warm {
		if err := k.warm(ctx, direct, obj); err != nil {
			return system.Endpoints{}, err
		}
	}
	return system.Endpoints{
		Controller: controller,
		Harness:    harness,
		Vars:       map[string]string{"kubeconfig": kubeconfig, "namespace": k.namespace, "url": controller.URL()},
	}, nil
}

func (k *Kube) warm(ctx context.Context, client dynamic.Interface, obj map[string]any) error {
	u := (&unstructured.Unstructured{Object: obj}).DeepCopy()
	mapping, err := k.mapping(u.GroupVersionKind())
	if err != nil {
		return err
	}
	resource := client.Resource(mapping.Resource)
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		_, err = resource.Namespace(k.namespace).Create(ctx, u, dryRun)
	} else {
		_, err = resource.Create(ctx, u, dryRun)
	}
	if err != nil {
		return fmt.Errorf("warming %s: %w", u.GetKind(), err)
	}
	return nil
}

func (k *Kube) listen(caller trace.Caller, upstream *url.URL, credentials *rest.Config, record func(trace.Event)) (*interpose.Listener, error) {
	transport, err := rest.TransportFor(credentials)
	if err != nil {
		return nil, err
	}
	listener, err := interpose.Listen(interpose.Options{
		System: Name, Caller: caller, Upstream: upstream, Transport: transport,
		Record: record, OnCall: k.observer.heard,
	})
	if err != nil {
		return nil, err
	}
	k.listeners = append(k.listeners, listener)
	return listener, nil
}

// writeKubeconfig writes a kubeconfig with no credentials.
func writeKubeconfig(path, server, namespace string) error {
	config := clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{Name: {Server: server}},
		Contexts:       map[string]*clientcmdapi.Context{Name: {Cluster: Name, Namespace: namespace}},
		CurrentContext: Name,
	}
	if err := clientcmd.WriteToFile(config, path); err != nil {
		return fmt.Errorf("writing the kubeconfig: %w", err)
	}
	return nil
}

// Snapshot lists every object of each watched kind as admin.
func (k *Kube) Snapshot(ctx context.Context) ([]system.Resource, error) {
	client, err := dynamic.NewForConfig(k.admin)
	if err != nil {
		return nil, err
	}
	var resources []system.Resource
	for _, gvr := range k.observer.watched() {
		list, err := client.Resource(gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", gvr.GroupResource(), err)
		}
		for _, item := range list.Items {
			body, err := json.Marshal(item.Object)
			if err != nil {
				return nil, err
			}
			resources = append(resources, system.Resource{
				Ref:  trace.Ref{System: Name, Group: gvr.Group, Resource: gvr.Resource, Namespace: item.GetNamespace(), Name: item.GetName()},
				UID:  string(item.GetUID()),
				Body: body,
			})
		}
	}
	return resources, nil
}

// Pending is false: envtest schedules no work of its own.
func (k *Kube) Pending() bool { return false }

// Fault returns ErrUnsupported. Request faults go to the controller
// listener.
func (k *Kube) Fault(f plan.Fault) (end func(), err error) { return nil, ErrUnsupported }

// Stop stops the observer, the listeners and envtest. Later calls do nothing.
func (k *Kube) Stop(ctx context.Context) error {
	k.stopOnce.Do(func() {
		var errs []error
		if k.observer != nil {
			k.observer.stop()
		}
		for _, listener := range k.listeners {
			errs = append(errs, listener.Close())
		}
		if k.env != nil {
			if err := k.env.Stop(); err != nil {
				errs = append(errs, fmt.Errorf("stopping envtest: %w", err))
			}
		}
		k.stopErr = errors.Join(errs...)
	})
	return k.stopErr
}

// Namespace is the run namespace. It is empty before Start.
func (k *Kube) Namespace() string { return k.namespace }

// Create, Patch and Delete act through the harness listener, and return once
// the observer has recorded their effect.

// Create creates obj in the run namespace when obj names none. It returns the
// created object's ref.
func (k *Kube) Create(ctx context.Context, obj map[string]any) (trace.Ref, error) {
	u := (&unstructured.Unstructured{Object: obj}).DeepCopy()
	mapping, err := k.mapping(u.GroupVersionKind())
	if err != nil {
		return trace.Ref{}, err
	}
	resource := k.harness.Resource(mapping.Resource)
	var created *unstructured.Unstructured
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if u.GetNamespace() == "" {
			u.SetNamespace(k.namespace)
		}
		created, err = resource.Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{})
	} else {
		created, err = resource.Create(ctx, u, metav1.CreateOptions{})
	}
	if err != nil {
		return trace.Ref{}, err
	}
	ref := trace.Ref{
		System: Name, Group: mapping.Resource.Group, Resource: mapping.Resource.Resource,
		Namespace: created.GetNamespace(), Name: created.GetName(),
	}
	return ref, k.awaitVersion(ctx, ref, created.GetResourceVersion())
}

func (k *Kube) awaitVersion(ctx context.Context, ref trace.Ref, rv string) error {
	return k.observer.await(ctx, describe(ref)+" at "+rv, func() bool { return k.observer.recorded(ref, rv) })
}

func describe(ref trace.Ref) string { return ref.Resource + " " + ref.Namespace + "/" + ref.Name }

// Patch applies a JSON merge patch through the harness listener.
func (k *Kube) Patch(ctx context.Context, ref trace.Ref, patch []byte) error {
	resource, err := k.resource(ref)
	if err != nil {
		return err
	}
	patched, err := resource.Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	return k.awaitVersion(ctx, ref, patched.GetResourceVersion())
}

// Delete deletes through the harness listener. A missing object is no error.
func (k *Kube) Delete(ctx context.Context, ref trace.Ref) error {
	resource, err := k.resource(ref)
	if err != nil {
		return err
	}
	err = resource.Delete(ctx, ref.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return k.observer.await(ctx, "the deletion of "+describe(ref), func() bool { return k.observer.recordedGone(ref) })
}

// WaitGone returns once ref names no object, finalizers included.
func (k *Kube) WaitGone(ctx context.Context, ref trace.Ref) error {
	resource, err := k.resource(ref)
	if err != nil {
		return err
	}
	return wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, err := resource.Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

// Ref returns the ref of an object of kind gvk named name in the run
// namespace, or cluster-scoped when the kind is.
func (k *Kube) Ref(gvk schema.GroupVersionKind, name string) (trace.Ref, error) {
	mapping, err := k.mapping(gvk)
	if err != nil {
		return trace.Ref{}, err
	}
	ref := trace.Ref{System: Name, Group: mapping.Resource.Group, Resource: mapping.Resource.Resource, Name: name}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ref.Namespace = k.namespace
	}
	return ref, nil
}

func (k *Kube) mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		k.mapper.Reset()
		mapping, err = k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, fmt.Errorf("mapping %s: %w", gvk, err)
	}
	return mapping, nil
}

// resource is the harness client for ref's kind at its preferred version.
func (k *Kube) resource(ref trace.Ref) (dynamic.ResourceInterface, error) {
	gvr, err := k.mapper.ResourceFor(schema.GroupVersionResource{Group: ref.Group, Resource: ref.Resource})
	if err != nil {
		return nil, fmt.Errorf("mapping %s.%s: %w", ref.Resource, ref.Group, err)
	}
	if ref.Namespace == "" {
		return k.harness.Resource(gvr), nil
	}
	return k.harness.Resource(gvr).Namespace(ref.Namespace), nil
}
