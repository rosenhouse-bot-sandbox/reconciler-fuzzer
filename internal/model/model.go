// Package model holds in-process stand-ins for other controllers. Each
// model writes through its own kube listener and records Work events.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// retryDelay is how long a model waits to sweep again after a failed call.
const retryDelay = 250 * time.Millisecond

// New returns the models every run starts after the Kubernetes system: the
// garbage collector and the namespace controller.
func New(k *kube.Kube) []system.System {
	gc := newModel(string(trace.GC), trace.GC, collect, gcWakes)
	namespaces := newModel(string(trace.Namespaces), trace.Namespaces,
		func(_ context.Context, objects []object, _ cluster) ([]op, bool) { return finish(objects), false }, namespaceWakes)
	gc.k, namespaces.k = k, k
	return []system.System{gc, namespaces}
}

// object is what a model holds of an object's latest version.
type object struct {
	ref trace.Ref
	metav1.PartialObjectMetadata
	body json.RawMessage
}

func (o object) deleting() bool { return o.DeletionTimestamp != nil }

// op is one write. A model sends a delete, a merge patch of metadata fields,
// or a namespace finalize.
type op struct {
	ref        trace.Ref
	apiVersion string
	// uid and rv are the object as the model held it. A delete names the
	// uid as its precondition, and a patch names both.
	uid, rv    string
	delete     bool
	foreground bool
	patch      map[string]any
	// finalize holds the Namespace to PUT to its finalize subresource.
	finalize json.RawMessage
}

// planner returns the writes a model would make to objects, and whether a
// read failed.
type planner func(ctx context.Context, objects []object, c cluster) ([]op, bool)

// waker says whether a version can give a model work, given what it holds.
type waker func(v object, deleted bool, held map[trace.Ref]object) bool

type writer interface {
	apply(ctx context.Context, o op) error
}

// model holds every object the observer records, and sweeps once per burst
// of waking versions.
type model struct {
	name   string
	caller trace.Caller
	k      *kube.Kube
	plan   planner
	wakes  waker
	record func(trace.Event)
	c      cluster
	w      writer

	mu      sync.Mutex
	objects map[trace.Ref]object
	// written maps each object the model wrote to the resourceVersion the
	// write named. The model waits for another version before writing again.
	written  map[trace.Ref]string
	pending  bool
	retrying bool
	wake     chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

var _ system.System = (*model)(nil)
var _ system.Stopper = (*model)(nil)

func newModel(name string, caller trace.Caller, p planner, w waker) *model {
	return &model{
		name: name, caller: caller, plan: p, wakes: w,
		objects: map[trace.Ref]object{}, written: map[trace.Ref]string{}, wake: make(chan struct{}, 1),
	}
}

func (m *model) Name() string { return m.name }

// Start opens the model's listener and subscribes to the observer. The
// models hold no endpoints.
func (m *model) Start(ctx context.Context, env *system.Env) (system.Endpoints, error) {
	config, err := m.k.Listen(m.caller)
	if err != nil {
		return system.Endpoints{}, err
	}
	c, err := newClient(config, m.k.Mapper())
	if err != nil {
		return system.Endpoints{}, err
	}
	m.record, m.c, m.w = env.Record, c, c
	ctx, m.cancel = context.WithCancel(context.WithoutCancel(ctx))
	m.done = make(chan struct{})
	m.k.Subscribe(m.hear)
	go m.run(ctx)
	return system.Endpoints{}, nil
}

func (m *model) Snapshot(context.Context) ([]system.Resource, error) { return nil, nil }

func (m *model) Pending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pending
}

func (m *model) Fault(plan.Fault) (func(), error) { return nil, kube.ErrUnsupported }

// Stop ends the sweeps and abandons one in progress.
func (m *model) Stop(context.Context) error {
	if m.cancel != nil {
		m.cancel()
		<-m.done
	}
	return nil
}

func (m *model) run(ctx context.Context) {
	defer close(m.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			m.sweep(ctx)
		}
	}
}

// hear holds v and queues a sweep when v can give the model work.
func (m *model) hear(v trace.Version) {
	o := object{ref: v.Ref, body: v.Body}
	if err := json.Unmarshal(v.Body, &o.PartialObjectMetadata); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v.Deleted {
		delete(m.objects, v.Ref)
	} else {
		m.objects[v.Ref] = o
	}
	if _, wrote := m.written[v.Ref]; wrote || m.wakes(o, v.Deleted, m.objects) {
		m.queue()
	}
}

// queue asks for a sweep. One queued sweep stands for any number of
// requests. The caller holds m.mu.
func (m *model) queue() {
	m.setPending(true)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *model) setPending(pending bool) {
	if pending != m.pending {
		m.pending = pending
		m.record(trace.Event{Work: &trace.Work{System: m.name, Pending: pending}})
	}
}

// sweep plans and sends the model's writes, skipping objects whose last
// write it has not yet observed.
func (m *model) sweep(ctx context.Context) {
	m.mu.Lock()
	objects := slices.SortedFunc(maps.Values(m.objects), func(a, b object) int {
		return strings.Compare(fmt.Sprint(a.ref), fmt.Sprint(b.ref))
	})
	for ref, rv := range m.written {
		if m.objects[ref].ResourceVersion != rv {
			delete(m.written, ref)
		}
	}
	awaited := maps.Clone(m.written)
	m.mu.Unlock()

	ops, failed := m.plan(ctx, objects, m.c)
	var sent []op
	for _, o := range ops {
		if _, wait := awaited[o.ref]; wait || ctx.Err() != nil {
			continue
		}
		err := m.w.apply(ctx, o)
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			failed = true
			continue
		}
		sent = append(sent, o)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range sent {
		if m.objects[o.ref].ResourceVersion == o.rv {
			m.written[o.ref] = o.rv
		}
	}
	if len(sent) > 0 {
		m.queue()
	}
	if failed && ctx.Err() == nil && !m.retrying {
		m.retrying = true
		time.AfterFunc(retryDelay, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.retrying = false
			if ctx.Err() == nil {
				m.queue()
			}
		})
	}
	if len(m.wake) == 0 && !m.retrying && len(m.written) == 0 {
		m.setPending(false)
	}
}
