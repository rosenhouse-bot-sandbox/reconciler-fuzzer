package fake_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/crud"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/fake"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

var crudfakeBinary string

// listingEnv makes the test binary a fake that answers each listing with
// the listing.json in its directory.
const listingEnv = "FAKE_TEST_LISTING"

func serveListing() {
	http.HandleFunc(fake.ResourcesPath, func(w http.ResponseWriter, _ *http.Request) {
		data, _ := os.ReadFile("listing.json")
		w.Write(data)
	})
	fmt.Println(http.ListenAndServe(os.Getenv(fake.AddrEnv), nil))
	os.Exit(1)
}

func TestMain(m *testing.M) {
	if os.Getenv(listingEnv) != "" {
		serveListing()
	}
	dir, err := os.MkdirTemp("", "crudfake")
	if err != nil {
		panic(err)
	}
	crudfakeBinary = filepath.Join(dir, "crudfake")
	build := exec.Command("go", "build", "-o", crudfakeBinary, "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/crudfake")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const cloudYAML = `collections:
  buckets:
    unique: [name]
    pending: CREATING
    ready: READY
    deleting: DELETING
    createDelay: %s
    deleteDelay: %s
`

type started struct {
	dir       string
	fake      *fake.Fake
	rec       *trace.Recorder
	endpoints system.Endpoints
	out       string
}

func start(t *testing.T, delay time.Duration) *started {
	t.Helper()
	return startCommand(t, delay, crudfakeBinary, "--config", "cloud.yaml")
}

func startCommand(t *testing.T, delay time.Duration, command ...string) *started {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloud.yaml"), fmt.Appendf(nil, cloudYAML, delay, delay), 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	rec, err := trace.NewRecorder(filepath.Join(out, "trace.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	f := fake.New(fake.Options{
		Name: "cloud", Command: command, Dir: dir, API: crud.New("cloud"), ModelFaults: true,
	})
	endpoints, err := f.Start(context.Background(), &system.Env{Record: rec.Record, Dir: out})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Stop(context.Background()) })
	return &started{dir: dir, fake: f, rec: rec, endpoints: endpoints, out: out}
}

func (s *started) do(t *testing.T, method, path, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, s.endpoints.Vars["url"]+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, data)
	}
	var object map[string]any
	json.Unmarshal(data, &object)
	return object
}

// eventually waits until found holds for the trace.
func (s *started) eventually(t *testing.T, what string, found func([]trace.Event) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !found(s.rec.Events()) {
		if time.Now().After(deadline) {
			t.Fatalf("the trace never showed %s:\n%s", what, versions(s.rec.Events()))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func versions(events []trace.Event) string {
	var out []string
	for _, e := range events {
		switch {
		case e.Version != nil:
			out = append(out, fmt.Sprintf("version %s %s deleted=%v %s", e.Version.UID, e.Version.RV, e.Version.Deleted, e.Version.Body))
		case e.Work != nil:
			out = append(out, fmt.Sprintf("work %s %v", e.Work.System, e.Work.Pending))
		case e.Call != nil:
			out = append(out, fmt.Sprintf("call %s %s %s %d", e.Call.Caller, e.Call.Verb, e.Call.Path, e.Call.Status))
		}
	}
	return strings.Join(out, "\n")
}

func hasVersion(uid, rv, state string, deleted bool) func([]trace.Event) bool {
	return func(events []trace.Event) bool {
		for _, e := range events {
			if v := e.Version; v != nil && v.UID == uid && v.RV == rv && v.Deleted == deleted && strings.Contains(string(v.Body), `"state":"`+state+`"`) {
				return true
			}
		}
		return false
	}
}

func count(events []trace.Event, keep func(trace.Event) bool) int {
	n := 0
	for _, e := range events {
		if keep(e) {
			n++
		}
	}
	return n
}

func TestStartServesTheControllerListenerAsURL(t *testing.T) {
	s := start(t, 50*time.Millisecond)
	if s.endpoints.Controller == nil || s.endpoints.Harness == nil || s.endpoints.Vars["url"] != s.endpoints.Controller.URL() {
		t.Fatalf("endpoints = %+v", s.endpoints)
	}
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the controller's create", func(events []trace.Event) bool {
		return count(events, func(e trace.Event) bool {
			return e.Call != nil && e.Call.Caller == trace.Controller && e.Call.Verb == "create" && e.Call.ResultUID == "cloud/buckets/buckets-1"
		}) == 1
	})
	if _, err := os.Stat(filepath.Join(s.out, "cloud.log")); err != nil {
		t.Errorf("the fake's log: %v", err)
	}
}

func TestTheObserverRecordsEachNewerVersionOnceAndTheDeletedLastBody(t *testing.T) {
	s := start(t, 50*time.Millisecond)
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	s.do(t, "GET", "/buckets/buckets-1", "")
	s.do(t, "DELETE", "/buckets/buckets-1", "")
	s.eventually(t, "the bucket deleted", hasVersion("cloud/buckets/buckets-1", "3", "DELETING", true))
	events := s.rec.Events()
	for _, rv := range []string{"1", "2", "3"} {
		if n := count(events, func(e trace.Event) bool { return e.Version != nil && e.Version.RV == rv && !e.Version.Deleted }); n != 1 {
			t.Errorf("%d versions at %s, want 1:\n%s", n, rv, versions(events))
		}
	}
	snapshot, err := s.fake.Snapshot(context.Background())
	if err != nil || len(snapshot) != 0 {
		t.Errorf("Snapshot = %v, %v, want none", snapshot, err)
	}
}

func TestTheObserverRecordsWorkWhenPendingChanges(t *testing.T) {
	s := start(t, 100*time.Millisecond)
	time.Sleep(3 * fake.ObservePeriod)
	if n := count(s.rec.Events(), func(e trace.Event) bool { return e.Work != nil }); n != 0 {
		t.Fatalf("%d work events before any work, want 0", n)
	}
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	isWork := func(pending bool) func(trace.Event) bool {
		return func(e trace.Event) bool {
			return e.Work != nil && e.Work.System == "cloud" && e.Work.Pending == pending
		}
	}
	s.eventually(t, "work pending", func(events []trace.Event) bool { return count(events, isWork(true)) == 1 })
	if !s.fake.Pending() {
		t.Error("Pending() = false while the bucket is creating")
	}
	s.eventually(t, "work done", func(events []trace.Event) bool { return count(events, isWork(false)) == 1 })
	time.Sleep(3 * fake.ObservePeriod)
	if n := count(s.rec.Events(), func(e trace.Event) bool { return e.Work != nil }); n != 2 || s.fake.Pending() {
		t.Errorf("%d work events, Pending() = %v, want 2 and false", n, s.fake.Pending())
	}
}

func TestStuckHoldsABucketCreatingUntilItsEnd(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	stuck := plan.Fault{System: "cloud", Stuck: true, Match: plan.Match{Resource: "buckets"}}
	if !s.fake.Takes(stuck) || s.fake.Takes(plan.Fault{System: "other", Stuck: true}) || s.fake.Takes(plan.Fault{System: "cloud", Drop: true}) {
		t.Fatal("Takes accepts the wrong faults")
	}
	end, err := s.fake.Fault(stuck)
	if err != nil {
		t.Fatal(err)
	}
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	time.Sleep(300 * time.Millisecond)
	if hasVersion("cloud/buckets/buckets-1", "2", "READY", false)(s.rec.Events()) {
		t.Fatal("the bucket became ready under stuck")
	}
	end()
	s.eventually(t, "the bucket ready after the end", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
}

func TestSlowDeleteHoldsADeletionUntilItsEnd(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	end, err := s.fake.Fault(plan.Fault{System: "cloud", SlowDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	s.do(t, "DELETE", "/buckets/buckets-1", "")
	time.Sleep(300 * time.Millisecond)
	if hasVersion("cloud/buckets/buckets-1", "3", "DELETING", true)(s.rec.Events()) {
		t.Fatal("the bucket went under slowDelete")
	}
	end()
	s.eventually(t, "the bucket gone after the end", hasVersion("cloud/buckets/buckets-1", "3", "DELETING", true))
}

func TestFaultRefusesARequestFault(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	if _, err := s.fake.Fault(plan.Fault{System: "cloud", Drop: true}); err == nil {
		t.Error("Fault took a request fault")
	}
}

func TestPatchAndDeleteReturnOnceTheTraceHoldsTheirResult(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
	ref := trace.Ref{System: "cloud", Resource: "buckets", Name: "buckets-1"}
	ctx := context.Background()
	if err := s.fake.Patch(ctx, ref, []byte(`{"size":2}`)); err != nil {
		t.Fatal(err)
	}
	if !hasVersion("cloud/buckets/buckets-1", "3", "READY", false)(s.rec.Events()) {
		t.Fatalf("Patch returned before the trace held version 3:\n%s", versions(s.rec.Events()))
	}
	if n := count(s.rec.Events(), func(e trace.Event) bool {
		return e.Call != nil && e.Call.Caller == trace.Harness && e.Call.Method == "PATCH" && e.Call.Path == "/buckets/buckets-1"
	}); n != 1 {
		t.Errorf("%d harness patches, want 1", n)
	}
	if err := s.fake.Delete(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	if !hasVersion("cloud/buckets/buckets-1", "4", "DELETING", false)(s.rec.Events()) &&
		!hasVersion("cloud/buckets/buckets-1", "4", "DELETING", true)(s.rec.Events()) {
		t.Fatalf("Delete returned before the trace held version 4:\n%s", versions(s.rec.Events()))
	}
	if err := s.fake.Patch(ctx, ref, []byte(`{"size":3}`)); !errors.Is(err, system.ErrGone) {
		t.Errorf("Patch of a deleted bucket = %v, want ErrGone", err)
	}
	missing := trace.Ref{System: "cloud", Resource: "buckets", Name: "buckets-9"}
	if err := s.fake.Delete(ctx, missing, ""); !errors.Is(err, system.ErrGone) {
		t.Errorf("Delete of a missing bucket = %v, want ErrGone", err)
	}
	if err := s.fake.Patch(ctx, missing, []byte(`{}`)); !errors.Is(err, system.ErrGone) {
		t.Errorf("Patch of a missing bucket = %v, want ErrGone", err)
	}
}

func TestAnExitTheHarnessDidNotCauseIsAnErrorNamingTheLog(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	if err := s.fake.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after a clean run = %v", err)
	}

	s = startCommand(t, 30*time.Millisecond, "timeout", "0.5", crudfakeBinary, "--config", "cloud.yaml")
	if _, err := s.fake.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(s.out, "cloud.log")
	deadline := time.Now().Add(5 * time.Second)
	for s.fake.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("The fake's exit was never recorded.")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.fake.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), log) {
		t.Errorf("Snapshot = %v, want an error naming %s", err, log)
	}
	if err := s.fake.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), log) {
		t.Errorf("Stop = %v, want an error naming %s", err, log)
	}
}

func TestStopEndsTheProcessGroup(t *testing.T) {
	s := startCommand(t, 30*time.Millisecond, "sh", "-c", crudfakeBinary+" --config cloud.yaml & echo $! > child.pid; wait")
	if err := s.fake.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the fake's child outlived Stop")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartFailsFastWhenTheFakeExits(t *testing.T) {
	out := t.TempDir()
	f := fake.New(fake.Options{Name: "cloud", Command: []string{"false"}, Dir: t.TempDir(), API: crud.New("cloud")})
	defer f.Stop(context.Background())
	began := time.Now()
	_, err := f.Start(context.Background(), &system.Env{Dir: out})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(out, "cloud.log")) || time.Since(began) > 2*time.Second {
		t.Errorf("Start = %v after %v, want a quick error naming the log", err, time.Since(began))
	}
}

func TestRequestFaultsWorkOnTheControllerListener(t *testing.T) {
	create := plan.Match{Verb: "create", Resource: "buckets"}
	for _, tc := range []struct {
		fault           plan.Fault
		status          int // 0 is a transport error.
		created, killed bool
	}{
		{fault: plan.Fault{Match: create, Error: &plan.Error{Code: 503}}, status: 503},
		{fault: plan.Fault{Match: create, Delay: new(plan.Duration(200 * time.Millisecond))}, status: 201, created: true},
		{fault: plan.Fault{Match: create, Drop: true}},
		{fault: plan.Fault{Match: create, DropResponse: true}, created: true},
		{fault: plan.Fault{Match: create, KillAfter: true}, created: true, killed: true},
	} {
		t.Run(tc.fault.Effect(), func(t *testing.T) {
			s := start(t, 30*time.Millisecond)
			var killed atomic.Bool
			s.endpoints.Controller.SetKill(func(context.Context) error { killed.Store(true); return nil })
			end, err := s.endpoints.Controller.Fault(tc.fault)
			if err != nil {
				t.Fatal(err)
			}
			defer end()
			began := time.Now()
			status := 0
			if resp, err := http.Post(s.endpoints.Vars["url"]+"/buckets", "application/json", strings.NewReader(`{"name":"b"}`)); err == nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			if status != tc.status || killed.Load() != tc.killed {
				t.Errorf("status %d, killed %v; want %d, %v", status, killed.Load(), tc.status, tc.killed)
			}
			if tc.fault.Delay != nil && time.Since(began) < 200*time.Millisecond {
				t.Errorf("the create took %v", time.Since(began))
			}
			resources, err := s.fake.Snapshot(context.Background())
			if err != nil || (len(resources) == 1) != tc.created {
				t.Errorf("Snapshot = %v, %v; want created %v", resources, err, tc.created)
			}
		})
	}
}

func TestStopAfterAFailedStartReturns(t *testing.T) {
	f := fake.New(fake.Options{Name: "cloud", Command: []string{"/nonexistent"}, Dir: t.TempDir(), API: crud.New("cloud")})
	if _, err := f.Start(context.Background(), &system.Env{Dir: t.TempDir()}); err == nil {
		t.Fatal("Start of a missing command succeeded")
	}
	f.Stop(context.Background())
}

func TestTheControllerListenerRefusesTheHarnessPaths(t *testing.T) {
	s := start(t, 30*time.Millisecond)
	for _, path := range []string{fake.ResourcesPath, fake.FaultsPath} {
		resp, err := http.Post(s.endpoints.Vars["url"]+path, "application/json", strings.NewReader(`{"effect":"stuck"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, resp.StatusCode)
		}
	}
	s.do(t, "POST", "/buckets", `{"name":"b"}`)
	s.eventually(t, "the bucket ready", hasVersion("cloud/buckets/buckets-1", "2", "READY", false))
}

// listing starts the test binary as a fake that lists what list writes.
func listing(t *testing.T) (f *fake.Fake, list func(string)) {
	t.Helper()
	dir := t.TempDir()
	list = func(listing string) {
		t.Helper()
		next := filepath.Join(dir, "next.json")
		if err := os.WriteFile(next, []byte(listing), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(next, filepath.Join(dir, "listing.json")); err != nil {
			t.Fatal(err)
		}
	}
	list(`{"resources":[]}`)
	t.Setenv(listingEnv, "1")
	f = fake.New(fake.Options{Name: "cloud", Command: []string{os.Args[0]}, Dir: dir, API: crud.New("cloud")})
	t.Cleanup(func() { f.Stop(context.Background()) })
	if _, err := f.Start(context.Background(), &system.Env{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return f, list
}

// failsWith waits until f fails with an error containing want.
func failsWith(t *testing.T, f *fake.Fake, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.Err(); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Err() = %v, want one containing %q", err, want)
	}
	if err := f.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("Stop = %v, want one containing %q", err, want)
	}
}

func TestAnUnreadableListingFailsTheFake(t *testing.T) {
	for name, item := range map[string]string{
		"numeric id": `{"id":7,"version":1}`,
		"no version": `{"id":"b-1"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f, list := listing(t)
			list(`{"resources":[{"collection":"buckets","body":` + item + `}]}`)
			failsWith(t, f, "unreadable buckets")
		})
	}
}

func TestAnIDListedAgainAfterItWasGoneFailsTheFake(t *testing.T) {
	f, list := listing(t)
	bucket := `{"resources":[{"collection":"buckets","body":{"id":"b-1","version":1}}]}`
	list(bucket)
	time.Sleep(5 * fake.ObservePeriod)
	list(`{"resources":[]}`)
	time.Sleep(5 * fake.ObservePeriod)
	if err := f.Err(); err != nil {
		t.Fatalf("Err() = %v before the id came back", err)
	}
	list(bucket)
	failsWith(t, f, "reused id buckets/b-1")
}
