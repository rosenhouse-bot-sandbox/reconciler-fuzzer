package trace_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestRecorderWritesWhatLoadReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := trace.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { r.Record(trace.Event{Proc: &trace.Proc{Event: trace.Start, PID: 7}}) })
	}
	wg.Wait()
	time.Sleep(time.Millisecond)
	r.Record(trace.Event{Phase: &trace.Phase{Step: 1, Name: trace.EndFaults}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := trace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 51 {
		t.Fatalf("loaded %d events, want 51", len(loaded))
	}
	for i, e := range loaded {
		if e.Seq != uint64(i+1) {
			t.Fatalf("event %d has Seq %d", i, e.Seq)
		}
		if i > 0 && e.At < loaded[i-1].At {
			t.Fatalf("event %d is at %v, before %v", i, e.At, loaded[i-1].At)
		}
	}
	if last := loaded[50]; last.Phase == nil || last.Phase.Step != 1 || last.At < time.Millisecond {
		t.Fatalf("last event = %+v", last)
	}
	if got := r.Events(); len(got) != 51 || got[50].Seq != 51 {
		t.Fatalf("Events() holds %d events", len(got))
	}
}

func TestEventsReturnsACopy(t *testing.T) {
	r, err := trace.NewRecorder(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Record(trace.Event{Proc: &trace.Proc{Event: trace.Start}})
	r.Events()[0].Seq = 99
	if r.Events()[0].Seq != 1 {
		t.Fatal("Events shares its slice")
	}
}

func TestLoadNamesTheMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	data := `{"seq":1,"at":0,"proc":{"event":"start"}}` + "\n" + "{oops\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := trace.Load(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("err = %v, want it to name line 2", err)
	}
}

func TestEventsSince(t *testing.T) {
	r, err := trace.NewRecorder(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for pid := range 3 {
		r.Record(trace.Event{Proc: &trace.Proc{Event: trace.Start, PID: pid}})
	}
	if got := r.EventsSince(1); len(got) != 2 || got[0].Proc.PID != 1 {
		t.Fatalf("EventsSince(1) = %+v", got)
	}
	if got := r.EventsSince(5); len(got) != 0 {
		t.Fatalf("EventsSince(5) = %+v", got)
	}
}

func TestLoadReadsWatchEndAndWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	r, err := trace.NewRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Record(trace.Event{Call: &trace.Call{Caller: trace.Controller, Watch: true, Stream: 3}})
	r.Record(trace.Event{WatchEnd: &trace.WatchEnd{Caller: trace.Controller, System: "kube", Stream: 3}})
	r.Record(trace.Event{Work: &trace.Work{System: "gc", Pending: true}})
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := trace.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 || loaded[0].Call.Stream != 3 || *loaded[1].WatchEnd != (trace.WatchEnd{Caller: trace.Controller, System: "kube", Stream: 3}) ||
		*loaded[2].Work != (trace.Work{System: "gc", Pending: true}) {
		t.Fatalf("loaded %+v", loaded)
	}
}
