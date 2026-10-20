package model

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/kube"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

type fakeWriter struct {
	mu      sync.Mutex
	applied []op
	err     error
}

func (f *fakeWriter) apply(_ context.Context, o op) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, o)
	return f.err
}

type works struct {
	mu   sync.Mutex
	list []bool
}

func (w *works) record(e trace.Event) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if e.Work != nil && e.Work.System == "gc" {
		w.list = append(w.list, e.Work.Pending)
	}
}

func (w *works) assert(t *testing.T, want ...bool) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !reflect.DeepEqual(w.list, want) && !(len(w.list) == 0 && len(want) == 0) {
		t.Errorf("work events: got %v, want %v", w.list, want)
	}
}

// testModel deletes every object named "doomed".
func testModel(w *fakeWriter) (*model, *works) {
	recorded := &works{}
	m := newModel("gc", trace.GC, func(_ context.Context, objects []object, _ cluster) ([]op, bool) {
		var ops []op
		for _, o := range objects {
			if o.Name == "doomed" {
				ops = append(ops, deletion(o))
			}
		}
		return ops, false
	}, func(o object, _ bool, _ map[trace.Ref]object) bool { return o.Name != "quiet" })
	m.record, m.w = recorded.record, w
	return m, recorded
}

func version(o object, deleted bool) trace.Version {
	body, _ := json.Marshal(o.PartialObjectMetadata)
	return trace.Version{Ref: o.ref, UID: string(o.UID), RV: o.ResourceVersion, Deleted: deleted, Body: body}
}

func TestModelRecordsWorkOnlyOnTransitions(t *testing.T) {
	m, recorded := testModel(&fakeWriter{})
	m.hear(version(configMap("quiet", "q"), false))
	recorded.assert(t)
	m.hear(version(configMap("a", "a"), false))
	m.hear(version(configMap("b", "b"), false))
	recorded.assert(t, true)
	if !m.Pending() {
		t.Error("Pending is false after a waking version.")
	}
	step(m)
	recorded.assert(t, true, false)
	if m.Pending() {
		t.Error("Pending is true after an idle sweep.")
	}
}

func TestModelStaysPendingUntilItsWriteIsObserved(t *testing.T) {
	w := &fakeWriter{}
	m, recorded := testModel(w)
	doomed := configMap("doomed", "d")
	m.hear(version(doomed, false))
	step(m)
	step(m)
	if len(w.applied) != 1 {
		t.Errorf("applied %d ops before the write was observed, want 1", len(w.applied))
	}
	recorded.assert(t, true)
	m.hear(version(doomed, true))
	step(m)
	recorded.assert(t, true, false)
}

func TestModelRetriesAFailedWrite(t *testing.T) {
	w := &fakeWriter{err: errors.New("unreachable")}
	m, recorded := testModel(w)
	m.hear(version(configMap("doomed", "d"), false))
	step(m)
	recorded.assert(t, true)
	select {
	case <-m.wake:
		t.Fatal("the retry came before the retry delay.")
	case <-time.After(retryDelay / 2):
	}
	select {
	case <-m.wake:
	case <-time.After(retryDelay):
		t.Fatal("no retry came.")
	}
	w.err = nil
	step(m)
	if len(w.applied) != 2 {
		t.Errorf("applied %d ops, want 2", len(w.applied))
	}
}

func TestModelsAreSystems(t *testing.T) {
	models := New(kube.New(kube.Options{}))
	var names []string
	for _, m := range models {
		names = append(names, m.Name())
		if _, ok := m.(system.Stopper); !ok {
			t.Errorf("%s is no Stopper.", m.Name())
		}
		if _, err := m.Fault(plan.Fault{}); !errors.Is(err, kube.ErrUnsupported) {
			t.Errorf("%s Fault: got %v, want ErrUnsupported", m.Name(), err)
		}
		if snapshot, err := m.Snapshot(context.Background()); snapshot != nil || err != nil {
			t.Errorf("%s Snapshot: got %v, %v", m.Name(), snapshot, err)
		}
	}
	if !reflect.DeepEqual(names, []string{"gc", "namespace"}) {
		t.Errorf("names: got %v", names)
	}
}

// step sweeps as the model's loop does.
func step(m *model) {
	select {
	case <-m.wake:
	default:
	}
	m.sweep(context.Background())
}
