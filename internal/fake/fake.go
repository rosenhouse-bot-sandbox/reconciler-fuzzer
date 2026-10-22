// Package fake runs a fake service as a System: its process, a controller
// and a harness listener, an observer, model faults and damage. DESIGN.md
// §7 defines the contract a fake serves.
package fake

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/interpose"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/system"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// The contract between the harness and a fake.
const (
	// AddrEnv names the address the fake must serve on.
	AddrEnv = "RECONCILER_FUZZER_ADDR"
	// ResourcesPath answers a Listing.
	ResourcesPath = "/_fake/resources"
	// FaultsPath takes a FaultRequest, and FaultsPath/<id> ends it.
	FaultsPath = "/_fake/faults"
	// ObservePeriod is how often the observer lists the fake.
	ObservePeriod = 20 * time.Millisecond
)

// Listing is the answer to GET ResourcesPath.
type Listing struct {
	// Collections names every collection the fake serves.
	Collections []string `json:"collections"`
	// Pending says a state change is scheduled.
	Pending   bool     `json:"pending"`
	Resources []Listed `json:"resources"`
}

// Listed is one resource of a Listing.
type Listed struct {
	Collection string          `json:"collection"`
	Body       json.RawMessage `json:"body"`
}

// FaultRequest starts a model fault. Empty Collection or ID matches all.
type FaultRequest struct {
	Effect     string `json:"effect"`
	Collection string `json:"collection,omitempty"`
	ID         string `json:"id,omitempty"`
}

// FaultStarted answers a FaultRequest.
type FaultStarted struct {
	ID string `json:"id"`
}

// The effects of a FaultRequest.
const (
	Stuck      = "stuck"
	SlowDelete = "slowDelete"
)

// API is an API map that can also read listed resources and spell the
// harness's writes.
type API interface {
	interpose.API
	// Object reads one listed resource.
	Object(collection string, body json.RawMessage) (interpose.Object, error)
	// Write spells a harness write of ref. A create's body is a Fixture's
	// body. What a patch's body means is the map's: crud's is a merge patch
	// of the listed body, and vault's of the secret's data.
	Write(op Op, ref trace.Ref, body []byte) (Write, error)
	// Gone reports whether an answer to a harness write says its object
	// is gone.
	Gone(status int, body []byte) bool
	// SetCollections names the collections the fake serves.
	SetCollections(names []string)
}

// Op names a harness write.
type Op string

const (
	Create Op = "create"
	Patch  Op = "patch"
	Delete Op = "delete"
)

// Write is one HTTP request of a harness write. Path may hold a query.
type Write struct {
	Method, Path, ContentType string
	Body                      []byte
}

// Lister is an API map of a real emulator, which serves no /_fake/ paths.
// The fake lists it through its own API instead, and sends it no model
// faults.
type Lister interface {
	// List lists every resource, sending each request with do to base,
	// the emulator's own address.
	List(ctx context.Context, do func(*http.Request) (*http.Response, error), base string) (Listing, error)
}

// Ender is an API map that numbers instances, and hears when the observer
// records instance uid of ref gone, so that a later resource of the same
// name is a new instance.
type Ender interface {
	Ended(ref trace.Ref, uid string)
}

// Authorizer is an API map that adds the harness's credentials to each
// request the harness sends.
type Authorizer interface {
	Authorize(r *http.Request)
}

// Fixture is a resource the harness creates once the fake answers.
type Fixture struct {
	Resource, Name string
	Body           json.RawMessage
}

// Options configure a fake.
type Options struct {
	Name string
	// Command runs the fake. Dir is its working directory.
	Command []string
	Dir     string
	API     API
	// SeenAll records Seen events on the controller listener for every
	// object, for a dependency fake.
	SeenAll bool
	// ModelFaults says the fake serves FaultsPath.
	ModelFaults bool
	// Fixtures are created through the harness listener before Start
	// returns.
	Fixtures []Fixture
	// Vars join Endpoints.Vars, such as a token.
	Vars map[string]string
}

// startTimeout bounds the wait for a new fake's listing, and awaitTimeout
// the wait for the observer to record a harness action's result.
var startTimeout = 10 * time.Second

const awaitTimeout = 10 * time.Second

// Fake is one fake service.
type Fake struct {
	o      Options
	record func(trace.Event)
	base   string
	log    string
	client *http.Client

	cmd     *exec.Cmd
	logFile *os.File
	exited  chan struct{}
	// exitErr is set by an exit that Stop's SIGKILL did not cause.
	exitErr error
	// failed is set by the first listing the observer could not read.
	failed error

	harness   *interpose.Listener
	listeners []*interpose.Listener
	cancel    context.CancelFunc
	observing chan struct{}

	mu       sync.Mutex
	stopping bool
	latest   map[string]observed
	gone     map[string]bool
	listed   bool
	pending  bool
	// passed is when the last finished observer pass started.
	passed time.Time

	stopOnce sync.Once
	stopErr  error
}

// observed is the last version the observer recorded of one instance.
type observed struct {
	version trace.Version
	// at is when it was recorded. A listing that started earlier cannot
	// show the instance gone.
	at time.Time
}

var (
	_ system.System  = (*Fake)(nil)
	_ system.Stopper = (*Fake)(nil)
)

// ErrNotStarted answers a call before Start.
var ErrNotStarted = errors.New("the fake service has not started")

// New returns a fake that Start runs.
func New(o Options) *Fake {
	return &Fake{o: o, latest: map[string]observed{}, gone: map[string]bool{}, client: &http.Client{Timeout: awaitTimeout}}
}

func (f *Fake) Name() string { return f.o.Name }

// Start runs the process, waits until its listing answers, opens both
// listeners and the observer, and creates the fixtures. Endpoints.Vars
// holds url and Options.Vars. Call Stop even when Start fails.
func (f *Fake) Start(ctx context.Context, env *system.Env) (system.Endpoints, error) {
	f.record = env.Record
	if f.record == nil {
		f.record = func(trace.Event) {}
	}
	if err := f.run(env.Dir); err != nil {
		return system.Endpoints{}, err
	}
	if err := f.waitListing(ctx); err != nil {
		return system.Endpoints{}, err
	}
	endpoints, err := f.listen()
	if err != nil {
		return system.Endpoints{}, err
	}
	observing, cancel := context.WithCancel(context.Background())
	f.cancel, f.observing = cancel, make(chan struct{})
	go f.observe(observing)
	if err := f.createFixtures(ctx); err != nil {
		return system.Endpoints{}, err
	}
	return endpoints, nil
}

func (f *Fake) createFixtures(ctx context.Context) error {
	if len(f.o.Fixtures) == 0 {
		return nil
	}
	answered := time.Now()
	for _, fixture := range f.o.Fixtures {
		ref := trace.Ref{System: f.o.Name, Resource: fixture.Resource, Name: fixture.Name}
		write, err := f.o.API.Write(Create, ref, fixture.Body)
		if err == nil {
			err = f.call(write.Method, f.harness.URL()+write.Path, write.ContentType, write.Body, 0, nil)
		}
		if err != nil {
			return fmt.Errorf("creating fixture %s %s of fake %s: %w", fixture.Resource, fixture.Name, f.o.Name, err)
		}
		answered = time.Now()
	}
	return f.awaitPass(ctx, answered)
}

func (f *Fake) run(dir string) error {
	if len(f.o.Command) == 0 {
		return fmt.Errorf("fake %s has no command", f.o.Name)
	}
	addr, err := freeAddr()
	if err != nil {
		return err
	}
	f.base = "http://" + addr
	f.log = filepath.Join(dir, f.o.Name+".log")
	if f.logFile, err = os.Create(f.log); err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(addr)
	words := make([]string, len(f.o.Command))
	for i, word := range f.o.Command {
		words[i] = strings.NewReplacer("${addr}", addr, "${port}", port).Replace(word)
	}
	cmd := exec.Command(words[0], words[1:]...)
	cmd.Dir = f.o.Dir
	cmd.Env = append(os.Environ(), AddrEnv+"="+addr)
	cmd.Stdout, cmd.Stderr = f.logFile, f.logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		f.logFile.Close()
		return fmt.Errorf("starting fake %s: %w", f.o.Name, err)
	}
	f.cmd, f.exited = cmd, make(chan struct{})
	go func() {
		err := f.cmd.Wait()
		status, _ := f.cmd.ProcessState.Sys().(syscall.WaitStatus)
		killed := status.Signaled() && status.Signal() == syscall.SIGKILL
		f.mu.Lock()
		if !f.stopping || !killed {
			f.exitErr = fmt.Errorf("fake %s exited (%v); its log is %s", f.o.Name, err, f.log)
		}
		f.mu.Unlock()
		close(f.exited)
	}()
	return nil
}

func freeAddr() (string, error) {
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer socket.Close()
	return socket.Addr().String(), nil
}

func (f *Fake) waitListing(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	for {
		_, err := f.list(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-f.exited:
			return f.Err()
		case <-ctx.Done():
			return fmt.Errorf("fake %s never gave a listing in %v; its log is %s: %w", f.o.Name, startTimeout, f.log, err)
		case <-time.After(ObservePeriod):
		}
	}
}

// Err is the error of an exit the harness did not cause, or else of a
// listing the observer could not read.
func (f *Fake) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cmp.Or(f.exitErr, f.failed)
}

// fail records err unless the fake already failed. The caller holds f.mu.
func (f *Fake) fail(err error) {
	if f.failed == nil {
		f.failed = fmt.Errorf("%w; the log of fake %s is %s", err, f.o.Name, f.log)
	}
}

func (f *Fake) listen() (system.Endpoints, error) {
	upstream, err := url.Parse(f.base)
	if err != nil {
		return system.Endpoints{}, err
	}
	open := func(caller trace.Caller, seenAll bool, refuse func(*http.Request) bool) (*interpose.Listener, error) {
		l, err := interpose.Listen(interpose.Options{
			System: f.o.Name, Caller: caller, Upstream: upstream, Record: f.record, ForwardAuthorization: true,
			API: f.o.API, SeenAll: seenAll, Shown: f.shown, Refuse: refuse,
		})
		if err == nil {
			f.listeners = append(f.listeners, l)
		}
		return l, err
	}
	controller, err := open(trace.Controller, f.o.SeenAll, harnessOnly)
	if err != nil {
		return system.Endpoints{}, err
	}
	if f.harness, err = open(trace.Harness, false, nil); err != nil {
		return system.Endpoints{}, err
	}
	vars := maps.Clone(f.o.Vars)
	if vars == nil {
		vars = map[string]string{}
	}
	vars["url"] = controller.URL()
	return system.Endpoints{Controller: controller, Harness: f.harness, Vars: vars}, nil
}

// harnessOnly matches the paths only the harness may use.
func harnessOnly(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/_fake/") }

func (f *Fake) shown(o interpose.Object) {
	if o.Partial {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.see(trace.Version{Ref: o.Ref, UID: o.UID, RV: o.RV, Body: o.Body})
}

// see records v when it is newer than the last recorded version of its
// instance. The caller holds f.mu.
func (f *Fake) see(v trace.Version) {
	if f.gone[v.UID] {
		return
	}
	if last, found := f.latest[v.UID]; found && !newer(v.RV, last.version.RV) {
		return
	}
	f.latest[v.UID] = observed{version: v, at: time.Now()}
	f.record(trace.Event{Version: &v})
}

// newer compares resource versions as numbers, or else says whether they
// differ.
func newer(rv, than string) bool {
	a, errA := strconv.ParseInt(rv, 10, 64)
	b, errB := strconv.ParseInt(than, 10, 64)
	if errA != nil || errB != nil {
		return rv != than
	}
	return a > b
}

func (f *Fake) observe(ctx context.Context) {
	defer close(f.observing)
	ticker := time.NewTicker(ObservePeriod)
	defer ticker.Stop()
	for {
		f.observeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (f *Fake) observeOnce(ctx context.Context) {
	started := time.Now()
	listing, err := f.list(ctx)
	if ctx.Err() != nil {
		return
	}
	var resources []listedResource
	if err == nil {
		resources, err = f.read(listing)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.fail(fmt.Errorf("listing fake %s: %w", f.o.Name, err))
		return
	}
	present := map[string]bool{}
	for _, r := range resources {
		present[r.UID] = true
		if f.gone[r.UID] {
			f.fail(fmt.Errorf("fake %s reused id %s/%s after it was gone", f.o.Name, r.Ref.Resource, r.Ref.Name))
			continue
		}
		f.see(trace.Version{Ref: r.Ref, UID: r.UID, RV: string(r.RV), Body: r.Body})
	}
	for uid, last := range f.latest {
		if present[uid] || !last.at.Before(started) {
			continue
		}
		deleted := last.version
		deleted.Deleted = true
		f.record(trace.Event{Version: &deleted})
		f.gone[uid] = true
		delete(f.latest, uid)
		if ender, ok := f.o.API.(Ender); ok {
			ender.Ended(deleted.Ref, uid)
		}
	}
	if listing.Pending != f.pending || (!f.listed && listing.Pending) {
		f.record(trace.Event{Work: &trace.Work{System: f.o.Name, Pending: listing.Pending}})
	}
	f.listed, f.pending, f.passed = true, listing.Pending, started
}

// listedResource is a listed resource with its resource version.
type listedResource struct {
	system.Resource
	RV string
}

func (f *Fake) read(listing Listing) ([]listedResource, error) {
	var resources []listedResource
	for _, l := range listing.Resources {
		o, err := f.o.API.Object(l.Collection, l.Body)
		if err != nil {
			return nil, fmt.Errorf("fake %s listed an unreadable %s: %w", f.o.Name, l.Collection, err)
		}
		resources = append(resources, listedResource{system.Resource{Ref: o.Ref, UID: o.UID, Body: o.Body}, o.RV})
	}
	return resources, nil
}

// list lists the fake, and names its collections to the map.
func (f *Fake) list(ctx context.Context) (Listing, error) {
	listing, err := f.listOnce(ctx)
	if err == nil {
		f.o.API.SetCollections(listing.Collections)
	}
	return listing, err
}

func (f *Fake) listOnce(ctx context.Context) (Listing, error) {
	if lister, ok := f.o.API.(Lister); ok {
		return lister.List(ctx, f.do, f.base)
	}
	var listing Listing
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+ResourcesPath, nil)
	if err != nil {
		return listing, err
	}
	resp, err := f.do(req)
	if err != nil {
		return listing, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return listing, fmt.Errorf("GET %s: %s", ResourcesPath, resp.Status)
	}
	return listing, json.NewDecoder(resp.Body).Decode(&listing)
}

// Snapshot lists the fake directly.
func (f *Fake) Snapshot(ctx context.Context) ([]system.Resource, error) {
	if f.exited == nil {
		return nil, ErrNotStarted
	}
	if err := f.Err(); err != nil {
		return nil, err
	}
	listing, err := f.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing fake %s, whose log is %s: %w", f.o.Name, f.log, err)
	}
	listed, err := f.read(listing)
	if err != nil {
		return nil, err
	}
	resources := []system.Resource{}
	for _, l := range listed {
		resources = append(resources, l.Resource)
	}
	return resources, nil
}

// Pending is the last listing's pending.
func (f *Fake) Pending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

// Takes reports whether f is a model fault on this fake, which serves
// FaultsPath.
func (f *Fake) Takes(fault plan.Fault) bool {
	return f.o.ModelFaults && fault.ModelFault() && fault.System == f.o.Name
}

// do sends a harness request with the map's credentials.
func (f *Fake) do(req *http.Request) (*http.Response, error) {
	if authorizer, ok := f.o.API.(Authorizer); ok {
		authorizer.Authorize(req)
	}
	return f.client.Do(req)
}

// Fault starts a model fault through FaultsPath.
func (f *Fake) Fault(fault plan.Fault) (end func(), err error) {
	if !f.Takes(fault) {
		return nil, fmt.Errorf("fake %s takes no fault %s", f.o.Name, fault)
	}
	effect := Stuck
	if fault.SlowDelete {
		effect = SlowDelete
	}
	body, err := json.Marshal(FaultRequest{Effect: effect, Collection: fault.Match.Resource, ID: fault.Match.Name})
	if err != nil {
		return nil, err
	}
	var started FaultStarted
	if err := f.call(http.MethodPost, f.base+FaultsPath, "application/json", body, http.StatusCreated, &started); err != nil {
		return nil, err
	}
	return func() {
		_ = f.call(http.MethodDelete, f.base+FaultsPath+"/"+started.ID, "", nil, http.StatusNoContent, nil)
	}, nil
}

// call sends a request to the fake and decodes its answer into out.
func (f *Fake) call(method, url, contentType string, body []byte, want int, out any) error {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := f.do(req)
	if err != nil {
		return fmt.Errorf("%s %s on fake %s, whose log is %s: %w", method, url, f.o.Name, f.log, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 && f.o.API.Gone(resp.StatusCode, data) {
		return fmt.Errorf("%s %s: %s: %w", method, url, data, system.ErrGone)
	}
	if resp.StatusCode != want && (want != 0 || resp.StatusCode/100 != 2) {
		return fmt.Errorf("%s %s: %s %s", method, url, resp.Status, data)
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// Patch sends the map's patch of ref through the harness listener, and
// returns once an observer pass that started after the answer has ended.
// An answer the map's Gone accepts returns system.ErrGone.
func (f *Fake) Patch(ctx context.Context, ref trace.Ref, patch []byte) error {
	return f.change(ctx, Patch, ref, patch)
}

// Delete is Patch for the map's delete of ref.
func (f *Fake) Delete(ctx context.Context, ref trace.Ref, _ plan.Propagation) error {
	return f.change(ctx, Delete, ref, nil)
}

func (f *Fake) change(ctx context.Context, op Op, ref trace.Ref, body []byte) error {
	if f.harness == nil {
		return ErrNotStarted
	}
	write, err := f.o.API.Write(op, ref, body)
	if err != nil {
		return err
	}
	if err := f.call(write.Method, f.harness.URL()+write.Path, write.ContentType, write.Body, 0, nil); err != nil {
		return err
	}
	return f.awaitPass(ctx, time.Now())
}

// awaitPass returns once an observer pass that started after answered has
// ended, or the fake has failed.
func (f *Fake) awaitPass(ctx context.Context, answered time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, awaitTimeout)
	defer cancel()
	for {
		f.mu.Lock()
		done := f.passed.After(answered)
		f.mu.Unlock()
		if done {
			return nil
		}
		if err := f.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no observer pass of %s ended: %w", f.o.Name, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Stop ends the process, the observer and the listeners. It returns the
// error of an exit the harness did not cause.
func (f *Fake) Stop(ctx context.Context) error {
	f.stopOnce.Do(func() {
		f.mu.Lock()
		f.stopping = true
		f.mu.Unlock()
		var errs []error
		if f.cancel != nil {
			f.cancel()
			<-f.observing
		}
		for _, l := range f.listeners {
			errs = append(errs, l.Close())
		}
		if f.cmd != nil {
			_ = syscall.Kill(-f.cmd.Process.Pid, syscall.SIGKILL)
			<-f.exited
			errs = append(errs, f.Err(), f.logFile.Close())
		}
		f.stopErr = errors.Join(errs...)
	})
	return f.stopErr
}
