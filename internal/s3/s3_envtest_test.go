//go:build envtest

package s3_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/s3"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

func TestTheMapReadsMotoThroughAFake(t *testing.T) {
	binary, err := filepath.Abs("../../bin/moto/bin/moto_server")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("%v; run make setup", err)
	}
	rec, err := trace.NewRecorder(filepath.Join(t.TempDir(), "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	f := fake.New(fake.Options{
		Name: "s3", Dir: t.TempDir(), API: s3.New("s3"),
		Command: []string{binary, "-H", "127.0.0.1", "-p", "${port}"},
	})
	defer f.Stop(context.Background())
	endpoints, err := f.Start(context.Background(), &system.Env{Record: rec.Record, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) http.Header {
		t.Helper()
		req, _ := http.NewRequest(method, endpoints.Vars["url"]+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=test/20261021/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=0")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, data)
		}
		return resp.Header
	}
	has := func(what string, match func(trace.Event) bool) trace.Event {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			events := rec.Events()
			if i := slices.IndexFunc(events, match); i >= 0 {
				return events[i]
			}
			if time.Now().After(deadline) {
				t.Fatalf("the trace never showed %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	call := func(verb, uid string) func(trace.Event) bool {
		return func(e trace.Event) bool {
			return e.Call != nil && e.Call.Verb == verb && e.Call.ResultUID == uid && e.Call.Status/100 == 2
		}
	}
	version := func(uid, body string, deleted bool) func(trace.Event) bool {
		return func(e trace.Event) bool {
			v := e.Version
			return v != nil && v.UID == uid && v.Deleted == deleted && strings.Contains(string(v.Body), body)
		}
	}

	do("PUT", "/fuzz-bucket", "")
	has("the bucket's create", call("create", "s3/buckets/fuzz-bucket/1"))
	do("PUT", "/fuzz-bucket?tagging", `<Tagging><TagSet><Tag><Key>team</Key><Value>a</Value></Tag></TagSet></Tagging>`)
	has("the tagged bucket", version("s3/buckets/fuzz-bucket/1", `"tags":{"team":"a"}`, false))

	etag := strings.Trim(do("PUT", "/fuzz-bucket/a/k.txt", "hello").Get("ETag"), `"`)
	put := has("the object's put", call("create", "s3/objects/fuzz-bucket/a/k.txt/1"))
	written := has("the object", version("s3/objects/fuzz-bucket/a/k.txt/1", `"etag":"`+etag+`"`, false))
	if put.Call.ResultRV != etag || written.Version.RV != etag {
		t.Errorf("the put's RV is %s and its version's %s, want %s", put.Call.ResultRV, written.Version.RV, etag)
	}
	do("DELETE", "/fuzz-bucket/a/k.txt", "")
	has("the object gone", version("s3/objects/fuzz-bucket/a/k.txt/1", etag, true))
	do("DELETE", "/fuzz-bucket", "")
	has("the bucket gone", version("s3/buckets/fuzz-bucket/1", `"name":"fuzz-bucket"`, true))
	if resources, err := f.Snapshot(context.Background()); err != nil || len(resources) != 0 {
		t.Errorf("Snapshot = %v, %v, want none", resources, err)
	}
}
