// Package crudfake is a small JSON server with asynchronous states and
// strict 404 and 409 answers. It copies no real API, and imports no other
// package of this module.
package crudfake

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// Config is the YAML file.
type Config struct {
	Collections map[string]Collection `json:"collections"`
}

// Collection says how one collection's resources behave.
type Collection struct {
	// Unique lists top-level fields no two live resources share.
	Unique      []string `json:"unique,omitempty"`
	Pending     string   `json:"pending"`
	Ready       string   `json:"ready"`
	Deleting    string   `json:"deleting"`
	CreateDelay Duration `json:"createDelay"`
	DeleteDelay Duration `json:"deleteDelay"`
}

// Duration reads a string such as 500ms.
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration must be a string such as 500ms: %s", b)
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

// The fake contract of DESIGN.md §7, duplicated from internal/fake.
const (
	contractPrefix = "_fake"
	resourcesPath  = "/_fake/resources"
	faultsPath     = "/_fake/faults"
	stuck          = "stuck"
	slowDelete     = "slowDelete"
)

type listing struct {
	Collections []string `json:"collections"`
	Pending     bool     `json:"pending"`
	Resources   []listed `json:"resources"`
}

type listed struct {
	Collection string          `json:"collection"`
	Body       json.RawMessage `json:"body"`
}

type faultRequest struct {
	Effect     string `json:"effect"`
	Collection string `json:"collection,omitempty"`
	ID         string `json:"id,omitempty"`
}

type faultStarted struct {
	ID string `json:"id"`
}

var reserved = []string{"id", "version", "state"}

// Load reads the YAML file strictly.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (cfg *Config) validate() error {
	if len(cfg.Collections) == 0 {
		return errors.New("no collections")
	}
	for name, c := range cfg.Collections {
		if name == "" || name == contractPrefix || strings.Contains(name, "/") {
			return fmt.Errorf("collection name %q is not allowed", name)
		}
		states := []string{c.Pending, c.Ready, c.Deleting}
		if c.Pending == "" || c.Ready == "" || c.Deleting == "" ||
			c.Pending == c.Ready || c.Ready == c.Deleting || c.Pending == c.Deleting {
			return fmt.Errorf("collection %s: pending, ready and deleting must be distinct and set, got %q", name, states)
		}
		if c.CreateDelay.Duration < 0 || c.DeleteDelay.Duration < 0 {
			return fmt.Errorf("collection %s: a delay is negative", name)
		}
		for _, f := range c.Unique {
			if isReserved(f) {
				return fmt.Errorf("collection %s: unique field %q is reserved", name, f)
			}
		}
	}
	return nil
}

func isReserved(field string) bool {
	for _, r := range reserved {
		if field == r {
			return true
		}
	}
	return false
}

// Server serves one Config's collections and the fake contract.
type Server struct {
	mu          sync.Mutex
	collections map[string]*collection
	faults      map[string]faultRequest
	nextFault   int
	closed      bool
}

type collection struct {
	Collection
	name  string
	next  int
	items map[string]*resource
}

type resource struct {
	n       int
	id      string
	version int
	state   string
	client  map[string]any
	// timer is the scheduled state change, or nil.
	timer *time.Timer
	gen   int
}

// New returns a server with no resources.
func New(cfg *Config) *Server {
	s := &Server{collections: map[string]*collection{}, faults: map[string]faultRequest{}}
	for name, c := range cfg.Collections {
		s.collections[name] = &collection{Collection: c, name: name, items: map[string]*resource{}}
	}
	return s
}

// Close stops the server's timers.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, c := range s.collections {
		for _, r := range c.items {
			s.unschedule(r)
		}
	}
}

type httpError struct {
	code    int
	message string
}

func (e *httpError) Error() string { return e.message }

func fail(code int, format string, args ...any) *httpError {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	code, body, err := s.route(r)
	s.mu.Unlock()
	if err != nil {
		code = err.code
		body = map[string]any{"error": map[string]any{"code": err.code, "message": err.message}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (s *Server) route(r *http.Request) (int, any, *httpError) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if parts[0] == contractPrefix {
		return s.routeContract(r, parts[1:])
	}
	c, ok := s.collections[parts[0]]
	if !ok {
		return 0, nil, fail(http.StatusNotFound, "no collection %q", parts[0])
	}
	switch len(parts) {
	case 1:
		switch r.Method {
		case http.MethodGet:
			return http.StatusOK, s.list(c, r), nil
		case http.MethodPost:
			return s.create(c, r)
		}
		return 0, nil, notAllowed(r)
	case 2:
		res, ok := c.items[parts[1]]
		if !ok {
			return 0, nil, fail(http.StatusNotFound, "no %s %q", parts[0], parts[1])
		}
		switch r.Method {
		case http.MethodGet:
			return http.StatusOK, res.body(), nil
		case http.MethodPut, http.MethodPatch:
			return s.write(c, res, r)
		case http.MethodDelete:
			return s.delete(c, res)
		}
		return 0, nil, notAllowed(r)
	}
	return 0, nil, fail(http.StatusNotFound, "no path %s", r.URL.Path)
}

func notAllowed(r *http.Request) *httpError {
	return fail(http.StatusMethodNotAllowed, "%s is not allowed on %s", r.Method, r.URL.Path)
}

func (s *Server) routeContract(r *http.Request, parts []string) (int, any, *httpError) {
	switch {
	case len(parts) == 1 && parts[0] == "resources":
		if r.Method != http.MethodGet {
			return 0, nil, notAllowed(r)
		}
		return http.StatusOK, s.listing(), nil
	case len(parts) == 1 && parts[0] == "faults":
		if r.Method != http.MethodPost {
			return 0, nil, notAllowed(r)
		}
		return s.startFault(r)
	case len(parts) == 2 && parts[0] == "faults":
		if r.Method != http.MethodDelete {
			return 0, nil, notAllowed(r)
		}
		return s.endFault(parts[1])
	}
	return 0, nil, fail(http.StatusNotFound, "no path %s", r.URL.Path)
}

func (s *Server) list(c *collection, r *http.Request) any {
	items := []any{}
	for _, res := range c.sorted() {
		if matches(res, r.URL.Query()) {
			items = append(items, res.body())
		}
	}
	return map[string]any{"items": items}
}

// matches compares each query value with a string field, or with the JSON
// text of any other field.
func matches(res *resource, query map[string][]string) bool {
	body := res.body()
	for field, values := range query {
		v, ok := body[field].(string)
		if !ok {
			text, err := json.Marshal(body[field])
			if err != nil || body[field] == nil {
				return false
			}
			v = string(text)
		}
		for _, want := range values {
			if v != want {
				return false
			}
		}
	}
	return true
}

func (s *Server) create(c *collection, r *http.Request) (int, any, *httpError) {
	client, err := readObject(r)
	if err != nil {
		return 0, nil, err
	}
	for _, f := range reserved {
		if _, ok := client[f]; ok {
			return 0, nil, fail(http.StatusBadRequest, "a client may not set %s", f)
		}
	}
	if err := c.checkUnique(client, nil); err != nil {
		return 0, nil, err
	}
	c.next++
	res := &resource{n: c.next, id: fmt.Sprintf("%s-%d", c.name, c.next), version: 1, state: c.Pending, client: client}
	c.items[res.id] = res
	s.schedule(c, res)
	return http.StatusCreated, res.body(), nil
}

// write answers PUT and PATCH. The body may repeat id and state as they
// are; a version that is not the current one answers 409.
func (s *Server) write(c *collection, res *resource, r *http.Request) (int, any, *httpError) {
	patch, err := readObject(r)
	if err != nil {
		return 0, nil, err
	}
	if res.state == c.Deleting {
		return 0, nil, fail(http.StatusConflict, "%s is %s", res.id, res.state)
	}
	if err := res.checkReserved(patch); err != nil {
		return 0, nil, err
	}
	next := patch
	if r.Method == http.MethodPatch {
		next = mergePatch(res.client, patch).(map[string]any)
	}
	if err := c.checkUnique(next, res); err != nil {
		return 0, nil, err
	}
	if !reflect.DeepEqual(next, res.client) {
		res.client = next
		res.version++
	}
	return http.StatusOK, res.body(), nil
}

func (res *resource) checkReserved(patch map[string]any) *httpError {
	current := res.body()
	for _, f := range reserved {
		v, ok := patch[f]
		if !ok {
			continue
		}
		delete(patch, f)
		if reflect.DeepEqual(v, current[f]) {
			continue
		}
		if f == "version" {
			return fail(http.StatusConflict, "%s is at version %d, not %v", res.id, res.version, v)
		}
		return fail(http.StatusBadRequest, "a client may not change %s", f)
	}
	return nil
}

func (s *Server) delete(c *collection, res *resource) (int, any, *httpError) {
	if res.state != c.Deleting {
		s.unschedule(res)
		res.state = c.Deleting
		res.version++
		s.schedule(c, res)
	}
	return http.StatusAccepted, res.body(), nil
}

func (c *collection) checkUnique(client map[string]any, self *resource) *httpError {
	for _, f := range c.Unique {
		v, ok := client[f]
		if !ok || v == nil {
			continue
		}
		for _, other := range c.items {
			if other != self && reflect.DeepEqual(other.client[f], v) {
				return fail(http.StatusConflict, "%s holds %s %v", other.id, f, v)
			}
		}
	}
	return nil
}

func (c *collection) sorted() []*resource {
	out := make([]*resource, 0, len(c.items))
	for _, res := range c.items {
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].n < out[j].n })
	return out
}

func (res *resource) body() map[string]any {
	out := make(map[string]any, len(res.client)+3)
	for k, v := range res.client {
		out[k] = v
	}
	out["id"] = res.id
	out["version"] = json.Number(strconv.Itoa(res.version))
	out["state"] = res.state
	return out
}

func readObject(r *http.Request) (map[string]any, *httpError) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, fail(http.StatusBadRequest, "the body must be one JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fail(http.StatusBadRequest, "the body must be one JSON object")
	}
	return obj, nil
}

// mergePatch applies an RFC 7386 merge patch.
func mergePatch(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	out := map[string]any{}
	if ok {
		for k, v := range t {
			out[k] = v
		}
	}
	for k, v := range p {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = mergePatch(out[k], v)
		}
	}
	return out
}

func (s *Server) listing() listing {
	names := make([]string, 0, len(s.collections))
	for name := range s.collections {
		names = append(names, name)
	}
	sort.Strings(names)
	l := listing{Collections: names, Resources: []listed{}}
	for _, name := range names {
		for _, res := range s.collections[name].sorted() {
			body, _ := json.Marshal(res.body())
			l.Resources = append(l.Resources, listed{Collection: name, Body: body})
			l.Pending = l.Pending || res.timer != nil
		}
	}
	return l
}

func (s *Server) startFault(r *http.Request) (int, any, *httpError) {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	var f faultRequest
	if err := dec.Decode(&f); err != nil {
		return 0, nil, fail(http.StatusBadRequest, "bad fault: %v", err)
	}
	if f.Effect != stuck && f.Effect != slowDelete {
		return 0, nil, fail(http.StatusBadRequest, "unknown effect %q", f.Effect)
	}
	if _, ok := s.collections[f.Collection]; f.Collection != "" && !ok {
		return 0, nil, fail(http.StatusBadRequest, "no collection %q", f.Collection)
	}
	s.nextFault++
	id := fmt.Sprintf("fault-%d", s.nextFault)
	s.faults[id] = f
	for _, c := range s.collections {
		for _, res := range c.items {
			if holds(f, c, res) {
				s.unschedule(res)
			}
		}
	}
	return http.StatusCreated, faultStarted{ID: id}, nil
}

func (s *Server) endFault(id string) (int, any, *httpError) {
	if _, ok := s.faults[id]; !ok {
		return 0, nil, fail(http.StatusNotFound, "no fault %q", id)
	}
	delete(s.faults, id)
	for _, c := range s.collections {
		for _, res := range c.items {
			s.schedule(c, res)
		}
	}
	return http.StatusNoContent, nil, nil
}

func holds(f faultRequest, c *collection, res *resource) bool {
	if (f.Collection != "" && f.Collection != c.name) || (f.ID != "" && f.ID != res.id) {
		return false
	}
	return (f.Effect == stuck && res.state == c.Pending) ||
		(f.Effect == slowDelete && res.state == c.Deleting)
}

// schedule starts res's next state change unless one is scheduled, none
// is due, or a fault holds res.
func (s *Server) schedule(c *collection, res *resource) {
	if s.closed || res.timer != nil || res.state == c.Ready {
		return
	}
	for _, f := range s.faults {
		if holds(f, c, res) {
			return
		}
	}
	delay := c.CreateDelay.Duration
	if res.state == c.Deleting {
		delay = c.DeleteDelay.Duration
	}
	res.gen++
	gen := res.gen
	res.timer = time.AfterFunc(delay, func() { s.advance(c, res, gen) })
}

func (s *Server) unschedule(res *resource) {
	if res.timer != nil {
		res.timer.Stop()
		res.timer = nil
		res.gen++
	}
}

func (s *Server) advance(c *collection, res *resource, gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.gen != gen || c.items[res.id] != res {
		return
	}
	res.timer = nil
	if res.state == c.Deleting {
		delete(c.items, res.id)
		return
	}
	res.state = c.Ready
	res.version++
}
