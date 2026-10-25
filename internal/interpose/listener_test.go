package interpose_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

type events struct {
	mu   sync.Mutex
	list []trace.Event
}

func (e *events) record(ev trace.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
}

func (e *events) calls() []trace.Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var calls []trace.Call
	for _, ev := range e.list {
		if ev.Call != nil {
			calls = append(calls, *ev.Call)
		}
	}
	return calls
}

// waitCalls waits until n calls are recorded.
func (e *events) waitCalls(t *testing.T, n int) []trace.Call {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if calls := e.calls(); len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("Recorded %d calls, want %d.", len(e.calls()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func listen(t *testing.T, upstream http.Handler, configure ...func(*interpose.Options)) (*interpose.Listener, *events) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	recorded := &events{}
	o := interpose.Options{System: "kube", Caller: trace.Controller, Upstream: u, Record: recorded.record}
	for _, c := range configure {
		c(&o)
	}
	l, err := interpose.Listen(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, recorded
}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
}

// counting counts the requests that reach it.
func counting(n *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) })
}

func do(t *testing.T, l *interpose.Listener, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, l.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func fault(t *testing.T, l *interpose.Listener, f plan.Fault) func() {
	t.Helper()
	end, err := l.Fault(f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	return end
}

func duration(d time.Duration) *plan.Duration {
	p := plan.Duration(d)
	return &p
}

func TestRecordsACall(t *testing.T) {
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusNotFound)
	}))

	do(t, l, "GET", "/apis/apps/v1/namespaces/ns1/deployments/d1/status")

	got := recorded.waitCalls(t, 1)[0]
	if got.Latency < 5*time.Millisecond {
		t.Errorf("Recorded latency %v, want at least the upstream's 5ms.", got.Latency)
	}
	got.Latency = 0
	want := trace.Call{
		Caller: trace.Controller, Method: "GET", Path: "/apis/apps/v1/namespaces/ns1/deployments/d1/status",
		Verb: "get", APIVersion: "v1", Subresource: "status", Status: http.StatusNotFound,
		Ref: trace.Ref{System: "kube", Group: "apps", Resource: "deployments", Namespace: "ns1", Name: "d1"},
	}
	if got != want {
		t.Errorf("Recorded\n\t%+v\nwant\n\t%+v", got, want)
	}
}

func TestDropsTheAuthorizationHeader(t *testing.T) {
	var seen atomic.Value
	l, _ := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("Impersonate-User"))
	}))
	req, _ := http.NewRequest("GET", l.URL()+"/api/v1/pods", nil)
	req.Header.Set("Authorization", "Bearer caller")
	req.Header.Set("Impersonate-User", "someone")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := seen.Load(); got != "|someone" {
		t.Errorf("The upstream saw Authorization|Impersonate-User %q, want %q.", got, "|someone")
	}
}

func TestUsesTheTransport(t *testing.T) {
	var seen atomic.Value
	l, _ := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
	}), func(o *interpose.Options) {
		o.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
			r.Header.Set("Authorization", "Bearer listener")
			return http.DefaultTransport.RoundTrip(r)
		})
	})

	do(t, l, "GET", "/api/v1/pods")

	if got := seen.Load(); got != "Bearer listener" {
		t.Errorf("The upstream saw Authorization %q, want the transport's.", got)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAnUnreachableUpstreamIsABadGateway(t *testing.T) {
	recorded := &events{}
	l, err := interpose.Listen(interpose.Options{
		Upstream: &url.URL{Scheme: "http", Host: "127.0.0.1:1"},
		Record:   recorded.record,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	if resp := do(t, l, "GET", "/api/v1/pods"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("Got status %d, want 502.", resp.StatusCode)
	}
	if got := recorded.waitCalls(t, 1)[0].Status; got != http.StatusBadGateway {
		t.Errorf("Recorded status %d, want 502.", got)
	}
}

func TestAWatchStreamsAndIsRecordedWhenItOpens(t *testing.T) {
	release := make(chan struct{})
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"type\":\"ADDED\"}\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer close(release)

	resp, err := http.Get(l.URL() + "/api/v1/namespaces/ns1/configmaps?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "{\"type\":\"ADDED\"}\n" {
		t.Fatalf("Read %q, %v before the upstream finished, want the first frame.", line, err)
	}
	got := recorded.waitCalls(t, 1)[0]
	if !got.Watch || got.Verb != "watch" || got.Status != http.StatusOK {
		t.Errorf("Recorded %+v, want an open watch with status 200.", got)
	}
}

const createdBody = `{"kind":"ConfigMap","metadata":{"name":"cm-x7k2p","uid":"u-1","resourceVersion":"42"}}`

func respond(contentType, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(body))
	})
}

func TestACreateRecordsTheResultingObject(t *testing.T) {
	l, recorded := listen(t, respond("application/json", createdBody))

	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")

	got := recorded.waitCalls(t, 1)[0]
	if got.ResultName != "cm-x7k2p" || got.ResultUID != "u-1" || got.ResultRV != "42" {
		t.Errorf("Recorded name %q, uid %q, rv %q, want cm-x7k2p, u-1, 42.", got.ResultName, got.ResultUID, got.ResultRV)
	}
}

func TestAProtobufCreateRecordsTheResultingObject(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm-x7k2p", UID: "u-1", ResourceVersion: "42"}, Data: map[string]string{"k": "v"}}
	raw, err := cm.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := (&runtime.Unknown{TypeMeta: runtime.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, Raw: raw}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	l, recorded := listen(t, respond("application/vnd.kubernetes.protobuf", "k8s\x00"+string(envelope)))

	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")

	got := recorded.waitCalls(t, 1)[0]
	if got.ResultName != "cm-x7k2p" || got.ResultUID != "u-1" || got.ResultRV != "42" {
		t.Errorf("Recorded name %q, uid %q, rv %q, want cm-x7k2p, u-1, 42.", got.ResultName, got.ResultUID, got.ResultRV)
	}
}

func TestAResponseOfAnotherTypeRecordsNoResult(t *testing.T) {
	for _, c := range []struct{ name, verb, contentType string }{
		{"protobuf without its magic", "POST", "application/vnd.kubernetes.protobuf"},
		{"yaml", "POST", "application/yaml"},
		{"a read", "GET", "application/json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, recorded := listen(t, respond(c.contentType, createdBody))

			do(t, l, c.verb, "/api/v1/namespaces/ns1/configmaps")

			got := recorded.waitCalls(t, 1)[0]
			if got.ResultName != "" || got.ResultUID != "" || got.ResultRV != "" {
				t.Errorf("Recorded name %q, uid %q, rv %q, want none.", got.ResultName, got.ResultUID, got.ResultRV)
			}
		})
	}
}

func TestAWriteRecordsTheFinalizersOfItsAnswer(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"some", `{"kind":"ConfigMap","metadata":{"name":"cm","uid":"u-1","resourceVersion":"42","finalizers":["a","b"]}}`, `["a","b"]`},
		{"none", createdBody, `[]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, recorded := listen(t, respond("application/json", c.body))

			do(t, l, "PUT", "/api/v1/namespaces/ns1/configmaps/cm")

			if got, _ := json.Marshal(recorded.waitCalls(t, 1)[0].ResultFinalizers); string(got) != c.want {
				t.Errorf("Recorded finalizers %s, want %s.", got, c.want)
			}
		})
	}
}

func TestADryRunOrAnAnswerWithoutAnObjectRecordsNoFinalizers(t *testing.T) {
	for _, c := range []struct{ name, method, path, body string }{
		{"a dry run", "PUT", "/api/v1/namespaces/ns1/configmaps/cm?dryRun=All", createdBody},
		{"a status", "DELETE", "/api/v1/namespaces/ns1/configmaps/cm", `{"kind":"Status","status":"Success"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, recorded := listen(t, respond("application/json", c.body))

			do(t, l, c.method, c.path)

			if got := recorded.waitCalls(t, 1)[0].ResultFinalizers; got != nil {
				t.Errorf("Recorded finalizers %q, want none.", *got)
			}
		})
	}
}

func TestPassesUpgradeRequestsThroughUnrecorded(t *testing.T) {
	var reached atomic.Int32
	l, recorded := listen(t, counting(&reached))
	req, _ := http.NewRequest("GET", l.URL()+"/api/v1/namespaces/ns1/pods/p1/exec", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "SPDY/3.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if reached.Load() != 1 {
		t.Errorf("The upstream saw %d requests, want 1.", reached.Load())
	}
	do(t, l, "GET", "/api/v1/pods")
	if calls := recorded.waitCalls(t, 1); len(calls) != 1 || calls[0].Path != "/api/v1/pods" {
		t.Errorf("Recorded %+v, want only the plain request.", calls)
	}
}

func TestNthPicksOnlyThatMatchingRequest(t *testing.T) {
	l, recorded := listen(t, ok())
	fault(t, l, plan.Fault{Match: plan.Match{Verb: "create"}, Nth: 2, Error: &plan.Error{Code: 500}})

	do(t, l, "GET", "/api/v1/namespaces/ns1/configmaps")
	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")
	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")
	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")

	var statuses []int
	for _, c := range recorded.waitCalls(t, 4) {
		statuses = append(statuses, c.Status)
	}
	if want := []int{200, 200, 500, 200}; !slices.Equal(statuses, want) {
		t.Errorf("Recorded statuses %v, want %v.", statuses, want)
	}
}

func TestNthCountsFromTheFaultsStart(t *testing.T) {
	l, recorded := listen(t, ok())
	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")
	fault(t, l, plan.Fault{Nth: 1, Error: &plan.Error{Code: 500}})

	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")

	if got := recorded.waitCalls(t, 2)[1].Status; got != 500 {
		t.Errorf("Recorded status %d, want 500 for the first request after the fault started.", got)
	}
}

func TestNthZeroPicksEveryMatchingRequest(t *testing.T) {
	l, recorded := listen(t, ok())
	fault(t, l, plan.Fault{Match: plan.Match{Resource: "configmaps"}, Error: &plan.Error{Code: 503}})

	do(t, l, "GET", "/api/v1/namespaces/ns1/configmaps")
	do(t, l, "GET", "/api/v1/namespaces/ns1/secrets")
	do(t, l, "POST", "/api/v1/namespaces/ns1/configmaps")

	var statuses []int
	var injected []string
	for _, c := range recorded.waitCalls(t, 3) {
		statuses = append(statuses, c.Status)
		injected = append(injected, c.Injected)
	}
	if want := []int{503, 200, 503}; !slices.Equal(statuses, want) {
		t.Errorf("Recorded statuses %v, want %v.", statuses, want)
	}
	if want := []string{"error(503)", "", "error(503)"}; !slices.Equal(injected, want) {
		t.Errorf("Recorded injected %q, want %q.", injected, want)
	}
}

func TestTheFirstFaultThatPicksARequestApplies(t *testing.T) {
	l, recorded := listen(t, ok())
	fault(t, l, plan.Fault{Nth: 2, Error: &plan.Error{Code: 500}})
	fault(t, l, plan.Fault{Error: &plan.Error{Code: 503}})

	do(t, l, "GET", "/api/v1/pods")
	do(t, l, "GET", "/api/v1/pods")

	var statuses []int
	for _, c := range recorded.waitCalls(t, 2) {
		statuses = append(statuses, c.Status)
	}
	if want := []int{503, 500}; !slices.Equal(statuses, want) {
		t.Errorf("Recorded statuses %v, want %v.", statuses, want)
	}
}

func TestAnErrorNeverReachesTheUpstream(t *testing.T) {
	var reached atomic.Int32
	l, recorded := listen(t, counting(&reached))
	fault(t, l, plan.Fault{Error: &plan.Error{Code: 429, RetryAfter: plan.Duration(1500 * time.Millisecond)}})

	resp, err := http.Post(l.URL()+"/api/v1/namespaces/ns1/configmaps", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if reached.Load() != 0 {
		t.Errorf("The upstream saw %d requests, want none.", reached.Load())
	}
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "2" {
		t.Errorf("Got status %d with Retry-After %q, want 429 with 2.", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if !strings.Contains(string(body), `"kind":"Status"`) || !strings.Contains(string(body), `"code":429`) {
		t.Errorf("Got body %s, want a Status of code 429.", body)
	}
	if got := recorded.waitCalls(t, 1)[0]; got.Status != 429 || got.Injected != "error(429)" {
		t.Errorf("Recorded %+v, want status 429 injected by error(429).", got)
	}
}

func TestAnErrorWithoutRetryAfterSetsNoHeader(t *testing.T) {
	l, _ := listen(t, ok())
	fault(t, l, plan.Fault{Error: &plan.Error{Code: 500}})

	if resp := do(t, l, "GET", "/api/v1/pods"); resp.Header.Get("Retry-After") != "" {
		t.Errorf("Got Retry-After %q, want none.", resp.Header.Get("Retry-After"))
	}
}

func TestADelayHoldsTheRequest(t *testing.T) {
	l, recorded := listen(t, ok())
	fault(t, l, plan.Fault{Delay: duration(50 * time.Millisecond)})
	start := time.Now()

	do(t, l, "GET", "/api/v1/pods")

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("The request took %v, want at least the 50ms delay.", elapsed)
	}
	if got := recorded.waitCalls(t, 1)[0]; got.Status != 200 || got.Injected != "delay(50ms)" {
		t.Errorf("Recorded %+v, want status 200 injected by delay(50ms).", got)
	}
}

func TestEndingADelayReleasesItsRequests(t *testing.T) {
	var reached atomic.Int32
	l, _ := listen(t, counting(&reached))
	end := fault(t, l, plan.Fault{Delay: duration(time.Hour)})
	done := make(chan struct{})
	go func() {
		defer close(done)
		do(t, l, "GET", "/api/v1/pods")
	}()
	time.Sleep(20 * time.Millisecond)
	if reached.Load() != 0 {
		t.Fatal("The upstream saw the request before the delay ended.")
	}

	end()
	end()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Ending the fault did not release the request.")
	}
	if reached.Load() != 1 {
		t.Errorf("The upstream saw %d requests, want 1.", reached.Load())
	}
	do(t, l, "GET", "/api/v1/pods")
}

func TestAnEndedFaultPicksNothing(t *testing.T) {
	l, recorded := listen(t, ok())
	end := fault(t, l, plan.Fault{Error: &plan.Error{Code: 500}})
	end()

	do(t, l, "GET", "/api/v1/pods")

	if got := recorded.waitCalls(t, 1)[0]; got.Status != 200 || got.Injected != "" {
		t.Errorf("Recorded %+v, want an untouched 200.", got)
	}
}

func TestAWriteWhoseClientLeftRecordsTheServersAnswer(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	created := respond("application/json", createdBody)
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		created.ServeHTTP(w, r)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", l.URL()+"/api/v1/namespaces/ns1/configmaps", nil)
	go func() {
		<-arrived
		cancel()
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("The request succeeded, want it canceled.")
	}

	if got := recorded.waitCalls(t, 1)[0]; got.Status != http.StatusCreated || got.ResultUID != "u-1" {
		t.Errorf("Recorded status %d, uid %q, want 201, u-1.", got.Status, got.ResultUID)
	}
}

func TestADropResponseWhoseClientLeftRecordsTheServersAnswer(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	created := respond("application/json", createdBody)
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		created.ServeHTTP(w, r)
	}))
	fault(t, l, plan.Fault{DropResponse: true})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", l.URL()+"/api/v1/namespaces/ns1/configmaps", nil)
	go func() {
		<-arrived
		cancel()
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("The request succeeded, want it canceled.")
	}

	if got := recorded.waitCalls(t, 1)[0]; got.Status != http.StatusCreated || got.ResultUID != "u-1" || !got.Unknown() {
		t.Errorf("Recorded %+v, want the unknown-outcome 201 with uid u-1.", got)
	}
}

func TestADelayEndsWhenTheClientGoesAway(t *testing.T) {
	var reached atomic.Int32
	l, recorded := listen(t, counting(&reached))
	fault(t, l, plan.Fault{Delay: duration(time.Hour)})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", l.URL()+"/api/v1/pods", nil)

	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("The request succeeded, want it canceled.")
	}

	if got := recorded.waitCalls(t, 1)[0]; got.Status != 0 {
		t.Errorf("Recorded status %d, want 0.", got.Status)
	}
	if reached.Load() != 0 {
		t.Errorf("The upstream saw %d requests, want none.", reached.Load())
	}
}

func TestADropAbortsTheConnection(t *testing.T) {
	var reached atomic.Int32
	l, recorded := listen(t, counting(&reached))
	fault(t, l, plan.Fault{Drop: true})

	if resp, err := http.Get(l.URL() + "/api/v1/pods"); err == nil {
		resp.Body.Close()
		t.Fatalf("Got status %d, want a transport error.", resp.StatusCode)
	}

	if got := recorded.waitCalls(t, 1)[0]; got.Status != 0 || got.Injected != "drop" {
		t.Errorf("Recorded %+v, want status 0 injected by drop.", got)
	}
	if reached.Load() != 0 {
		t.Errorf("The upstream saw %d requests, want none.", reached.Load())
	}
}

func TestFaultValidates(t *testing.T) {
	l, _ := listen(t, ok())
	for _, f := range []plan.Fault{
		{},
		{Error: &plan.Error{}},
		{Error: &plan.Error{Code: 500}, Drop: true},
		{Error: &plan.Error{Code: 500}, Delay: duration(time.Second)},
		{Delay: duration(time.Second), Drop: true},
		{Drop: true, DropResponse: true},
		{DropResponse: true, KillAfter: true},
	} {
		if _, err := l.Fault(f); err == nil {
			t.Errorf("Fault(%+v) succeeded, want an error.", f)
		}
	}
}

func TestOnCallHearsTheCallAfterRecord(t *testing.T) {
	var order []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	heard := make(chan trace.Call, 1)
	l, _ := listen(t, ok(), func(o *interpose.Options) {
		o.Record = func(e trace.Event) { note("record " + e.Call.Path) }
		o.OnCall = func(c trace.Call) { note("call " + c.Path); heard <- c }
	})

	do(t, l, "GET", "/api/v1/pods")

	select {
	case <-heard:
	case <-time.After(5 * time.Second):
		t.Fatal("OnCall never heard the call.")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"record /api/v1/pods", "call /api/v1/pods"}; !slices.Equal(order, want) {
		t.Errorf("Heard %q, want %q.", order, want)
	}
}

func TestCloseWaitsForInFlightRequests(t *testing.T) {
	entered := make(chan struct{})
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	go http.Get(l.URL() + "/api/v1/pods")
	<-entered

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	if calls := recorded.calls(); len(calls) != 1 {
		t.Errorf("Close returned with %d calls recorded, want the in-flight one.", len(calls))
	}
	if _, err := http.Get(l.URL() + "/api/v1/pods"); err == nil {
		t.Error("The listener answered after Close.")
	}
}

func TestWatchesGetStreamsAndRecordTheirEnd(t *testing.T) {
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}\n"))
	}))

	do(t, l, "GET", "/api/v1/configmaps?watch=true")
	do(t, l, "GET", "/api/v1/configmaps")
	do(t, l, "GET", "/api/v1/namespaces/ns1/configmaps?watch=true")

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	recorded.mu.Lock()
	defer recorded.mu.Unlock()
	var got []string
	for _, e := range recorded.list {
		switch {
		case e.Call != nil:
			got = append(got, fmt.Sprintf("call %s stream %d", e.Call.Verb, e.Call.Stream))
		case e.WatchEnd != nil:
			got = append(got, fmt.Sprintf("end %s %s stream %d", e.WatchEnd.Caller, e.WatchEnd.System, e.WatchEnd.Stream))
		}
	}
	want := []string{"call watch stream 1", "end controller kube stream 1", "call list stream 0", "call watch stream 2", "end controller kube stream 2"}
	if !slices.Equal(got, want) {
		t.Errorf("Recorded %q, want %q.", got, want)
	}
}

func TestADropResponseCommitsAndAbortsTheConnection(t *testing.T) {
	var reached atomic.Int32
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		respond("application/json", createdBody).ServeHTTP(w, r)
	}))
	fault(t, l, plan.Fault{DropResponse: true})

	if resp, err := http.Post(l.URL()+"/api/v1/namespaces/ns1/configmaps", "application/json", nil); err == nil {
		resp.Body.Close()
		t.Fatalf("Got status %d, want a transport error.", resp.StatusCode)
	}

	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	calls := recorded.calls()
	if len(calls) != 1 {
		t.Fatalf("Recorded %d calls, want 1.", len(calls))
	}
	if got := calls[0]; got.Status != http.StatusCreated || got.Injected != trace.DropResponse || !got.Unknown() ||
		got.ResultName != "cm-x7k2p" || got.ResultUID != "u-1" || got.ResultRV != "42" {
		t.Errorf("Recorded %+v, want the upstream's 201 and result, injected by dropResponse.", got)
	}
	if reached.Load() != 1 {
		t.Errorf("The upstream saw %d requests, want 1.", reached.Load())
	}
}

func TestAKillAfterKillsAfterRecordingAndBeforeAborting(t *testing.T) {
	l, recorded := listen(t, respond("application/json", createdBody))
	var killed atomic.Bool
	l.SetKill(func(context.Context) error {
		if calls := recorded.calls(); len(calls) != 1 || calls[0].Injected != trace.KillAfter || calls[0].ResultUID != "u-1" {
			t.Errorf("At the kill, recorded %+v, want the killAfter call with its result.", calls)
		}
		time.Sleep(20 * time.Millisecond)
		killed.Store(true)
		return nil
	})
	fault(t, l, plan.Fault{KillAfter: true})

	if resp, err := http.Post(l.URL()+"/api/v1/namespaces/ns1/configmaps", "application/json", nil); err == nil {
		resp.Body.Close()
		t.Fatalf("Got status %d, want a transport error.", resp.StatusCode)
	}
	if !killed.Load() {
		t.Error("The client saw the abort before the kill returned.")
	}
}

func TestUnknownOutcomeFaultsSkipWatches(t *testing.T) {
	for _, f := range []plan.Fault{{DropResponse: true, Nth: 1}, {KillAfter: true, Nth: 1}} {
		t.Run(f.Effect(), func(t *testing.T) {
			l, _ := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}\n")) }))
			l.SetKill(func(context.Context) error { return nil })
			fault(t, l, f)

			do(t, l, "GET", "/api/v1/configmaps?watch=true")
			// A reused connection would make the client retry the list.
			fresh := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			if resp, err := fresh.Get(l.URL() + "/api/v1/configmaps"); err == nil {
				resp.Body.Close()
				t.Fatalf("The list after the watch got status %d, want a transport error.", resp.StatusCode)
			}
		})
	}
}

var gadgets = schema.GroupResource{Group: "toy.reconciler-fuzzer", Resource: "gadgets"}

func seeingGadgets(o *interpose.Options) { o.Seen = []schema.GroupResource{gadgets} }

func (e *events) seen() []trace.Seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	var seen []trace.Seen
	for _, ev := range e.list {
		if ev.Seen != nil {
			seen = append(seen, *ev.Seen)
		}
	}
	return seen
}

func gadgetJSON(name, uid, rv string) string {
	return fmt.Sprintf(`{"kind":"Gadget","metadata":{"name":%q,"namespace":"ns1","uid":%q,"resourceVersion":%q}}`, name, uid, rv)
}

func seenGadget(name, uid, rv string) trace.Seen {
	return trace.Seen{
		Caller: trace.Controller, UID: uid, RV: rv,
		Ref: trace.Ref{System: "kube", Group: gadgets.Group, Resource: gadgets.Resource, Namespace: "ns1", Name: name},
	}
}

func respondOK(contentType, encoding, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if encoding != "" {
			w.Header().Set("Content-Encoding", encoding)
		}
		if encoding == "gzip" {
			var zipped bytes.Buffer
			z := gzip.NewWriter(&zipped)
			z.Write([]byte(body))
			z.Close()
			body = zipped.String()
		}
		w.Write([]byte(body))
	})
}

const gadgetsPath = "/apis/toy.reconciler-fuzzer/v1/namespaces/ns1/gadgets"

func TestAGetOfASeenKindRecordsSeen(t *testing.T) {
	for _, encoding := range []string{"", "gzip"} {
		t.Run("encoding "+encoding, func(t *testing.T) {
			l, recorded := listen(t, respondOK("application/json", encoding, gadgetJSON("g1", "u1", "5")), seeingGadgets)

			do(t, l, "GET", gadgetsPath+"/g1")

			recorded.waitCalls(t, 1)
			if got, want := recorded.seen(), []trace.Seen{seenGadget("g1", "u1", "5")}; !reflect.DeepEqual(got, want) {
				t.Errorf("Recorded %+v, want %+v.", got, want)
			}
		})
	}
}

func TestAListOfASeenKindRecordsSeenForEachItem(t *testing.T) {
	body := `{"kind":"GadgetList","metadata":{"resourceVersion":"9"},"items":[` + gadgetJSON("g1", "u1", "5") + "," + gadgetJSON("g2", "u2", "6") + "]}"
	l, recorded := listen(t, respondOK("application/json", "", body), seeingGadgets)

	do(t, l, "GET", gadgetsPath)

	recorded.waitCalls(t, 1)
	want := []trace.Seen{seenGadget("g1", "u1", "5"), seenGadget("g2", "u2", "6")}
	if got := recorded.seen(); !reflect.DeepEqual(got, want) {
		t.Errorf("Recorded %+v, want %+v.", got, want)
	}
}

func TestAWatchOfASeenKindRecordsSeenAsItForwardsEachEvent(t *testing.T) {
	release := make(chan struct{})
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		frame := func(kind, object string) {
			fmt.Fprintf(w, `{"type":%q,"object":%s}`+"\n", kind, object)
			w.(http.Flusher).Flush()
		}
		frame("ADDED", gadgetJSON("g1", "u1", "5"))
		<-release
		frame("MODIFIED", gadgetJSON("g1", "u1", "6"))
		frame("BOOKMARK", `{"kind":"Gadget","metadata":{"resourceVersion":"7"}}`)
		frame("ERROR", `{"kind":"Status","metadata":{}}`)
		frame("DELETED", gadgetJSON("g1", "u1", "8"))
	}), seeingGadgets)

	resp, err := http.Get(l.URL() + gadgetsPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(recorded.seen()) == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("Recorded no Seen while the watch was open.")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	io.Copy(io.Discard, resp.Body)
	l.Close()

	added, modified, deleted := seenGadget("g1", "u1", "5"), seenGadget("g1", "u1", "6"), seenGadget("g1", "u1", "8")
	deleted.Deleted = true
	want := []trace.Seen{added, modified, deleted}
	for i := range want {
		want[i].Stream = 1
	}
	if got := recorded.seen(); !reflect.DeepEqual(got, want) {
		t.Errorf("Recorded %+v, want %+v.", got, want)
	}
	recorded.mu.Lock()
	defer recorded.mu.Unlock()
	if last := recorded.list[len(recorded.list)-1]; last.WatchEnd == nil {
		t.Errorf("The last event is %+v, want the watch's end after its Seen events.", last)
	}
}

func TestResponsesThatShowNoSeenKindRecordNoSeen(t *testing.T) {
	for _, c := range []struct {
		name    string
		path    string
		handler http.Handler
	}{
		{"another kind", "/api/v1/namespaces/ns1/configmaps/g1", respondOK("application/json", "", gadgetJSON("g1", "u1", "5"))},
		{"a 404", gadgetsPath + "/g1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(gadgetJSON("g1", "u1", "5")))
		})},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, recorded := listen(t, c.handler, seeingGadgets)

			do(t, l, "GET", c.path)

			recorded.waitCalls(t, 1)
			if got := recorded.seen(); len(got) > 0 {
				t.Errorf("Recorded %+v, want no Seen.", got)
			}
		})
	}
}

func TestAWriteOfASeenKindRecordsSeen(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "PATCH"} {
		t.Run(method, func(t *testing.T) {
			l, recorded := listen(t, respondOK("application/json", "", gadgetJSON("g1", "u1", "5")), seeingGadgets)

			path := gadgetsPath
			if method != "POST" {
				path += "/g1"
			}
			do(t, l, method, path)

			recorded.waitCalls(t, 1)
			if got, want := recorded.seen(), []trace.Seen{seenGadget("g1", "u1", "5")}; !reflect.DeepEqual(got, want) {
				t.Errorf("Recorded %+v, want %+v.", got, want)
			}
		})
	}
}

func TestAResponseOfASeenKindItCannotDecodeRecordsAnOpaqueSeen(t *testing.T) {
	big := `{"items":[` + strings.Repeat(gadgetJSON("g1", "u1", "5")+",", interpose.MaxResultBody/80) + gadgetJSON("g1", "u1", "5") + "]}"
	for _, c := range []struct {
		name, path string
		handler    http.Handler
	}{
		{"protobuf", gadgetsPath + "/g1", respondOK("application/vnd.kubernetes.protobuf", "", "k8s\x00")},
		{"a protobuf watch", gadgetsPath + "?watch=true", respondOK("application/vnd.kubernetes.protobuf;stream=watch", "", "")},
		{"an unknown encoding", gadgetsPath + "/g1", respondOK("application/json", "br", gadgetJSON("g1", "u1", "5"))},
		{"a watch in an unknown encoding", gadgetsPath + "?watch=true", respondOK("application/json", "br", "")},
		{"bad JSON", gadgetsPath + "/g1", respondOK("application/json", "", "{")},
		{"a big list", gadgetsPath, respondOK("application/json", "", big)},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, recorded := listen(t, c.handler, seeingGadgets)

			do(t, l, "GET", c.path)

			recorded.waitCalls(t, 1)
			l.Close()
			want := seenGadget("", "", "")
			want.Opaque = true
			switch {
			case strings.Contains(c.path, "watch"):
				want.Stream = 1
			case c.name != "a big list":
				want.Ref.Name = "g1"
			}
			if got := recorded.seen(); !reflect.DeepEqual(got, []trace.Seen{want}) {
				t.Errorf("Recorded %+v, want %+v.", got, []trace.Seen{want})
			}
		})
	}
}

func TestADroppedResponseRecordsNoSeen(t *testing.T) {
	l, recorded := listen(t, respondOK("application/json", "", gadgetJSON("g1", "u1", "5")), seeingGadgets)
	fault(t, l, plan.Fault{DropResponse: true})

	if resp, err := http.Get(l.URL() + gadgetsPath + "/g1"); err == nil {
		resp.Body.Close()
	}

	recorded.waitCalls(t, 1)
	if got := recorded.seen(); len(got) > 0 {
		t.Errorf("Recorded %+v, want no Seen.", got)
	}
}

func TestAFailedCreateRecordsTheRequestedName(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"metadata":{"name":"cm-a"}}`, "cm-a"},
		{`{"metadata":{"generateName":"cm-b-"}}`, "cm-b-"},
	} {
		var forwarded atomic.Value
		l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			forwarded.Store(string(body))
			w.WriteHeader(http.StatusInternalServerError)
		}))

		resp, err := http.Post(l.URL()+"/api/v1/namespaces/ns1/configmaps", "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if got := recorded.waitCalls(t, 1)[0]; got.RequestName != c.want {
			t.Errorf("Recorded request name %q, want %q.", got.RequestName, c.want)
		}
		if forwarded.Load() != c.body {
			t.Errorf("Forwarded %q, want %q.", forwarded.Load(), c.body)
		}
	}
}

// slowSeen delays recording each Seen.
func slowSeen(o *interpose.Options) {
	record := o.Record
	o.Record = func(e trace.Event) {
		if e.Seen != nil {
			time.Sleep(50 * time.Millisecond)
		}
		record(e)
	}
}

func TestAWatchRecordsSeenBeforeTheCallerGetsTheEvent(t *testing.T) {
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"type":"ADDED","object":%s}`+"\n", gadgetJSON("g1", "u1", "5"))
	}), seeingGadgets, slowSeen)

	resp, err := http.Get(l.URL() + gadgetsPath + "?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if len(recorded.seen()) != 1 {
		t.Error("The caller got the event before its Seen was recorded.")
	}
}

func TestAGetRecordsSeenBeforeTheCallerGetsTheObject(t *testing.T) {
	body := gadgetJSON("g1", "u1", "5")
	l, recorded := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		io.WriteString(w, body)
		time.Sleep(100 * time.Millisecond)
	}), seeingGadgets, slowSeen)

	resp, err := http.Get(l.URL() + gadgetsPath + "/g1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if len(recorded.seen()) != 1 {
		t.Error("The caller got the object before its Seen was recorded.")
	}
	if string(got) != body {
		t.Errorf("Got %q, want %q.", got, body)
	}
}

func TestForwardsTheAuthorizationHeaderWhenAsked(t *testing.T) {
	var seen atomic.Value
	l, _ := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
	}), func(o *interpose.Options) { o.ForwardAuthorization = true })
	req, _ := http.NewRequest("GET", l.URL()+"/api/v1/pods", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 caller")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := seen.Load(); got != "AWS4-HMAC-SHA256 caller" {
		t.Errorf("The upstream saw Authorization %q, want the caller's.", got)
	}
}

// streaming sends its header, then each frame it receives, until frames
// closes or the client leaves.
func streaming(frames <-chan string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case frame, open := <-frames:
				if !open {
					return
				}
				fmt.Fprintln(w, frame)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
}

// watchLines opens a watch and hands each line it reads to the channel.
func watchLines(t *testing.T, l *interpose.Listener, path string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", l.URL()+path+"?watch=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		defer resp.Body.Close()
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return lines, cancel
}

func gadgetFrame(rv string) string {
	return fmt.Sprintf(`{"type":"MODIFIED","object":%s}`, gadgetJSON("g1", "u1", rv))
}

func expectLines(t *testing.T, lines <-chan string, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-lines:
			if got != w {
				t.Fatalf("Got line %s, want %s.", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Got no line, want %s.", w)
		}
	}
}

func expectNoLine(t *testing.T, lines <-chan string) {
	t.Helper()
	select {
	case got := <-lines:
		t.Fatalf("Got line %s during the hold.", got)
	case <-time.After(150 * time.Millisecond):
	}
}

var holdGadgets = plan.Fault{HoldWatch: true, Match: plan.Match{Group: gadgets.Group, Resource: gadgets.Resource}}

func TestAHoldWatchPausesWatchEventsAndDeliversThemInOrder(t *testing.T) {
	for _, path := range []string{gadgetsPath, "/apis/toy.reconciler-fuzzer/v1/namespaces/ns2/gadgets", "/apis/toy.reconciler-fuzzer/v1/gadgets"} {
		t.Run(path, func(t *testing.T) {
			frames := make(chan string)
			l, recorded := listen(t, streaming(frames), seeingGadgets)
			lines, _ := watchLines(t, l, path)
			frames <- gadgetFrame("1")
			expectLines(t, lines, gadgetFrame("1"))

			end := fault(t, l, holdGadgets)
			frames <- gadgetFrame("2")
			frames <- gadgetFrame("3")
			expectNoLine(t, lines)
			if got := len(recorded.seen()); got != 1 {
				t.Errorf("Recorded %d Seen during the hold, want only the one before it.", got)
			}

			end()
			expectLines(t, lines, gadgetFrame("2"), gadgetFrame("3"))
			var rvs []string
			for _, s := range recorded.seen() {
				rvs = append(rvs, s.RV)
			}
			if want := []string{"1", "2", "3"}; !slices.Equal(rvs, want) {
				t.Errorf("Recorded Seen versions %v, want %v.", rvs, want)
			}
		})
	}
}

func TestAHoldWatchHoldsAWatchOpenedDuringIt(t *testing.T) {
	frames := make(chan string)
	l, _ := listen(t, streaming(frames))
	end := fault(t, l, holdGadgets)
	lines, _ := watchLines(t, l, gadgetsPath)
	frames <- gadgetFrame("1")
	expectNoLine(t, lines)

	end()
	expectLines(t, lines, gadgetFrame("1"))
}

func TestAHoldWatchLastsUntilEveryHoldOnTheKindEnds(t *testing.T) {
	frames := make(chan string)
	l, _ := listen(t, streaming(frames))
	endFirst := fault(t, l, holdGadgets)
	endSecond := fault(t, l, holdGadgets)
	lines, _ := watchLines(t, l, gadgetsPath)
	frames <- gadgetFrame("1")

	endFirst()
	expectNoLine(t, lines)
	endSecond()
	expectLines(t, lines, gadgetFrame("1"))
}

func TestAHoldWatchSparesOtherKindsAndRequests(t *testing.T) {
	frames := make(chan string)
	l, recorded := listen(t, streaming(frames))
	fault(t, l, plan.Fault{HoldWatch: true, Match: plan.Match{Resource: "gadgets"}})
	fault(t, l, plan.Fault{Drop: true, Match: plan.Match{Verb: "get"}, Nth: 1})
	lines, _ := watchLines(t, l, gadgetsPath)
	frames <- gadgetFrame("1")
	expectLines(t, lines, gadgetFrame("1"))

	fault(t, l, holdGadgets)
	if _, err := http.Get(l.URL() + gadgetsPath + "/g1"); err == nil {
		t.Error("A get under a hold escaped the drop, so the hold counted or picked it.")
	}
	close(frames)
	recorded.waitCalls(t, 2)
	for _, c := range recorded.calls() {
		if c.Watch && c.Injected != "" {
			t.Errorf("The watch has injected %q, want none.", c.Injected)
		}
	}
}

func TestClosingReleasesAHeldWatch(t *testing.T) {
	frames := make(chan string, 1)
	l, _ := listen(t, streaming(frames))
	fault(t, l, holdGadgets)
	lines, _ := watchLines(t, l, gadgetsPath)
	frames <- gadgetFrame("1")
	time.Sleep(50 * time.Millisecond)

	closed := make(chan error)
	go func() { closed <- l.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a held watch.")
	}
	for range lines {
	}
}

func TestAHeldWatchEndsWhenItsClientLeaves(t *testing.T) {
	frames := make(chan string, 1)
	l, recorded := listen(t, streaming(frames))
	fault(t, l, holdGadgets)
	lines, cancel := watchLines(t, l, gadgetsPath)
	frames <- gadgetFrame("1")
	time.Sleep(50 * time.Millisecond)

	cancel()
	for range lines {
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		recorded.mu.Lock()
		ended := slices.ContainsFunc(recorded.list, func(e trace.Event) bool { return e.WatchEnd != nil })
		recorded.mu.Unlock()
		if ended {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("The held watch did not end after its client left.")
		}
		time.Sleep(time.Millisecond)
	}
}
