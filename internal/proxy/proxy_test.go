package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// startProxy puts a proxy in front of upstream and stops both when the test ends.
func startProxy(t *testing.T, upstream http.Handler) *proxy.Proxy {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)

	p, err := proxy.Start(&rest.Config{Host: server.URL}, proxy.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	return p
}

func okUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
}

// finishedRecords waits for the proxy to complete count records, which it does
// after the client already has its response (see Proxy.Log).
func finishedRecords(t *testing.T, p *proxy.Proxy, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		log := p.Log()
		if len(log) >= count && !slices.ContainsFunc(log, func(r proxy.Request) bool { return r.Latency <= 0 }) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("The proxy recorded %+v, want %d finished records.", log, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func do(t *testing.T, p *proxy.Proxy, method, target string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, p.URL()+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, values := range header {
		req.Header[k] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, target, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func onlyRequest(t *testing.T, p *proxy.Proxy) proxy.Request {
	t.Helper()
	log := p.Log()
	if len(log) != 1 {
		t.Fatalf("The log holds %d requests, want 1: %+v", len(log), log)
	}
	return log[0]
}

// onlyFinishedRequest waits for that record's exchange to end. The client sees
// its response before the proxy completes the record, so a test that asserts on
// timing waits for it (see Proxy.Log).
func onlyFinishedRequest(t *testing.T, p *proxy.Proxy) proxy.Request {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := onlyRequest(t, p)
		if got.Latency > 0 {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("The proxy never finished the record %+v.", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRecordsTheParsedRequest(t *testing.T) {
	cases := []struct {
		method string
		target string
		want   proxy.Request
	}{
		{"GET", "/api/v1/namespaces/ns1/configmaps", proxy.Request{
			Verb: "list", Version: "v1", Resource: "configmaps", Namespace: "ns1"}},
		{"GET", "/api/v1/namespaces/ns1/configmaps/cm1", proxy.Request{
			Verb: "get", Version: "v1", Resource: "configmaps", Namespace: "ns1", Name: "cm1"}},
		{"GET", "/api/v1/namespaces/ns1/configmaps?resourceVersion=7&watch=true", proxy.Request{
			Verb: "watch", Version: "v1", Resource: "configmaps", Namespace: "ns1", Watch: true}},
		{"GET", "/api/v1/namespaces/ns1/configmaps/cm1?watch=true", proxy.Request{
			Verb: "watch", Version: "v1", Resource: "configmaps", Namespace: "ns1", Name: "cm1", Watch: true}},
		{"GET", "/api/v1/namespaces/ns1/configmaps?watch=false", proxy.Request{
			Verb: "list", Version: "v1", Resource: "configmaps", Namespace: "ns1"}},
		{"POST", "/api/v1/namespaces/ns1/configmaps", proxy.Request{
			Verb: "create", Version: "v1", Resource: "configmaps", Namespace: "ns1"}},
		{"PUT", "/api/v1/namespaces/ns1/configmaps/cm1", proxy.Request{
			Verb: "update", Version: "v1", Resource: "configmaps", Namespace: "ns1", Name: "cm1"}},
		{"PATCH", "/api/v1/namespaces/ns1/configmaps/cm1", proxy.Request{
			Verb: "patch", Version: "v1", Resource: "configmaps", Namespace: "ns1", Name: "cm1"}},
		{"DELETE", "/api/v1/namespaces/ns1/configmaps/cm1", proxy.Request{
			Verb: "delete", Version: "v1", Resource: "configmaps", Namespace: "ns1", Name: "cm1"}},
		{"DELETE", "/api/v1/namespaces/ns1/configmaps", proxy.Request{
			Verb: "deletecollection", Version: "v1", Resource: "configmaps", Namespace: "ns1"}},
		{"GET", "/api/v1/configmaps", proxy.Request{
			Verb: "list", Version: "v1", Resource: "configmaps"}},
		{"GET", "/api/v1/nodes/node1", proxy.Request{
			Verb: "get", Version: "v1", Resource: "nodes", Name: "node1"}},
		{"GET", "/api/v1/namespaces/ns1", proxy.Request{
			Verb: "get", Version: "v1", Resource: "namespaces", Namespace: "ns1", Name: "ns1"}},
		{"PUT", "/api/v1/namespaces/ns1/finalize", proxy.Request{
			Verb: "update", Version: "v1", Resource: "namespaces", Namespace: "ns1", Name: "ns1", Subresource: "finalize"}},
		{"GET", "/api/v1/namespaces/ns1/pods/p1/log", proxy.Request{
			Verb: "get", Version: "v1", Resource: "pods", Namespace: "ns1", Name: "p1", Subresource: "log"}},
		{"POST", "/api/v1/namespaces/ns1/serviceaccounts/sa1/token", proxy.Request{
			Verb: "create", Version: "v1", Resource: "serviceaccounts", Namespace: "ns1", Name: "sa1", Subresource: "token"}},
		{"GET", "/apis/apps/v1/namespaces/ns1/deployments/d1", proxy.Request{
			Verb: "get", Group: "apps", Version: "v1", Resource: "deployments", Namespace: "ns1", Name: "d1"}},
		{"PUT", "/apis/apps/v1/namespaces/ns1/deployments/d1/status", proxy.Request{
			Verb: "update", Group: "apps", Version: "v1", Resource: "deployments", Namespace: "ns1", Name: "d1", Subresource: "status"}},
		{"PATCH", "/apis/toy.reconciler-fuzzer/v1/namespaces/ns1/widgets/w1/status", proxy.Request{
			Verb: "patch", Group: "toy.reconciler-fuzzer", Version: "v1", Resource: "widgets", Namespace: "ns1", Name: "w1", Subresource: "status"}},
		{"GET", "/apis/toy.reconciler-fuzzer/v1/widgets", proxy.Request{
			Verb: "list", Group: "toy.reconciler-fuzzer", Version: "v1", Resource: "widgets"}},
		{"POST", "/apis/coordination.k8s.io/v1/namespaces/ns1/leases", proxy.Request{
			Verb: "create", Group: "coordination.k8s.io", Version: "v1", Resource: "leases", Namespace: "ns1"}},
		{"GET", "/api/v1", proxy.Request{Verb: "get", Version: "v1"}},
		{"GET", "/apis/apps/v1", proxy.Request{Verb: "get", Group: "apps", Version: "v1"}},
		{"GET", "/apis", proxy.Request{Verb: "get"}},
		{"GET", "/healthz", proxy.Request{Verb: "get"}},
		{"POST", "/fancy", proxy.Request{Verb: "post"}},
	}

	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			p := startProxy(t, okUpstream())

			do(t, p, c.method, c.target, nil)

			got := onlyRequest(t, p)
			got.Start, got.Latency = time.Time{}, 0
			want := c.want
			want.Status = http.StatusOK
			want.Path, _, _ = strings.Cut(c.target, "?")
			if got != want {
				t.Errorf("Recorded\n\t%+v\nwant\n\t%+v", got, want)
			}
		})
	}
}

func TestRecordsTheUpstreamStatusAndLatency(t *testing.T) {
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNotFound)
	}))
	before := time.Now()

	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps/cm1", nil)

	got := onlyFinishedRequest(t, p)
	if got.Status != http.StatusNotFound {
		t.Errorf("Recorded status %d, want 404.", got.Status)
	}
	if got.Latency < 10*time.Millisecond {
		t.Errorf("Recorded latency %v, want at least the upstream's 10ms.", got.Latency)
	}
	if got.Start.Before(before) || got.Start.After(time.Now()) {
		t.Errorf("Recorded start %v, which is outside the test's window.", got.Start)
	}
}

func TestRecordsAnUnreachableAPIServerAsABadGateway(t *testing.T) {
	server := httptest.NewServer(okUpstream())
	p, err := proxy.Start(&rest.Config{Host: server.URL}, proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop() })
	server.Close()

	resp := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("The client saw %d, want 502.", resp.StatusCode)
	}
	if got := onlyRequest(t, p).Status; got != http.StatusBadGateway {
		t.Errorf("The request is recorded with status %d, want 502.", got)
	}
}

func TestStripsTheInboundAuthorizationHeader(t *testing.T) {
	seen := make(chan string, 1)
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
	}))

	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", http.Header{
		"Authorization": {"Bearer target-token"}})

	if got := <-seen; got != "" {
		t.Errorf("The upstream saw Authorization %q, want it stripped.", got)
	}
}

func TestPassesTheTargetsImpersonationHeadersThrough(t *testing.T) {
	seen := make(chan http.Header, 1)
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header
	}))

	sent := http.Header{
		"Impersonate-User":         {"someone"},
		"Impersonate-Uid":          {"some-uid"},
		"Impersonate-Group":        {"group-a", "group-b"},
		"Impersonate-Extra-Scopes": {"scope-a", "scope-b"}}
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", sent)

	got := <-seen
	for name, values := range sent {
		if !slices.Equal(got.Values(name), values) {
			t.Errorf("The upstream saw %s %q, want %q as the target sent it.", name, got.Values(name), values)
		}
	}
}

func TestPassesUpgradeRequestsThroughUnrecorded(t *testing.T) {
	p := startProxy(t, okUpstream())

	do(t, p, "GET", "/api/v1/namespaces/ns1/pods/p1/exec", http.Header{
		"Connection": {"Upgrade"},
		"Upgrade":    {"SPDY/3.1"}})

	if log := p.Log(); len(log) != 0 {
		t.Errorf("An upgrade request was recorded: %+v", log)
	}
}

func TestStreamsResponsesWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "a")
		http.NewResponseController(w).Flush()
		<-release
		io.WriteString(w, "b")
	}))

	first := make(chan byte, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps?watch=true")
		if err != nil {
			return
		}
		defer resp.Body.Close()
		b := make([]byte, 1)
		if _, err := io.ReadFull(resp.Body, b); err == nil {
			first <- b[0]
		}
		io.Copy(io.Discard, resp.Body)
	}()

	select {
	case got := <-first:
		if got != 'a' {
			t.Errorf("Read %q from the stream, want %q.", got, "a")
		}
	case <-time.After(5 * time.Second):
		t.Error("The proxy buffered the response instead of streaming it.")
	}
	close(release)
	<-done
}

func TestRecordsAWatchAtItsStartAndItsDurationAtItsEnd(t *testing.T) {
	open := make(chan struct{})
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		close(open)
		<-r.Context().Done()
	}))

	resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps?watch=true")
	if err != nil {
		t.Fatal(err)
	}
	<-open

	inFlight := onlyRequest(t, p)
	if !inFlight.Watch || inFlight.Status != http.StatusOK {
		t.Errorf("An open watch is recorded as %+v, want watch=true and status 200.", inFlight)
	}
	if inFlight.Latency != 0 {
		t.Errorf("An open watch has latency %v, want 0 until it ends.", inFlight.Latency)
	}

	time.Sleep(10 * time.Millisecond)
	resp.Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		ended := onlyRequest(t, p)
		if ended.Latency >= 10*time.Millisecond {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("A closed watch still has latency %v, want its duration.", ended.Latency)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWriteLogWritesOneJSONObjectPerRequest(t *testing.T) {
	p := startProxy(t, okUpstream())
	do(t, p, "POST", "/apis/toy.reconciler-fuzzer/v1/namespaces/ns1/widgets", nil)
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps?watch=true", nil)

	// The client has its response before the proxy stamps the exchange's
	// latency, so the log is read once the records are finished, as the
	// envtest tier's recordsAfter does (see Proxy.Log).
	finishedRecords(t, p, 2)
	var buf strings.Builder
	if err := p.WriteLog(&buf); err != nil {
		t.Fatalf("WriteLog returned an error: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("WriteLog wrote %d lines, want 2: %q", len(lines), buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("The first line is not JSON: %v", err)
	}
	want := map[string]any{
		"verb": "create", "group": "toy.reconciler-fuzzer", "version": "v1", "resource": "widgets",
		"namespace": "ns1", "path": "/apis/toy.reconciler-fuzzer/v1/namespaces/ns1/widgets",
		"status": float64(200),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("The log line has %s=%v, want %v.", k, got[k], v)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, got["start"].(string)); err != nil {
		t.Errorf("The log line has start=%v, which is not RFC 3339: %v", got["start"], err)
	}
	if _, ok := got["latencyNs"].(float64); !ok {
		t.Errorf("The log line has latencyNs=%v, want a number.", got["latencyNs"])
	}
	for _, absent := range []string{"name", "subresource", "fault"} {
		if _, ok := got[absent]; ok {
			t.Errorf("The log line carries an empty %s.", absent)
		}
	}
	if !strings.Contains(lines[1], `"watch":true`) {
		t.Errorf("The watch line %q does not record the watch.", lines[1])
	}
}

func TestLogReturnsACopy(t *testing.T) {
	p := startProxy(t, okUpstream())
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	p.Log()[0].Verb = "mutated"

	if got := onlyRequest(t, p).Verb; got != "list" {
		t.Errorf("The proxy kept a caller's change to the log: verb %q, want list.", got)
	}
}

func TestLogIsReadableWhileRequestsAreInFlight(t *testing.T) {
	p := startProxy(t, okUpstream())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps")
				if err != nil {
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	for range 500 {
		for _, r := range p.Log() {
			if r.Verb != "list" {
				t.Fatalf("Log returned a half-written record: %+v", r)
			}
		}
	}
	wg.Wait()

	if log := p.Log(); len(log) != 200 {
		t.Fatalf("The log holds %d requests, want 200.", len(log))
	}
}

func TestKubeconfigPointsAtTheProxyWithoutCredentials(t *testing.T) {
	p := startProxy(t, okUpstream())
	path := filepath.Join(t.TempDir(), "kube", "config")

	if err := p.Kubeconfig(path, "ns"); err != nil {
		t.Fatalf("Kubeconfig returned an error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := clientcmd.Load(raw)
	if err != nil {
		t.Fatalf("The kubeconfig does not parse: %v", err)
	}
	client, err := clientcmd.NewDefaultClientConfig(*loaded, nil).ClientConfig()
	if err != nil {
		t.Fatalf("The kubeconfig does not resolve to a rest.Config: %v", err)
	}
	if client.Host != p.URL() {
		t.Errorf("The kubeconfig points at %q, want the proxy at %q.", client.Host, p.URL())
	}
	if client.BearerToken != "" || client.CertFile != "" || len(client.CertData) != 0 || client.Username != "" {
		t.Errorf("The kubeconfig carries credentials: %+v", client)
	}
	if len(loaded.AuthInfos) != 0 {
		t.Errorf("The kubeconfig declares users: %+v", loaded.AuthInfos)
	}
}

func TestKubeconfigNamesTheNamespace(t *testing.T) {
	p := startProxy(t, okUpstream())
	path := filepath.Join(t.TempDir(), "kubeconfig")

	if err := p.Kubeconfig(path, "reconciler-fuzzer-run-x"); err != nil {
		t.Fatalf("Kubeconfig returned an error: %v", err)
	}

	loaded, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	namespace, _, err := clientcmd.NewDefaultClientConfig(*loaded, nil).Namespace()
	if err != nil || namespace != "reconciler-fuzzer-run-x" {
		t.Errorf("The kubeconfig names the namespace %q (%v), want reconciler-fuzzer-run-x.", namespace, err)
	}
}

func TestStopEndsTheListener(t *testing.T) {
	server := httptest.NewServer(okUpstream())
	t.Cleanup(server.Close)
	p, err := proxy.Start(&rest.Config{Host: server.URL}, proxy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	url := p.URL()

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}

	if _, err := http.Get(url + "/healthz"); err == nil {
		t.Error("The proxy still serves requests after Stop.")
	}
}

func TestStartRejectsAConfigItCannotBuildATransportFor(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:6443"}
	cfg.TLSClientConfig.CAFile = filepath.Join(t.TempDir(), "no-such-ca.crt")

	p, err := proxy.Start(cfg, proxy.Options{})

	if err == nil {
		p.Stop()
		t.Fatal("Start accepted a config whose CA file does not exist.")
	}
	if p != nil {
		t.Error("Start returned a non-nil Proxy together with an error.")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("full") }

func TestWriteLogReportsAWriteFailure(t *testing.T) {
	p := startProxy(t, okUpstream())
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	err := p.WriteLog(failingWriter{})

	if err == nil {
		t.Fatal("WriteLog reported no error although every write failed.")
	}
}

func TestKubeconfigReportsAWriteFailure(t *testing.T) {
	p := startProxy(t, okUpstream())

	err := p.Kubeconfig(t.TempDir(), "ns") // A directory is not writable as a file.

	if err == nil {
		t.Fatal("Kubeconfig reported no error although the path is a directory.")
	}
}

func TestRecordsACanceledRequestWithoutAStatus(t *testing.T) {
	p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL()+"/api/v1/namespaces/ns1/configmaps", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("The request succeeded although the client canceled it.")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		recorded := onlyRequest(t, p)
		if recorded.Latency > 0 {
			if recorded.Status != 0 {
				t.Errorf("A request the target canceled is recorded with status %d, want 0.", recorded.Status)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("The canceled request never ended.")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An open request carries no latency, so a report sampled mid-run does not
// contradict requests.jsonl (DESIGN.md §5.7).
func TestAnOpenRequestMarshalsWithNoLatency(t *testing.T) {
	open := proxy.Request{Verb: "watch", Watch: true, Status: http.StatusOK}
	done := open
	done.Latency = time.Millisecond

	for _, request := range []struct {
		name    string
		request proxy.Request
		carries bool
	}{
		{"a watch still open", open, false},
		{"a request that finished", done, true},
	} {
		t.Run(request.name, func(t *testing.T) {
			encoded, err := json.Marshal(request.request)
			if err != nil {
				t.Fatal(err)
			}
			if carries := strings.Contains(string(encoded), "latencyNs"); carries != request.carries {
				t.Errorf("%s marshals as %s.", request.name, encoded)
			}
		})
	}
}
