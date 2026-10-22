// Package crud is the crud API map: it reads a REST JSON API of
// collections as resources. DESIGN.md §5 defines it.
package crud

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Name is the map's name in a systems entry's api key.
const Name = "crud"

// Map reads one system's requests.
type Map struct {
	system      string
	mu          sync.Mutex
	collections map[string]bool
}

var _ interpose.API = (*Map)(nil)

// New returns the map for the system named system. It maps no request
// until SetCollections.
func New(system string) *Map { return &Map{system: system} }

// SetCollections names the collections the system serves.
func (m *Map) SetCollections(names []string) {
	collections := map[string]bool{}
	for _, name := range names {
		collections[name] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collections = collections
}

func (m *Map) serves(collection string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.collections[collection]
}

var verbs = map[[2]string]string{
	{http.MethodGet, ""}:      "list",
	{http.MethodPost, ""}:     "create",
	{http.MethodGet, "id"}:    "get",
	{http.MethodPut, "id"}:    "update",
	{http.MethodPatch, "id"}:  "patch",
	{http.MethodDelete, "id"}: "delete",
}

// Classify reads /<c> and /<c>/<id> for each collection c.
func (m *Map) Classify(r *http.Request) interpose.Request {
	req := interpose.Request{Verb: strings.ToLower(r.Method), Unmapped: true}
	req.Ref.System = m.system
	segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(segments) > 2 || !m.serves(segments[0]) || len(segments) == 2 && segments[1] == "" {
		return req
	}
	key := [2]string{r.Method, ""}
	if len(segments) == 2 {
		key[1] = "id"
	}
	verb, ok := verbs[key]
	if !ok {
		return req
	}
	req.Verb, req.Unmapped = verb, false
	req.Ref.Resource = segments[0]
	if len(segments) == 2 {
		req.Ref.Name = segments[1]
	}
	if verb == "create" {
		req.RequestName = requestedName(r)
	}
	return req
}

// requestedName reads the body's name and leaves r's body as it was.
func requestedName(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, interpose.MaxResultBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	var object struct {
		Name any `json:"name"`
	}
	if err != nil || json.Unmarshal(body, &object) != nil {
		return ""
	}
	name, _ := object.Name.(string)
	return name
}

// Objects reads a list's items, or else one object. An empty list gives an
// empty, non-nil slice.
func (m *Map) Objects(req interpose.Request, _ http.Header, body []byte) []interpose.Object {
	raws := []json.RawMessage{body}
	if req.Verb == "list" {
		var list struct {
			Items *[]json.RawMessage `json:"items"`
		}
		if json.Unmarshal(body, &list) != nil || list.Items == nil {
			return nil
		}
		raws = *list.Items
	}
	objects := make([]interpose.Object, 0, len(raws))
	for _, raw := range raws {
		object, err := m.Object(req.Ref.Resource, raw)
		if err != nil {
			return nil
		}
		objects = append(objects, object)
	}
	return objects
}

// Object reads a JSON object with a string id and a version, a number or
// a string that changes with every change of the object.
func (m *Map) Object(collection string, body json.RawMessage) (interpose.Object, error) {
	var fields struct {
		ID      *string `json:"id"`
		Version any     `json:"version"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return interpose.Object{}, err
	}
	if fields.ID == nil {
		return interpose.Object{}, errors.New("the object has no string id")
	}
	var rv string
	switch v := fields.Version.(type) {
	case json.Number:
		rv = v.String()
	case string:
		rv = v
	default:
		return interpose.Object{}, errors.New("the object has no number or string version")
	}
	o := interpose.Object{UID: m.system + "/" + collection + "/" + *fields.ID, RV: rv, Body: body}
	o.Ref.System, o.Ref.Resource, o.Ref.Name = m.system, collection, *fields.ID
	return o, nil
}

// Error is {"error": {"code", "message"}}.
func (m *Map) Error(code int, message string) (string, []byte) {
	var e struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	e.Error.Code, e.Error.Message = code, message
	body, _ := json.Marshal(e)
	return "application/json", body
}

// Write spells a POST to the collection, or a merge PATCH or DELETE of
// the id.
func (m *Map) Write(op fake.Op, ref trace.Ref, body []byte) (fake.Write, error) {
	switch op {
	case fake.Create:
		return fake.Write{Method: http.MethodPost, Path: "/" + ref.Resource, ContentType: "application/json", Body: body}, nil
	case fake.Patch:
		return fake.Write{Method: http.MethodPatch, Path: "/" + ref.Resource + "/" + ref.Name, ContentType: "application/merge-patch+json", Body: body}, nil
	case fake.Delete:
		return fake.Write{Method: http.MethodDelete, Path: "/" + ref.Resource + "/" + ref.Name}, nil
	}
	return fake.Write{}, fmt.Errorf("crud has no write %q", op)
}

// Gone accepts a 404 or a 409.
func (m *Map) Gone(status int, _ []byte) bool {
	return status == http.StatusNotFound || status == http.StatusConflict
}
