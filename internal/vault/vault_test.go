package vault_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/vault"
)

// recorded reads a response that testdata/record.sh saved.
func recorded(t *testing.T, name string) (*http.Response, []byte) {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name+".http"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	resp, err := http.ReadResponse(bufio.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func newMap() *vault.Map {
	m := vault.New("vault", "root")
	m.SetCollections([]string{"secret", "kv/team"})
	return m
}

func ref(mount, path string) trace.Ref {
	return trace.Ref{System: "vault", Resource: mount, Name: path}
}

func classify(m *vault.Map, method, target string) interpose.Request {
	return m.Classify(httptest.NewRequest(method, target, nil))
}

func TestClassifiesEachKVPath(t *testing.T) {
	m := newMap()
	none := trace.Ref{System: "vault"}
	for _, c := range []struct {
		method, target string
		want           interpose.Request
	}{
		{"GET", "/v1/secret/data/app/db", interpose.Request{Verb: "get", Ref: ref("secret", "app/db")}},
		{"POST", "/v1/secret/data/app/db", interpose.Request{Verb: "create", Ref: ref("secret", "app/db"), RequestName: "app/db"}},
		{"PUT", "/v1/kv/team/data/x", interpose.Request{Verb: "create", Ref: ref("kv/team", "x"), RequestName: "x"}},
		{"PATCH", "/v1/secret/data/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"DELETE", "/v1/secret/data/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"GET", "/v1/secret/metadata/app/db", interpose.Request{Verb: "get", Ref: ref("secret", "app/db")}},
		{"LIST", "/v1/secret/metadata/app/", interpose.Request{Verb: "list", Ref: ref("secret", "")}},
		{"GET", "/v1/secret/metadata/?list=true", interpose.Request{Verb: "list", Ref: ref("secret", "")}},
		{"POST", "/v1/secret/metadata/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"PATCH", "/v1/secret/metadata/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"DELETE", "/v1/secret/metadata/app/db", interpose.Request{Verb: "delete", Ref: ref("secret", "app/db")}},
		{"POST", "/v1/secret/delete/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"POST", "/v1/secret/undelete/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"PUT", "/v1/secret/destroy/app/db", interpose.Request{Verb: "patch", Ref: ref("secret", "app/db")}},
		{"GET", "/v1/sys/mounts", interpose.Request{Verb: "get", Ref: none}},
		{"POST", "/v1/auth/token/lookup-self", interpose.Request{Verb: "post", Ref: none}},
		{"GET", "/v1/secret/config", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"GET", "/v1/secret/subkeys/app/db", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"GET", "/v1/secret/data/", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"GET", "/v1/cubbyhole/x", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"GET", "/v1/secret/destroy/app/db", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"GET", "/ui/", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
	} {
		if got := classify(m, c.method, c.target); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s = %+v, want %+v", c.method, c.target, got, c.want)
		}
	}
}

func TestAWriteIsCreateUntilAnInstanceIsLiveAndAfterItsDelete(t *testing.T) {
	m := newMap()
	create := classify(m, "POST", "/v1/secret/data/app/db")
	_, body := recorded(t, "create")
	objects := m.Objects(create, nil, body)
	if len(objects) != 1 || objects[0].UID != "vault/secret/app/db/1" || !objects[0].Partial || objects[0].Ref != ref("secret", "app/db") {
		t.Fatalf("the create showed %+v", objects)
	}
	update := classify(m, "PUT", "/v1/secret/data/app/db")
	_, body = recorded(t, "update")
	if update.Verb != "update" || m.Objects(update, nil, body)[0].UID != "vault/secret/app/db/1" {
		t.Errorf("the second write is %s", update.Verb)
	}
	_, body = recorded(t, "get-data")
	if got := m.Objects(classify(m, "GET", "/v1/secret/data/app/db"), nil, body); len(got) != 1 || !strings.Contains(string(got[0].Body), `"p2"`) {
		t.Errorf("the get showed %+v", got)
	}
	_, body = recorded(t, "delete")
	if got := m.Objects(classify(m, "DELETE", "/v1/secret/metadata/app/db"), nil, body); len(got) != 1 || got[0].UID != "vault/secret/app/db/1" {
		t.Errorf("the delete showed %+v", got)
	}
	again := classify(m, "POST", "/v1/secret/data/app/db")
	if again.Verb != "create" || m.Objects(again, nil, nil)[0].UID != "vault/secret/app/db/2" {
		t.Errorf("a write after the delete is %s", again.Verb)
	}
}

func TestResponsesWithNoInstanceShowNone(t *testing.T) {
	m := newMap()
	_, body := recorded(t, "soft-delete")
	for _, c := range []struct{ method, target string }{
		{"DELETE", "/v1/secret/data/top"},
		{"DELETE", "/v1/secret/metadata/top"},
		{"GET", "/v1/sys/mounts"},
		{"LIST", "/v1/secret/metadata/"},
	} {
		if got := m.Objects(classify(m, c.method, c.target), nil, body); got == nil || len(got) != 0 {
			t.Errorf("%s %s showed %v, want none", c.method, c.target, got)
		}
	}
	if got := m.Objects(classify(m, "PATCH", "/v1/secret/data/top"), nil, []byte("<html>")); len(got) != 0 {
		t.Errorf("an unreadable answer showed %v", got)
	}
}

// replay answers each request with the response record.sh saved under
// testdata/listing for it.
func replay(t *testing.T) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Vault-Token") != "" {
			t.Errorf("List authorized %s itself", r.URL)
		}
		name := strings.NewReplacer("/", "_", "?", "_", "=", "_", "&", "_").Replace(r.Method + r.URL.RequestURI())
		resp, body := recorded(t, filepath.Join("listing", name))
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return resp, nil
	}
}

func TestListReadsEachSecretsMetadataAndCurrentData(t *testing.T) {
	m := vault.New("vault", "root")
	listing, err := m.List(context.Background(), replay(t), "http://vault.test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(listing.Collections, []string{"secret"}) || len(listing.Resources) != 2 {
		t.Fatalf("listing = %+v", listing)
	}
	var db, top vault.Listed
	json.Unmarshal(listing.Resources[0].Body, &db)
	json.Unmarshal(listing.Resources[1].Body, &top)
	if db.Path != "app/db" || db.CurrentVersion != 2 || string(db.Data) != `{"password":"p2"}` || len(db.Versions) != 2 || db.Versions["1"].Deleted {
		t.Errorf("app/db = %+v", db)
	}
	if top.Path != "top" || string(top.Data) != "null" || !top.Versions["1"].Deleted || string(top.CustomMetadata) != `{"owner":"me"}` || top.UpdatedTime == "" {
		t.Errorf("top = %+v", top)
	}

	m.SetCollections(listing.Collections)
	first, err := m.Object("secret", listing.Resources[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := m.Object("secret", listing.Resources[1].Body)
	if first.Ref != ref("secret", "app/db") || first.UID != "vault/secret/app/db/1" || first.Partial || first.RV == "" || first.RV == other.RV {
		t.Errorf("Object = %+v, other RV %s", first, other.RV)
	}
	if update := classify(m, "POST", "/v1/secret/data/app/db"); update.Verb != "update" {
		t.Errorf("a write of a listed secret is %s", update.Verb)
	}
	m.Ended(first.Ref, first.UID)
	if again, _ := m.Object("secret", listing.Resources[0].Body); again.UID != "vault/secret/app/db/2" {
		t.Errorf("a secret listed after it ended is %s", again.UID)
	}
}

func TestObjectRejectsABodyWithNoPath(t *testing.T) {
	if _, err := newMap().Object("secret", json.RawMessage(`{"data":{}}`)); err == nil {
		t.Error("Object read a secret with no path")
	}
}

func TestWritesSpellDataAndMetadataRequests(t *testing.T) {
	m := newMap()
	for _, c := range []struct {
		op   fake.Op
		want fake.Write
	}{
		{fake.Create, fake.Write{Method: "POST", Path: "/v1/secret/data/app/db", ContentType: "application/json", Body: []byte(`{"data":{"k":"v"}}`)}},
		{fake.Patch, fake.Write{Method: "PATCH", Path: "/v1/secret/data/app/db", ContentType: "application/merge-patch+json", Body: []byte(`{"data":{"k":"v"}}`)}},
		{fake.Delete, fake.Write{Method: "DELETE", Path: "/v1/secret/metadata/app/db"}},
	} {
		body := []byte(`{"k":"v"}`)
		if c.op == fake.Delete {
			body = nil
		}
		got, err := m.Write(c.op, ref("secret", "app/db"), body)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Write(%s) = %+v, %v; want %+v", c.op, got, err, c.want)
		}
	}
}

func TestErrorAndAuthorizeSpeakVault(t *testing.T) {
	m := newMap()
	if contentType, body := m.Error(503, "injected"); contentType != "application/json" || string(body) != `{"errors":["injected"]}` {
		t.Errorf("Error = %s %s", contentType, body)
	}
	r := httptest.NewRequest("GET", "/v1/sys/mounts", nil)
	m.Authorize(r)
	if r.Header.Get("X-Vault-Token") != "root" {
		t.Errorf("Authorize set %v", r.Header)
	}
}

func TestGoneAcceptsOnlyA404(t *testing.T) {
	m := vault.New("vault", "root")
	if !m.Gone(404, []byte(`{"errors":[]}`)) || m.Gone(400, []byte(`{"errors":["check-and-set parameter did not match the current version"]}`)) {
		t.Error("Gone wants only a 404")
	}
}
