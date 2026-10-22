// Package s3 is the s3 API map: it reads an S3 API's buckets and objects
// as resources.
//
// It maps path-style requests, and ignores the x-id query parameter. A
// virtual-hosted request, whose Host is a dotted name, is unmapped: the
// listener sends it to the emulator's address, which reads it as
// path-style.
//
//   - GET / is list of buckets. POST / with Action=GetCallerIdentity is
//     STS, which moto serves on the same address, outside any resource.
//   - /<b>: PUT is create, HEAD is get, DELETE is delete, and GET is list
//     of the objects in b.
//   - /<b>?<sub>, such as tagging, versioning or policy, is get, update or
//     delete of the bucket, with subresource <sub>. POST /<b>?delete is
//     deletecollection of its objects.
//   - /<b>/<k>: GET and HEAD are get, PUT is create or update, DELETE is
//     delete of an object. DELETE ?versionId is delete with subresource
//     versionId, which may not end the instance.
//   - Multipart uploads, object subresources, and anything else, are
//     unmapped.
//
// A bucket's ref is {system, resource: buckets, name: b}, and an object's
// {system, resource: objects, namespace: b, name: k}. A listed bucket is
// {name, creationDate, versioning, tags, policy}, and an object {bucket,
// key, etag, versionId, size, lastModified}. A bucket's resourceVersion
// hashes its body without creationDate; an object's is its ETag, and
// "/<version ID>" in a versioned bucket. A response's object is partial,
// except the latest versions a GET ?versions shows. Restore ignores
// creationDate, lastModified and versionId. An injected error is an S3 <Error>
// document whose code fits the status.
//
// A harness create makes a bucket, or puts an object whose body's content
// is its data. A create's name may be <b>/<k> for an object. A bucket's
// patch replaces its tags with the patch's, and an object's puts its
// content. A delete deletes the bucket or the object. The harness signs
// nothing: its Authorization names a fake key, which moto accepts.
package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Name is the map's name in a systems entry's api key.
const Name = "s3"

// The resources the map reads.
const (
	Buckets = "buckets"
	Objects = "objects"
)

// Map reads one S3 server's requests.
type Map struct {
	system    string
	instances *fake.Instances
}

var (
	_ fake.API        = (*Map)(nil)
	_ fake.Lister     = (*Map)(nil)
	_ fake.Ender      = (*Map)(nil)
	_ fake.Authorizer = (*Map)(nil)
)

// New returns the map for the system named system.
func New(system string) *Map { return &Map{system: system, instances: fake.NewInstances()} }

// listParams are the query parameters of a list of objects.
var listParams = []string{
	"list-type", "versions", "prefix", "delimiter", "marker", "max-keys", "encoding-type", "continuation-token",
	"start-after", "fetch-owner", "key-marker", "version-id-marker", "optional-object-attributes",
}

// bucketKey reads the bucket and key of a path-style request. virtual says
// the request is virtual-hosted instead.
func bucketKey(r *http.Request) (bucket, key string, virtual bool) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.Contains(host, ".") && net.ParseIP(host) == nil {
		return "", "", true
	}
	bucket, key, _ = strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	return bucket, key, false
}

// callerIdentity says r is STS's GetCallerIdentity. It leaves r's body
// readable.
func callerIdentity(r *http.Request) bool {
	if r.URL.Query().Get("Action") == "GetCallerIdentity" {
		return true
	}
	if r.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
	form, _ := url.ParseQuery(string(body))
	return err == nil && form.Get("Action") == "GetCallerIdentity"
}

// params lists the query's keys, without x-id and the response- overrides.
func params(q url.Values) []string {
	var keys []string
	for k := range q {
		if k != "x-id" && !strings.HasPrefix(k, "response-") {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func (m *Map) Classify(r *http.Request) interpose.Request {
	req := interpose.Request{Verb: strings.ToLower(r.Method), Ref: trace.Ref{System: m.system}, Unmapped: true}
	bucket, key, virtual := bucketKey(r)
	if virtual {
		return req
	}
	keys := params(r.URL.Query())
	ref := trace.Ref{System: m.system}
	verb, sub := "", ""
	switch {
	case bucket == "" && r.Method == http.MethodPost:
		req.Unmapped = !callerIdentity(r)
		return req
	case bucket == "":
		if r.Method == http.MethodGet {
			verb, ref.Resource = "list", Buckets
		}
	case key == "":
		ref.Resource, ref.Name = Buckets, bucket
		listing := slices.ContainsFunc(keys, func(k string) bool { return slices.Contains(listParams, k) })
		switch {
		case slices.Contains(keys, "uploads"):
		case r.Method == http.MethodPost && slices.Equal(keys, []string{"delete"}):
			verb, ref = "deletecollection", trace.Ref{System: m.system, Resource: Objects, Namespace: bucket}
		case listing || len(keys) == 0 && r.Method == http.MethodGet:
			if r.Method == http.MethodGet {
				verb, ref = "list", trace.Ref{System: m.system, Resource: Objects, Namespace: bucket}
			}
		case len(keys) > 0:
			sub = keys[0]
			verb = map[string]string{http.MethodGet: "get", http.MethodHead: "get", http.MethodPut: "update", http.MethodDelete: "delete"}[r.Method]
		default:
			verb = map[string]string{http.MethodHead: "get", http.MethodPut: "create", http.MethodDelete: "delete"}[r.Method]
		}
	default:
		ref = trace.Ref{System: m.system, Resource: Objects, Namespace: bucket, Name: key}
		switch {
		case slices.Equal(keys, []string{"versionId"}) && r.Method == http.MethodDelete:
			verb, sub = "delete", "versionId"
		case len(keys) > 0 && !slices.Equal(keys, []string{"versionId"}):
		case r.Method == http.MethodPut:
			verb = "update"
			if !m.instances.Live(ref) {
				verb, req.RequestName = "create", key
			}
		default:
			verb = map[string]string{http.MethodGet: "get", http.MethodHead: "get", http.MethodDelete: "delete"}[r.Method]
		}
	}
	if verb == "" {
		return req
	}
	if verb == "create" && ref.Resource == Buckets {
		req.RequestName = bucket
	}
	req.Verb, req.Ref, req.Subresource, req.Unmapped = verb, ref, sub, false
	return req
}

// Objects shows a partial object of the request's ref, or a list's
// objects. A create starts an instance, and a delete without subresource
// ends one.
func (m *Map) Objects(req interpose.Request, header http.Header, body []byte) []interpose.Object {
	ref := req.Ref
	switch {
	case req.Verb == "list" && ref.Resource == Buckets:
		var list listAllMyBuckets
		if xml.Unmarshal(body, &list) != nil {
			return nil
		}
		objects := []interpose.Object{}
		for _, b := range list.Buckets {
			objects = append(objects, m.partial(trace.Ref{System: m.system, Resource: Buckets, Name: b.Name}, "", m.instances.Shown))
		}
		return objects
	case req.Verb == "list":
		return m.listed(ref.Namespace, body)
	case req.Verb == "deletecollection":
		var result struct {
			Deleted []struct {
				Key          string
				VersionID    string `xml:"VersionId"`
				DeleteMarker bool
			} `xml:"Deleted"`
		}
		if xml.Unmarshal(body, &result) != nil {
			return nil
		}
		objects := []interpose.Object{}
		for _, d := range result.Deleted {
			if d.VersionID != "" && !d.DeleteMarker {
				continue
			}
			object := trace.Ref{System: m.system, Resource: Objects, Namespace: ref.Namespace, Name: d.Key}
			if uid, ended := m.instances.Deleted(object); ended {
				objects = append(objects, interpose.Object{Ref: object, UID: uid, Partial: true})
			}
		}
		return objects
	case req.Verb == "delete" && req.Subresource == "":
		uid, ended := m.instances.Deleted(ref)
		if !ended {
			return []interpose.Object{}
		}
		return []interpose.Object{{Ref: ref, UID: uid, Partial: true}}
	case req.Verb == "create":
		return []interpose.Object{m.partial(ref, objectRV(header, body), m.instances.Created)}
	}
	return []interpose.Object{m.partial(ref, objectRV(header, body), m.instances.Shown)}
}

// partial is a response's object of ref. An object's RV comes from rv.
func (m *Map) partial(ref trace.Ref, rv string, instance func(trace.Ref) string) interpose.Object {
	o := interpose.Object{Ref: ref, UID: instance(ref), Partial: true}
	if ref.Resource == Objects {
		o.RV = rv
	}
	return o
}

// objectRV reads an object's ETag and version ID from a response.
func objectRV(header http.Header, body []byte) string {
	etag := header.Get("ETag")
	if etag == "" {
		var copied struct{ ETag string }
		if xml.Unmarshal(body, &copied) == nil {
			etag = copied.ETag
		}
	}
	if etag == "" {
		return ""
	}
	return rv(etag, header.Get("x-amz-version-id"))
}

func rv(etag, versionID string) string {
	etag = strings.Trim(etag, `"`)
	if versionID == "" || versionID == "null" {
		return etag
	}
	return etag + "/" + versionID
}

// listed reads the latest versions a GET ?versions shows as objects, or a
// list's contents as partial objects.
func (m *Map) listed(bucket string, body []byte) []interpose.Object {
	var versions listVersions
	if xml.Unmarshal(body, &versions) != nil {
		return nil
	}
	objects := []interpose.Object{}
	if versions.XMLName.Local == "ListVersionsResult" {
		for _, v := range versions.Versions {
			if !v.IsLatest {
				continue
			}
			o, err := m.Object(Objects, v.listed(bucket))
			if err != nil {
				return nil
			}
			objects = append(objects, o)
		}
		return objects
	}
	var list struct {
		XMLName  xml.Name
		Contents []struct{ Key string } `xml:"Contents"`
	}
	if xml.Unmarshal(body, &list) != nil || list.XMLName.Local != "ListBucketResult" {
		return nil
	}
	for _, c := range list.Contents {
		object := trace.Ref{System: m.system, Resource: Objects, Namespace: bucket, Name: c.Key}
		objects = append(objects, interpose.Object{Ref: object, UID: m.instances.Shown(object), Partial: true})
	}
	return objects
}

// Error is an S3 <Error> document.
func (m *Map) Error(code int, message string) (string, []byte) {
	name, known := errorCodes[code]
	if !known {
		name = strings.ReplaceAll(http.StatusText(code), " ", "")
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: name, Message: message})
	return "application/xml", append([]byte(xml.Header), body...)
}

var errorCodes = map[int]string{
	http.StatusBadRequest:          "InvalidRequest",
	http.StatusForbidden:           "AccessDenied",
	http.StatusNotFound:            "NoSuchKey",
	http.StatusConflict:            "OperationAborted",
	http.StatusPreconditionFailed:  "PreconditionFailed",
	http.StatusTooManyRequests:     "SlowDown",
	http.StatusInternalServerError: "InternalError",
	http.StatusServiceUnavailable:  "ServiceUnavailable",
}

// Bucket is a listed bucket's body. Policy is the policy document, or nil.
type Bucket struct {
	Name         string            `json:"name"`
	CreationDate string            `json:"creationDate"`
	Versioning   string            `json:"versioning"`
	Tags         map[string]string `json:"tags"`
	Policy       *string           `json:"policy"`
}

// Object is a listed object's body.
type Object struct {
	Bucket       string `json:"bucket"`
	Key          string `json:"key"`
	ETag         string `json:"etag"`
	VersionID    string `json:"versionId"`
	Size         int64  `json:"size"`
	LastModified string `json:"lastModified"`
}

// Object reads a listed bucket or object.
func (m *Map) Object(collection string, body json.RawMessage) (interpose.Object, error) {
	ref := trace.Ref{System: m.system, Resource: collection}
	var version string
	switch collection {
	case Buckets:
		var b Bucket
		if err := json.Unmarshal(body, &b); err != nil {
			return interpose.Object{}, err
		}
		if b.Name == "" {
			return interpose.Object{}, errors.New("the bucket has no name")
		}
		ref.Name = b.Name
		b.CreationDate = ""
		hashed, _ := json.Marshal(b)
		sum := sha256.Sum256(hashed)
		version = hex.EncodeToString(sum[:8])
	case Objects:
		var o Object
		if err := json.Unmarshal(body, &o); err != nil {
			return interpose.Object{}, err
		}
		if o.Bucket == "" || o.Key == "" || o.ETag == "" {
			return interpose.Object{}, errors.New("the object has no bucket, key or etag")
		}
		ref.Namespace, ref.Name, version = o.Bucket, o.Key, rv(o.ETag, o.VersionID)
	default:
		return interpose.Object{}, fmt.Errorf("s3 has no collection %q", collection)
	}
	return interpose.Object{Ref: ref, UID: m.instances.Shown(ref), RV: version, Body: body}, nil
}

func escape(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// Write spells a PUT or DELETE of the bucket or object.
func (m *Map) Write(op fake.Op, ref trace.Ref, body []byte) (fake.Write, error) {
	path := "/" + escape(ref.Name)
	if ref.Resource == Objects && ref.Namespace != "" {
		path = "/" + escape(ref.Namespace) + path
	}
	if ref.Resource != Buckets && ref.Resource != Objects {
		return fake.Write{}, fmt.Errorf("s3 has no resource %q", ref.Resource)
	}
	switch {
	case op == fake.Delete:
		return fake.Write{Method: http.MethodDelete, Path: path}, nil
	case op == fake.Create && ref.Resource == Buckets:
		return fake.Write{Method: http.MethodPut, Path: path}, nil
	case op == fake.Patch && ref.Resource == Buckets:
		var patch struct {
			Tags map[string]*string `json:"tags"`
		}
		if err := json.Unmarshal(body, &patch); err != nil || patch.Tags == nil {
			return fake.Write{}, fmt.Errorf("a bucket's patch needs tags: %s", body)
		}
		return fake.Write{Method: http.MethodPut, Path: path + "?tagging", ContentType: "application/xml", Body: tagging(patch.Tags)}, nil
	case op == fake.Create || op == fake.Patch:
		var object struct {
			Content *string `json:"content"`
		}
		if err := json.Unmarshal(body, &object); err != nil || object.Content == nil {
			return fake.Write{}, fmt.Errorf("an object's %s needs content: %s", op, body)
		}
		return fake.Write{Method: http.MethodPut, Path: path, ContentType: "application/octet-stream", Body: []byte(*object.Content)}, nil
	}
	return fake.Write{}, fmt.Errorf("s3 has no write %q", op)
}

// tagging is a Tagging document of the tags that are not null.
func tagging(tags map[string]*string) []byte {
	type tag struct{ Key, Value string }
	var set []tag
	for k, v := range tags {
		if v != nil {
			set = append(set, tag{k, *v})
		}
	}
	slices.SortFunc(set, func(a, b tag) int { return strings.Compare(a.Key, b.Key) })
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"Tagging"`
		Tags    []tag    `xml:"TagSet>Tag"`
	}{Tags: set})
	return body
}

type listAllMyBuckets struct {
	Buckets []struct {
		Name, CreationDate string
	} `xml:"Buckets>Bucket"`
}

type listVersions struct {
	XMLName             xml.Name
	IsTruncated         bool
	NextKeyMarker       string
	NextVersionIdMarker string
	Versions            []version `xml:"Version"`
}

type version struct {
	Key, ETag, VersionId, LastModified string
	Size                               int64
	IsLatest                           bool
}

func (v version) listed(bucket string) json.RawMessage {
	if v.VersionId == "null" {
		v.VersionId = ""
	}
	body, _ := json.Marshal(Object{
		Bucket: bucket, Key: v.Key, ETag: strings.Trim(v.ETag, `"`), VersionID: v.VersionId,
		Size: v.Size, LastModified: v.LastModified,
	})
	return body
}

// List reads each bucket, its versioning, tags and policy, and the latest
// version of each of its objects.
func (m *Map) List(ctx context.Context, do func(*http.Request) (*http.Response, error), base string) (fake.Listing, error) {
	l := lister{ctx: ctx, do: do, base: base}
	var all listAllMyBuckets
	if _, err := l.get("/", &all); err != nil {
		return fake.Listing{}, err
	}
	listing := fake.Listing{Collections: []string{Buckets, Objects}, Resources: []fake.Listed{}}
	for _, b := range all.Buckets {
		bucket, objects, err := l.bucket(b.Name, b.CreationDate)
		if errors.Is(err, errNoSuchBucket) {
			continue
		}
		if err != nil {
			return fake.Listing{}, err
		}
		listing.Resources = append(listing.Resources, fake.Listed{Collection: Buckets, Body: bucket})
		for _, o := range objects {
			listing.Resources = append(listing.Resources, fake.Listed{Collection: Objects, Body: o})
		}
	}
	return listing, nil
}

var errNoSuchBucket = errors.New("NoSuchBucket")

type lister struct {
	ctx  context.Context
	do   func(*http.Request) (*http.Response, error)
	base string
}

// get decodes a 200 answer into out, or returns it raw into a *[]byte. It
// reports false for a 404, and errNoSuchBucket for one whose code says so.
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
	case http.StatusOK:
	case http.StatusNotFound:
		var e struct{ Code string }
		if xml.Unmarshal(body, &e) == nil && e.Code == errNoSuchBucket.Error() {
			return false, errNoSuchBucket
		}
		return false, nil
	default:
		return false, fmt.Errorf("GET %s: %s %s", path, resp.Status, bytes.TrimSpace(body))
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = body
		return true, nil
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("GET %s: %w", path, err)
	}
	return true, nil
}

func (l lister) bucket(name, creationDate string) (json.RawMessage, []json.RawMessage, error) {
	path := "/" + url.PathEscape(name)
	b := Bucket{Name: name, CreationDate: creationDate, Tags: map[string]string{}}
	var versioning struct{ Status string }
	if _, err := l.get(path+"?versioning", &versioning); err != nil {
		return nil, nil, err
	}
	b.Versioning = versioning.Status
	var tags struct {
		Tags []struct{ Key, Value string } `xml:"TagSet>Tag"`
	}
	if _, err := l.get(path+"?tagging", &tags); err != nil {
		return nil, nil, err
	}
	for _, t := range tags.Tags {
		b.Tags[t.Key] = t.Value
	}
	var policy []byte
	found, err := l.get(path+"?policy", &policy)
	if err != nil {
		return nil, nil, err
	}
	if found {
		b.Policy = new(string(policy))
	}
	var objects []json.RawMessage
	query := url.Values{"versions": {""}}
	for {
		var page listVersions
		if _, err := l.get(path+"?"+strings.TrimSuffix(query.Encode(), "="), &page); err != nil {
			return nil, nil, err
		}
		for _, v := range page.Versions {
			if v.IsLatest {
				objects = append(objects, v.listed(name))
			}
		}
		if !page.IsTruncated {
			break
		}
		query = url.Values{"versions": {""}, "key-marker": {page.NextKeyMarker}, "version-id-marker": {page.NextVersionIdMarker}}
	}
	body, err := json.Marshal(b)
	return body, objects, err
}

func (m *Map) SetCollections([]string) {}

func (m *Map) Ended(ref trace.Ref, uid string) { m.instances.Ended(ref, uid) }

// Authorize names a fake key unless the request has an Authorization.
func (m *Map) Authorize(r *http.Request) {
	if r.Header.Get("Authorization") != "" {
		return
	}
	day := time.Now().UTC().Format("20060102")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=reconciler-fuzzer/"+day+"/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0")
}

// Gone accepts a 404 whose code is NoSuchBucket or NoSuchKey, or that has
// no body, as a HEAD's has.
func (m *Map) Gone(status int, body []byte) bool {
	if status != http.StatusNotFound {
		return false
	}
	var e struct{ Code string }
	return len(bytes.TrimSpace(body)) == 0 || xml.Unmarshal(body, &e) == nil && (e.Code == "NoSuchBucket" || e.Code == "NoSuchKey")
}
