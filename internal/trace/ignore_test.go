package trace_test

import (
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestIgnored(t *testing.T) {
	for _, c := range []struct {
		group, resource string
		ignored         bool
	}{
		{"", "events", true},
		{"events.k8s.io", "events", true},
		{"coordination.k8s.io", "leases", true},
		{"authentication.k8s.io", "tokenreviews", true},
		{"authorization.k8s.io", "subjectaccessreviews", true},
		{"authorization.k8s.io", "selfsubjectaccessreviews", true},
		{"authorization.k8s.io", "localsubjectaccessreviews", true},
		{"authorization.k8s.io", "selfsubjectrulesreviews", true},
		{"", "", true},
		{"", "configmaps", false},
		{"apps", "events", false},
		{"coordination.k8s.io", "leasecandidates", false},
		{"toy.reconciler-fuzzer", "widgets", false},
	} {
		call := trace.Call{Ref: trace.Ref{System: "kube", Group: c.group, Resource: c.resource}}
		if got := trace.Ignored(call); got != c.ignored {
			t.Errorf("Ignored(%s/%s) = %v, want %v", c.group, c.resource, got, c.ignored)
		}
	}
}

func TestIgnoredListsOnlyKubeKinds(t *testing.T) {
	if trace.Ignored(trace.Call{Ref: trace.Ref{System: "cloud", Resource: "events"}}) {
		t.Error("a fake's events collection is ignored")
	}
	if !trace.Ignored(trace.Call{Ref: trace.Ref{System: "cloud"}, Unmapped: true}) {
		t.Error("an unmapped request is judged")
	}
}
