package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

func TestMatchesSelectsOnVerbResourceAndName(t *testing.T) {
	request := proxy.Request{Verb: "create", Resource: "configmaps", Name: "widget-3", Namespace: "ns1"}
	cases := []struct {
		matcher proxy.RequestMatcher
		want    bool
	}{
		{proxy.RequestMatcher{}, true},
		{proxy.RequestMatcher{Verb: "create"}, true},
		{proxy.RequestMatcher{Verb: "delete"}, false},
		{proxy.RequestMatcher{Resource: "configmaps"}, true},
		{proxy.RequestMatcher{Resource: "secrets"}, false},
		{proxy.RequestMatcher{Name: "widget-3"}, true},
		{proxy.RequestMatcher{Name: "widget-4"}, false},
		{proxy.RequestMatcher{Name: "widget-*"}, true},
		{proxy.RequestMatcher{Name: "gadget-*"}, false},
		{proxy.RequestMatcher{Name: "["}, false},
		{proxy.RequestMatcher{Verb: "create", Resource: "configmaps", Name: "widget-3"}, true},
		{proxy.RequestMatcher{Verb: "create", Resource: "configmaps", Name: "widget-9"}, false},
		{proxy.RequestMatcher{Verb: "create", Resource: "secrets", Name: "widget-3"}, false},
	}

	for _, c := range cases {
		if got := c.matcher.Matches(request); got != c.want {
			t.Errorf("%+v matched %+v: %t, want %t", c.matcher, request, got, c.want)
		}
	}
}

// faultedProxy fronts an upstream that fails the test if a faulted request
// reaches it.
func faultedProxy(t *testing.T, seed int64, specs ...proxy.FaultSpec) *proxy.Proxy {
	t.Helper()
	server := httptest.NewServer(okUpstream())
	t.Cleanup(server.Close)

	p, err := proxy.Start(&rest.Config{Host: server.URL}, proxy.Options{Seed: seed})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})
	for _, spec := range specs {
		p.AddFault(spec)
	}
	return p
}

func TestInjectedErrorReplacesTheUpstreamResponse(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Verb: "create", Resource: "configmaps"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
	})

	resp := do(t, p, "POST", "/api/v1/namespaces/ns1/configmaps", nil)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("The client saw %d, want 500.", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("The response has Content-Type %q, want application/json.", got)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("The response has Content-Encoding %q, want none.", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var status metav1.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("The body %q is not a Status: %v", body, err)
	}
	want := metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     http.StatusInternalServerError,
		Reason:   metav1.StatusReasonInternalError,
		Message:  "reconciler-fuzzer fault: create /api/v1/namespaces/ns1/configmaps",
		Details:  &metav1.StatusDetails{Kind: "configmaps"},
	}
	if status.TypeMeta != want.TypeMeta || status.Status != want.Status || status.Code != want.Code ||
		status.Reason != want.Reason || status.Message != want.Message {
		t.Errorf("The body decodes to\n\t%+v\nwant\n\t%+v", status, want)
	}
	if status.Details == nil || status.Details.Kind != want.Details.Kind {
		t.Errorf("The Status details are %+v, want the resource in Kind.", status.Details)
	}

	recorded := onlyRequest(t, p)
	if recorded.Status != http.StatusInternalServerError || recorded.Fault != "error(500)" {
		t.Errorf("The faulted request is recorded as %+v, want status 500 and fault error(500).", recorded)
	}
}

func TestErrorCodeChoosesTheReason(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{Action: proxy.Error{Code: http.StatusConflict}})

	resp := do(t, p, "PUT", "/api/v1/namespaces/ns1/configmaps/cm1", nil)

	var status metav1.Status
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Reason != metav1.StatusReasonConflict {
		t.Errorf("A 409 fault has reason %q, want Conflict.", status.Reason)
	}
}

func TestDelayHoldsTheRequestBack(t *testing.T) {
	const delay = 200 * time.Millisecond
	p := faultedProxy(t, 0, proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Verb: "get"},
		Action: proxy.Delay{For: delay},
	})

	start := time.Now()
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps/cm1", nil)
	delayed := time.Since(start)

	start = time.Now()
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil) // A list is not a get.
	undelayed := time.Since(start)

	if delayed < delay {
		t.Errorf("The delayed request took %v, want at least %v.", delayed, delay)
	}
	if undelayed >= delay {
		t.Errorf("An unmatched request took %v, want no delay.", undelayed)
	}
	if got := p.Log()[0].Fault; got != "delay(200ms)" {
		t.Errorf("The delayed request records fault %q, want delay(200ms).", got)
	}
}

// A delay holds a request until the proxy forwards it, whether its response
// then ends or streams on as a watch's does.
func TestHeldReportsARequestUntilTheProxyForwardsIt(t *testing.T) {
	const delay = 200 * time.Millisecond
	for _, test := range []struct{ name, path string }{
		{"a get", "/api/v1/namespaces/ns1/configmaps/cm1"},
		{"a list", "/api/v1/namespaces/ns1/configmaps"},
		{"a watch", "/api/v1/namespaces/ns1/configmaps?watch=true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarded := make(chan struct{})
			p := startProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(forwarded)
				w.WriteHeader(http.StatusOK)
				http.NewResponseController(w).Flush()
				if r.URL.Query().Get("watch") == "true" {
					<-r.Context().Done()
				}
			}))
			p.AddFault(proxy.FaultSpec{Action: proxy.Delay{For: delay}})

			answered := make(chan struct{})
			go func() {
				defer close(answered)
				if resp, err := http.Get(p.URL() + test.path); err == nil {
					resp.Body.Close()
				}
			}()
			arrived := arrival(t, p)
			whileHeld, _ := p.Held(time.Now())
			select {
			case <-forwarded:
			case <-time.After(5 * time.Second):
				t.Fatal("The proxy never forwarded the request.")
			}
			afterward, released := p.Held(time.Now())
			beforeIt, notYet := p.Held(arrived)
			<-answered

			if !whileHeld {
				t.Error("The proxy reports no request held during the delay.")
			}
			if afterward || released.Before(arrived.Add(delay)) {
				t.Errorf("Once forwarded, the proxy reports (%t, %v), want no request held and a release %v after %v.", afterward, released, delay, arrived)
			}
			if beforeIt || !notYet.IsZero() {
				t.Errorf("Of the requests before %v, the proxy reports (%t, %v), want none.", arrived, beforeIt, notYet)
			}
		})
	}
}

// A target that gives up on a held request ends its hold.
func TestHeldReleasesARequestTheTargetGaveUpOn(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{Action: proxy.Delay{For: time.Hour}})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL()+"/api/v1/namespaces/ns1/configmaps", nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("The request succeeded although the client gave up on it.")
	}

	deadline := time.Now().Add(5 * time.Second)
	for holding, released := p.Held(time.Now()); holding || released.IsZero(); holding, released = p.Held(time.Now()) {
		if time.Now().After(deadline) {
			t.Fatalf("The proxy still reports (%t, %v) for a request the client gave up on.", holding, released)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A fault lasts until the proxy releases what it held, though it applies to
// no request after the one it holds.
func TestAFaultsWindowLastsWhileItHoldsARequest(t *testing.T) {
	const delay, runsFor = time.Second, 500 * time.Millisecond
	for _, test := range []struct {
		name   string
		until  proxy.Trigger
		retire func(p *proxy.Proxy, id proxy.FaultID, added time.Time)
	}{
		{"a count spent on the request", proxy.Trigger{Count: 1}, func(*proxy.Proxy, proxy.FaultID, time.Time) {}},
		{"a duration that runs out", proxy.Trigger{For: runsFor},
			func(_ *proxy.Proxy, _ proxy.FaultID, added time.Time) { time.Sleep(time.Until(added.Add(runsFor))) }},
		{"RemoveFault", proxy.Trigger{}, func(p *proxy.Proxy, id proxy.FaultID, _ time.Time) { p.RemoveFault(id) }},
		{"ClearFaults", proxy.Trigger{}, func(p *proxy.Proxy, _ proxy.FaultID, _ time.Time) { p.ClearFaults() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := faultedProxy(t, 0)
			id := p.AddFault(proxy.FaultSpec{Action: proxy.Delay{For: delay}, Until: test.until})
			added := time.Now()

			answered := make(chan struct{})
			go func() {
				defer close(answered)
				if resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps"); err == nil {
					resp.Body.Close()
				}
			}()
			arrived := arrival(t, p)
			test.retire(p, id, added)
			whileHeld := p.Window(id)
			<-answered
			afterward := p.Window(id)

			if !whileHeld.Retired.IsZero() {
				t.Errorf("The fault retired at %v while the proxy held its request.", whileHeld.Retired)
			}
			if afterward.Retired.Before(arrived.Add(delay)) {
				t.Errorf("The fault retired at %v, want no sooner than the release %v after %v.", afterward.Retired, delay, arrived)
			}
		})
	}
}

// Faults hold requests apart, and the one that releases last need not be the
// first fault.
func TestHeldReportsTheLastReleaseOfAnyFault(t *testing.T) {
	const longer, shorter = 600 * time.Millisecond, 100 * time.Millisecond
	p := faultedProxy(t, 0,
		proxy.FaultSpec{Match: proxy.RequestMatcher{Resource: "configmaps"}, Action: proxy.Delay{For: longer}},
		proxy.FaultSpec{Match: proxy.RequestMatcher{Resource: "secrets"}, Action: proxy.Delay{For: shorter}})

	answered := make(chan struct{})
	go func() {
		defer close(answered)
		if resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps"); err == nil {
			resp.Body.Close()
		}
	}()
	arrived := arrival(t, p)
	do(t, p, "GET", "/api/v1/namespaces/ns1/secrets", nil)
	<-answered

	if holding, released := p.Held(time.Now()); holding || released.Before(arrived.Add(longer)) {
		t.Errorf("The proxy reports (%t, %v), want no request held and a release %v after %v.", holding, released, longer, arrived)
	}
}

func TestADelayThatStillAppliesStaysOpenOnceItReleasesARequest(t *testing.T) {
	p := faultedProxy(t, 0)
	id := p.AddFault(proxy.FaultSpec{Action: proxy.Delay{For: time.Millisecond}})

	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	if window := p.Window(id); window.First.IsZero() || !window.Retired.IsZero() {
		t.Errorf("The delay ran %+v, want it applied and still applying.", window)
	}
}

// arrival waits for the proxy to record the one request a test sent.
func arrival(t *testing.T, p *proxy.Proxy) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.Log()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("The request never reached the proxy.")
		}
		time.Sleep(time.Millisecond)
	}
	return onlyRequest(t, p).Start
}

func TestDropClosesTheConnectionWithoutAResponse(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "configmaps"},
		Action: proxy.Drop{},
	})

	_, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps")

	if err == nil {
		t.Error("The client got a response, want a closed connection.")
	}
	recorded := onlyRequest(t, p)
	if recorded.Status != 0 || recorded.Fault != "drop" {
		t.Errorf("The dropped request is recorded as %+v, want status 0 and fault drop.", recorded)
	}
}

// faultPattern reports which of n sequential requests the proxy faulted.
func faultPattern(t *testing.T, seed int64, fraction float64, n int) []bool {
	t.Helper()
	p := faultedProxy(t, seed, proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Verb: "get", Resource: "configmaps", Fraction: fraction},
		Action: proxy.Error{Code: http.StatusInternalServerError},
	})
	for i := range n {
		do(t, p, "GET", fmt.Sprintf("/api/v1/namespaces/ns1/configmaps/cm%d", i), nil)
	}
	pattern := make([]bool, 0, n)
	for _, r := range p.Log() {
		pattern = append(pattern, r.Fault != "")
	}
	return pattern
}

func TestFractionFaultsAShareOfMatchingRequestsPerSeed(t *testing.T) {
	const n = 100
	half := faultPattern(t, 1, 0.5, n)

	faulted := 0
	for _, f := range half {
		if f {
			faulted++
		}
	}
	if faulted < n/4 || faulted > 3*n/4 {
		t.Errorf("A fraction of 0.5 faulted %d of %d requests.", faulted, n)
	}
	if replayed := faultPattern(t, 1, 0.5, n); !slices.Equal(half, replayed) {
		t.Error("The same seed faulted a different set of requests.")
	}
	if other := faultPattern(t, 2, 0.5, n); slices.Equal(half, other) {
		t.Error("A different seed faulted the same set of requests.")
	}
	for i, f := range faultPattern(t, 1, 0, n) {
		if !f {
			t.Fatalf("An unset fraction skipped request %d.", i)
		}
	}
}

func TestEachFaultDrawsItsOwnFraction(t *testing.T) {
	half := func(resource string) proxy.FaultSpec {
		return proxy.FaultSpec{
			Match:  proxy.RequestMatcher{Resource: resource, Fraction: 0.5},
			Action: proxy.Error{Code: http.StatusInternalServerError},
		}
	}
	p := faultedProxy(t, 1, half("configmaps"), half("secrets"))

	const n = 20
	for i := range n {
		do(t, p, "GET", fmt.Sprintf("/api/v1/namespaces/ns1/configmaps/cm%d", i), nil)
		do(t, p, "GET", fmt.Sprintf("/api/v1/namespaces/ns1/secrets/s%d", i), nil)
	}

	faulted := map[string][]bool{}
	for _, r := range p.Log() {
		faulted[r.Resource] = append(faulted[r.Resource], r.Fault != "")
	}
	if slices.Equal(faulted["configmaps"], faulted["secrets"]) {
		t.Errorf("Both faults faulted the requests %v, want each to draw its own.", faulted["configmaps"])
	}
}

func TestUntilCountEndsTheFault(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "configmaps"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{Count: 2},
	})

	for range 3 {
		do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	}

	var statuses []int
	for _, r := range p.Log() {
		statuses = append(statuses, r.Status)
	}
	if want := []int{500, 500, 200}; !slices.Equal(statuses, want) {
		t.Errorf("The statuses were %v, want %v.", statuses, want)
	}
}

func TestUntilDurationEndsTheFault(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{For: 50 * time.Millisecond},
	})

	first := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	time.Sleep(60 * time.Millisecond)
	second := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	if first.StatusCode != http.StatusInternalServerError {
		t.Errorf("The first request got %d, want the fault's 500.", first.StatusCode)
	}
	if second.StatusCode != http.StatusOK {
		t.Errorf("A request after the fault's duration got %d, want 200.", second.StatusCode)
	}
}

func TestACountSpentBeforeTheDurationEndsTheFault(t *testing.T) {
	p := faultedProxy(t, 0)
	id := p.AddFault(proxy.FaultSpec{
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{Count: 1, For: 50 * time.Millisecond},
	})

	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	spent := p.Window(id).Retired
	time.Sleep(60 * time.Millisecond)

	if got := p.Window(id).Retired; spent.IsZero() || !got.Equal(spent) {
		t.Errorf("The fault retired at %v once spent and at %v once its duration passed, want the moment it was spent.", spent, got)
	}
}

func TestClearFaultsRestoresTheUpstream(t *testing.T) {
	spec := proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}}
	p := faultedProxy(t, 0, spec, spec)

	p.ClearFaults()
	resp := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("A request after ClearFaults got %d, want 200.", resp.StatusCode)
	}
	if got := onlyRequest(t, p).Fault; got != "" {
		t.Errorf("The request records fault %q, want none.", got)
	}
}

func TestRemoveFaultStopsOnlyTheFaultItNames(t *testing.T) {
	spec := proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}}
	p := faultedProxy(t, 0)
	first, second := p.AddFault(spec), p.AddFault(spec)

	p.RemoveFault(second)
	faulted := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	p.RemoveFault(first)
	restored := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)

	if faulted.StatusCode != http.StatusInternalServerError || p.Window(first).First.IsZero() {
		t.Errorf("A request after removing the second fault got %d, and the first fault ran %+v, want the first fault applied.",
			faulted.StatusCode, p.Window(first))
	}
	if restored.StatusCode != http.StatusOK {
		t.Errorf("A request after removing both faults got %d, want 200.", restored.StatusCode)
	}
}

// A removed fault keeps its window, which ends where it was removed.
func TestRemovingAFaultClosesItsWindow(t *testing.T) {
	error500 := func(resource string) proxy.FaultSpec {
		return proxy.FaultSpec{Match: proxy.RequestMatcher{Resource: resource}, Action: proxy.Error{Code: http.StatusInternalServerError}}
	}
	p := faultedProxy(t, 0)
	removed, cleared := p.AddFault(error500("configmaps")), p.AddFault(error500("secrets"))
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	do(t, p, "GET", "/api/v1/namespaces/ns1/secrets", nil)

	before := time.Now()
	p.RemoveFault(removed)
	atRemoval := p.Window(removed)
	p.ClearFaults()

	if atRemoval.First.IsZero() || atRemoval.Retired.Before(before) {
		t.Errorf("The removed fault ran %+v, want the request it faulted and an end where it was removed, after %v.", atRemoval, before)
	}
	if got := p.Window(removed); got != atRemoval {
		t.Errorf("ClearFaults moved the window of a removed fault from %+v to %+v.", atRemoval, got)
	}
	if got := p.Window(cleared); got.First.IsZero() || got.Retired.Before(atRemoval.Retired) {
		t.Errorf("The cleared fault ran %+v, want the request it faulted and an end where it was cleared.", got)
	}
}

// Requests race the removal, and the fault's window still holds every request
// the proxy faulted with it.
func TestAFaultsWindowHoldsTheRequestsThatRaceItsRemoval(t *testing.T) {
	for name, remove := range map[string]func(*proxy.Proxy, proxy.FaultID){
		"RemoveFault": (*proxy.Proxy).RemoveFault,
		"ClearFaults": func(p *proxy.Proxy, _ proxy.FaultID) { p.ClearFaults() },
	} {
		t.Run(name, func(t *testing.T) {
			p := faultedProxy(t, 0)
			id := p.AddFault(proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}})
			answered, removed := make(chan struct{}, 1), make(chan struct{})
			var clients sync.WaitGroup
			for range 4 {
				clients.Go(func() {
					for {
						resp, err := http.Get(p.URL() + "/api/v1/namespaces/ns1/configmaps")
						if err != nil {
							t.Error(err)
						} else {
							io.Copy(io.Discard, resp.Body)
							resp.Body.Close()
						}
						select {
						case answered <- struct{}{}:
						default:
						}
						select {
						case <-removed:
							return
						default:
						}
					}
				})
			}

			<-answered
			remove(p, id)
			close(removed)
			clients.Wait()

			retired := p.Window(id).Retired
			for _, r := range p.Log() {
				if r.Fault != "" && r.Start.After(retired) {
					t.Fatalf("The proxy faulted a request that arrived at %v, after the fault retired at %v.", r.Start, retired)
				}
			}
		})
	}
}

func TestTheFirstMatchingFaultWins(t *testing.T) {
	p := faultedProxy(t, 0,
		proxy.FaultSpec{Match: proxy.RequestMatcher{Verb: "create"}, Action: proxy.Error{Code: http.StatusConflict}},
		proxy.FaultSpec{Action: proxy.Error{Code: http.StatusInternalServerError}})

	resp := do(t, p, "POST", "/api/v1/namespaces/ns1/configmaps", nil)

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("The client saw %d, want the first matching fault's 409.", resp.StatusCode)
	}
}

func TestUpgradeRequestsAreNeverFaulted(t *testing.T) {
	p := faultedProxy(t, 0, proxy.FaultSpec{Action: proxy.Drop{}})

	resp := do(t, p, "GET", "/api/v1/namespaces/ns1/pods/p1/exec", http.Header{
		"Connection": {"Upgrade"},
		"Upgrade":    {"SPDY/3.1"}})

	if resp.StatusCode != http.StatusOK {
		t.Errorf("An upgrade request got %d, want the upstream's 200.", resp.StatusCode)
	}
}

// The Runner reads Window to bound a fault's excuse: it runs from the first
// request the proxy faulted to the one that spent the trigger (§5.2, §6).
func TestWindowNamesWhatTheProxyDidWithEachFault(t *testing.T) {
	p := faultedProxy(t, 0)
	counted := p.AddFault(proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "configmaps"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{Count: 1},
	})
	timed := p.AddFault(proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "secrets"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{For: 50 * time.Millisecond},
	})
	endless := p.AddFault(proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "widgets"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
	})

	if window := p.Window(counted); !window.First.IsZero() {
		t.Fatalf("Window says %v before any request, and no fault has been applied yet.", window)
	}
	spent := time.Now()
	do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	time.Sleep(60 * time.Millisecond)

	if window := p.Window(counted); window.First.Before(spent) || window.Retired.Before(spent) {
		t.Errorf("The count-of-1 fault ran %+v, want it applied and spent on the request after %v.", window, spent)
	}
	window := p.Window(timed)
	if !window.First.IsZero() {
		t.Errorf("The fault matching secrets was applied at %v, and the request was for configmaps.", window.First)
	}
	if window.Retired.IsZero() || window.Retired.After(time.Now()) {
		t.Errorf("The 50ms fault retired at %v, want the moment its window closed.", window.Retired)
	}
	if window := p.Window(endless); !window.First.IsZero() || !window.Retired.IsZero() {
		t.Errorf("The fault with no trigger ran %+v, and no request matched it.", window)
	}

	spentAt, timedOut := p.Window(counted).Retired, p.Window(timed).Retired
	p.ClearFaults()
	if got := p.Window(counted).Retired; !got.Equal(spentAt) {
		t.Errorf("Clearing a spent fault moved its end from %v to %v.", spentAt, got)
	}
	if got := p.Window(timed).Retired; !got.Equal(timedOut) {
		t.Errorf("Clearing a fault whose time ran out moved its end from %v to %v.", timedOut, got)
	}
}

// Two fault ops can inject equal specs. Removing the one that ran out leaves
// the other applying, with its own window.
func TestRemovingAFaultLeavesAnEqualOneApplying(t *testing.T) {
	spec := proxy.FaultSpec{
		Match:  proxy.RequestMatcher{Resource: "configmaps"},
		Action: proxy.Error{Code: http.StatusInternalServerError},
		Until:  proxy.Trigger{Count: 3},
	}
	p := faultedProxy(t, 0)
	spent, applying := p.AddFault(spec), p.AddFault(spec)
	for range 4 {
		do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil)
	}
	own := p.Window(applying)

	p.RemoveFault(spent)

	if got := p.Window(applying); got != own || !got.Retired.IsZero() {
		t.Errorf("The remaining fault ran %+v, want its own window %+v, still open.", got, own)
	}
	if resp := do(t, p, "GET", "/api/v1/namespaces/ns1/configmaps", nil); resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("A request after the removal got %d, want the remaining fault's 500.", resp.StatusCode)
	}
}
