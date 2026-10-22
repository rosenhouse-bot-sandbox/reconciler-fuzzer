package interpose_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const (
	bucket1 = `{"id":"b-1","name":"logs","version":3}`
	bucket2 = `{"id":"b-2","name":"data","version":5}`
)

// cloud answers like a crud API: a create or get shows bucket1, and a list
// shows both buckets.
func cloud(reached *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/buckets" && r.Method == "GET":
			io.WriteString(w, `{"items":[`+bucket1+`,`+bucket2+`]}`)
		case r.URL.Path == "/buckets" && r.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, bucket1)
		case r.URL.Path == "/buckets/bad":
			io.WriteString(w, `{"name":"no id"}`)
		default:
			io.WriteString(w, bucket1)
		}
	})
}

type shown struct {
	mu   sync.Mutex
	list []interpose.Object
}

func (s *shown) add(o interpose.Object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, o)
}

func (s *shown) objects() []interpose.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]interpose.Object(nil), s.list...)
}

func listenCloud(t *testing.T, reached *atomic.Int32, configure ...func(*interpose.Options)) (*interpose.Listener, *events, *shown) {
	t.Helper()
	heard := &shown{}
	l, recorded := listen(t, cloud(reached), append([]func(*interpose.Options){func(o *interpose.Options) {
		m := crud.New("cloud")
		m.SetCollections([]string{"buckets"})
		o.System, o.API, o.Shown = "cloud", m, heard.add
	}}, configure...)...)
	return l, recorded, heard
}

func seeingAll(o *interpose.Options) { o.SeenAll = true }

func seeingBuckets(o *interpose.Options) {
	o.Seen = []schema.GroupResource{{Resource: "buckets"}}
}

func cloudRef(name string) trace.Ref {
	return trace.Ref{System: "cloud", Resource: "buckets", Name: name}
}

func cloudObject(id, rv, body string) interpose.Object {
	return interpose.Object{Ref: cloudRef(id), UID: "cloud/buckets/" + id, RV: rv, Body: json.RawMessage(body)}
}

func cloudSeen(id, rv string) trace.Seen {
	return trace.Seen{Caller: trace.Controller, Ref: cloudRef(id), UID: "cloud/buckets/" + id, RV: rv}
}

func postBucket(l *interpose.Listener) (*http.Response, error) {
	return http.Post(l.URL()+"/buckets", "application/json", strings.NewReader(`{"name":"logs"}`))
}

func TestAnAPIMapClassifiesACreateAndRecordsItsResult(t *testing.T) {
	var reached atomic.Int32
	l, recorded, _ := listenCloud(t, &reached)

	resp, err := postBucket(l)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	got := recorded.waitCalls(t, 1)[0]
	got.Latency = 0
	want := trace.Call{
		Caller: trace.Controller, Method: "POST", Path: "/buckets", Verb: "create", Ref: cloudRef(""),
		Status: 201, RequestName: "logs", ResultName: "b-1", ResultUID: "cloud/buckets/b-1", ResultRV: "3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Recorded %+v, want %+v.", got, want)
	}
	if string(body) != bucket1 {
		t.Errorf("The caller got %s, want %s.", body, bucket1)
	}
}

func TestAnAPIMapRecordsAnUnmappedRequest(t *testing.T) {
	var reached atomic.Int32
	l, recorded, heard := listenCloud(t, &reached, seeingAll)

	do(t, l, "GET", "/buckets/b-1/acl")

	got := recorded.waitCalls(t, 1)[0]
	if !got.Unmapped || got.Verb != "get" || got.Ref != (trace.Ref{System: "cloud"}) || got.Status != 200 {
		t.Errorf("Recorded %+v, want an unmapped get of no resource with status 200.", got)
	}
	if len(recorded.seen()) > 0 || len(heard.objects()) > 0 {
		t.Errorf("Recorded seen %+v and showed %+v, want neither.", recorded.seen(), heard.objects())
	}
}

func TestAnAPIMapShowsEveryObjectAndRecordsSeenOfItsKinds(t *testing.T) {
	both := []interpose.Object{cloudObject("b-1", "3", bucket1), cloudObject("b-2", "5", bucket2)}
	for _, c := range []struct {
		name      string
		configure func(*interpose.Options)
		seen      []trace.Seen
	}{
		{"SeenAll", seeingAll, []trace.Seen{cloudSeen("b-1", "3"), cloudSeen("b-2", "5")}},
		{"a Seen kind", seeingBuckets, []trace.Seen{cloudSeen("b-1", "3"), cloudSeen("b-2", "5")}},
		{"another Seen kind", seeingGadgets, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var reached atomic.Int32
			l, recorded, heard := listenCloud(t, &reached, c.configure)

			do(t, l, "GET", "/buckets")

			recorded.waitCalls(t, 1)
			if got := recorded.seen(); !reflect.DeepEqual(got, c.seen) {
				t.Errorf("Recorded %+v, want %+v.", got, c.seen)
			}
			if got := heard.objects(); !reflect.DeepEqual(got, both) {
				t.Errorf("Showed %+v, want %+v.", got, both)
			}
		})
	}
}

func TestAnAPIMapRecordsAnOpaqueSeenOfAnUnreadableBody(t *testing.T) {
	var reached atomic.Int32
	l, recorded, heard := listenCloud(t, &reached, seeingAll)

	do(t, l, "GET", "/buckets/bad")

	recorded.waitCalls(t, 1)
	want := []trace.Seen{{Caller: trace.Controller, Ref: cloudRef("bad"), Opaque: true}}
	if got := recorded.seen(); !reflect.DeepEqual(got, want) {
		t.Errorf("Recorded %+v, want %+v.", got, want)
	}
	if len(heard.objects()) > 0 {
		t.Errorf("Showed %+v, want nothing.", heard.objects())
	}
}

func TestAnAPIMapRecordsSeenBeforeTheCallerGetsTheObject(t *testing.T) {
	var reached atomic.Int32
	l, recorded, _ := listenCloud(t, &reached, seeingAll, slowSeen)

	do(t, l, "GET", "/buckets/b-1")

	if len(recorded.seen()) != 1 {
		t.Error("The caller got the object before its Seen was recorded.")
	}
}

func TestAnAPIMapAnswersAnErrorInItsOwnForm(t *testing.T) {
	var reached atomic.Int32
	l, recorded, _ := listenCloud(t, &reached)
	fault(t, l, plan.Fault{
		Match: plan.Match{Verb: "create", Resource: "buckets"},
		Error: &plan.Error{Code: 503, RetryAfter: plan.Duration(time.Second)},
	})

	do(t, l, "GET", "/buckets")
	resp, err := postBucket(l)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "1" || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Got status %d, Retry-After %q, Content-Type %q, want 503, 1, application/json.",
			resp.StatusCode, resp.Header.Get("Retry-After"), resp.Header.Get("Content-Type"))
	}
	var e struct {
		Error struct{ Code int } `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Code != 503 {
		t.Errorf("Got body %s, want a crud error of code 503.", body)
	}
	if reached.Load() != 1 {
		t.Errorf("The upstream saw %d requests, want only the list.", reached.Load())
	}
	if got := recorded.waitCalls(t, 2)[1]; got.Status != 503 || got.Injected != "error(503)" {
		t.Errorf("Recorded %+v, want status 503 injected by error(503).", got)
	}
}

func TestAnAPIMapRequestCanBeDelayed(t *testing.T) {
	var reached atomic.Int32
	l, recorded, _ := listenCloud(t, &reached)
	fault(t, l, plan.Fault{Match: plan.Match{Name: "b-1"}, Delay: duration(50 * time.Millisecond)})
	start := time.Now()

	do(t, l, "GET", "/buckets/b-1")

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("The request took %v, want at least the 50ms delay.", elapsed)
	}
	if got := recorded.waitCalls(t, 1)[0]; got.Status != 200 || got.Injected != "delay(50ms)" {
		t.Errorf("Recorded %+v, want status 200 injected by delay(50ms).", got)
	}
}

func TestAnAPIMapRequestCanBeDropped(t *testing.T) {
	var reached atomic.Int32
	l, recorded, _ := listenCloud(t, &reached)
	fault(t, l, plan.Fault{Match: plan.Match{Verb: "create"}, Drop: true})

	if resp, err := postBucket(l); err == nil {
		resp.Body.Close()
		t.Fatalf("Got status %d, want a transport error.", resp.StatusCode)
	}

	if got := recorded.waitCalls(t, 1)[0]; got.Status != 0 || got.Injected != "drop" || got.RequestName != "logs" {
		t.Errorf("Recorded %+v, want a create of logs with status 0 injected by drop.", got)
	}
	if reached.Load() != 0 {
		t.Errorf("The upstream saw %d requests, want none.", reached.Load())
	}
}

func TestAnAPIMapUnknownOutcomeShowsButRecordsNoSeen(t *testing.T) {
	for _, f := range []plan.Fault{{DropResponse: true}, {KillAfter: true}} {
		t.Run(f.Effect(), func(t *testing.T) {
			var reached atomic.Int32
			var killed atomic.Bool
			l, recorded, heard := listenCloud(t, &reached, seeingAll)
			l.SetKill(func(context.Context) error {
				killed.Store(true)
				return nil
			})
			fault(t, l, f)

			if resp, err := postBucket(l); err == nil {
				resp.Body.Close()
				t.Fatalf("Got status %d, want a transport error.", resp.StatusCode)
			}

			l.Close()
			calls := recorded.calls()
			if len(calls) != 1 || calls[0].Injected != f.Effect() || calls[0].Status != 201 || calls[0].ResultUID != "cloud/buckets/b-1" {
				t.Errorf("Recorded %+v, want the upstream's 201 and result, injected by %s.", calls, f.Effect())
			}
			if got := recorded.seen(); len(got) > 0 {
				t.Errorf("Recorded %+v, want no Seen.", got)
			}
			if got, want := heard.objects(), []interpose.Object{cloudObject("b-1", "3", bucket1)}; !reflect.DeepEqual(got, want) {
				t.Errorf("Showed %+v, want %+v.", got, want)
			}
			if killed.Load() != f.KillAfter {
				t.Errorf("Killed %v, want %v.", killed.Load(), f.KillAfter)
			}
		})
	}
}

// partial is the crud map with every object Partial and its header's ETag
// as RV.
type partial struct{ *crud.Map }

func (p partial) Objects(req interpose.Request, header http.Header, body []byte) []interpose.Object {
	objects := p.Map.Objects(req, header, body)
	for i := range objects {
		objects[i].Partial, objects[i].RV = true, header.Get("ETag")
	}
	return objects
}

func TestAPartialObjectGivesTheResultButNoSeen(t *testing.T) {
	var reached atomic.Int32
	l, recorded, heard := listenCloud(t, &reached, seeingAll, func(o *interpose.Options) {
		o.API = partial{o.API.(*crud.Map)}
	})

	resp, err := postBucket(l)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := recorded.waitCalls(t, 1)[0]
	if got.ResultUID != "cloud/buckets/b-1" {
		t.Errorf("Recorded result %q, want cloud/buckets/b-1.", got.ResultUID)
	}
	if len(recorded.seen()) > 0 {
		t.Errorf("Recorded seen %+v, want none of a partial object.", recorded.seen())
	}
	if objects := heard.objects(); len(objects) != 1 || !objects[0].Partial {
		t.Errorf("Showed %+v, want the partial object.", objects)
	}
}
