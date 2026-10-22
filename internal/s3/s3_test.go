package s3_test

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
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/s3"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// recorded reads a response to method that testdata/record.sh saved.
func recorded(t *testing.T, name, method string) (*http.Response, []byte) {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", name+".http"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	resp, err := http.ReadResponse(bufio.NewReader(file), &http.Request{Method: method})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func bucket(name string) trace.Ref { return trace.Ref{System: "s3", Resource: "buckets", Name: name} }

func object(b, k string) trace.Ref {
	return trace.Ref{System: "s3", Resource: "objects", Namespace: b, Name: k}
}

// classify reads a request to a listener's address.
func classify(m *s3.Map, method, target string) interpose.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:9000"
	return m.Classify(r)
}

func TestClassifiesPathStyleRequests(t *testing.T) {
	m := s3.New("s3")
	none := trace.Ref{System: "s3"}
	objects := trace.Ref{System: "s3", Resource: "objects", Namespace: "logs"}
	for _, c := range []struct {
		method, target string
		want           interpose.Request
	}{
		{"GET", "/", interpose.Request{Verb: "list", Ref: trace.Ref{System: "s3", Resource: "buckets"}}},
		{"PUT", "/logs", interpose.Request{Verb: "create", Ref: bucket("logs"), RequestName: "logs"}},
		{"PUT", "/logs?x-id=CreateBucket", interpose.Request{Verb: "create", Ref: bucket("logs"), RequestName: "logs"}},
		{"HEAD", "/logs", interpose.Request{Verb: "get", Ref: bucket("logs")}},
		{"DELETE", "/logs", interpose.Request{Verb: "delete", Ref: bucket("logs")}},
		{"GET", "/logs", interpose.Request{Verb: "list", Ref: objects}},
		{"GET", "/logs?list-type=2&prefix=a", interpose.Request{Verb: "list", Ref: objects}},
		{"GET", "/logs?versions", interpose.Request{Verb: "list", Ref: objects}},
		{"GET", "/logs?tagging", interpose.Request{Verb: "get", Ref: bucket("logs"), Subresource: "tagging"}},
		{"PUT", "/logs?versioning", interpose.Request{Verb: "update", Ref: bucket("logs"), Subresource: "versioning"}},
		{"DELETE", "/logs?policy", interpose.Request{Verb: "delete", Ref: bucket("logs"), Subresource: "policy"}},
		{"POST", "/logs?delete", interpose.Request{Verb: "deletecollection", Ref: objects}},
		{"PUT", "/logs/a/k.txt?x-id=PutObject", interpose.Request{Verb: "create", Ref: object("logs", "a/k.txt"), RequestName: "a/k.txt"}},
		{"GET", "/logs/a/k.txt?response-content-type=text", interpose.Request{Verb: "get", Ref: object("logs", "a/k.txt")}},
		{"HEAD", "/logs/a/k.txt?versionId=v1", interpose.Request{Verb: "get", Ref: object("logs", "a/k.txt")}},
		{"DELETE", "/logs/a/k.txt", interpose.Request{Verb: "delete", Ref: object("logs", "a/k.txt")}},
		{"DELETE", "/logs/a/k.txt?versionId=v1", interpose.Request{Verb: "delete", Ref: object("logs", "a/k.txt"), Subresource: "versionId"}},
		{"POST", "/logs/big?uploads", interpose.Request{Verb: "post", Ref: none, Unmapped: true}},
		{"PUT", "/logs/big?partNumber=1&uploadId=u", interpose.Request{Verb: "put", Ref: none, Unmapped: true}},
		{"GET", "/logs?uploads", interpose.Request{Verb: "get", Ref: none, Unmapped: true}},
		{"PUT", "/logs/a/k.txt?tagging", interpose.Request{Verb: "put", Ref: none, Unmapped: true}},
		{"POST", "/logs", interpose.Request{Verb: "post", Ref: none, Unmapped: true}},
		{"DELETE", "/", interpose.Request{Verb: "delete", Ref: none, Unmapped: true}},
	} {
		if got := classify(m, c.method, c.target); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %s = %+v, want %+v", c.method, c.target, got, c.want)
		}
	}
	virtual := httptest.NewRequest("PUT", "/a/k.txt", nil)
	virtual.Host = "logs.s3.localhost:4566"
	if got := m.Classify(virtual); !got.Unmapped {
		t.Errorf("a virtual-hosted put = %+v, want it unmapped", got)
	}
	local := httptest.NewRequest("PUT", "/logs/k", nil)
	local.Host = "localhost:4566"
	if got := m.Classify(local); got.Ref != object("logs", "k") {
		t.Errorf("a put to localhost = %+v", got)
	}
}

// STS shares moto's address. No other service's request is placed.
func TestPlacesOnlyGetCallerIdentityOutsideAnyResource(t *testing.T) {
	m := s3.New("s3")
	for body, unmapped := range map[string]bool{
		"Action=GetCallerIdentity&Version=2011-06-15": false,
		"Action=CreateQueue&QueueName=q":              true,
		"":                                            true,
	} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		r.Host = "127.0.0.1:9000"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got := m.Classify(r)
		if got.Unmapped != unmapped || got.Ref.Resource != "" {
			t.Errorf("POST / %q = %+v, want unmapped %v", body, got, unmapped)
		}
		if rest, _ := io.ReadAll(r.Body); string(rest) != body {
			t.Errorf("POST / %q left the body %q", body, rest)
		}
	}
}

func show(t *testing.T, m *s3.Map, method, target, name string) []interpose.Object {
	t.Helper()
	resp, body := recorded(t, name, method)
	return m.Objects(classify(m, method, target), resp.Header, body)
}

func one(t *testing.T, what string, objects []interpose.Object, uid, rv string) {
	t.Helper()
	if len(objects) != 1 || objects[0].UID != uid || objects[0].RV != rv || !objects[0].Partial {
		t.Errorf("%s showed %+v, want %s at %q", what, objects, uid, rv)
	}
}

func TestResponsesShowPartialObjectsWithETagsAndVersions(t *testing.T) {
	m := s3.New("s3")
	one(t, "a bucket's create", show(t, m, "PUT", "/logs-bucket", "create-bucket"), "s3/buckets/logs-bucket/1", "")
	one(t, "a tagging put", show(t, m, "PUT", "/logs-bucket?tagging", "put-tagging"), "s3/buckets/logs-bucket/1", "")
	one(t, "an object's put", show(t, m, "PUT", "/logs-bucket/a/k.txt", "put-object"),
		"s3/objects/logs-bucket/a/k.txt/1", "5d41402abc4b2a76b9719d911017c592")
	if verb := classify(m, "PUT", "/logs-bucket/a/k.txt").Verb; verb != "update" {
		t.Errorf("a put of a live object is %s", verb)
	}
	one(t, "a head", show(t, m, "HEAD", "/logs-bucket/a/k.txt", "head-object"),
		"s3/objects/logs-bucket/a/k.txt/1", "5d41402abc4b2a76b9719d911017c592")
	one(t, "a versioned put", show(t, m, "PUT", "/data-bucket/k", "put-versioned"),
		"s3/objects/data-bucket/k/1", "6654c734ccab8f440ff0825eb443dc7f/97c55ef9-6212-4aba-bb7d-afe287e45fa4")
	listed := show(t, m, "GET", "/logs-bucket?list-type=2", "list-objects")
	if len(listed) != 1 || listed[0].Ref != object("logs-bucket", "a/k.txt") || !listed[0].Partial {
		t.Errorf("a list showed %+v", listed)
	}
	if got := show(t, m, "GET", "/", "list-buckets"); len(got) != 1 || got[0].Ref != bucket("logs-bucket") {
		t.Errorf("a list of buckets showed %+v", got)
	}
	versions := show(t, m, "GET", "/data-bucket?versions", "listing/GET_data-bucket_versions")
	if len(versions) != 1 || versions[0].Partial || versions[0].RV != "1b267619c4812cc46ee281747884ca50/673feb4e-6fb0-4d40-97d1-675e3cf3d1f6" {
		t.Errorf("a list of versions showed %+v", versions)
	}
}

func TestDeletesEndInstances(t *testing.T) {
	m := s3.New("s3")
	show(t, m, "PUT", "/logs-bucket", "create-bucket")
	show(t, m, "PUT", "/logs-bucket/a/k.txt", "put-object")
	show(t, m, "PUT", "/data-bucket/k", "put-versioned")
	one(t, "a multi-object delete", show(t, m, "POST", "/logs-bucket?delete", "delete-objects"), "s3/objects/logs-bucket/a/k.txt/1", "")
	one(t, "an object's delete", show(t, m, "DELETE", "/data-bucket/k", "delete-object"), "s3/objects/data-bucket/k/1", "")
	if got := show(t, m, "DELETE", "/data-bucket/k", "delete-object"); got == nil || len(got) != 0 {
		t.Errorf("a second delete showed %v", got)
	}
	one(t, "a bucket's delete", show(t, m, "DELETE", "/logs-bucket", "delete-bucket"), "s3/buckets/logs-bucket/1", "")
	one(t, "a put after the delete", show(t, m, "PUT", "/data-bucket/k", "put-versioned-again"),
		"s3/objects/data-bucket/k/2", "1b267619c4812cc46ee281747884ca50/673feb4e-6fb0-4d40-97d1-675e3cf3d1f6")
	if got := show(t, m, "PUT", "/logs-bucket/a/k.txt", "multipart"); len(got) != 1 || got[0].RV != "" {
		t.Errorf("an answer with no ETag showed %+v", got)
	}
}

// replay answers each request with the response record.sh saved under
// testdata/listing for it.
func replay(t *testing.T) func(*http.Request) (*http.Response, error) {
	return func(r *http.Request) (*http.Response, error) {
		name := strings.NewReplacer("/", "_", "?", "_", "=", "_", "&", "_").Replace(r.Method + r.URL.RequestURI())
		resp, body := recorded(t, filepath.Join("listing", name), r.Method)
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
		return resp, nil
	}
}

func TestAVersionedMultiObjectDeleteEndsOnlyAtADeleteMarker(t *testing.T) {
	m := s3.New("s3")
	show(t, m, "PUT", "/kept-bucket/k", "put-versioned")
	if got := show(t, m, "POST", "/kept-bucket?delete", "delete-objects-version"); got == nil || len(got) != 0 {
		t.Errorf("deleting an old version showed %+v, want nothing ended", got)
	}
	one(t, "a delete marker", show(t, m, "POST", "/kept-bucket?delete", "delete-objects-marker"), "s3/objects/kept-bucket/k/1", "")
}

func TestListReadsBucketsAndTheirLatestObjects(t *testing.T) {
	m := s3.New("s3")
	listing, err := m.List(context.Background(), replay(t), "http://moto.test")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range listing.Resources {
		got = append(got, l.Collection+" "+string(l.Body))
	}
	want := []string{
		`buckets {"name":"logs-bucket","creationDate":"2026-10-21T13:52:12.000Z","versioning":"","tags":{"team":"a"},"policy":null}`,
		`objects {"bucket":"logs-bucket","key":"a/k.txt","etag":"5d41402abc4b2a76b9719d911017c592","versionId":"","size":5,"lastModified":"2026-10-21T13:52:12.000Z"}`,
		`buckets {"name":"data-bucket","creationDate":"2026-10-21T13:52:12.000Z","versioning":"Enabled","tags":{},"policy":"{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Principal\":\"*\",\"Action\":\"s3:GetObject\",\"Resource\":\"arn:aws:s3:::data-bucket/*\"}]}"}`,
		`objects {"bucket":"data-bucket","key":"k","etag":"1b267619c4812cc46ee281747884ca50","versionId":"673feb4e-6fb0-4d40-97d1-675e3cf3d1f6","size":2,"lastModified":"2026-10-21T13:52:12.000Z"}`,
	}
	if !reflect.DeepEqual(listing.Collections, []string{"buckets", "objects"}) || !reflect.DeepEqual(got, want) {
		t.Errorf("listing = %v\n%s\nwant\n%s", listing.Collections, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	o, err := m.Object("objects", listing.Resources[3].Body)
	if err != nil || o.Ref != object("data-bucket", "k") || o.UID != "s3/objects/data-bucket/k/1" ||
		o.RV != "1b267619c4812cc46ee281747884ca50/673feb4e-6fb0-4d40-97d1-675e3cf3d1f6" {
		t.Errorf("Object = %+v, %v", o, err)
	}
	b, err := m.Object("buckets", listing.Resources[0].Body)
	if err != nil || b.Ref != bucket("logs-bucket") || b.RV == "" {
		t.Fatalf("Object = %+v, %v", b, err)
	}
	later, _ := m.Object("buckets", json.RawMessage(strings.Replace(string(listing.Resources[0].Body), "13:52:12", "14:00:00", 1)))
	retagged, _ := m.Object("buckets", json.RawMessage(strings.Replace(string(listing.Resources[0].Body), `"a"`, `"b"`, 1)))
	if later.RV != b.RV || retagged.RV == b.RV {
		t.Errorf("RVs %s, later %s, retagged %s: want creationDate ignored and tags hashed", b.RV, later.RV, retagged.RV)
	}
	m.Ended(b.Ref, b.UID)
	if again, _ := m.Object("buckets", listing.Resources[0].Body); again.UID != "s3/buckets/logs-bucket/2" {
		t.Errorf("a bucket listed after it ended is %s", again.UID)
	}
	for _, bad := range []string{`{"key":"k"}`, `[]`} {
		if _, err := m.Object("objects", json.RawMessage(bad)); err == nil {
			t.Errorf("Object read %s", bad)
		}
	}
}

func TestWritesSpellBucketAndObjectRequests(t *testing.T) {
	m := s3.New("s3")
	for _, c := range []struct {
		op   fake.Op
		ref  trace.Ref
		body string
		want fake.Write
	}{
		{fake.Create, bucket("logs"), `{}`, fake.Write{Method: "PUT", Path: "/logs"}},
		{fake.Create, trace.Ref{System: "s3", Resource: "objects", Name: "logs/a/k"}, `{"content":"hi"}`,
			fake.Write{Method: "PUT", Path: "/logs/a/k", ContentType: "application/octet-stream", Body: []byte("hi")}},
		{fake.Patch, object("logs", "a b"), `{"content":"yo"}`,
			fake.Write{Method: "PUT", Path: "/logs/a%20b", ContentType: "application/octet-stream", Body: []byte("yo")}},
		{fake.Patch, bucket("logs"), `{"tags":{"z":"1","a":"2","gone":null}}`, fake.Write{Method: "PUT", Path: "/logs?tagging", ContentType: "application/xml",
			Body: []byte(`<Tagging><TagSet><Tag><Key>a</Key><Value>2</Value></Tag><Tag><Key>z</Key><Value>1</Value></Tag></TagSet></Tagging>`)}},
		{fake.Delete, bucket("logs"), ``, fake.Write{Method: "DELETE", Path: "/logs"}},
		{fake.Delete, object("logs", "k"), ``, fake.Write{Method: "DELETE", Path: "/logs/k"}},
	} {
		got, err := m.Write(c.op, c.ref, []byte(c.body))
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Write(%s, %v) = %+v %s, %v; want %+v %s", c.op, c.ref, got, got.Body, err, c.want, c.want.Body)
		}
	}
	if _, err := m.Write(fake.Patch, bucket("logs"), []byte(`{"versioning":"Enabled"}`)); err == nil {
		t.Error("a bucket's patch without tags was spelled")
	}
}

func TestErrorAndAuthorizeSpeakS3(t *testing.T) {
	m := s3.New("s3")
	for code, name := range map[int]string{503: "ServiceUnavailable", 404: "NoSuchKey", 502: "BadGateway"} {
		contentType, body := m.Error(code, "injected")
		if contentType != "application/xml" || !strings.Contains(string(body), "<Code>"+name+"</Code><Message>injected</Message>") {
			t.Errorf("Error(%d) = %s %s", code, contentType, body)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	m.Authorize(r)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=") {
		t.Errorf("Authorize set %v", r.Header)
	}
	r.Header.Set("Authorization", "mine")
	if m.Authorize(r); r.Header.Get("Authorization") != "mine" {
		t.Error("Authorize replaced an Authorization")
	}
}

func TestGoneAcceptsOnlyAMissingBucketOrKey(t *testing.T) {
	m := s3.New("aws")
	for name, c := range map[string]struct {
		status int
		body   string
		gone   bool
	}{
		"NoSuchBucket":     {404, `<Error><Code>NoSuchBucket</Code></Error>`, true},
		"NoSuchKey":        {404, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`, true},
		"a HEAD's 404":     {404, ``, true},
		"another 404":      {404, `<Error><Code>NoSuchTagSet</Code></Error>`, false},
		"BucketNotEmpty":   {409, `<Error><Code>BucketNotEmpty</Code></Error>`, false},
		"OperationAborted": {409, `<Error><Code>OperationAborted</Code></Error>`, false},
	} {
		if got := m.Gone(c.status, []byte(c.body)); got != c.gone {
			t.Errorf("%s: Gone = %v, want %v", name, got, c.gone)
		}
	}
}
