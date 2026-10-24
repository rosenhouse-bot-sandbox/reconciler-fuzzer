package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const awaitTimeout = 30 * time.Second

// observer list-watches kinds as admin and records each object version once.
// The informer restarts a watch that ends and relists when its
// resourceVersion expires.
type observer struct {
	ctx       context.Context
	cancel    context.CancelFunc
	client    dynamic.Interface
	mapper    meta.ResettableRESTMapper
	discovery discovery.DiscoveryInterface
	record    func(trace.Event)

	mu sync.Mutex
	// kinds holds the preferred resource of each kind the observer watches,
	// or asked to watch.
	kinds map[schema.GroupResource]schema.GroupVersionResource
	seen  map[seenKey]bool
	// gone holds the UID of each instance the observer saw deleted, being
	// deleted, or replaced by another UID.
	gone map[string]bool
	uids map[trace.Ref]string
	// latest holds each live object's last recorded version.
	latest  map[trace.Ref]trace.Version
	hear    []func(trace.Version)
	stopped bool
	wg      sync.WaitGroup
}

func newObserver() *observer {
	return &observer{
		record: func(trace.Event) {}, kinds: map[schema.GroupResource]schema.GroupVersionResource{},
		seen: map[seenKey]bool{}, gone: map[string]bool{}, uids: map[trace.Ref]string{}, latest: map[trace.Ref]trace.Version{},
	}
}

// seenKey includes deleted because a deletion may carry the object's last
// resourceVersion.
type seenKey struct {
	ref     trace.Ref
	rv      string
	deleted bool
}

// watch starts watching gr unless the observer already does, and waits for
// the initial list. A kind it cannot map is tried again on the next call.
func (o *observer) watch(gr schema.GroupResource) error {
	o.mu.Lock()
	if _, found := o.kinds[gr]; found {
		o.mu.Unlock()
		return nil
	}
	o.kinds[gr] = schema.GroupVersionResource{}
	o.mu.Unlock()

	gvr, err := o.watchable(gr)
	if err != nil {
		o.mu.Lock()
		delete(o.kinds, gr)
		o.mu.Unlock()
		return err
	}
	if gvr.Empty() {
		return nil
	}
	o.mu.Lock()
	o.kinds[gr] = gvr
	if o.stopped {
		o.mu.Unlock()
		return nil
	}
	o.wg.Add(1)
	o.mu.Unlock()

	informer := dynamicinformer.NewFilteredDynamicInformer(o.client, gvr, metav1.NamespaceAll, 0, cache.Indexers{}, nil).Informer()
	registration, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { o.see(gr, obj, false) },
		UpdateFunc: func(_, obj any) { o.see(gr, obj, false) },
		DeleteFunc: func(obj any) { o.see(gr, obj, true) },
	})
	if err != nil {
		o.wg.Done()
		return err
	}
	go func() {
		defer o.wg.Done()
		informer.RunWithContext(o.ctx)
	}()
	if !cache.WaitForCacheSync(o.ctx.Done(), registration.HasSynced) {
		return fmt.Errorf("listing %s: %w", gr, o.ctx.Err())
	}
	return nil
}

// watchAll watches each of grs, and waits for their initial lists. The
// watches run together, since the first list of a new CRD can take a second.
// It returns their errors, except for a kind the server does not serve.
func (o *observer) watchAll(grs []schema.GroupResource) error {
	errs := make([]error, len(grs))
	var watches sync.WaitGroup
	for i, gr := range grs {
		watches.Go(func() {
			if err := o.watch(gr); !meta.IsNoMatchError(err) {
				errs[i] = err
			}
		})
	}
	watches.Wait()
	return errors.Join(errs...)
}

// watchable maps gr to its preferred version, or to nothing when the server
// cannot list and watch it. It maps a resource that the mapper finds only in
// another group, such as deployments asked for in the core group, to nothing.
// The observer records each version under gr.
func (o *observer) watchable(gr schema.GroupResource) (schema.GroupVersionResource, error) {
	partial := schema.GroupVersionResource{Group: gr.Group, Resource: gr.Resource}
	gvr, err := o.mapper.ResourceFor(partial)
	if meta.IsNoMatchError(err) {
		o.mapper.Reset()
		gvr, err = o.mapper.ResourceFor(partial)
	}
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("mapping %s: %w", gr, err)
	}
	if gvr.GroupResource() != gr {
		return schema.GroupVersionResource{}, nil
	}
	resources, err := o.discovery.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("discovering %s: %w", gvr.GroupVersion(), err)
	}
	for _, r := range resources.APIResources {
		if r.Name == gr.Resource && slices.Contains(r.Verbs, "list") && slices.Contains(r.Verbs, "watch") {
			return gvr, nil
		}
	}
	return schema.GroupVersionResource{}, nil
}

// stop ends every watch and waits for them.
func (o *observer) stop() {
	o.mu.Lock()
	o.stopped = true
	o.mu.Unlock()
	o.cancel()
	o.wg.Wait()
}

// heard watches the kind of a call a listener recorded.
func (o *observer) heard(c trace.Call) {
	if trace.Ignored(c) || o.ctx.Err() != nil {
		return
	}
	gr := schema.GroupResource{Group: c.Ref.Group, Resource: c.Ref.Resource}
	o.mu.Lock()
	_, found := o.kinds[gr]
	o.mu.Unlock()
	if !found {
		go o.watch(gr)
	}
}

func (o *observer) see(gr schema.GroupResource, obj any, deleted bool) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	ref := trace.Ref{System: Name, Group: gr.Group, Resource: gr.Resource, Namespace: u.GetNamespace(), Name: u.GetName()}
	key := seenKey{ref, u.GetResourceVersion(), deleted}
	o.mu.Lock()
	defer o.mu.Unlock()
	uid := string(u.GetUID())
	if previous := o.uids[ref]; previous != "" && previous != uid {
		o.gone[previous] = true
	}
	o.uids[ref] = uid
	if deleted || u.GetDeletionTimestamp() != nil {
		o.gone[uid] = true
	}
	if o.seen[key] {
		return
	}
	o.seen[key] = true
	body, err := json.Marshal(u.Object)
	if err != nil {
		return
	}
	v := trace.Version{Ref: ref, UID: uid, RV: key.rv, Deleted: deleted, Body: body}
	o.record(trace.Event{Version: &v})
	if deleted {
		delete(o.latest, ref)
	} else {
		o.latest[ref] = v
	}
	for _, hear := range o.hear {
		hear(v)
	}
}

// recorded says whether the observer recorded rv of ref.
func (o *observer) recorded(ref trace.Ref, rv string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen[seenKey{ref, rv, false}] || o.seen[seenKey{ref, rv, true}]
}

// recordedGone says whether the observer saw instance uid of ref deleted,
// being deleted, or replaced.
func (o *observer) recordedGone(ref trace.Ref, uid string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.gone[uid] || (o.uids[ref] != "" && o.uids[ref] != uid)
}

// await waits until done holds, so that a harness action returns only after
// the trace holds its effect.
func (o *observer) await(ctx context.Context, what string, done func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, awaitTimeout)
	defer cancel()
	err := wait.PollUntilContextCancel(ctx, 5*time.Millisecond, true, func(context.Context) (bool, error) { return done(), nil })
	if err != nil {
		return fmt.Errorf("the observer did not record %s: %w", what, err)
	}
	return nil
}

// watched returns the resources the observer watches.
func (o *observer) watched() []schema.GroupVersionResource {
	o.mu.Lock()
	defer o.mu.Unlock()
	var gvrs []schema.GroupVersionResource
	for _, gvr := range o.kinds {
		if !gvr.Empty() {
			gvrs = append(gvrs, gvr)
		}
	}
	slices.SortFunc(gvrs, func(a, b schema.GroupVersionResource) int {
		return strings.Compare(a.Group+"/"+a.Resource, b.Group+"/"+b.Resource)
	})
	return gvrs
}
