package interpose_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
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
		calls = append(calls, *ev.Call)
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
