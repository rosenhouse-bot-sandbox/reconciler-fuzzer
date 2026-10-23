package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
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

func TestReadRBACKeepsARolesNamespace(t *testing.T) {
	roles, _, err := readRBAC([]string{"testdata/role-with-namespace.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0].Namespace != "fixed" {
		t.Errorf("Roles are %+v, want writer in fixed.", roles)
	}
}

func TestPlanSetup(t *testing.T) {
	fixtures := []*unstructured.Unstructured{
		object("v1", "ConfigMap", "", "local"),
		object("v1", "ConfigMap", "far", "remote"),
		object("rbac.authorization.k8s.io/v1", "ClusterRole", "ignored", "cluster"),
		object("v1", "Secret", "declared", "s"),
		object("v1", "Namespace", "", "declared"),
		object("v1", "ConfigMap", "run", "explicit"),
		object("v1", "ConfigMap", "far", "again"),
	}
	roles := []rbacv1.Role{
		{ObjectMeta: metav1.ObjectMeta{Name: "local"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "roles"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "far"}},
	}
	clusterScoped := map[string]bool{"Namespace": true, "ClusterRole": true}
	setup, err := planSetup("run", roles, fixtures, func(gvk schema.GroupVersionKind) (bool, error) {
		return !clusterScoped[gvk.Kind], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	names := func(objects []*unstructured.Unstructured) string {
		var got []string
		for _, o := range objects {
			got = append(got, o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName())
		}
		return strings.Join(got, " ")
	}
	if got, want := names(setup.namespaces), "Namespace//declared"; got != want {
		t.Errorf("Namespace fixtures are %s, want %s.", got, want)
	}
	if got, want := strings.Join(setup.missing, " "), "far roles"; got != want {
		t.Errorf("Missing namespaces are %s, want %s.", got, want)
	}
	if got, want := names(setup.fixtures), "ConfigMap//local ConfigMap/far/remote ClusterRole/ignored/cluster Secret/declared/s ConfigMap/run/explicit ConfigMap/far/again"; got != want {
		t.Errorf("Fixtures are %s, want %s.", got, want)
	}
}

func TestPlanSetupReportsAnUnmappableKind(t *testing.T) {
	_, err := planSetup("run", nil, []*unstructured.Unstructured{object("v1", "Nope", "", "n")}, func(schema.GroupVersionKind) (bool, error) {
		return false, errors.New("no such kind")
	})
	if err == nil || !strings.Contains(err.Error(), "no such kind") {
		t.Errorf("planSetup = %v, want the mapping error.", err)
	}
}

func object(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetAPIVersion(apiVersion)
	o.SetKind(kind)
	o.SetNamespace(namespace)
	o.SetName(name)
	return o
}

func TestReadRBACRejects(t *testing.T) {
	for file, want := range map[string]string{
		"not-rbac.yaml":        "v1 ConfigMap is not",
		"role-misspelled.yaml": "rulez",
		"missing.yaml":         "no such file",
	} {
		_, _, err := readRBAC([]string{"testdata/" + file})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("readRBAC(%s) = %v, want an error naming %q.", file, err, want)
		}
	}
}

func TestReadObjectsReadsEveryDocument(t *testing.T) {
	objects, err := readObjects([]string{"testdata/objects.yaml"}, nil)
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
	_, err := readObjects([]string{"testdata/kindless.yaml"}, nil)
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Errorf("readObjects(kindless.yaml) = %v, want an error naming kind.", err)
	}
}

func TestReadObjectsExpandsVars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.yaml")
	os.WriteFile(path, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: store}\ndata: {url: '${systems.vault.url}/v1'}\n"), 0o644)
	objects, err := readObjects([]string{path}, map[string]string{"systems.vault.url": "http://127.0.0.1:9"})
	if err != nil {
		t.Fatal(err)
	}
	if url, _, _ := unstructured.NestedString(objects[0].Object, "data", "url"); url != "http://127.0.0.1:9/v1" {
		t.Errorf("url = %s", url)
	}
	if _, err := readObjects([]string{path}, nil); err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "systems.vault.url") {
		t.Errorf("readObjects = %v, want an error naming the fixture and key", err)
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
	o := newObserver()
	o.record = func(e trace.Event) { versions = append(versions, *e.Version) }
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

func TestSubscribersHearEachVersionAfterItIsRecorded(t *testing.T) {
	var order []string
	o := newObserver()
	o.record = func(e trace.Event) { order = append(order, "record "+e.Version.RV) }
	k := &Kube{observer: o}
	k.Subscribe(func(v trace.Version) { order = append(order, "first "+v.RV) })
	k.Subscribe(func(v trace.Version) { order = append(order, "second "+v.RV) })
	c := &unstructured.Unstructured{}
	c.SetName("c")
	for _, rv := range []string{"5", "5", "6"} {
		c.SetResourceVersion(rv)
		o.see(schema.GroupResource{Resource: "configmaps"}, c, false)
	}
	want := []string{"record 5", "first 5", "second 5", "record 6", "first 6", "second 6"}
	if !slices.Equal(order, want) {
		t.Errorf("Heard %q, want %q.", order, want)
	}
}

func TestObserverKnowsWhatItRecorded(t *testing.T) {
	o := newObserver()
	gr := schema.GroupResource{Resource: "configmaps"}
	ref := trace.Ref{System: "kube", Resource: "configmaps", Namespace: "rf-1", Name: "c"}
	c := &unstructured.Unstructured{}
	c.SetNamespace("rf-1")
	c.SetName("c")
	c.SetUID("u1")
	c.SetResourceVersion("5")
	o.see(gr, c, false)
	if !o.recorded(ref, "5") || o.recorded(ref, "6") || o.recordedGone(ref, "u1") {
		t.Errorf("After version 5: recorded(5) %v, recorded(6) %v, gone %v.", o.recorded(ref, "5"), o.recorded(ref, "6"), o.recordedGone(ref, "u1"))
	}
	c.SetResourceVersion("6")
	now := metav1.Now()
	c.SetDeletionTimestamp(&now)
	o.see(gr, c, false)
	if !o.recordedGone(ref, "u1") {
		t.Error("A version being deleted is not gone.")
	}
	c.SetResourceVersion("7")
	c.SetDeletionTimestamp(nil)
	o.see(gr, c, true)
	if !o.recordedGone(ref, "u1") {
		t.Error("A deleted object is not gone.")
	}
	c.SetUID("u2")
	c.SetResourceVersion("8")
	o.see(gr, c, false)
	if !o.recordedGone(ref, "u1") || o.recordedGone(ref, "u2") {
		t.Errorf("After a recreate: u1 gone %v, u2 gone %v; want true, false.", o.recordedGone(ref, "u1"), o.recordedGone(ref, "u2"))
	}
}

func TestObserverCountsAnotherUIDAsGone(t *testing.T) {
	o := newObserver()
	gr := schema.GroupResource{Resource: "configmaps"}
	ref := trace.Ref{System: "kube", Resource: "configmaps", Namespace: "rf-1", Name: "c"}
	c := &unstructured.Unstructured{}
	c.SetNamespace("rf-1")
	c.SetName("c")
	c.SetUID("u1")
	c.SetResourceVersion("5")
	o.see(gr, c, false)
	c.SetUID("u2")
	c.SetResourceVersion("9")
	o.see(gr, c, false)
	if !o.recordedGone(ref, "u1") {
		t.Error("An object replaced unseen is not gone.")
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

// servesDeployments is a discovery client whose every group version serves
// deployments.
type servesDeployments struct{ discovery.DiscoveryInterface }

func (servesDeployments) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return &metav1.APIResourceList{APIResources: []metav1.APIResource{{Name: "deployments", Verbs: []string{"list", "watch"}}}}, nil
}

func TestObserverCannotWatchAKindTheMapperFindsInAnotherGroup(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	o := &observer{mapper: staticMapper{mapper}, discovery: servesDeployments{}}
	want := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	if gvr, err := o.watchable(want.GroupResource()); gvr != want || err != nil {
		t.Errorf("watchable(deployments.apps) = %v, %v, want %v", gvr, err, want)
	}
	// An RBAC rule can name deployments in the core group, and the mapper
	// finds them in apps. The observer would record them as core objects.
	if gvr, err := o.watchable(schema.GroupResource{Resource: "deployments"}); !gvr.Empty() || err != nil {
		t.Errorf("watchable(deployments) = %v, %v, want nothing", gvr, err)
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

func crd(group, kind, plural string, versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: group, Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, Plural: plural}, Versions: versions,
	}}
}

func crdVersion(name string, served, storage, status bool) apiextensionsv1.CustomResourceDefinitionVersion {
	v := apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: served, Storage: storage}
	if status {
		v.Subresources = &apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}}
	}
	return v
}

func TestStatusKindsListsServedStorageVersionsWithStatus(t *testing.T) {
	got := statusKinds([]*apiextensionsv1.CustomResourceDefinition{
		crd("a.io", "Gadget", "gadgets", crdVersion("v1alpha1", true, false, false), crdVersion("v1", true, true, true)),
		crd("a.io", "Plain", "plains", crdVersion("v1", true, true, false)),
		crd("a.io", "Old", "olds", crdVersion("v1", true, false, true), crdVersion("v2", true, true, false)),
		crd("a.io", "Unserved", "unserveds", crdVersion("v1", false, true, true)),
	})
	want := []trace.Kind{{Group: "a.io", Version: "v1", Kind: "Gadget", Resource: "gadgets"}}
	if !slices.Equal(got, want) {
		t.Errorf("Got %+v, want %+v.", got, want)
	}
}

func TestALateSubscriberFirstHearsEachLiveObjectsLatestVersion(t *testing.T) {
	o := newObserver()
	k := &Kube{observer: o}
	see := func(name, rv string, deleted bool) {
		c := &unstructured.Unstructured{}
		c.SetName(name)
		c.SetResourceVersion(rv)
		o.see(schema.GroupResource{Resource: "configmaps"}, c, deleted)
	}
	see("b", "1", false)
	see("a", "2", false)
	see("b", "3", false)
	see("gone", "4", false)
	see("gone", "5", true)
	var heard []string
	k.Subscribe(func(v trace.Version) { heard = append(heard, v.Ref.Name+" "+v.RV) })
	see("a", "6", false)
	if want := []string{"a 2", "b 3", "a 6"}; !slices.Equal(heard, want) {
		t.Errorf("Heard %q, want %q.", heard, want)
	}
}

func TestDeleteSendsPropagationAndTheUIDPrecondition(t *testing.T) {
	for propagation, want := range map[plan.Propagation]string{"": "", plan.Foreground: "Foreground"} {
		var options metav1.DeleteOptions
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodDelete {
				json.NewDecoder(r.Body).Decode(&options)
				w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Success"}`))
				return
			}
			w.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","namespace":"ns","uid":"u"}}`))
		}))
		harness, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		o := newObserver()
		o.gone["u"] = true
		k := &Kube{mapper: newStaticMapper(), harness: harness, observer: o}
		if err := k.Delete(context.Background(), trace.Ref{Resource: "configmaps", Namespace: "ns", Name: "c"}, propagation); err != nil {
			t.Fatal(err)
		}
		server.Close()
		got := ""
		if options.PropagationPolicy != nil {
			got = string(*options.PropagationPolicy)
		}
		if got != want || options.Preconditions == nil || *options.Preconditions.UID != "u" {
			t.Errorf("Delete with %q sent %+v, want propagation %q and UID u.", propagation, options, want)
		}
	}
}
