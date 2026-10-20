// Package interpose serves one listener per caller in front of a system. A
// listener forwards requests, injects request faults and records each
// request as a Call.
package interpose

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/plan"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// MaxResultBody is the largest response body a listener reads a result from.
const MaxResultBody = 8 << 20

// Options configure a Listener.
type Options struct {
	// System names the system in each recorded Ref.
	System string
	Caller trace.Caller
	// Upstream is where requests go. Transport carries the caller's
	// credentials, so the listener drops any Authorization header.
	Upstream  *url.URL
	Transport http.RoundTripper
	Record    func(trace.Event)
	// OnCall hears each call after it is recorded.
	OnCall func(trace.Call)
}

// Listener serves plain HTTP on 127.0.0.1. Watch responses stream through
// as they arrive.
type Listener struct {
	o       Options
	url     string
	server  *http.Server
	reverse *httputil.ReverseProxy

	streams atomic.Uint64

	mu       sync.Mutex
	closed   bool
	inFlight sync.WaitGroup
	faults   []*activeFault
	kill     func(context.Context) error
}

type activeFault struct {
	fault   plan.Fault
	matched int
	// ended closes when the fault ends.
	ended chan struct{}
}

// Listen starts a listener. The caller must call Close.
func Listen(o Options) (*Listener, error) {
	if o.Transport == nil {
		o.Transport = http.DefaultTransport
	}
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for %s: %w", o.Caller, err)
	}
	l := &Listener{o: o, url: "http://" + socket.Addr().String()}
	l.reverse = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.Header.Del("Authorization")
			r.SetURL(o.Upstream)
		},
		Transport:     o.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() != nil {
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	l.server = &http.Server{Handler: http.HandlerFunc(l.serve)}
	go l.server.Serve(socket)
	return l, nil
}

// URL is the address callers send requests to.
func (l *Listener) URL() string { return l.url }

// Fault starts a request fault. Its Nth counts matching requests from this
// call. Calling end stops it and releases every request it delays.
func (l *Listener) Fault(f plan.Fault) (end func(), err error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	active := &activeFault{fault: f, ended: make(chan struct{})}
	l.mu.Lock()
	l.faults = append(l.faults, active)
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			for i, other := range l.faults {
				if other == active {
					l.faults = append(l.faults[:i:i], l.faults[i+1:]...)
					break
				}
			}
			close(active.ended)
		})
	}, nil
}

// Close stops the listener and every connection through it.
func (l *Listener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	err := l.server.Close()
	l.inFlight.Wait()
	if err != nil {
		return fmt.Errorf("closing the %s listener: %w", l.o.Caller, err)
	}
	return nil
}

func (l *Listener) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.inFlight.Add(1)
	return true
}

// pick counts the request against every active fault and returns the first
// that picks it.
func (l *Listener) pick(r Request) *activeFault {
	l.mu.Lock()
	defer l.mu.Unlock()
	var picked *activeFault
	for _, f := range l.faults {
		if !Matches(f.fault.Match, r) || r.Watch && unknownOutcome(f.fault) {
			continue
		}
		f.matched++
		if picked == nil && (f.fault.Nth == 0 || f.fault.Nth == f.matched) {
			picked = f
		}
	}
	return picked
}

func (l *Listener) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		l.reverse.ServeHTTP(w, r)
		return
	}
	if !l.begin() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer l.inFlight.Done()

	req := Classify(r)
	req.Ref.System = l.o.System
	rec := &recorder{
		ResponseWriter: w,
		listener:       l,
		start:          time.Now(),
		call: trace.Call{
			Caller: l.o.Caller, Method: r.Method, Path: r.URL.Path,
			Verb: req.Verb, APIVersion: req.APIVersion, Ref: req.Ref,
			Subresource: req.Subresource, Watch: req.Watch,
		},
		decode: mutating(req.Verb),
	}
	if req.Watch {
		rec.call.Stream = l.streams.Add(1)
	}
	defer rec.finish()

	f := l.pick(req)
	if f != nil {
		rec.call.Injected = f.fault.Effect()
	}
	switch {
	case f == nil:
	case f.fault.Error != nil:
		writeStatus(rec, *f.fault.Error, req, r.URL.Path)
		return
	case f.fault.Drop:
		drop(w)
		return
	case f.fault.Delay != nil:
		hold(r.Context(), f, time.Duration(*f.fault.Delay))
	case unknownOutcome(f.fault):
		l.answerUnseen(rec, upstream(r, rec.decode), f.fault.KillAfter)
		return
	}
	l.reverse.ServeHTTP(rec, upstream(r, rec.decode))
}

// upstream keeps a write going after its client leaves, since the server
// can commit it and the trace needs the server's answer.
func upstream(r *http.Request, write bool) *http.Request {
	if write && r.Context().Err() == nil {
		return r.WithContext(context.WithoutCancel(r.Context()))
	}
	return r
}

func unknownOutcome(f plan.Fault) bool { return f.DropResponse || f.KillAfter }

// answerUnseen lets the upstream answer in full and records the call. Then,
// for killAfter, it kills the controller. Last it aborts the client's
// connection without a response.
func (l *Listener) answerUnseen(rec *recorder, r *http.Request, kill bool) {
	buffered := *rec
	buffered.ResponseWriter = httptest.NewRecorder()
	rec.recorded = true
	l.reverse.ServeHTTP(&buffered, r)
	buffered.finish()
	l.mu.Lock()
	killFunc := l.kill
	l.mu.Unlock()
	if kill && killFunc != nil {
		killFunc(context.WithoutCancel(r.Context()))
	}
	drop(rec.ResponseWriter)
}

// hold waits for d, the fault's end or the client's departure.
func hold(ctx context.Context, f *activeFault, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-f.ended:
	case <-ctx.Done():
	}
}

// recorder notes what the response carries. Unwrap lets
// http.ResponseController reach the flusher that watches need.
type recorder struct {
	http.ResponseWriter
	listener *Listener
	start    time.Time
	call     trace.Call
	decode   bool
	body     *bytes.Buffer
	recorded bool
}

func (rec *recorder) WriteHeader(status int) {
	rec.call.Status = status
	header := rec.Header()
	if rec.decode && status >= 200 && status < 300 && decodable(header.Get("Content-Type")) {
		rec.body = &bytes.Buffer{}
	}
	rec.ResponseWriter.WriteHeader(status)
	if rec.call.Watch {
		rec.record()
	}
}

func (rec *recorder) Write(p []byte) (int, error) {
	if rec.call.Status == 0 {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.body != nil && rec.body.Len() <= MaxResultBody {
		rec.body.Write(p[:min(len(p), MaxResultBody+1-rec.body.Len())])
	}
	return rec.ResponseWriter.Write(p)
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *recorder) finish() {
	if rec.body != nil && rec.body.Len() <= MaxResultBody {
		rec.readResult(rec.Header().Get("Content-Encoding"))
	}
	rec.record()
	if record := rec.listener.o.Record; rec.call.Watch && record != nil {
		record(trace.Event{WatchEnd: &trace.WatchEnd{Caller: rec.call.Caller, System: rec.call.Ref.System, Stream: rec.call.Stream}})
	}
}

func (rec *recorder) readResult(encoding string) {
	var body io.Reader = rec.body
	switch encoding {
	case "", "identity":
	case "gzip":
		unzipped, err := gzip.NewReader(body)
		if err != nil {
			return
		}
		body = io.LimitReader(unzipped, MaxResultBody)
	default:
		return
	}
	var meta metav1.ObjectMeta
	var err error
	if media(rec.Header().Get("Content-Type")) == protobufType {
		meta, err = protobufMeta(body)
	} else {
		var object struct {
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		err = json.NewDecoder(body).Decode(&object)
		meta = object.Metadata
	}
	if err != nil {
		return
	}
	rec.call.ResultName = meta.Name
	rec.call.ResultUID = string(meta.UID)
	rec.call.ResultRV = meta.ResourceVersion
}

const protobufType = "application/vnd.kubernetes.protobuf"

var protobufMagic = []byte("k8s\x00")

// protobufMeta decodes the metadata of a Kubernetes protobuf object, which
// is field 1 of every object inside the runtime.Unknown envelope.
func protobufMeta(body io.Reader) (metav1.ObjectMeta, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return metav1.ObjectMeta{}, err
	}
	if !bytes.HasPrefix(data, protobufMagic) {
		return metav1.ObjectMeta{}, errors.New("no protobuf magic")
	}
	var envelope runtime.Unknown
	if err := envelope.Unmarshal(data[len(protobufMagic):]); err != nil {
		return metav1.ObjectMeta{}, err
	}
	var meta metav1.ObjectMeta
	raw := envelope.Raw
	tag, n := binary.Uvarint(raw)
	if tag != 1<<3|2 {
		return meta, errors.New("field 1 does not lead the object")
	}
	size, m := binary.Uvarint(raw[n:])
	if m <= 0 || uint64(len(raw[n+m:])) < size {
		return meta, errors.New("truncated metadata")
	}
	return meta, meta.Unmarshal(raw[n+m : n+m+int(size)])
}

func (rec *recorder) record() {
	if rec.recorded {
		return
	}
	rec.recorded = true
	rec.call.Latency = time.Since(rec.start)
	o := rec.listener.o
	if o.Record != nil {
		call := rec.call
		o.Record(trace.Event{Call: &call})
	}
	if o.OnCall != nil {
		o.OnCall(rec.call)
	}
}

func media(contentType string) string {
	media, _, _ := mime.ParseMediaType(contentType)
	return media
}

func decodable(contentType string) bool {
	m := media(contentType)
	return m == "application/json" || m == protobufType
}

// writeStatus answers with a metav1.Status, which a client decodes whatever
// content type it asked for.
func writeStatus(w http.ResponseWriter, e plan.Error, r Request, path string) {
	retryAfter := int(math.Ceil(time.Duration(e.RetryAfter).Seconds()))
	resource := schema.GroupResource{Group: r.Ref.Group, Resource: r.Ref.Resource}
	status := apierrors.NewGenericServerResponse(e.Code, r.Verb, resource, r.Ref.Name, "", retryAfter, false).ErrStatus
	status.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	status.Message = fmt.Sprintf("reconciler-fuzzer fault: %s %s", r.Verb, path)
	body, _ := json.Marshal(status)
	w.Header().Set("Content-Type", "application/json")
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(e.Code)
	w.Write(body)
}

// drop closes the connection without a response.
func drop(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	conn.Close()
}

// SetKill sets what a killAfter fault calls to kill the controller. It
// returns once the controller has exited.
func (l *Listener) SetKill(kill func(context.Context) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kill = kill
}
