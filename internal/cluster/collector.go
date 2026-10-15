package cluster

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
)

const (
	// The collector sweeps on watch events, so its watches never resync.
	noResync = 0
	// cacheSyncTimeout bounds the wait for the watches to catch up.
	cacheSyncTimeout = time.Minute
	// retryDelay is how long the collector waits to sweep again after a call
	// to the API server failed. It stays well inside the second within which
	// the collector deletes an object after its owner's deletion event.
	retryDelay = 250 * time.Millisecond
)

// CollectorOptions configure the garbage-collector emulation.
type CollectorOptions struct {
	// Namespace is the run namespace. The collector looks nowhere else.
	Namespace string
	// Kinds are the namespaced kinds the collector watches. An owner of any
	// other kind counts as live.
	Kinds []schema.GroupVersionKind
	// Mapper resolves those kinds to the resources the collector lists, and
	// the version an ownerReference names.
	Mapper apimeta.RESTMapper
	// Log defaults to slog.Default().
	Log *slog.Logger
}

// Validate reports why the options cannot start a collector.
func (o CollectorOptions) Validate() error {
	if o.Namespace == "" {
		return errors.New("a run namespace is required")
	}
	if len(o.Kinds) == 0 {
		return errors.New("at least one kind to watch is required")
	}
	if o.Mapper == nil {
		return errors.New("a RESTMapper is required")
	}
	return nil
}

// Collector deletes an object of a watched kind once every owner in its
// ownerReferences is gone, as kube-controller-manager would. envtest runs no
// controller manager. The collector holds its own client, so its writes reach
// the API server directly and never the proxy.
type Collector struct {
	client     metadata.Interface
	namespace  string
	mapper     apimeta.RESTMapper
	watched    map[schema.GroupKind]watchedKind
	log        *slog.Logger
	unresolved map[Unresolved]bool
	deletions  []deletion

	informers metadatainformer.SharedInformerFactory
	events    chan struct{}
	cancel    context.CancelFunc
	stopped   chan struct{}
}

// watchedKind is one kind the collector watches, at the version it lists.
type watchedKind struct {
	kind     schema.GroupVersionKind
	resource schema.GroupVersionResource
	store    cache.Store
}

// Unresolved is an owner the collector cannot resolve and the dependent that
// names it. The collector counts that owner as live, so it never deletes the
// dependent.
type Unresolved struct {
	DependentKind schema.GroupVersionKind
	DependentName string
	// OwnerKind is the kind at the version the reference names.
	OwnerKind schema.GroupVersionKind
	OwnerName string
	// Unserved says the API server does not serve OwnerKind. Otherwise the
	// collector does not watch the owner's kind.
	Unserved bool
}

// StartCollector runs the collector over opts.Namespace until Stop.
func StartCollector(config *rest.Config, opts CollectorOptions) (*Collector, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("starting the collector: %w", err)
	}
	config = collectorConfig(config)
	client, err := metadata.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building the collector's client: %w", err)
	}
	watched, err := namespacedKinds(opts.Mapper, opts.Kinds)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	c := &Collector{
		client:     client,
		namespace:  opts.Namespace,
		mapper:     opts.Mapper,
		watched:    watched,
		log:        log,
		unresolved: map[Unresolved]bool{},
		informers:  metadatainformer.NewFilteredSharedInformerFactory(client, noResync, opts.Namespace, nil),
		events:     make(chan struct{}, 1),
		stopped:    make(chan struct{}),
	}
	sweepOn := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.notify() },
		UpdateFunc: func(any, any) { c.notify() },
		DeleteFunc: func(any) { c.notify() },
	}
	for group, kind := range c.watched {
		informer := c.informers.ForResource(kind.resource).Informer()
		if _, err := informer.AddEventHandler(sweepOn); err != nil {
			return nil, fmt.Errorf("watching %s: %w", kind.kind, err)
		}
		kind.store = informer.GetStore()
		c.watched[group] = kind
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	if err := c.sync(ctx); err != nil {
		cancel()
		c.informers.Shutdown()
		return nil, err
	}
	go c.run(ctx)
	return c, nil
}

// Stop stops the collector and abandons a sweep in progress. It names each
// owner the collector could not resolve, once per dependent.
func (c *Collector) Stop() []Unresolved {
	c.cancel()
	<-c.stopped
	c.informers.Shutdown()
	return c.unresolvedOwners()
}

func (c *Collector) unresolvedOwners() []Unresolved {
	found := slices.Collect(maps.Keys(c.unresolved))
	slices.SortFunc(found, func(a, b Unresolved) int {
		return cmp.Or(
			strings.Compare(a.DependentKind.String(), b.DependentKind.String()),
			strings.Compare(a.DependentName, b.DependentName),
			strings.Compare(a.OwnerKind.String(), b.OwnerKind.String()),
			strings.Compare(a.OwnerName, b.OwnerName),
		)
	})
	return found
}

// collectorConfig is the collector's own connection to the API server. Its
// user agent tells the collector's writes from the target's.
func collectorConfig(config *rest.Config) *rest.Config {
	config = rest.CopyConfig(config)
	config.UserAgent = "reconciler-fuzzer-collector"
	return config
}

// namespacedKinds maps each kind to the resource it is served at. A run owns
// one namespace, so a cluster-scoped kind is an error.
func namespacedKinds(mapper apimeta.RESTMapper, kinds []schema.GroupVersionKind) (map[schema.GroupKind]watchedKind, error) {
	watched := map[schema.GroupKind]watchedKind{}
	for _, gvk := range kinds {
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, fmt.Errorf("resolving the kind %s: %w", gvk, err)
		}
		if mapping.Scope.Name() != apimeta.RESTScopeNameNamespace {
			return nil, fmt.Errorf("the collector watches namespaced kinds only, and %s is cluster-scoped", gvk)
		}
		watched[gvk.GroupKind()] = watchedKind{kind: mapping.GroupVersionKind, resource: mapping.Resource}
	}
	return watched, nil
}

// sync starts the watches and waits for their caches, so that the first sweep
// sees every object already in the namespace.
func (c *Collector) sync(ctx context.Context) error {
	c.informers.Start(ctx.Done())
	deadline, cancel := context.WithTimeout(ctx, cacheSyncTimeout)
	defer cancel()
	for resource, synced := range c.informers.WaitForCacheSync(deadline.Done()) {
		if !synced {
			return fmt.Errorf("syncing the collector's watch on %s", resource)
		}
	}
	return nil
}

func (c *Collector) run(ctx context.Context) {
	defer close(c.stopped)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.events:
			c.sweep(ctx, c.objects())
		}
	}
}

// notify asks for a sweep. One pending request stands for any number of
// events, which is what keeps a burst to a single sweep.
func (c *Collector) notify() {
	select {
	case c.events <- struct{}{}:
	default:
	}
}

// sweep deletes every object whose owners are all gone.
func (c *Collector) sweep(ctx context.Context, objects []object) {
	live := c.liveOwners(ctx, objects)
	for _, obj := range objects {
		collect, unresolved := live.collectible(obj)
		for _, owner := range unresolved {
			c.unresolved[owner] = true
		}
		if collect {
			c.delete(ctx, obj, live.gone(obj))
		}
	}
}

// object is one watched object in the run namespace.
type object struct {
	kind     schema.GroupVersionKind
	resource schema.GroupVersionResource
	meta     *metav1.PartialObjectMetadata
}

// objects lists what the watches hold in the run namespace. An object already
// marked for deletion is left out, because its deletion is under way.
func (c *Collector) objects() []object {
	var found []object
	for _, watched := range c.watched {
		for _, cached := range watched.store.List() {
			meta, ok := cached.(*metav1.PartialObjectMetadata)
			if !ok || meta.Namespace != c.namespace || meta.DeletionTimestamp != nil {
				continue
			}
			found = append(found, object{kind: watched.kind, resource: watched.resource, meta: meta})
		}
	}
	return found
}

// liveOwners reads each owner the objects name, once per sweep. The read is
// live because the watch on one kind can lag the watch on another, and a
// dependent that arrives before its owner must not be collected.
func (c *Collector) liveOwners(ctx context.Context, objects []object) owners {
	live := owners{mapper: c.mapper, watched: c.watched, resolved: map[ownerKey]owner{}}
	for _, obj := range objects {
		for _, ref := range obj.meta.OwnerReferences {
			key := keyOf(ref)
			kind, how := live.resolve(ref)
			_, alreadyRead := live.resolved[key]
			if how != resolvable || alreadyRead {
				continue
			}
			found, err := c.client.Resource(kind.resource).Namespace(c.namespace).
				Get(ctx, key.name, metav1.GetOptions{})
			switch {
			case err == nil:
				live.resolved[key] = owner{uid: found.UID}
			case apierrors.IsNotFound(err):
				live.resolved[key] = owner{}
			default:
				live.resolved[key] = owner{unreadable: true}
				c.logFailure(ctx, "The collector could not read an owner.",
					"kind", key.kind.Kind, "name", key.name, "error", err)
			}
		}
	}
	return live
}

// delete removes obj unless it changed since the collector read it, and
// records the attempt. The preconditions catch a name taken over by another
// object, and a dependent adopted by a live owner in the meantime; either
// change brings a watch event of its own, and with it another sweep. A
// stopped collector sends no delete.
func (c *Collector) delete(ctx context.Context, obj object, owners []goneOwner) {
	if ctx.Err() != nil {
		return
	}
	uid, version := obj.meta.UID, obj.meta.ResourceVersion
	preconditions := metav1.Preconditions{UID: &uid, ResourceVersion: &version}
	at := time.Now()
	err := c.client.Resource(obj.resource).Namespace(c.namespace).
		Delete(ctx, obj.meta.Name, metav1.DeleteOptions{Preconditions: &preconditions})
	tried := deletion{
		Time: at, Kind: observe.KindName(obj.kind), Namespace: c.namespace, Name: obj.meta.Name,
		UID: uid, ResourceVersion: version, Owners: owners, Result: resultOf(err),
	}
	if err != nil {
		tried.Error = err.Error()
	}
	c.deletions = append(c.deletions, tried)
	if tried.Result == failed {
		c.logFailure(ctx, "The collector could not delete an object.",
			"resource", obj.resource.Resource, "name", obj.meta.Name, "error", err)
	}
}

// deletion is one delete the collector tried.
type deletion struct {
	// Time is when the collector sent the delete.
	Time time.Time `json:"time"`
	// Kind is written group/version/Kind.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// UID and ResourceVersion are the object as the collector read it, which
	// the delete names as its preconditions.
	UID             types.UID `json:"uid"`
	ResourceVersion string    `json:"resourceVersion"`
	// Owners are the object's ownerReferences, every one of them gone.
	Owners []goneOwner `json:"owners"`
	// Result is deleted, not found, conflict or error. Deleted says the API
	// server accepted the delete, which a finalizer can still hold. Conflict
	// says the object changed since the collector read it.
	Result string `json:"result"`
	// Error says why the delete failed: the API server's answer, or the
	// cancellation where Stop cut the call short.
	Error string `json:"error,omitempty"`
}

// goneOwner is an ownerReference whose owner the collector found gone.
type goneOwner struct {
	metav1.OwnerReference
	// Gone is not found, or another UID where an object of a different UID
	// now holds the owner's name.
	Gone string `json:"gone"`
	// FoundUID is that object's UID.
	FoundUID types.UID `json:"foundUID,omitempty"`
}

// A deletion's Result and an owner's Gone take these values.
const (
	deleted    = "deleted"
	notFound   = "not found"
	conflict   = "conflict"
	failed     = "error"
	anotherUID = "another UID"
)

func resultOf(err error) string {
	switch {
	case err == nil:
		return deleted
	case apierrors.IsNotFound(err):
		return notFound
	case apierrors.IsConflict(err):
		return conflict
	default:
		return failed
	}
}

// WriteLog writes each delete the collector tried, one JSON object per line,
// in the order it tried them. It reads what the sweeps recorded, so it runs
// once Stop has returned.
func (c *Collector) WriteLog(w io.Writer) error {
	encoder := json.NewEncoder(w)
	for _, tried := range c.deletions {
		if err := encoder.Encode(tried); err != nil {
			return fmt.Errorf("writing the collector's deletes: %w", err)
		}
	}
	return nil
}

// logFailure reports a call the collector could not make and asks for another
// sweep, because a failed call brings no watch event of its own. A call cut
// short by Stop is not a failure.
func (c *Collector) logFailure(ctx context.Context, message string, args ...any) {
	if ctx.Err() != nil {
		return
	}
	c.log.Error(message, args...)
	time.AfterFunc(retryDelay, c.notify)
}

// ownerKey identifies an owner within the run namespace. A reference may name
// the owner at any version the API server serves, so the key holds none.
type ownerKey struct {
	kind schema.GroupKind
	name string
}

func keyOf(ref metav1.OwnerReference) ownerKey {
	return ownerKey{kind: schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).GroupKind(), name: ref.Name}
}

// owner is what one live read found. The zero value says the owner is gone.
type owner struct {
	uid types.UID
	// unreadable marks an owner the collector failed to read. It counts as
	// live, so a failed read never costs a dependent its life.
	unreadable bool
}

// owners is what the collector knows about the run namespace: the kinds the
// API server serves, the kinds the collector watches, and the owners it
// resolved.
type owners struct {
	mapper   apimeta.RESTMapper
	watched  map[schema.GroupKind]watchedKind
	resolved map[ownerKey]owner
}

// resolution says whether the collector can resolve an owner, or why not.
type resolution int

const (
	resolvable resolution = iota
	unwatchedKind
	unservedVersion
)

// resolve finds the watched kind a reference names. Like the garbage
// collector, it resolves a reference only at a version the API server serves.
func (o owners) resolve(ref metav1.OwnerReference) (watchedKind, resolution) {
	gvk := schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind)
	kind, watched := o.watched[gvk.GroupKind()]
	if !watched {
		return watchedKind{}, unwatchedKind
	}
	if _, err := o.mapper.RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return watchedKind{}, unservedVersion
	}
	return kind, resolvable
}

// collectible reports whether obj must be deleted: it has at least one
// ownerReference and every owner is gone. Only an owner read and found missing
// counts as gone, and a name read back under a different UID counts the same.
// An owner the collector cannot resolve counts as live and comes back as
// unresolved.
func (o owners) collectible(obj object) (bool, []Unresolved) {
	refs := obj.meta.OwnerReferences
	collect := len(refs) > 0
	var unresolved []Unresolved
	for _, ref := range refs {
		if _, how := o.resolve(ref); how != resolvable {
			unresolved = append(unresolved, Unresolved{
				DependentKind: obj.kind,
				DependentName: obj.meta.Name,
				OwnerKind:     schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind),
				OwnerName:     ref.Name,
				Unserved:      how == unservedVersion,
			})
			collect = false
			continue
		}
		if found, read := o.resolved[keyOf(ref)]; !read || found.unreadable || found.uid == ref.UID {
			collect = false
		}
	}
	return collect, unresolved
}

// gone says why each owner of a collectible object counts as gone.
func (o owners) gone(obj object) []goneOwner {
	gone := make([]goneOwner, len(obj.meta.OwnerReferences))
	for i, ref := range obj.meta.OwnerReferences {
		gone[i] = goneOwner{OwnerReference: ref, Gone: notFound}
		if found := o.resolved[keyOf(ref)]; found.uid != "" {
			gone[i].Gone, gone[i].FoundUID = anotherUID, found.uid
		}
	}
	return gone
}
