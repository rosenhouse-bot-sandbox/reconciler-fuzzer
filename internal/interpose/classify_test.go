package interpose_test

import (
	"net/http/httptest"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

type ref = trace.Ref

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		method, target string
		want           interpose.Request
	}{
		{"GET", "/api/v1/namespaces/ns1/configmaps", interpose.Request{
			Verb: "list", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1"}}},
		{"GET", "/api/v1/namespaces/ns1/configmaps/cm1", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}}},
		{"GET", "/api/v1/namespaces/ns1/configmaps?resourceVersion=7&watch=true", interpose.Request{
			Verb: "watch", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1"}, Watch: true}},
		{"GET", "/api/v1/namespaces/ns1/configmaps/cm1?watch=true", interpose.Request{
			Verb: "watch", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}, Watch: true}},
		{"GET", "/api/v1/namespaces/ns1/configmaps?watch=false", interpose.Request{
			Verb: "list", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1"}}},
		{"HEAD", "/api/v1/namespaces/ns1/configmaps/cm1", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}}},
		{"POST", "/api/v1/namespaces/ns1/configmaps", interpose.Request{
			Verb: "create", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1"}}},
		{"PUT", "/api/v1/namespaces/ns1/configmaps/cm1", interpose.Request{
			Verb: "update", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}}},
		{"PATCH", "/api/v1/namespaces/ns1/configmaps/cm1", interpose.Request{
			Verb: "patch", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}}},
		{"DELETE", "/api/v1/namespaces/ns1/configmaps/cm1", interpose.Request{
			Verb: "delete", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1", Name: "cm1"}}},
		{"DELETE", "/api/v1/namespaces/ns1/configmaps", interpose.Request{
			Verb: "deletecollection", APIVersion: "v1", Ref: ref{Resource: "configmaps", Namespace: "ns1"}}},
		{"GET", "/api/v1/configmaps", interpose.Request{
			Verb: "list", APIVersion: "v1", Ref: ref{Resource: "configmaps"}}},
		{"GET", "/api/v1/nodes/node1", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Resource: "nodes", Name: "node1"}}},
		{"GET", "/api/v1/namespaces", interpose.Request{
			Verb: "list", APIVersion: "v1", Ref: ref{Resource: "namespaces"}}},
		{"GET", "/api/v1/namespaces/ns1", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Resource: "namespaces", Name: "ns1"}}},
		{"PUT", "/api/v1/namespaces/ns1/finalize", interpose.Request{
			Verb: "update", APIVersion: "v1", Ref: ref{Resource: "namespaces", Name: "ns1"}, Subresource: "finalize"}},
		{"PATCH", "/api/v1/namespaces/ns1/status", interpose.Request{
			Verb: "patch", APIVersion: "v1", Ref: ref{Resource: "namespaces", Name: "ns1"}, Subresource: "status"}},
		{"GET", "/api/v1/namespaces/ns1/pods/p1/log", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Resource: "pods", Namespace: "ns1", Name: "p1"}, Subresource: "log"}},
		{"POST", "/api/v1/namespaces/ns1/serviceaccounts/sa1/token", interpose.Request{
			Verb: "create", APIVersion: "v1", Ref: ref{Resource: "serviceaccounts", Namespace: "ns1", Name: "sa1"}, Subresource: "token"}},
		{"GET", "/apis/apps/v1/namespaces/ns1/deployments/d1", interpose.Request{
			Verb: "get", APIVersion: "v1", Ref: ref{Group: "apps", Resource: "deployments", Namespace: "ns1", Name: "d1"}}},
		{"PUT", "/apis/apps/v1/namespaces/ns1/deployments/d1/status", interpose.Request{
			Verb: "update", APIVersion: "v1", Ref: ref{Group: "apps", Resource: "deployments", Namespace: "ns1", Name: "d1"}, Subresource: "status"}},
		{"PATCH", "/apis/toy.reconciler-fuzzer/v1/namespaces/ns1/widgets/w1/status", interpose.Request{
			Verb: "patch", APIVersion: "v1", Ref: ref{Group: "toy.reconciler-fuzzer", Resource: "widgets", Namespace: "ns1", Name: "w1"}, Subresource: "status"}},
		{"GET", "/apis/toy.reconciler-fuzzer/v1/widgets", interpose.Request{
			Verb: "list", APIVersion: "v1", Ref: ref{Group: "toy.reconciler-fuzzer", Resource: "widgets"}}},
		{"GET", "/api/v1", interpose.Request{Verb: "get", APIVersion: "v1"}},
		{"GET", "/apis/apps/v1", interpose.Request{Verb: "get", APIVersion: "v1", Ref: ref{Group: "apps"}}},
		{"GET", "/apis", interpose.Request{Verb: "get"}},
		{"GET", "/healthz", interpose.Request{Verb: "get"}},
		{"POST", "/fancy", interpose.Request{Verb: "post"}},
	} {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			got := interpose.Classify(httptest.NewRequest(c.method, c.target, nil))
			if got != c.want {
				t.Errorf("Classify gave\n\t%+v\nwant\n\t%+v", got, c.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	r := interpose.Request{Verb: "patch", Ref: ref{Group: "apps", Resource: "deployments", Name: "d1"}, Subresource: "status"}
	for _, c := range []struct {
		name  string
		match plan.Match
		want  bool
	}{
		{"empty", plan.Match{}, true},
		{"every field", plan.Match{Verb: "patch", Group: "apps", Resource: "deployments", Subresource: "status", Name: "d1"}, true},
		{"verb", plan.Match{Verb: "get"}, false},
		{"group", plan.Match{Group: "batch"}, false},
		{"resource", plan.Match{Resource: "pods"}, false},
		{"subresource", plan.Match{Subresource: "scale"}, false},
		{"name", plan.Match{Name: "d2"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := interpose.Matches(c.match, r); got != c.want {
				t.Errorf("Matches(%+v) = %v, want %v", c.match, got, c.want)
			}
		})
	}
}

func TestMatchesNoSubresource(t *testing.T) {
	m := plan.Match{Verb: "update", Resource: "clusters", Subresource: "-"}
	for _, c := range []struct {
		subresource string
		want        bool
	}{
		{"", true},
		{"status", false},
		{"-", false},
	} {
		r := interpose.Request{Verb: "update", Ref: ref{Group: "toy", Resource: "clusters", Name: "c"}, Subresource: c.subresource}
		if got := interpose.Matches(m, r); got != c.want {
			t.Errorf("Matches(%+v) of subresource %q = %v, want %v.", m, c.subresource, got, c.want)
		}
	}
}
