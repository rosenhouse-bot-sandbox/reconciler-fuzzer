package trace_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

func TestLooks(t *testing.T) {
	const ms = time.Millisecond
	pw, other := tt.Ref("secrets", "ns", "pw"), tt.Ref("secrets", "ns", "other")
	secrets, bucket := tt.Ref("secrets", "ns", ""), trace.Ref{System: "cloud", Resource: "buckets", Name: "b"}
	call := func(caller trace.Caller, verb string, ref trace.Ref, status int, change func(*trace.Call)) trace.Event {
		e := tt.Call(caller, verb, ref, status, "")
		e.Call.Latency = 100 * ms
		if change != nil {
			change(e.Call)
		}
		return e
	}
	list := func(ref trace.Ref, change func(*trace.Call)) trace.Event {
		return call(trace.Controller, "list", ref, 200, change)
	}
	watched := func(e trace.Event) trace.Event { e.Seen.Stream = 3; return e }
	deleted := func(e trace.Event) trace.Event { e.Seen.Deleted = true; return e }
	run := trace.Event{Run: &trace.Run{References: []trace.Reference{
		{From: trace.Kind{Group: "toy", Resource: "databases"}, To: trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}, Namespaced: true, Name: "spec.secret", Ready: "true"},
		{From: trace.Kind{Group: "toy", Resource: "databases"}, System: "cloud", To: trace.Kind{Resource: "buckets"}, Name: "spec.bucket", Ready: "true"},
		{From: trace.Kind{Group: "toy", Resource: "databases"}, System: "cloud", To: trace.Kind{Resource: "configmaps"}, Name: "spec.settings", Ready: "true"},
	}}}
	var b tt.Builder
	b.Add(run).
		At(1*time.Second).Add(tt.Seen(trace.Controller, pw, "u1", "1"), tt.Seen(trace.Harness, pw, "u1", "1")).
		At(2*time.Second).Add(call(trace.Controller, "get", pw, 404, nil), call(trace.Controller, "get", other, 404, nil)).
		At(2950*ms).Add(tt.Seen(trace.Controller, other, "u2", "1"), trace.Event{Seen: &trace.Seen{Caller: trace.Controller, Ref: tt.Ref("secrets", "elsewhere", ""), Opaque: true}}).
		At(3*time.Second).Add(list(secrets, nil)).
		At(3950*ms).Add(tt.Seen(trace.Controller, pw, "u1", "1")).
		At(4*time.Second).Add(list(secrets, nil)).
		At(5*time.Second).Add(list(secrets, func(c *trace.Call) { c.Partial = true }),
		list(tt.Ref("configmaps", "ns", ""), nil), list(tt.Ref("other/secrets", "ns", ""), nil), list(tt.Ref("secrets", "elsewhere", ""), nil),
		list(secrets, func(c *trace.Call) { c.Injected = trace.DropResponse }), list(secrets, func(c *trace.Call) { c.Status, c.Injected = 0, "drop" }),
		list(secrets, func(c *trace.Call) { c.Method = "HEAD" }),
		call(trace.Controller, "get", pw, 404, func(c *trace.Call) { c.Injected = trace.KillAfter }),
		call(trace.Controller, "get", pw, 404, func(c *trace.Call) { c.Subresource = "status" }),
		call(trace.Controller, "delete", pw, 404, nil), call(trace.Controller, "get", pw, 500, nil),
		call(trace.Controller, "list", secrets, 500, nil), call(trace.Harness, "list", secrets, 200, nil),
		call(trace.Controller, "watch", secrets, 200, func(c *trace.Call) { c.Watch = true })).
		At(5950*ms).Add(watched(tt.Seen(trace.Controller, pw, "u1", "1"))).
		At(6*time.Second).Add(list(secrets, nil)).
		At(7*time.Second).Add(watched(deleted(tt.Seen(trace.Controller, pw, "u1", "2"))), trace.Event{Seen: &trace.Seen{Caller: trace.Controller, Ref: secrets, Opaque: true}}).
		At(8*time.Second).Add(list(tt.Ref("secrets", "", ""), nil)).
		At(8550*ms).Add(trace.Event{Seen: &trace.Seen{Caller: trace.Controller, Ref: secrets, Opaque: true}}).
		At(8600*ms).Add(list(secrets, nil)).
		At(9*time.Second).Add(call(trace.Controller, "get", bucket, 404, nil), list(trace.Ref{System: "cloud", Resource: "buckets"}, nil))
	ix := b.Index()
	describe := func(looks []trace.Event) []string {
		var described []string
		for _, e := range looks {
			switch {
			case e.Seen != nil && e.Seen.Deleted:
				described = append(described, fmt.Sprintf("%v deleted", e.At))
			case e.Seen != nil:
				described = append(described, fmt.Sprintf("%v seen", e.At))
			default:
				described = append(described, fmt.Sprintf("%v %s %s %d", e.At, e.Call.Verb, e.Call.Ref.Namespace, e.Call.Status))
			}
		}
		return described
	}
	want := []string{"1s seen", "2s get ns 404", "3s list ns 200", "3.95s seen", "5.95s seen", "6s list ns 200", "7s deleted", "8s list  200"}
	if got := describe(ix.Looks(trace.Controller, pw)); !slices.Equal(got, want) {
		t.Errorf("Looks(controller, pw) = %q, want %q.", got, want)
	}
	if got, want := describe(ix.Looks(trace.Controller, bucket)), []string{"9s get  404"}; !slices.Equal(got, want) {
		t.Errorf("Looks(controller, bucket) = %q, want %q, as a fake's list shows nothing missing.", got, want)
	}
	for _, ref := range []trace.Ref{tt.Ref("configmaps", "ns", "c"), tt.Ref("other/secrets", "ns", "pw")} {
		if got := ix.Looks(trace.Controller, ref); len(got) != 0 {
			t.Errorf("Looks(controller, %v) = %q, want none, as no reference names its kube kind.", ref, describe(got))
		}
	}
	if got, want := describe(ix.Looks(trace.Harness, pw)), []string{"1s seen", "5s list ns 200"}; !slices.Equal(got, want) {
		t.Errorf("Looks(harness, pw) = %q, want %q.", got, want)
	}
}
