package crud_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var _ fake.API = (*crud.Map)(nil)

func newMap() *crud.Map {
	m := crud.New("cloud")
	m.SetCollections([]string{"buckets", "object-stores"})
	return m
}

func ref(resource, name string) trace.Ref {
	return trace.Ref{System: "cloud", Resource: resource, Name: name}
}

func TestClassifiesEachVerb(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         interpose.Request
	}{
		{"GET", "/buckets", interpose.Request{Verb: "list", Ref: ref("buckets", "")}},
		{"GET", "/buckets?owner=a", interpose.Request{Verb: "list", Ref: ref("buckets", "")}},
		{"POST", "/buckets", interpose.Request{Verb: "create", Ref: ref("buckets", "")}},
		{"GET", "/buckets/b-1", interpose.Request{Verb: "get", Ref: ref("buckets", "b-1")}},
		{"PUT", "/buckets/b-1", interpose.Request{Verb: "update", Ref: ref("buckets", "b-1")}},
		{"PATCH", "/buckets/b-1", interpose.Request{Verb: "patch", Ref: ref("buckets", "b-1")}},
		{"DELETE", "/object-stores/b-1", interpose.Request{Verb: "delete", Ref: ref("object-stores", "b-1")}},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if got := newMap().Classify(httptest.NewRequest(c.method, c.path, nil)); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Got %+v, want %+v.", got, c.want)
			}
		})
	}
}

func TestLeavesOtherRequestsUnmapped(t *testing.T) {
	for _, c := range []struct{ method, path, verb string }{
		{"GET", "/", "get"},
		{"GET", "/buckets/b-1/acl", "get"},
		{"GET", "/buckets/", "get"},
		{"GET", "/_fake/resources", "get"},
		{"GET", "/Buckets", "get"},
		{"GET", "/9buckets", "get"},
		{"GET", "/v1/buckets", "get"},
		{"DELETE", "/v1/buckets", "delete"},
		{"GET", "/disks", "get"},
		{"HEAD", "/buckets/b-1", "head"},
		{"POST", "/buckets/b-1", "post"},
		{"DELETE", "/buckets", "delete"},
		{"PUT", "/buckets", "put"},
		{"OPTIONS", "/buckets", "options"},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			want := interpose.Request{Verb: c.verb, Unmapped: true, Ref: trace.Ref{System: "cloud"}}
			if got := newMap().Classify(httptest.NewRequest(c.method, c.path, nil)); !reflect.DeepEqual(got, want) {
				t.Errorf("Got %+v, want %+v.", got, want)
			}
		})
	}
}

func TestACreateRequestsTheBodysNameAndLeavesTheBodyReadable(t *testing.T) {
	body := `{"name":"logs","tags":{"cr-uid":"u1"}}`
	r := httptest.NewRequest("POST", "/buckets", strings.NewReader(body))

	if got := newMap().Classify(r); got.RequestName != "logs" {
		t.Errorf("Got request name %q, want logs.", got.RequestName)
	}
	if rest, _ := io.ReadAll(r.Body); string(rest) != body {
		t.Errorf("Left body %q, want %q.", rest, body)
	}
}

func TestOnlyACreateWithAStringNameRequestsOne(t *testing.T) {
	for _, c := range []struct{ method, body string }{
		{"POST", `{"name":7}`},
		{"POST", `not json`},
		{"PUT", `{"name":"logs"}`},
	} {
		path := "/buckets"
		if c.method == "PUT" {
			path += "/b-1"
		}
		if got := newMap().Classify(httptest.NewRequest(c.method, path, strings.NewReader(c.body))); got.RequestName != "" {
			t.Errorf("%s %s: got request name %q, want none.", c.method, c.body, got.RequestName)
		}
	}
}

func object(resource, id, rv, body string) interpose.Object {
	return interpose.Object{Ref: ref(resource, id), UID: "cloud/" + resource + "/" + id, RV: rv, Body: json.RawMessage(body)}
}

func TestObjectsOfAListAreItsItems(t *testing.T) {
	m := newMap()
	b1, b2 := `{"id":"b-1","version":3}`, `{"id":"b-2","version":"7"}`
	list := m.Classify(httptest.NewRequest("GET", "/buckets", nil))

	got := m.Objects(list, []byte(`{"items":[`+b1+`, `+b2+`]}`))

	want := []interpose.Object{object("buckets", "b-1", "3", b1), object("buckets", "b-2", "7", b2)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Got %+v, want %+v.", got, want)
	}
}

func TestAnEmptyListShowsNoObjectsButIsRead(t *testing.T) {
	m := newMap()
	got := m.Objects(m.Classify(httptest.NewRequest("GET", "/buckets", nil)), []byte(`{"items":[]}`))
	if got == nil || len(got) != 0 {
		t.Errorf("Got %#v, want an empty, non-nil slice.", got)
	}
}

func TestObjectsOfAnyOtherVerbIsOneObject(t *testing.T) {
	m := newMap()
	body := `{"id":"b-1","version":12,"state":"CREATING"}`
	for _, method := range []string{"POST", "GET", "DELETE"} {
		path := "/buckets/b-1"
		if method == "POST" {
			path = "/buckets"
		}
		got := m.Objects(m.Classify(httptest.NewRequest(method, path, nil)), []byte(body))
		if want := []interpose.Object{object("buckets", "b-1", "12", body)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v, want %+v.", method, got, want)
		}
	}
}

func TestObjectsOfAnUnreadableBodyIsNil(t *testing.T) {
	m := newMap()
	list := m.Classify(httptest.NewRequest("GET", "/buckets", nil))
	get := m.Classify(httptest.NewRequest("GET", "/buckets/b-1", nil))
	for _, c := range []struct {
		req  interpose.Request
		body string
	}{
		{get, `not json`},
		{get, ``},
		{get, `{"version":1}`},
		{get, `{"id":5}`},
		{get, `[]`},
		{list, `{"id":"b-1"}`},
		{list, `{"items":[{"id":"b-1"},{"name":"x"}]}`},
	} {
		if got := m.Objects(c.req, []byte(c.body)); got != nil {
			t.Errorf("%s %s: got %+v, want nil.", c.req.Verb, c.body, got)
		}
	}
}

func TestObjectReadsAListedResource(t *testing.T) {
	body := `{"id":"b-1","version":4}`
	got, err := newMap().Object("buckets", json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := object("buckets", "b-1", "4", body); !reflect.DeepEqual(got, want) {
		t.Errorf("Got %+v, want %+v.", got, want)
	}
	for _, bad := range []string{`{"version":4}`, `{"id":"b-1"}`, `{"id":"b-1","version":true}`} {
		if _, err := newMap().Object("buckets", json.RawMessage(bad)); err == nil {
			t.Errorf("Got no error for %s.", bad)
		}
	}
}

func TestErrorIsAJSONErrorObject(t *testing.T) {
	contentType, body := newMap().Error(503, "try later")
	if contentType != "application/json" || string(body) != `{"error":{"code":503,"message":"try later"}}` {
		t.Errorf("Got %s %s.", contentType, body)
	}
}

func TestPathAddressesOneResource(t *testing.T) {
	if got := newMap().Path("buckets", "b-1"); got != "/buckets/b-1" {
		t.Errorf("Got %q, want /buckets/b-1.", got)
	}
}
