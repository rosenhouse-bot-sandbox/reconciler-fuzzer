package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crudfake"
)

// fakeCloud serves crudfake's buckets, with request faults for tests.
type fakeCloud struct {
	*httptest.Server
	fake *crudfake.Server

	mu sync.Mutex
	// dropResponses aborts that many POSTs after the fake commits them.
	dropResponses int
	// unavailable answers every request 503 while set.
	unavailable bool
	posts       int
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	cfg := &crudfake.Config{Collections: map[string]crudfake.Collection{"buckets": {
		Unique:  []string{"name"},
		Pending: Creating, Ready: Ready, Deleting: Deleting,
		CreateDelay: crudfake.Duration{Duration: 20 * time.Millisecond},
		DeleteDelay: crudfake.Duration{Duration: 20 * time.Millisecond},
	}}}
	c := &fakeCloud{fake: crudfake.New(cfg)}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(func() { c.Close(); c.fake.Close() })
	return c
}

func (c *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	unavailable, drop := c.unavailable, false
	if r.Method == http.MethodPost {
		c.posts++
		if c.dropResponses > 0 {
			c.dropResponses--
			drop = true
		}
	}
	c.mu.Unlock()
	if unavailable {
		http.Error(w, `{"error": {"code": 503}}`, http.StatusServiceUnavailable)
		return
	}
	if !drop {
		c.fake.ServeHTTP(w, r)
		return
	}
	c.fake.ServeHTTP(httptest.NewRecorder(), r)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func (c *fakeCloud) set(f func(*fakeCloud)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

func (c *fakeCloud) postCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts
}

func (c *fakeCloud) client() *Cloud { return &Cloud{Endpoint: c.URL} }

func TestCloudCreatesGetsFindsPatchesAndDeletes(t *testing.T) {
	cloud := newFakeCloud(t).client()
	ctx := t.Context()
	created, err := cloud.Create(ctx, CloudBucket{Name: "b-1", Region: "eu", Labels: map[string]string{"k8s-uid": "1"}})
	if err != nil || created.ID == "" || created.State != Creating || created.Labels["k8s-uid"] != "1" {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	got, err := cloud.Get(ctx, created.ID)
	if err != nil || got.Name != "b-1" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	found, err := cloud.FindByName(ctx, "b-1")
	if err != nil || len(found) != 1 || found[0].ID != created.ID {
		t.Fatalf("FindByName = %+v, %v", found, err)
	}
	if found, _ := cloud.FindByName(ctx, "b-2"); len(found) != 0 {
		t.Fatalf("FindByName(b-2) = %+v", found)
	}
	patched, err := cloud.Patch(ctx, created.ID, "us")
	if err != nil || patched.Region != "us" {
		t.Fatalf("Patch = %+v, %v", patched, err)
	}
	deleted, err := cloud.Delete(ctx, created.ID)
	if err != nil || deleted.State != Deleting {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
}

func TestCloudClassifiesErrors(t *testing.T) {
	fc := newFakeCloud(t)
	cloud := fc.client()
	ctx := t.Context()
	if _, err := cloud.Get(ctx, "buckets-9"); !IsNotFound(err) {
		t.Errorf("Get of a missing bucket = %v, want not found", err)
	}
	if _, err := cloud.Create(ctx, CloudBucket{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cloud.Create(ctx, CloudBucket{Name: "b"}); !IsConflict(err) {
		t.Errorf("a second create = %v, want a conflict", err)
	}
	fc.set(func(c *fakeCloud) { c.dropResponses = 1 })
	if _, err := cloud.Create(ctx, CloudBucket{Name: "c"}); !IsUnknownOutcome(err) {
		t.Errorf("a dropped response = %v, want an unknown outcome", err)
	}
	fc.set(func(c *fakeCloud) { c.unavailable = true })
	var status *StatusError
	if _, err := cloud.Create(ctx, CloudBucket{Name: "d"}); !errors.As(err, &status) || status.Code != 503 || IsUnknownOutcome(err) {
		t.Errorf("a 503 = %v, want a StatusError 503", err)
	}
}

// list lists every bucket, deleting ones included.
func (c *Cloud) list(ctx context.Context) ([]CloudBucket, error) {
	var list struct{ Items []CloudBucket }
	err := c.do(ctx, http.MethodGet, "/buckets", nil, &list)
	return list.Items, err
}
