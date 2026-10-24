package model

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const SlowDeleteFinalizer = trace.SlowDeleteFinalizer

// Stage plays the controller of one models entry's kind. Its name and
// caller are trace.ModelCaller of the kind. DESIGN.md §7 says what it
// writes, and what stuck and slowDelete do.
type Stage struct {
	k      *kube.Kube
	m      config.Model
	gvr    schema.GroupVersionResource
	w      stageWriter
	record func(trace.Event)

	readyFields, stuckFields map[string]any

	// sweeping serializes sweeps, so a fault's start and end write in
	// order with the loop's writes.
	sweeping sync.Mutex

	mu       sync.Mutex
	objects  map[trace.Ref]*held
	faults   []*plan.Fault
	ended    int
	pending  bool
	sending  bool
	retrying bool
	wake     chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

var _ system.System = (*Stage)(nil)
var _ system.Stopper = (*Stage)(nil)

// held is what the stage knows of one object.
type held struct {
	ref     trace.Ref
	uid, rv string
	// generation was first heard at heardAt.
	generation int64
	heardAt    time.Time
	deleting   bool
	// deletingAt is when the stage first heard the object being deleted.
	deletingAt time.Time
	finalizers []string
	ready      *stageCondition
	// awaited is the resourceVersion a sent write made, or, after a
	// conflict, conflicted is the one it named.
	awaited, conflicted string
	timer               *time.Timer
	due                 time.Time
	// fresh is the message a fault's end writes.
	fresh string
}

type stageCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// stageOp writes finalizers, or else status, at resourceVersion rv.
type stageOp struct {
	ref        trace.Ref
	uid, rv    string
	finalizers []string
	status     map[string]any
}

type stageWriter interface {
	// patch merge-patches the object or its subresource and returns the
	// new resourceVersion.
	patch(ctx context.Context, ref trace.Ref, subresource string, patch []byte) (rv string, err error)
}

// NewStage returns the model of m. Start it after the Kubernetes system.
func NewStage(k *kube.Kube, m config.Model) (*Stage, error) {
	mapping, err := k.Mapper().RESTMapping(m.Kind.GroupKind(), m.Kind.Version)
	if err != nil {
		return nil, fmt.Errorf("mapping model kind %s: %w", m.Kind, err)
	}
	s := newStage(mapping.Resource, m, nil, nil)
	s.k = k
	return s, nil
}

func newStage(gvr schema.GroupVersionResource, m config.Model, w stageWriter, record func(trace.Event)) *Stage {
	stuck := m.StuckStatus
	if stuck == nil {
		stuck = m.Status
	}
	return &Stage{
		m: m, gvr: gvr, w: w, record: record,
		readyFields: replacing(m.Status, stuck), stuckFields: replacing(stuck, m.Status),
		objects: map[trace.Ref]*held{}, wake: make(chan struct{}, 1),
	}
}

// replacing returns a merge patch that sets fields in place of others, which
// an earlier write may have set. It removes each field of others that fields
// lacks, in nested objects too.
func replacing(fields, others map[string]any) map[string]any {
	patch := map[string]any{}
	for key := range others {
		if _, found := fields[key]; !found {
			patch[key] = nil
		}
	}
	for key, field := range fields {
		inner, isObject := field.(map[string]any)
		otherInner, otherIsObject := others[key].(map[string]any)
		if isObject && otherIsObject {
			field = replacing(inner, otherInner)
		}
		patch[key] = field
	}
	return patch
}

func (s *Stage) Name() string { return string(s.caller()) }

func (s *Stage) caller() trace.Caller { return trace.ModelCaller(s.gvr.Group, s.gvr.Resource) }

// Start opens the stage's listener and subscribes to the observer.
func (s *Stage) Start(ctx context.Context, env *system.Env) (system.Endpoints, error) {
	config, err := s.k.Listen(s.caller())
	if err != nil {
		return system.Endpoints{}, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return system.Endpoints{}, err
	}
	s.w, s.record = dynamicWriter{client.Resource(s.gvr)}, env.Record
	ctx, s.cancel = context.WithCancel(context.WithoutCancel(ctx))
	s.done = make(chan struct{})
	s.k.Subscribe(s.hear)
	go s.run(ctx)
	return system.Endpoints{}, nil
}

func (s *Stage) Snapshot(context.Context) ([]system.Resource, error) { return nil, nil }

func (s *Stage) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// Takes reports whether f is a model fault on this stage's kind.
func (s *Stage) Takes(f plan.Fault) bool {
	return f.ModelFault() && (f.System == "" || f.System == kube.Name) &&
		(f.Match.Group == "" || f.Match.Group == s.gvr.Group) && f.Match.Resource == s.gvr.Resource
}

// Fault starts stuck or slowDelete on the objects f matches, and returns
// once the stage has written them.
func (s *Stage) Fault(f plan.Fault) (end func(), err error) {
	if !s.Takes(f) {
		return nil, fmt.Errorf("%s takes no fault %s", s.Name(), f)
	}
	active := &f
	s.mu.Lock()
	s.faults = append(s.faults, active)
	s.mu.Unlock()
	s.sweep(context.Background())
	var once sync.Once
	return func() { once.Do(func() { s.end(active) }) }, nil
}

// end removes the fault, marks each object it matched for a fresh status,
// and sweeps, which removes finalizers before writing status.
func (s *Stage) end(f *plan.Fault) {
	s.mu.Lock()
	s.faults = slices.DeleteFunc(s.faults, func(other *plan.Fault) bool { return other == f })
	s.ended++
	for _, h := range s.objects {
		if matches(f, h) {
			h.fresh = fmt.Sprintf("fault %d ended", s.ended)
		}
	}
	s.mu.Unlock()
	s.sweep(context.Background())
}

// Stop ends the timers and sweeps.
func (s *Stage) Stop(context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.objects {
		h.stopTimer()
	}
	return nil
}

func (s *Stage) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
			s.sweep(ctx)
		}
	}
}

func matches(f *plan.Fault, h *held) bool { return f.Match.Name == "" || f.Match.Name == h.ref.Name }

func (s *Stage) active(h *held, stuck bool) bool {
	return slices.ContainsFunc(s.faults, func(f *plan.Fault) bool { return f.Stuck == stuck && f.SlowDelete == !stuck && matches(f, h) })
}

func (h *held) stopTimer() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

func (h *held) waiting() bool { return h.awaited != "" || h.conflicted != "" }

// hear holds v and queues a sweep when v gives the stage work.
func (s *Stage) hear(v trace.Version) {
	if v.Ref.Group != s.gvr.Group || v.Ref.Resource != s.gvr.Resource {
		return
	}
	var body struct {
		metav1.PartialObjectMetadata
		Status struct {
			Conditions []stageCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(v.Body, &body); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.objects[v.Ref]
	if v.Deleted || h != nil && h.uid != v.UID {
		if h != nil {
			h.stopTimer()
		}
		delete(s.objects, v.Ref)
		h = nil
	}
	if !v.Deleted {
		if h == nil {
			h = &held{ref: v.Ref, uid: v.UID}
			s.objects[v.Ref] = h
		}
		if body.Generation != h.generation || h.heardAt.IsZero() {
			h.generation, h.heardAt = body.Generation, time.Now()
		}
		h.rv, h.deleting, h.finalizers, h.ready = v.RV, body.DeletionTimestamp != nil, body.Finalizers, nil
		for _, c := range body.Status.Conditions {
			if c.Type == "Ready" {
				h.ready = &c
			}
		}
		if h.awaited != "" && observed(h.rv, h.awaited) || h.conflicted != "" && h.rv != h.conflicted {
			h.awaited, h.conflicted = "", ""
		}
		if ops := s.decide(h, time.Now()); len(ops) > 0 {
			s.queue()
			return
		}
	}
	s.setPending(s.busy())
}

// observed says whether a heard resourceVersion is, or follows, the awaited
// one. Kubernetes resourceVersions are opaque, but envtest's are integers.
func observed(heard, awaited string) bool {
	if heard == awaited {
		return true
	}
	h, errH := strconv.ParseUint(heard, 10, 64)
	a, errA := strconv.ParseUint(awaited, 10, 64)
	return errH == nil && errA == nil && h > a
}

// decide returns the writes h needs now, and schedules h's timer when its
// Ready write is due later. The caller holds s.mu.
func (s *Stage) decide(h *held, now time.Time) []stageOp {
	if h.waiting() {
		return nil
	}
	var ops []stageOp
	has := slices.Contains(h.finalizers, SlowDeleteFinalizer)
	switch hold := s.active(h, false); {
	case hold && !has && !h.deleting:
		ops = append(ops, stageOp{ref: h.ref, uid: h.uid, rv: h.rv, finalizers: append(slices.Clone(h.finalizers), SlowDeleteFinalizer)})
	case !hold && has:
		ops = append(ops, stageOp{ref: h.ref, uid: h.uid, rv: h.rv, finalizers: slices.DeleteFunc(slices.Clone(h.finalizers), func(f string) bool { return f == SlowDeleteFinalizer })})
	}
	h.stopTimer()
	if h.deleting {
		return ops
	}
	status := func(fields map[string]any, c metav1.Condition) stageOp {
		c.Type, c.ObservedGeneration, c.LastTransitionTime = "Ready", h.generation, metav1.NewTime(now)
		patch := maps.Clone(fields)
		patch["conditions"] = []any{c}
		return stageOp{ref: h.ref, uid: h.uid, rv: h.rv, status: patch}
	}
	switch {
	case s.active(h, true):
		h.fresh = ""
		if r := h.ready; r == nil || r.Status != "False" || r.Reason != trace.StuckReason || r.ObservedGeneration != h.generation {
			ops = append(ops, status(s.stuckFields, metav1.Condition{Status: metav1.ConditionFalse, Reason: trace.StuckReason, Message: "a stuck fault holds it"}))
		}
	case h.fresh != "":
		ops = append(ops, status(s.readyFields, metav1.Condition{Status: metav1.ConditionTrue, Reason: "Modelled", Message: h.fresh}))
	case h.ready != nil && h.ready.Status == "True" && h.ready.ObservedGeneration == h.generation:
	case !now.Before(h.heardAt.Add(s.m.After)):
		ops = append(ops, status(s.readyFields, metav1.Condition{Status: metav1.ConditionTrue, Reason: "Modelled", Message: "modelled ready"}))
	default:
		h.due = h.heardAt.Add(s.m.After)
		h.timer = time.AfterFunc(h.due.Sub(now), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.queue()
		})
	}
	return ops
}

// queue asks for a sweep. The caller holds s.mu.
func (s *Stage) queue() {
	s.setPending(true)
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// busy says whether the stage has a write scheduled, being sent or not yet
// observed. The caller holds s.mu.
func (s *Stage) busy() bool {
	if len(s.wake) > 0 || s.sending || s.retrying {
		return true
	}
	for _, h := range s.objects {
		if h.timer != nil || h.waiting() {
			return true
		}
	}
	return false
}

func (s *Stage) setPending(pending bool) {
	if pending != s.pending {
		s.pending = pending
		if s.record != nil {
			s.record(trace.Event{Work: &trace.Work{System: s.Name(), Pending: pending}})
		}
	}
}

// sweep sends every write the held objects need: finalizers first, then
// status.
func (s *Stage) sweep(ctx context.Context) {
	s.sweeping.Lock()
	defer s.sweeping.Unlock()
	s.mu.Lock()
	now := time.Now()
	var ops []stageOp
	for _, ref := range slices.SortedFunc(maps.Keys(s.objects), func(a, b trace.Ref) int {
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
	}) {
		ops = append(ops, s.decide(s.objects[ref], now)...)
	}
	slices.SortStableFunc(ops, func(a, b stageOp) int {
		return boolOrder(a.status != nil) - boolOrder(b.status != nil)
	})
	s.sending = len(ops) > 0
	s.setPending(s.busy())
	s.mu.Unlock()

	type result struct {
		op       stageOp
		rv       string
		conflict bool
	}
	var results []result
	failed := map[trace.Ref]bool{}
	rvs := map[trace.Ref]string{}
	retry := false
	for _, op := range ops {
		if failed[op.ref] || ctx.Err() != nil {
			continue
		}
		if rv, found := rvs[op.ref]; found {
			op.rv = rv
		}
		rv, err := s.w.patch(ctx, op.ref, subresource(op), op.patch())
		switch {
		case err == nil:
			rvs[op.ref] = rv
			results = append(results, result{op: op, rv: rv})
		case apierrors.IsConflict(err):
			results = append(results, result{op: op, conflict: true})
			failed[op.ref] = true
		case apierrors.IsNotFound(err):
			failed[op.ref] = true
		default:
			failed[op.ref], retry = true, true
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sending = false
	for _, r := range results {
		h := s.objects[r.op.ref]
		if h == nil || h.uid != r.op.uid {
			continue
		}
		switch {
		case r.conflict && h.rv == r.op.rv:
			h.awaited, h.conflicted = "", r.op.rv
		case r.conflict:
			// The newer version's hear queued a sweep.
		case !observed(h.rv, r.rv):
			h.awaited, h.conflicted = r.rv, ""
		}
		if r.op.status != nil && !r.conflict {
			h.fresh = ""
		}
	}
	if retry && ctx.Err() == nil && !s.retrying {
		s.retrying = true
		time.AfterFunc(retryDelay, func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.retrying = false
			if ctx.Err() == nil {
				s.queue()
			}
		})
	}
	s.setPending(s.busy())
}

func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

func subresource(op stageOp) string {
	if op.status != nil {
		return "status"
	}
	return ""
}

func (op stageOp) patch() []byte {
	metadata := map[string]any{"uid": op.uid, "resourceVersion": op.rv}
	patch := map[string]any{"metadata": metadata}
	if op.status != nil {
		patch["status"] = op.status
	} else {
		metadata["finalizers"] = append([]string{}, op.finalizers...)
	}
	raw, _ := json.Marshal(patch)
	return raw
}

type dynamicWriter struct {
	resource dynamic.NamespaceableResourceInterface
}

func (d dynamicWriter) patch(ctx context.Context, ref trace.Ref, subresource string, patch []byte) (string, error) {
	var subresources []string
	if subresource != "" {
		subresources = []string{subresource}
	}
	patched, err := d.resource.Namespace(ref.Namespace).Patch(ctx, ref.Name, types.MergePatchType, patch, metav1.PatchOptions{}, subresources...)
	if err != nil {
		return "", err
	}
	return patched.GetResourceVersion(), nil
}
