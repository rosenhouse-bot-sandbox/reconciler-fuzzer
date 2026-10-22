// Package vault is the vault API map: it reads a Vault server's KV version
// 2 mounts as resources.
//
// It maps /v1/<mount>/ for each mount of type kv, version 2, that
// GET /v1/sys/mounts lists. A ref is {system, resource: mount, name: path}.
//
//   - data/<path>: GET is get; POST and PUT write a version, create or
//     update; PATCH is patch; DELETE deletes the latest version, a patch.
//   - metadata/<path>: GET is get; LIST, or GET ?list=true, is list of the
//     mount; POST, PUT and PATCH are patch; DELETE is delete, which ends
//     the instance.
//   - delete/, undelete/ and destroy/<path> are patch.
//   - sys/ and auth/ paths have no resource, so no check counts them.
//   - Any other path is unmapped.
//
// The listed body is {path, current_version, versions: {"<n>": {deleted,
// destroyed}}, data, custom_metadata, created_time, updated_time}, where
// data is the current version's, or null when it is deleted or destroyed.
// Its resourceVersion hashes it, so it changes with every write, delete,
// undelete and destroy. A response's object is partial. Restore compares
// only path, data and custom_metadata. An injected error is {"errors":
// [message]}. The harness sends X-Vault-Token.
//
// A harness create or patch writes a version of data/<path> with the body
// as its data; a patch is a merge patch of the data, not of the listed
// body, so {"k": "v"} sets the data's key k. A delete deletes
// metadata/<path>. A 404 answer says the secret is gone.
package vault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Name is the map's name in a systems entry's api key.
const Name = "vault"

// Map reads one Vault server's requests.
type Map struct {
	system    string
	token     string
	instances *fake.Instances

	mu     sync.Mutex
	mounts []string
}

var (
	_ fake.API        = (*Map)(nil)
	_ fake.Lister     = (*Map)(nil)
	_ fake.Ender      = (*Map)(nil)
	_ fake.Authorizer = (*Map)(nil)
)

// New returns the map for the system named system, whose harness requests
// carry token.
func New(system, token string) *Map {
	return &Map{system: system, token: token, instances: fake.NewInstances()}
}

// SetCollections names the KV version 2 mounts, without their slashes.
func (m *Map) SetCollections(mounts []string) {
	sorted := slices.Clone(mounts)
	// Longest first, so that a nested mount wins.
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts = sorted
}

// mount splits rest into the mount it starts with and what follows it.
func (m *Map) mount(rest string) (mount, after string, found bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mount := range m.mounts {
		if after, found := strings.CutPrefix(rest, mount+"/"); found {
			return mount, after, true
		}
	}
	return "", "", false
}

func (m *Map) Classify(r *http.Request) interpose.Request {
	req := interpose.Request{Verb: strings.ToLower(r.Method), Ref: trace.Ref{System: m.system}, Unmapped: true}
	rest, found := strings.CutPrefix(r.URL.Path, "/v1/")
	if !found {
		return req
	}
	if strings.HasPrefix(rest, "sys/") || strings.HasPrefix(rest, "auth/") {
		req.Unmapped = false
		return req
	}
	mount, after, found := m.mount(rest)
	if !found {
		return req
	}
	op, path, _ := strings.Cut(after, "/")
	ref := trace.Ref{System: m.system, Resource: mount, Name: path}
	verb := ""
	switch {
	case op == "metadata" && (r.Method == "LIST" || r.Method == http.MethodGet && r.URL.Query().Get("list") == "true"):
		verb, ref.Name = "list", ""
	case path == "" || strings.HasSuffix(path, "/"):
	case op == "data":
		verb = map[string]string{http.MethodGet: "get", http.MethodPost: "write", http.MethodPut: "write", http.MethodPatch: "patch", http.MethodDelete: "patch"}[r.Method]
	case op == "metadata":
		verb = map[string]string{http.MethodGet: "get", http.MethodPost: "patch", http.MethodPut: "patch", http.MethodPatch: "patch", http.MethodDelete: "delete"}[r.Method]
	case op == "delete" || op == "undelete" || op == "destroy":
		verb = map[string]string{http.MethodPost: "patch", http.MethodPut: "patch"}[r.Method]
	}
	if verb == "" {
		return req
	}
	if verb == "write" {
		verb = "update"
		if !m.instances.Live(ref) {
			verb, req.RequestName = "create", path
		}
	}
	req.Verb, req.Ref, req.Unmapped = verb, ref, false
	return req
}

// Objects shows a partial object of the request's ref. A create starts an
// instance, and a delete ends one. A list, or a path with no resource,
// shows none.
func (m *Map) Objects(req interpose.Request, _ http.Header, body []byte) []interpose.Object {
	if req.Ref.Resource == "" || req.Verb == "list" {
		return []interpose.Object{}
	}
	var uid string
	switch req.Verb {
	case "create":
		uid = m.instances.Created(req.Ref)
	case "delete":
		ended, found := m.instances.Deleted(req.Ref)
		if !found {
			return []interpose.Object{}
		}
		uid = ended
	default:
		if !m.instances.Live(req.Ref) {
			return []interpose.Object{}
		}
		uid = m.instances.Shown(req.Ref)
	}
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if len(body) > 0 && json.Unmarshal(body, &response) != nil {
		return nil
	}
	return []interpose.Object{{Ref: req.Ref, UID: uid, Body: response.Data, Partial: true}}
}

// Error is {"errors": [message]}, as Vault answers.
func (m *Map) Error(_ int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string][]string{"errors": {message}})
	return "application/json", body
}

// Listed is the body of a listed secret.
type Listed struct {
	Path           string             `json:"path"`
	CurrentVersion int                `json:"current_version"`
	Versions       map[string]Version `json:"versions"`
	Data           json.RawMessage    `json:"data"`
	CustomMetadata json.RawMessage    `json:"custom_metadata"`
	CreatedTime    string             `json:"created_time"`
	UpdatedTime    string             `json:"updated_time"`
}

// Version is one version's state in a Listed.
type Version struct {
	Deleted   bool `json:"deleted"`
	Destroyed bool `json:"destroyed"`
}

// Object reads a listed secret of mount.
func (m *Map) Object(mount string, body json.RawMessage) (interpose.Object, error) {
	var listed Listed
	if err := json.Unmarshal(body, &listed); err != nil {
		return interpose.Object{}, err
	}
	if listed.Path == "" {
		return interpose.Object{}, errors.New("the secret has no path")
	}
	ref := trace.Ref{System: m.system, Resource: mount, Name: listed.Path}
	sum := sha256.Sum256(body)
	return interpose.Object{Ref: ref, UID: m.instances.Shown(ref), RV: hex.EncodeToString(sum[:8]), Body: body}, nil
}

// Write spells a write of data/<path>, or a DELETE of metadata/<path>.
func (m *Map) Write(op fake.Op, ref trace.Ref, body []byte) (fake.Write, error) {
	path := "/v1/" + ref.Resource
	switch op {
	case fake.Create, fake.Patch:
		data, err := json.Marshal(map[string]json.RawMessage{"data": body})
		if err != nil {
			return fake.Write{}, err
		}
		if op == fake.Create {
			return fake.Write{Method: http.MethodPost, Path: path + "/data/" + ref.Name, ContentType: "application/json", Body: data}, nil
		}
		return fake.Write{Method: http.MethodPatch, Path: path + "/data/" + ref.Name, ContentType: "application/merge-patch+json", Body: data}, nil
	case fake.Delete:
		return fake.Write{Method: http.MethodDelete, Path: path + "/metadata/" + ref.Name}, nil
	}
	return fake.Write{}, fmt.Errorf("vault has no write %q", op)
}

// List reads the KV version 2 mounts, then each secret's metadata and
// current data.
func (m *Map) List(ctx context.Context, do func(*http.Request) (*http.Response, error), base string) (fake.Listing, error) {
	l := lister{ctx: ctx, do: do, base: base}
	var mounts map[string]json.RawMessage
	if _, err := l.get("/v1/sys/mounts", &mounts); err != nil {
		return fake.Listing{}, err
	}
	listing := fake.Listing{Collections: []string{}, Resources: []fake.Listed{}}
	for path, raw := range mounts {
		var mount struct {
			Type    string            `json:"type"`
			Options map[string]string `json:"options"`
		}
		if !strings.HasSuffix(path, "/") || json.Unmarshal(raw, &mount) != nil || mount.Type != "kv" || mount.Options["version"] != "2" {
			continue
		}
		listing.Collections = append(listing.Collections, strings.TrimSuffix(path, "/"))
	}
	slices.Sort(listing.Collections)
	for _, mount := range listing.Collections {
		paths, err := l.walk(mount, "")
		if err != nil {
			return fake.Listing{}, err
		}
		for _, path := range paths {
			body, found, err := l.secret(mount, path)
			if err != nil {
				return fake.Listing{}, err
			}
			if found {
				listing.Resources = append(listing.Resources, fake.Listed{Collection: mount, Body: body})
			}
		}
	}
	return listing, nil
}

type lister struct {
	ctx  context.Context
	do   func(*http.Request) (*http.Response, error)
	base string
}

// get decodes the data of a 200 answer into out, and reports false for a
// 404.
func (l lister) get(path string, out any) (bool, error) {
	req, err := http.NewRequestWithContext(l.ctx, http.MethodGet, l.base+path, nil)
	if err != nil {
		return false, err
	}
	resp, err := l.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("GET %s: %s %s", path, resp.Status, bytes.TrimSpace(body))
	}
	var answer struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	return true, json.Unmarshal(answer.Data, out)
}

func escape(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// walk lists the secret paths under prefix.
func (l lister) walk(mount, prefix string) ([]string, error) {
	var list struct {
		Keys []string `json:"keys"`
	}
	if _, err := l.get("/v1/"+escape(mount)+"/metadata/"+escape(prefix)+"?list=true", &list); err != nil {
		return nil, err
	}
	var paths []string
	for _, key := range list.Keys {
		if !strings.HasSuffix(key, "/") {
			paths = append(paths, prefix+key)
			continue
		}
		below, err := l.walk(mount, prefix+key)
		if err != nil {
			return nil, err
		}
		paths = append(paths, below...)
	}
	return paths, nil
}

// secret reads one listed secret. It reports false when the path is gone.
func (l lister) secret(mount, path string) (json.RawMessage, bool, error) {
	var metadata struct {
		CurrentVersion int             `json:"current_version"`
		CustomMetadata json.RawMessage `json:"custom_metadata"`
		CreatedTime    string          `json:"created_time"`
		UpdatedTime    string          `json:"updated_time"`
		Versions       map[string]struct {
			DeletionTime string `json:"deletion_time"`
			Destroyed    bool   `json:"destroyed"`
		} `json:"versions"`
	}
	prefix := "/v1/" + escape(mount)
	if found, err := l.get(prefix+"/metadata/"+escape(path), &metadata); !found || err != nil {
		return nil, false, err
	}
	listed := Listed{
		Path: path, CurrentVersion: metadata.CurrentVersion, Versions: map[string]Version{}, Data: json.RawMessage("null"),
		CustomMetadata: metadata.CustomMetadata, CreatedTime: metadata.CreatedTime, UpdatedTime: metadata.UpdatedTime,
	}
	for n, v := range metadata.Versions {
		listed.Versions[n] = Version{Deleted: deleted(v.DeletionTime), Destroyed: v.Destroyed}
	}
	if listed.CustomMetadata == nil {
		listed.CustomMetadata = json.RawMessage("null")
	}
	if metadata.CurrentVersion > 0 {
		var data struct {
			Data json.RawMessage `json:"data"`
		}
		found, err := l.get(prefix+"/data/"+escape(path)+"?version="+strconv.Itoa(metadata.CurrentVersion), &data)
		if err != nil {
			return nil, false, err
		}
		if found && data.Data != nil {
			listed.Data = data.Data
		}
	}
	body, err := json.Marshal(listed)
	return body, true, err
}

// deleted reports whether a version's deletion time has passed.
func deleted(deletionTime string) bool {
	if deletionTime == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, deletionTime)
	return err != nil || !at.After(time.Now())
}

func (m *Map) Ended(ref trace.Ref, uid string) { m.instances.Ended(ref, uid) }

// Authorize sets X-Vault-Token.
func (m *Map) Authorize(r *http.Request) { r.Header.Set("X-Vault-Token", m.token) }

// Gone accepts a 404.
func (m *Map) Gone(status int, _ []byte) bool { return status == http.StatusNotFound }
