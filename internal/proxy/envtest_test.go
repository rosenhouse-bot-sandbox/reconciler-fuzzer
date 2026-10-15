//go:build envtest

package proxy_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/cluster"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

const namespace = "default"

func configMap(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// recordsAfter waits for the count records the proxy appended after mark to
// finish. The client sees its response before the proxy completes the record,
// so a test that asserts on timing waits for it (see Proxy.Log).
func recordsAfter(t *testing.T, p *proxy.Proxy, mark, count int) []proxy.Request {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		records := p.Log()[mark:]
		if len(records) >= count && !slices.ContainsFunc(records, func(r proxy.Request) bool {
			return r.Latency <= 0
		}) {
			return records
		}
		if time.Now().After(deadline) {
			t.Fatalf("The proxy recorded %+v, want %d finished records.", records, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireRecords compares the recorded requests with want, ignoring the
// timing fields, which it checks are set.
func requireRecords(t *testing.T, got, want []proxy.Request) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("The log holds %d requests, want %d: %+v", len(got), len(want), got)
	}

	for i, record := range got {
		if record.Start.IsZero() || record.Latency <= 0 {
			t.Errorf("Request %d is recorded without timing: %+v", i, record)
		}
		record.Start, record.Latency = time.Time{}, 0
		if record != want[i] {
			t.Errorf("Request %d is recorded as\n\t%+v\nwant\n\t%+v", i, record, want[i])
		}
	}
}

// TestProxyInFrontOfTheAPIServer runs its cases in order against one control
// plane, because each start costs seconds.
func TestProxyInFrontOfTheAPIServer(t *testing.T) {
	ctx := t.Context()

	c, err := cluster.Start(cluster.Options{})
	if err != nil {
		t.Fatalf("Starting the test cluster failed: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stopping the test cluster failed: %v", err)
		}
	})

	p, err := proxy.Start(c.Config(), proxy.Options{})
	if err != nil {
		t.Fatalf("Starting the proxy failed: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Stop(); err != nil {
			t.Errorf("Stopping the proxy failed: %v", err)
		}
	})

	// The target's view: the proxy URL and no credentials of its own.
	client, err := kubernetes.NewForConfig(&rest.Config{Host: p.URL()})
	if err != nil {
		t.Fatalf("Building a clientset for the proxy failed: %v", err)
	}
	configMaps := client.CoreV1().ConfigMaps(namespace)

	t.Run("records a create and a get", func(t *testing.T) {
		mark := len(p.Log())

		if _, err := configMaps.Create(ctx, configMap("recorded"), metav1.CreateOptions{}); err != nil {
			t.Fatalf("Creating a ConfigMap through the proxy failed: %v", err)
		}
		if _, err := configMaps.Get(ctx, "recorded", metav1.GetOptions{}); err != nil {
			t.Fatalf("Getting a ConfigMap through the proxy failed: %v", err)
		}

		const collection = "/api/v1/namespaces/default/configmaps"
		requireRecords(t, recordsAfter(t, p, mark, 2), []proxy.Request{
			{Verb: "create", Version: "v1", Resource: "configmaps", Namespace: namespace,
				Path: collection, Status: 201},
			{Verb: "get", Version: "v1", Resource: "configmaps", Namespace: namespace, Name: "recorded",
				Path: collection + "/recorded", Status: 200},
		})
	})

	t.Run("streams a watch", func(t *testing.T) {
		mark := len(p.Log())

		watch, err := configMaps.Watch(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Starting a watch through the proxy failed: %v", err)
		}
		defer watch.Stop()
		if _, err := configMaps.Create(ctx, configMap("watched"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}

		deadline := time.After(10 * time.Second)
		for {
			select {
			case event := <-watch.ResultChan():
				object, ok := event.Object.(*corev1.ConfigMap)
				if ok && object.Name == "watched" {
					requireWatchRecorded(t, p.Log()[mark:])
					return
				}
			case <-deadline:
				t.Fatal("The watch event did not arrive; the proxy buffered the stream.")
			}
		}
	})

	t.Run("injects an error the target decodes", func(t *testing.T) {
		mark := len(p.Log())
		p.AddFault(proxy.FaultSpec{
			Match:  proxy.RequestMatcher{Verb: "create", Resource: "configmaps"},
			Action: proxy.Error{Code: 500},
		})
		defer p.ClearFaults()

		_, err := configMaps.Create(ctx, configMap("faulted"), metav1.CreateOptions{})

		var status apierrors.APIStatus
		if !errors.As(err, &status) {
			t.Fatalf("Creating a ConfigMap returned %v, want a decodable status error.", err)
		}
		if status.Status().Code != 500 || status.Status().Reason != metav1.StatusReasonInternalError {
			t.Errorf("The target decoded %+v, want a 500 InternalError.", status.Status())
		}
		if want := "reconciler-fuzzer fault: create /api/v1/namespaces/default/configmaps"; status.Status().Message != want {
			t.Errorf("The target decoded the message %q, want %q.", status.Status().Message, want)
		}
		requireRecords(t, recordsAfter(t, p, mark, 1), []proxy.Request{
			{Verb: "create", Version: "v1", Resource: "configmaps", Namespace: namespace,
				Path: "/api/v1/namespaces/default/configmaps", Status: 500, Fault: "error(500)"},
		})
		if _, err := configMaps.Get(ctx, "faulted", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("Getting the faulted ConfigMap returned %v, want NotFound: the create reached the API server.", err)
		}
	})

	t.Run("injects an error a protobuf client decodes", func(t *testing.T) {
		protobuf, err := kubernetes.NewForConfig(&rest.Config{
			Host: p.URL(),
			ContentConfig: rest.ContentConfig{
				AcceptContentTypes: "application/vnd.kubernetes.protobuf,application/json",
				ContentType:        "application/vnd.kubernetes.protobuf",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		p.AddFault(proxy.FaultSpec{
			Match:  proxy.RequestMatcher{Verb: "create"},
			Action: proxy.Error{Code: 500},
		})
		defer p.ClearFaults()

		_, err = protobuf.CoreV1().ConfigMaps(namespace).Create(ctx, configMap("protobuf"), metav1.CreateOptions{})

		var status apierrors.APIStatus
		if !errors.As(err, &status) {
			t.Fatalf("A protobuf client got %v, want a decodable status error.", err)
		}
		if want := "reconciler-fuzzer fault: create /api/v1/namespaces/default/configmaps"; status.Status().Message != want {
			t.Errorf("A protobuf client decoded the message %q, want %q.", status.Status().Message, want)
		}
	})

	t.Run("delays a matched request", func(t *testing.T) {
		const delay = 500 * time.Millisecond
		p.AddFault(proxy.FaultSpec{
			Match:  proxy.RequestMatcher{Verb: "get", Resource: "configmaps", Name: "recorded"},
			Action: proxy.Delay{For: delay},
		})
		defer p.ClearFaults()

		start := time.Now()
		if _, err := configMaps.Get(ctx, "recorded", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < delay {
			t.Errorf("The delayed get took %v, want at least %v.", elapsed, delay)
		}
	})

	t.Run("serves normally once the faults are cleared", func(t *testing.T) {
		p.AddFault(proxy.FaultSpec{Action: proxy.Error{Code: 500}})
		p.ClearFaults()
		mark := len(p.Log())

		if _, err := configMaps.Create(ctx, configMap("cleared"), metav1.CreateOptions{}); err != nil {
			t.Fatalf("Creating a ConfigMap after ClearFaults failed: %v", err)
		}

		requireRecords(t, recordsAfter(t, p, mark, 1), []proxy.Request{
			{Verb: "create", Version: "v1", Resource: "configmaps", Namespace: namespace,
				Path: "/api/v1/namespaces/default/configmaps", Status: 201},
		})
	})
}

func requireWatchRecorded(t *testing.T, records []proxy.Request) {
	t.Helper()
	var watches []proxy.Request
	for _, record := range records {
		if record.Watch {
			watches = append(watches, record)
		}
	}
	if len(watches) != 1 {
		t.Fatalf("The log holds %d watches, want 1: %+v", len(watches), records)
	}
	watched := watches[0]
	if watched.Verb != "watch" || watched.Resource != "configmaps" || watched.Namespace != namespace {
		t.Errorf("The watch is recorded as %+v, want a watch of configmaps in %s.", watched, namespace)
	}
	if watched.Status != 200 {
		t.Errorf("The open watch is recorded with status %d, want 200.", watched.Status)
	}
	if watched.Latency != 0 {
		t.Errorf("The open watch is recorded with latency %v, want 0 until it ends.", watched.Latency)
	}
}
