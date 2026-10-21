package crudfake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
)

const (
	short = 30 * time.Millisecond
	long  = time.Hour
)

func buckets(createDelay, deleteDelay time.Duration) *Config {
	return &Config{Collections: map[string]Collection{
		"buckets": {
			Unique:      []string{"name"},
			Pending:     "CREATING",
			Ready:       "READY",
			Deleting:    "DELETING",
			CreateDelay: Duration{createDelay},
			DeleteDelay: Duration{deleteDelay},
		},
	}}
}

type client struct {
	t   *testing.T
	url string
}

func serve(t *testing.T, cfg *Config) client {
	t.Helper()
	s := New(cfg)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { ts.Close(); s.Close() })
	return client{t, ts.URL}
}

func (c client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.url+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, out
}

func (c client) want(method, path, body string, code int) map[string]any {
	c.t.Helper()
	got, out := c.do(method, path, body)
	if got != code {
		c.t.Fatalf("%s %s %s: got %d %v, want %d", method, path, body, got, out, code)
	}
	return out
}

func (c client) state(id string) any {
	c.t.Helper()
	code, out := c.do("GET", "/buckets/"+id, "")
	if code == http.StatusNotFound {
		return nil
	}
	return out["state"]
}

func (c client) eventually(id string, state any) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.state(id) != state {
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: got state %v, want %v", id, c.state(id), state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (c client) stays(id string, state any) {
	c.t.Helper()
	time.Sleep(5 * short)
	if got := c.state(id); got != state {
		c.t.Fatalf("%s: got state %v, want it held at %v", id, got, state)
	}
}

func (c client) listing() fake.Listing {
	c.t.Helper()
	resp, err := http.Get(c.url + fake.ResourcesPath)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var l fake.Listing
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		c.t.Fatal(err)
	}
	return l
}

func (c client) fault(effect, collection, id string) string {
	c.t.Helper()
	body, _ := json.Marshal(fake.FaultRequest{Effect: effect, Collection: collection, ID: id})
	resp, err := http.Post(c.url+fake.FaultsPath, "application/json", bytes.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("fault %s: got %d", effect, resp.StatusCode)
	}
	var started fake.FaultStarted
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&started); err != nil || started.ID == "" {
		c.t.Fatalf("fault %s: %v %+v", effect, err, started)
	}
	return started.ID
}

func TestCreateIsPendingThenReady(t *testing.T) {
	c := serve(t, buckets(short, long))
	out := c.want("POST", "/buckets", `{"name":"a","tags":{"k":"v"}}`, http.StatusCreated)
	want := map[string]any{"id": "buckets-1", "version": 1.0, "state": "CREATING", "name": "a", "tags": map[string]any{"k": "v"}}
	if !equal(out, want) {
		t.Fatalf("got %v, want %v", out, want)
	}
	c.eventually("buckets-1", "READY")
	if got := c.want("GET", "/buckets/buckets-1", "", http.StatusOK)["version"]; got != 2.0 {
		t.Fatalf("got version %v, want 2", got)
	}
	if id := c.want("POST", "/buckets", `{"name":"b"}`, http.StatusCreated)["id"]; id != "buckets-2" {
		t.Fatalf("got id %v, want buckets-2", id)
	}
}

func TestDeleteIsDeletingThenGone(t *testing.T) {
	c := serve(t, buckets(0, short))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	out := c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	if out["state"] != "DELETING" || out["version"] != 3.0 {
		t.Fatalf("got %v, want DELETING at version 3", out)
	}
	c.eventually("buckets-1", nil)
}

func TestDeleteOfDeletingIsUnchanged(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	first := c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	again := c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	if !equal(first, again) {
		t.Fatalf("got %v, want %v", again, first)
	}
}

func TestDeleteOfPendingStopsTheCreate(t *testing.T) {
	c := serve(t, buckets(short, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	c.stays("buckets-1", "DELETING")
}

func TestUnknownCollectionOrIDIsNotFound(t *testing.T) {
	c := serve(t, buckets(0, short))
	for _, tc := range []struct{ method, path string }{
		{"GET", "/"},
		{"GET", "/disks"},
		{"POST", "/disks"},
		{"GET", "/buckets/buckets-1"},
		{"PUT", "/buckets/buckets-1"},
		{"PATCH", "/buckets/buckets-1"},
		{"DELETE", "/buckets/buckets-1"},
		{"GET", "/buckets/buckets-1/x"},
	} {
		out := c.want(tc.method, tc.path, `{}`, http.StatusNotFound)
		wantError(t, out, http.StatusNotFound)
	}
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	c.eventually("buckets-1", nil)
	c.want("GET", "/buckets/buckets-1", "", http.StatusNotFound)
}

func TestWrongMethodIsNotAllowed(t *testing.T) {
	c := serve(t, buckets(0, short))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/buckets"},
		{"PUT", "/buckets"},
		{"POST", "/buckets/buckets-1"},
		{"POST", fake.ResourcesPath},
		{"GET", fake.FaultsPath},
		{"GET", fake.FaultsPath + "/fault-1"},
	} {
		wantError(t, c.want(tc.method, tc.path, "", http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	}
}

func TestUniqueValueConflicts(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	wantError(t, c.want("POST", "/buckets", `{"name":"a"}`, http.StatusConflict), http.StatusConflict)
	c.want("POST", "/buckets", `{"name":"b"}`, http.StatusCreated)
	c.want("POST", "/buckets", `{}`, http.StatusCreated)
	c.want("POST", "/buckets", `{}`, http.StatusCreated)
	c.want("PUT", "/buckets/buckets-2", `{"name":"a"}`, http.StatusConflict)
	c.want("PATCH", "/buckets/buckets-2", `{"name":"a"}`, http.StatusConflict)
	c.want("PUT", "/buckets/buckets-1", `{"name":"a","size":1}`, http.StatusOK)
}

func TestUniqueValueOfDeletingResourceConflicts(t *testing.T) {
	c := serve(t, buckets(0, short))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusConflict)
	c.eventually("buckets-1", nil)
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
}

func TestWriteToDeletingResourceConflicts(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	c.want("PUT", "/buckets/buckets-1", `{"name":"b"}`, http.StatusConflict)
	c.want("PATCH", "/buckets/buckets-1", `{"name":"b"}`, http.StatusConflict)
}

func TestBadRequests(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/buckets", `{`},
		{"POST", "/buckets", `[]`},
		{"POST", "/buckets", `"a"`},
		{"POST", "/buckets", ``},
		{"POST", "/buckets", `{} {}`},
		{"POST", "/buckets", `{"id":"x"}`},
		{"POST", "/buckets", `{"version":1}`},
		{"POST", "/buckets", `{"state":"READY"}`},
		{"PUT", "/buckets/buckets-1", `[]`},
		{"PUT", "/buckets/buckets-1", `{"id":"buckets-2"}`},
		{"PUT", "/buckets/buckets-1", `{"state":"DELETING"}`},
		{"PATCH", "/buckets/buckets-1", `{`},
		{"PATCH", "/buckets/buckets-1", `{"id":null}`},
		{"PATCH", "/buckets/buckets-1", `{"state":"DELETING"}`},
		{"POST", fake.FaultsPath, `{"effect":"melt"}`},
		{"POST", fake.FaultsPath, `{`},
	} {
		wantError(t, c.want(tc.method, tc.path, tc.body, http.StatusBadRequest), http.StatusBadRequest)
	}
}

func TestPutReplacesClientFields(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a","size":1}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	out := c.want("PUT", "/buckets/buckets-1", `{"name":"a","tags":{"k":"v"}}`, http.StatusOK)
	want := map[string]any{"id": "buckets-1", "version": 3.0, "state": "READY", "name": "a", "tags": map[string]any{"k": "v"}}
	if !equal(out, want) {
		t.Fatalf("got %v, want %v", out, want)
	}
}

func TestPutAcceptsTheResourceItRead(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	read, _ := json.Marshal(c.want("GET", "/buckets/buckets-1", "", http.StatusOK))
	if got := c.want("PUT", "/buckets/buckets-1", string(read), http.StatusOK); got["version"] != 2.0 {
		t.Fatalf("got %v, want version 2", got)
	}
}

func TestStaleVersionConflicts(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	c.want("PUT", "/buckets/buckets-1", `{"name":"a","version":1}`, http.StatusConflict)
	c.want("PATCH", "/buckets/buckets-1", `{"size":1,"version":1}`, http.StatusConflict)
	c.want("PATCH", "/buckets/buckets-1", `{"size":1,"version":2}`, http.StatusOK)
}

func TestPatchMerges(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a","size":1,"tags":{"k":"v","x":"y"}}`, http.StatusCreated)
	out := c.want("PATCH", "/buckets/buckets-1", `{"size":null,"tags":{"x":null,"z":"w"}}`, http.StatusOK)
	if !equal(out["tags"], map[string]any{"k": "v", "z": "w"}) || out["size"] != nil || out["name"] != "a" {
		t.Fatalf("got %v", out)
	}
}

func TestNoOpWritesKeepTheVersion(t *testing.T) {
	c := serve(t, buckets(0, long))
	c.want("POST", "/buckets", `{"name":"a","size":1}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	for _, tc := range []struct{ method, body string }{
		{"PUT", `{"size":1,"name":"a"}`},
		{"PATCH", `{"size":1}`},
		{"PATCH", `{"other":null}`},
		{"PATCH", `{}`},
	} {
		if got := c.want(tc.method, "/buckets/buckets-1", tc.body, http.StatusOK)["version"]; got != 2.0 {
			t.Fatalf("%s %s: got version %v, want 2", tc.method, tc.body, got)
		}
	}
}

func TestListFilters(t *testing.T) {
	c := serve(t, buckets(0, long))
	for i := 0; i < 11; i++ {
		c.want("POST", "/buckets", `{"owner":"x","tier":"cold"}`, http.StatusCreated)
	}
	c.want("POST", "/buckets", `{"owner":"y","tier":"cold"}`, http.StatusCreated)
	c.want("POST", "/buckets", `{"owner":"x","tier":"hot"}`, http.StatusCreated)
	c.want("POST", "/buckets", `{"owner":1}`, http.StatusCreated)

	ids := func(query string) []string {
		var out []string
		for _, item := range c.want("GET", "/buckets"+query, "", http.StatusOK)["items"].([]any) {
			out = append(out, item.(map[string]any)["id"].(string))
		}
		return out
	}
	all := ids("")
	if len(all) != 14 || all[1] != "buckets-2" || all[9] != "buckets-10" {
		t.Fatalf("got %v, want 14 in id order", all)
	}
	if got := ids("?owner=y"); !equal(got, []string{"buckets-12"}) {
		t.Fatalf("got %v", got)
	}
	if got := ids("?owner=x&tier=hot"); !equal(got, []string{"buckets-13"}) {
		t.Fatalf("got %v", got)
	}
	if got := ids("?owner=1"); !equal(got, []string{"buckets-14"}) {
		t.Fatalf("got %v, want the number's match", got)
	}
	if got := ids("?version=2&tier=hot"); !equal(got, []string{"buckets-13"}) {
		t.Fatalf("got %v, want the version's match", got)
	}
}

func TestEmptyListHasItems(t *testing.T) {
	c := serve(t, buckets(0, long))
	if items, ok := c.want("GET", "/buckets", "", http.StatusOK)["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("got %v, want []", items)
	}
}

func TestResourcesListsAllCollectionsAndPending(t *testing.T) {
	cfg := buckets(short, long)
	cfg.Collections["disks"] = Collection{Pending: "P", Ready: "R", Deleting: "D"}
	c := serve(t, cfg)
	if l := c.listing(); l.Pending || l.Resources == nil || len(l.Resources) != 0 || !equal(l.Collections, []string{"buckets", "disks"}) {
		t.Fatalf("got %+v, want an empty idle listing of both collections", l)
	}
	c.want("POST", "/disks", `{}`, http.StatusCreated)
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("POST", "/buckets", `{"name":"b"}`, http.StatusCreated)
	l := c.listing()
	if !l.Pending {
		t.Fatal("got pending false while a create is scheduled")
	}
	var got []string
	for _, r := range l.Resources {
		var body map[string]any
		if err := json.Unmarshal(r.Body, &body); err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Collection+"/"+body["id"].(string))
	}
	if !equal(got, []string{"buckets/buckets-1", "buckets/buckets-2", "disks/disks-1"}) {
		t.Fatalf("got %v", got)
	}
	c.eventually("buckets-2", "READY")
	if c.listing().Pending {
		t.Fatal("got pending true once all are ready")
	}
}

func TestStuckHoldsPendingThenReleases(t *testing.T) {
	c := serve(t, buckets(short, long))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	id := c.fault("stuck", "buckets", "")
	c.want("POST", "/buckets", `{"name":"b"}`, http.StatusCreated)
	if c.listing().Pending {
		t.Fatal("got pending true while every pending resource is held")
	}
	c.stays("buckets-1", "CREATING")
	c.stays("buckets-2", "CREATING")
	c.want("DELETE", fake.FaultsPath+"/"+id, "", http.StatusNoContent)
	if !c.listing().Pending {
		t.Fatal("got pending false after the fault ended")
	}
	c.eventually("buckets-1", "READY")
	c.eventually("buckets-2", "READY")
}

func TestStuckMatchesID(t *testing.T) {
	c := serve(t, buckets(short, long))
	c.fault("stuck", "", "buckets-2")
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("POST", "/buckets", `{"name":"b"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	c.stays("buckets-2", "CREATING")
}

func TestStuckMatchesCollection(t *testing.T) {
	cfg := buckets(short, long)
	cfg.Collections["disks"] = Collection{Pending: "P", Ready: "R", Deleting: "D"}
	c := serve(t, cfg)
	c.fault("stuck", "disks", "")
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
}

func TestOverlappingFaultsHoldUntilTheLastEnds(t *testing.T) {
	c := serve(t, buckets(short, long))
	first := c.fault("stuck", "", "")
	second := c.fault("stuck", "buckets", "buckets-1")
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.want("DELETE", fake.FaultsPath+"/"+first, "", http.StatusNoContent)
	c.stays("buckets-1", "CREATING")
	c.want("DELETE", fake.FaultsPath+"/"+second, "", http.StatusNoContent)
	c.eventually("buckets-1", "READY")
}

func TestSlowDeleteHoldsDeletingThenReleases(t *testing.T) {
	c := serve(t, buckets(0, short))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	id := c.fault("slowDelete", "", "")
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	if c.listing().Pending {
		t.Fatal("got pending true while the delete is held")
	}
	c.stays("buckets-1", "DELETING")
	c.want("DELETE", fake.FaultsPath+"/"+id, "", http.StatusNoContent)
	c.eventually("buckets-1", nil)
}

func TestSlowDeleteHoldsAScheduledDelete(t *testing.T) {
	c := serve(t, buckets(0, short))
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
	c.want("DELETE", "/buckets/buckets-1", "", http.StatusAccepted)
	c.fault("slowDelete", "", "")
	c.stays("buckets-1", "DELETING")
}

func TestSlowDeleteDoesNotHoldCreates(t *testing.T) {
	c := serve(t, buckets(short, long))
	c.fault("slowDelete", "", "")
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	c.eventually("buckets-1", "READY")
}

func TestAFaultOnAnUnknownCollectionIsABadRequest(t *testing.T) {
	c := serve(t, buckets(0, long))
	body, _ := json.Marshal(fake.FaultRequest{Effect: "stuck", Collection: "bucket"})
	wantError(t, c.want("POST", fake.FaultsPath, string(body), http.StatusBadRequest), http.StatusBadRequest)
}

func TestEndingAnUnknownFaultIsNotFound(t *testing.T) {
	c := serve(t, buckets(0, long))
	wantError(t, c.want("DELETE", fake.FaultsPath+"/fault-1", "", http.StatusNotFound), http.StatusNotFound)
	id := c.fault("stuck", "", "")
	c.want("DELETE", fake.FaultsPath+"/"+id, "", http.StatusNoContent)
	c.want("DELETE", fake.FaultsPath+"/"+id, "", http.StatusNotFound)
}

func TestCloseStopsTransitions(t *testing.T) {
	s := New(buckets(short, long))
	ts := httptest.NewServer(s)
	defer ts.Close()
	c := client{t, ts.URL}
	c.want("POST", "/buckets", `{"name":"a"}`, http.StatusCreated)
	s.Close()
	c.stays("buckets-1", "CREATING")
}

func TestLoad(t *testing.T) {
	got, err := Load(write(t, `
collections:
  buckets:
    unique: [name]
    pending: CREATING
    ready: READY
    deleting: DELETING
    createDelay: 500ms
    deleteDelay: 1s
`))
	if err != nil {
		t.Fatal(err)
	}
	want := buckets(500*time.Millisecond, time.Second)
	if !equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadRejects(t *testing.T) {
	for name, yaml := range map[string]string{
		"unknown field":    "collections: {b: {pending: P, ready: R, deleting: D, colour: red}}",
		"bad duration":     "collections: {b: {pending: P, ready: R, deleting: D, createDelay: soon}}",
		"numeric duration": "collections: {b: {pending: P, ready: R, deleting: D, createDelay: 5}}",
		"negative delay":   "collections: {b: {pending: P, ready: R, deleting: D, deleteDelay: -1s}}",
		"empty pending":    "collections: {b: {ready: R, deleting: D}}",
		"empty ready":      "collections: {b: {pending: P, deleting: D}}",
		"empty deleting":   "collections: {b: {pending: P, ready: R}}",
		"same states":      "collections: {b: {pending: P, ready: P, deleting: D}}",
		"no collections":   "collections: {}",
		"reserved name":    "collections: {_fake: {pending: P, ready: R, deleting: D}}",
		"slash in name":    "collections: {a/b: {pending: P, ready: R, deleting: D}}",
		"not yaml":         "collections: [",
		"reserved unique":  "collections: {b: {unique: [id], pending: P, ready: R, deleting: D}}",
	} {
		if _, err := Load(write(t, yaml)); err == nil {
			t.Errorf("%s: got no error", name)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("missing file: got no error")
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantError(t *testing.T, out map[string]any, code int) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok || e["code"] != float64(code) || e["message"] == "" || len(out) != 1 {
		t.Fatalf("got body %v, want an error with code %d", out, code)
	}
}

func equal(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return bytes.Equal(ja, jb)
}
