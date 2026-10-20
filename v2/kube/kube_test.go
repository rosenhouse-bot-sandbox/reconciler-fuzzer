package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

func TestReadRBACSplitsRolesAndClusterRoles(t *testing.T) {
	roles, clusterRoles, err := readRBAC([]string{"testdata/rbac.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0].Name != "writer" || roles[0].Rules[0].Verbs[0] != "create" {
		t.Errorf("Roles are %+v, want writer.", roles)
	}
	if len(clusterRoles) != 1 || clusterRoles[0].Name != "reader" {
		t.Errorf("ClusterRoles are %+v, want reader.", clusterRoles)
	}
}

func TestReadRBACRejects(t *testing.T) {
	for file, want := range map[string]string{
		"not-rbac.yaml":            "v1 ConfigMap is not",
		"role-with-namespace.yaml": "metadata.namespace",
		"role-misspelled.yaml":     "rulez",
		"missing.yaml":             "no such file",
	} {
		_, _, err := readRBAC([]string{"testdata/" + file})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("readRBAC(%s) = %v, want an error naming %q.", file, err, want)
		}
	}
}

func TestReadObjectsReadsEveryDocument(t *testing.T) {
	objects, err := readObjects([]string{"testdata/objects.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objects {
		got = append(got, o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName())
	}
	if strings.Join(got, " ") != "ConfigMap//one Widget/elsewhere/two" {
		t.Errorf("Objects are %v.", got)
	}
}

func TestReadObjectsRejectsAnObjectWithNoKind(t *testing.T) {
	_, err := readObjects([]string{"testdata/kindless.yaml"})
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("readObjects(kindless.yaml) = %v, want an error naming kind.", err)
	}
}

func TestNamespaceNameIsRFAndEightHex(t *testing.T) {
	a, b := newNamespaceName(), newNamespaceName()
	format := regexp.MustCompile(`^rf-[0-9a-f]{8}$`)
	if !format.MatchString(a) || a == b {
		t.Errorf("Namespace names are %q and %q.", a, b)
	}
}

func TestObserverRecordsEachVersionOnce(t *testing.T) {
	var versions []trace.Version
	o := &observer{record: func(e trace.Event) { versions = append(versions, *e.Version) }, seen: map[seenKey]bool{}, gone: map[trace.Ref]bool{}}
	gr := schema.GroupResource{Group: "toy.reconciler-fuzzer", Resource: "widgets"}
	w := &unstructured.Unstructured{}
	w.SetNamespace("rf-1")
	w.SetName("w")
	w.SetUID("u")
	w.SetResourceVersion("5")

	o.see(gr, w, false)
	o.see(gr, w, false)
	o.see(gr, w, true)
	w.SetResourceVersion("6")
	o.see(gr, w, false)

	if len(versions) != 3 {
		t.Fatalf("Recorded %d versions, want 3: %+v", len(versions), versions)
	}
	want := trace.Ref{System: "kube", Group: gr.Group, Resource: gr.Resource, Namespace: "rf-1", Name: "w"}
	first := versions[0]
	if first.Ref != want || first.UID != "u" || first.RV != "5" || first.Deleted {
		t.Errorf("First version is %+v.", first)
	}
	var body map[string]any
	if err := json.Unmarshal(first.Body, &body); err != nil || body["metadata"].(map[string]any)["name"] != "w" {
		t.Errorf("Body is %s.", first.Body)
	}
	if !versions[1].Deleted || versions[2].RV != "6" {
		t.Errorf("Versions are %+v.", versions)
	}
}

func TestObserverKnowsWhatItRecorded(t *testing.T) {
	o := &observer{record: func(trace.Event) {}, seen: map[seenKey]bool{}, gone: map[trace.Ref]bool{}}
	gr := schema.GroupResource{Resource: "configmaps"}
	ref := trace.Ref{System: "kube", Resource: "configmaps", Namespace: "rf-1", Name: "c"}
	c := &unstructured.Unstructured{}
	c.SetNamespace("rf-1")
	c.SetName("c")
	c.SetResourceVersion("5")
	o.see(gr, c, false)
	if !o.recorded(ref, "5") || o.recorded(ref, "6") || o.recordedGone(ref) {
		t.Errorf("After version 5: recorded(5) %v, recorded(6) %v, gone %v.", o.recorded(ref, "5"), o.recorded(ref, "6"), o.recordedGone(ref))
	}
	c.SetResourceVersion("6")
	now := metav1.Now()
	c.SetDeletionTimestamp(&now)
	o.see(gr, c, false)
	if !o.recordedGone(ref) {
		t.Error("A version being deleted is not gone.")
	}
	c.SetResourceVersion("7")
	c.SetDeletionTimestamp(nil)
	o.see(gr, c, false)
	if o.recordedGone(ref) {
		t.Error("A recreated object is still gone.")
	}
	o.see(gr, c, true)
	if !o.recordedGone(ref) {
		t.Error("A deleted object is not gone.")
	}
}

func TestAwaitGivesUpWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&observer{}).await(ctx, "nothing", func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Errorf("await = %v.", err)
	}
}

func TestRuleKindsSkipsWildcardsSubresourcesAndIgnoredKinds(t *testing.T) {
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{"", "apps"}, Resources: []string{"configmaps", "deployments/status", "events", "*"}},
		{APIGroups: []string{"*", "coordination.k8s.io"}, Resources: []string{"leases", "configmaps"}},
	}
	got := ruleKinds([]rbacv1.Role{{Rules: rules}}, []rbacv1.ClusterRole{{Rules: rules[:1]}})
	want := []schema.GroupResource{{Resource: "configmaps"}, {Group: "apps", Resource: "configmaps"}, {Group: "apps", Resource: "events"}, {Group: "coordination.k8s.io", Resource: "configmaps"}}
	if !slices.Equal(got, want) {
		t.Errorf("ruleKinds = %v, want %v", got, want)
	}
}

// staticMapper is a RESTMapper that knows ConfigMaps only.
type staticMapper struct{ meta.RESTMapper }

func (staticMapper) Reset() {}

func newStaticMapper() staticMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)
	return staticMapper{mapper}
}

func TestObserverRetriesAKindItCouldNotMap(t *testing.T) {
	o := &observer{ctx: context.Background(), mapper: newStaticMapper(), kinds: map[schema.GroupResource]schema.GroupVersionResource{}}
	gr := schema.GroupResource{Group: "toy", Resource: "widgets"}
	if err := o.watch(gr); err == nil {
		t.Fatal("watch mapped an unknown kind")
	}
	if _, kept := o.kinds[gr]; kept {
		t.Error("the observer keeps a kind it could not map, so it never tries it again")
	}
}

func TestWaitGoneReturnsAnErrorOtherThanNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "c", errors.New("no")).ErrStatus)
	}))
	defer server.Close()
	harness, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	k := &Kube{mapper: newStaticMapper(), harness: harness}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := k.WaitGone(ctx, trace.Ref{Resource: "configmaps", Namespace: "ns", Name: "c"}); !apierrors.IsForbidden(err) {
		t.Fatalf("WaitGone = %v, want the Forbidden error", err)
	}
}

func TestWarmCreatesWithADryRunInTheNamespace(t *testing.T) {
	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}`))
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	k := &Kube{mapper: newStaticMapper(), namespace: "ns"}
	if err := k.warm(context.Background(), client, map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "c"}}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/v1/namespaces/ns/configmaps" || got.URL.Query().Get("dryRun") != "All" {
		t.Errorf("warm sent %s %s", got.Method, got.URL)
	}
}
