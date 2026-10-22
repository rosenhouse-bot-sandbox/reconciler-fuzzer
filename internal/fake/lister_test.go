package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// emulator is a crud map that lists crudfake through its own API, as an
// emulator's map does. Its responses' objects are partial, with an RV no
// listing shows.
type emulator struct {
	*crud.Map
	mu         sync.Mutex
	authorized []string
	ended      []string
	// neverGone makes Gone accept no answer.
	neverGone bool
}

func (e *emulator) Gone(status int, body []byte) bool {
	return !e.neverGone && e.Map.Gone(status, body)
}

func (e *emulator) List(ctx context.Context, do func(*http.Request) (*http.Response, error), base string) (fake.Listing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/buckets", nil)
	if err != nil {
		return fake.Listing{}, err
	}
	resp, err := do(req)
	if err != nil {
		return fake.Listing{}, err
	}
	defer resp.Body.Close()
	var list struct{ Items []json.RawMessage }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fake.Listing{}, err
	}
	listing := fake.Listing{Collections: []string{"buckets"}}
	for _, item := range list.Items {
		listing.Resources = append(listing.Resources, fake.Listed{Collection: "buckets", Body: item})
	}
	return listing, nil
}

func (e *emulator) Objects(req interpose.Request, header http.Header, body []byte) []interpose.Object {
	objects := e.Map.Objects(req, header, body)
	for i := range objects {
		objects[i].Partial, objects[i].RV = true, "partial"
	}
	return objects
}

func (e *emulator) Authorize(r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.authorized = append(e.authorized, r.Method+" "+r.URL.Path)
}

func (e *emulator) Ended(_ trace.Ref, uid string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ended = append(e.ended, uid)
}

func (e *emulator) seen() (authorized, ended []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.authorized), slices.Clone(e.ended)
}

func startEmulator(t *testing.T, o fake.Options) (*started, *emulator) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloud.yaml"), fmt.Appendf(nil, cloudYAML, "1ms", "1ms"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	rec, err := trace.NewRecorder(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	e := &emulator{Map: crud.New("cloud")}
	o.Name, o.Dir, o.API = "cloud", dir, e
	if o.Command == nil {
		o.Command = []string{crudfakeBinary, "--config", "cloud.yaml"}
	}
	f := fake.New(o)
	t.Cleanup(func() { f.Stop(context.Background()) })
	endpoints, err := f.Start(context.Background(), &system.Env{Record: rec.Record, Dir: out})
	if err != nil {
		t.Fatal(err)
	}
	return &started{dir: dir, fake: f, rec: rec, endpoints: endpoints, out: out}, e
}

func TestAListerMapListsTheEmulatorThroughItsOwnAPI(t *testing.T) {
	s, e := startEmulator(t, fake.Options{})
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	if hasVersion("cloud/buckets/buckets-1", "partial", "CREATING", false)(s.rec.Events()) {
		t.Error("a partial object recorded a version")
	}
	resources, err := s.fake.Snapshot(context.Background())
	if err != nil || len(resources) != 1 {
		t.Errorf("Snapshot = %v, %v, want the bucket", resources, err)
	}
	if s.fake.Takes(plan.Fault{System: "cloud", Stuck: true}) {
		t.Error("a fake without ModelFaults takes a model fault")
	}
	if authorized, _ := e.seen(); !slices.Contains(authorized, "GET /buckets") || slices.Contains(authorized, "GET "+fake.ResourcesPath) {
		t.Errorf("authorized %v, want listings of /buckets only", authorized)
	}
}

func TestAnEmulatorHearsEachInstanceTheObserverSawGone(t *testing.T) {
	s, e := startEmulator(t, fake.Options{})
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	s.do(t, "DELETE", "/buckets/buckets-1", "")
	s.eventually(t, "the bucket gone", func([]trace.Event) bool {
		_, ended := e.seen()
		return slices.Equal(ended, []string{"cloud/buckets/buckets-1"})
	})
}

func TestStartCreatesTheFixturesAsTheHarness(t *testing.T) {
	s, e := startEmulator(t, fake.Options{
		Fixtures: []fake.Fixture{{Resource: "buckets", Name: "b", Body: json.RawMessage(`{"name":"b"}`)}},
		Vars:     map[string]string{"token": "t0"},
	})
	created := count(s.rec.Events(), func(e trace.Event) bool {
		return e.Call != nil && e.Call.Caller == trace.Harness && e.Call.Verb == "create" && e.Call.ResultUID == "cloud/buckets/buckets-1"
	})
	if created != 1 || !hasVersion("cloud/buckets/buckets-1", "1", "CREATING", false)(s.rec.Events()) &&
		!hasVersion("cloud/buckets/buckets-1", "2", "READY", false)(s.rec.Events()) {
		t.Fatalf("Start returned before the trace held the harness's fixture:\n%s", versions(s.rec.Events()))
	}
	if s.endpoints.Vars["token"] != "t0" || s.endpoints.Vars["url"] == "" {
		t.Errorf("Vars = %v, want token and url", s.endpoints.Vars)
	}
	if authorized, _ := e.seen(); !slices.Contains(authorized, "POST /buckets") {
		t.Errorf("authorized %v, want the fixture's create", authorized)
	}
}

func TestAnEmulatorsPatchAndDeleteReturnAfterAnObserverPass(t *testing.T) {
	s, e := startEmulator(t, fake.Options{})
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	ref := trace.Ref{System: "cloud", Resource: "buckets", Name: "buckets-1"}
	if err := s.fake.Patch(context.Background(), ref, []byte(`{"size":2}`)); err != nil {
		t.Fatal(err)
	}
	if !hasVersion("cloud/buckets/buckets-1", "3", "READY", false)(s.rec.Events()) {
		t.Fatalf("Patch returned before the trace held version 3:\n%s", versions(s.rec.Events()))
	}
	if err := s.fake.Delete(context.Background(), ref, ""); err != nil {
		t.Fatal(err)
	}
	if !hasVersion("cloud/buckets/buckets-1", "4", "DELETING", false)(s.rec.Events()) &&
		!hasVersion("cloud/buckets/buckets-1", "3", "READY", true)(s.rec.Events()) {
		t.Fatalf("Delete returned before the trace held its result:\n%s", versions(s.rec.Events()))
	}
	authorized, _ := e.seen()
	for _, want := range []string{"PATCH /buckets/buckets-1", "DELETE /buckets/buckets-1"} {
		if !slices.Contains(authorized, want) {
			t.Errorf("authorized %v, want %s", authorized, want)
		}
	}
}

func TestTheCommandExpandsTheAddressAndPort(t *testing.T) {
	s, _ := startEmulator(t, fake.Options{Command: []string{"sh", "-c", `
		case "$1" in *:"$2") ;; *) exit 3 ;; esac
		[ "$1" = "$` + fake.AddrEnv + `" ] || exit 4
		exec "$0" --config cloud.yaml`, crudfakeBinary, "${addr}", "${port}"}})
	if _, err := s.fake.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.endpoints.Vars["url"], "${") {
		t.Errorf("url = %s", s.endpoints.Vars["url"])
	}
	time.Sleep(3 * fake.ObservePeriod)
	if err := s.fake.Err(); err != nil {
		t.Error(err)
	}
}

func TestTheMapSaysWhichAnswersMeanGone(t *testing.T) {
	s, e := startEmulator(t, fake.Options{})
	missing := trace.Ref{System: "cloud", Resource: "buckets", Name: "buckets-9"}
	if err := s.fake.Delete(context.Background(), missing, ""); !errors.Is(err, system.ErrGone) {
		t.Errorf("Delete of a missing bucket = %v, want ErrGone", err)
	}
	e.neverGone = true
	if err := s.fake.Delete(context.Background(), missing, ""); err == nil || errors.Is(err, system.ErrGone) {
		t.Errorf("Delete of a missing bucket = %v, want an error that is not ErrGone", err)
	}
}

// refused is an emulator map whose listings the emulator refuses.
type refused struct{ *crud.Map }

func (refused) List(context.Context, func(*http.Request) (*http.Response, error), string) (fake.Listing, error) {
	return fake.Listing{}, errors.New("GET /v1/sys/mounts: 403 Forbidden permission denied")
}

func TestAStartThatNeverListsNamesTheListingsLastError(t *testing.T) {
	t.Cleanup(fake.SetStartTimeout(100 * time.Millisecond))
	f := fake.New(fake.Options{Name: "vault", Command: []string{"sleep", "30"}, Dir: t.TempDir(), API: refused{crud.New("vault")}})
	t.Cleanup(func() { f.Stop(context.Background()) })
	_, err := f.Start(context.Background(), &system.Env{Record: func(trace.Event) {}, Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") || strings.Contains(err.Error(), fake.ResourcesPath) {
		t.Errorf("Start = %v, want the listing's 403", err)
	}
}
