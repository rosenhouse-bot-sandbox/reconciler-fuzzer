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
	// Seen lists the kinds whose objects the listener records as Seen
	// events when a get, a list, a watch event or a write's result shows
	// them to Caller.
	Seen []schema.GroupResource
	// API maps the system's requests. Nil is Kubernetes.
	API API
	// SeenAll records Seen events for every object a response shows.
	SeenAll bool
	// Shown hears every object a 2xx response carries, unknown outcomes
	// included. Only an API map calls it.
	Shown func(Object)
	// Refuse matches requests the listener answers 404 without forwarding.
	// It needs an API.
	Refuse func(*http.Request) bool
}

// Listener serves plain HTTP on 127.0.0.1. Watch responses stream through
// as they arrive.
type Listener struct {
	o       Options
	seen    map[schema.GroupResource]bool
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
	l := &Listener{o: o, url: "http://" + socket.Addr().String(), seen: map[schema.GroupResource]bool{}}
	for _, gr := range o.Seen {
		l.seen[gr] = true
	}
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

	req := l.classify(r)
	rec := &recorder{
		ResponseWriter: w,
		listener:       l,
		start:          time.Now(),
		call: trace.Call{
			Caller: l.o.Caller, Method: r.Method, Path: r.URL.Path,
			Verb: req.Verb, APIVersion: req.APIVersion, Ref: req.Ref,
			Subresource: req.Subresource, Watch: req.Watch,
			RequestName: req.RequestName, Unmapped: req.Unmapped,
		},
		req:    req,
		decode: mutating(req.Verb),
		seen:   showing(req.Verb) && (l.o.API != nil || l.sees(req.Ref)),
	}
	if req.Watch {
		rec.call.Stream = l.streams.Add(1)
	}
	defer rec.finish()
	if l.o.Refuse != nil && l.o.Refuse(r) {
		contentType, body := l.o.API.Error(http.StatusNotFound, "reconciler-fuzzer refuses "+r.URL.Path+" on this listener")
		rec.Header().Set("Content-Type", contentType)
		rec.WriteHeader(http.StatusNotFound)
		rec.Write(body)
		return
	}

	f := l.pick(req)
	if f != nil {
		rec.call.Injected = f.fault.Effect()
	}
	switch {
	case f == nil:
	case f.fault.Error != nil:
		l.writeError(rec, *f.fault.Error, req, r.URL.Path)
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

func (l *Listener) classify(r *http.Request) Request {
	if l.o.API != nil {
		req := l.o.API.Classify(r)
		req.Ref.System = l.o.System
		return req
	}
	req := Classify(r)
	req.Ref.System = l.o.System
	if req.Verb == "create" {
		req.RequestName = requestedName(r)
	}
	return req
}

// sees reports whether the listener records Seen events of ref's kind.
func (l *Listener) sees(ref trace.Ref) bool {
	return ref.Resource != "" && (l.o.SeenAll || l.seen[schema.GroupResource{Group: ref.Group, Resource: ref.Resource}])
}

// upstream keeps a write going after its client leaves, since the server
// can commit it and the trace needs the server's answer.
func upstream(r *http.Request, write bool) *http.Request {
	if write && r.Context().Err() == nil {
		return r.WithContext(context.WithoutCancel(r.Context()))
	}
	return r
}

// requestedName reads a JSON create's name, or else its generateName, from
// its first MaxResultBody bytes, and leaves r's body as it was.
func requestedName(r *http.Request) string {
	if r.Body == nil || media(r.Header.Get("Content-Type")) != "application/json" {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxResultBody+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	var object objectMeta
	if err != nil || json.Unmarshal(body, &object) != nil {
		return ""
	}
	if object.Metadata.Name != "" {
		return object.Metadata.Name
	}
	return object.Metadata.GenerateName
}

// showing reports whether a response to verb shows objects.
func showing(verb string) bool {
	return reading(verb) || verb == "create" || verb == "update" || verb == "patch"
}

func unknownOutcome(f plan.Fault) bool { return f.DropResponse || f.KillAfter }

// answerUnseen lets the upstream answer in full and records the call. Then,
// for killAfter, it kills the controller. Last it aborts the client's
// connection without a response.
func (l *Listener) answerUnseen(rec *recorder, r *http.Request, kill bool) {
	buffered := *rec
	buffered.ResponseWriter = httptest.NewRecorder()
	// The caller sees no object.
	buffered.seen = false
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
	req      Request
	decode   bool
	body     *bytes.Buffer
	recorded bool

	// seen says the response can show objects of a Seen kind, or under
	// an API map that it can show objects. A watch hands each write to
	// frames and waits until it is decoded. Any other response holds its
	// body in seenBody until its Seen events are recorded. An API map's
	// mapped 2xx response always holds its body there.
	seen     bool
	seenBody *bytes.Buffer
	frames   *feed
	decoded  chan struct{}
}

func (rec *recorder) WriteHeader(status int) {
	rec.call.Status = status
	header := rec.Header()
	if rec.listener.o.API != nil {
		if status >= 200 && status < 300 && !rec.call.Unmapped {
			rec.seenBody = &bytes.Buffer{}
		}
		rec.ResponseWriter.WriteHeader(status)
		return
	}
	if rec.decode && status >= 200 && status < 300 && decodable(header.Get("Content-Type")) {
		rec.body = &bytes.Buffer{}
	}
	switch {
	case !rec.seen || status < 200 || status > 299:
	case media(header.Get("Content-Type")) != "application/json":
		rec.recordOpaque()
	case rec.call.Watch:
		rec.decodeWatch(header.Get("Content-Encoding"))
	default:
		rec.seenBody = &bytes.Buffer{}
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
	if rec.seenBody != nil {
		rec.seenBody.Write(p)
		if rec.seenBody.Len() <= MaxResultBody {
			return len(p), nil
		}
		p, rec.seenBody = rec.seenBody.Bytes(), nil
		if rec.seen && rec.listener.sees(rec.call.Ref) {
			rec.recordOpaque()
		}
	}
	if rec.frames != nil {
		rec.frames.write(p, rec.decoded)
	}
	return rec.ResponseWriter.Write(p)
}

func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *recorder) finish() {
	if rec.body != nil && rec.body.Len() <= MaxResultBody {
		rec.readResult(rec.Header().Get("Content-Encoding"))
	}
	switch {
	case rec.seenBody == nil:
	case rec.listener.o.API != nil:
		rec.readObjects(rec.Header().Get("Content-Encoding"))
		rec.ResponseWriter.Write(rec.seenBody.Bytes())
	default:
		rec.readSeen(rec.Header().Get("Content-Encoding"))
		rec.ResponseWriter.Write(rec.seenBody.Bytes())
	}
	if rec.frames != nil {
		close(rec.frames.chunks)
		<-rec.decoded
	}
	rec.record()
	if record := rec.listener.o.Record; rec.call.Watch && record != nil {
		record(trace.Event{WatchEnd: &trace.WatchEnd{Caller: rec.call.Caller, System: rec.call.Ref.System, Stream: rec.call.Stream}})
	}
}

// decompress undoes encoding, or returns nil for an unknown encoding.
func decompress(body io.Reader, encoding string) io.Reader {
	switch encoding {
	case "", "identity":
		return body
	case "gzip":
		unzipped, err := gzip.NewReader(body)
		if err != nil {
			return nil
		}
		return io.LimitReader(unzipped, MaxResultBody)
	}
	return nil
}

func (rec *recorder) readResult(encoding string) {
	body := decompress(rec.body, encoding)
	if body == nil {
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

// readObjects shows each object an API map reads, records a Seen of each
// of a Seen kind, and takes a write's result from the first.
func (rec *recorder) readObjects(encoding string) {
	o := rec.listener.o
	var objects []Object
	if body := decompress(bytes.NewReader(rec.seenBody.Bytes()), encoding); body != nil {
		if data, err := io.ReadAll(body); err == nil {
			objects = o.API.Objects(rec.req, data)
		}
	}
	if objects == nil && rec.seen && rec.listener.sees(rec.call.Ref) {
		rec.recordOpaque()
	}
	if len(objects) > 0 && mutating(rec.call.Verb) {
		rec.call.ResultName, rec.call.ResultUID, rec.call.ResultRV = objects[0].Ref.Name, objects[0].UID, objects[0].RV
	}
	for _, object := range objects {
		if o.Shown != nil {
			o.Shown(object)
		}
		if rec.seen && rec.listener.sees(object.Ref) && o.Record != nil {
			o.Record(trace.Event{Seen: &trace.Seen{Caller: rec.call.Caller, Ref: object.Ref, UID: object.UID, RV: object.RV}})
		}
	}
}

type objectMeta struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
}

// readSeen records a Seen for each list item, or the one object of any
// other response.
func (rec *recorder) readSeen(encoding string) {
	body := decompress(bytes.NewReader(rec.seenBody.Bytes()), encoding)
	if body == nil {
		rec.recordOpaque()
		return
	}
	var objects []objectMeta
	if rec.call.Verb == "list" {
		var list struct {
			Items []objectMeta `json:"items"`
		}
		if json.NewDecoder(body).Decode(&list) != nil {
			rec.recordOpaque()
			return
		}
		objects = list.Items
	} else {
		var object objectMeta
		if json.NewDecoder(body).Decode(&object) != nil {
			rec.recordOpaque()
			return
		}
		objects = []objectMeta{object}
	}
	for _, o := range objects {
		rec.recordSeen(o.Metadata, false)
	}
}

// decodeWatch decodes the watch's frames as the listener forwards them, and
// records a Seen for each ADDED, MODIFIED or DELETED event.
func (rec *recorder) decodeWatch(encoding string) {
	rec.frames, rec.decoded = &feed{chunks: make(chan []byte), drained: make(chan struct{})}, make(chan struct{})
	go func() {
		defer close(rec.decoded)
		body := decompress(rec.frames, encoding)
		if body == nil {
			rec.recordOpaque()
			return
		}
		decoder := json.NewDecoder(body)
		for {
			var event struct {
				Type   string     `json:"type"`
				Object objectMeta `json:"object"`
			}
			if err := decoder.Decode(&event); err != nil {
				return
			}
			switch event.Type {
			case "ADDED", "MODIFIED", "DELETED":
				rec.recordSeen(event.Object.Metadata, event.Type == "DELETED")
			}
		}
	}()
}

// feed hands a watch's writes to its decoder. A write returns once the
// decoder asks for more, so it has recorded every whole frame written.
type feed struct {
	chunks  chan []byte
	drained chan struct{}
	rest    []byte
	waiting bool
}

func (f *feed) Read(p []byte) (int, error) {
	if len(f.rest) == 0 {
		if f.waiting {
			f.drained <- struct{}{}
			f.waiting = false
		}
		chunk, open := <-f.chunks
		if !open {
			return 0, io.EOF
		}
		f.rest, f.waiting = chunk, true
	}
	n := copy(p, f.rest)
	f.rest = f.rest[n:]
	return n, nil
}

// write waits until the decoder took p, or until it stopped.
func (f *feed) write(p []byte, stopped <-chan struct{}) {
	select {
	case f.chunks <- p:
	case <-stopped:
		return
	}
	select {
	case <-f.drained:
	case <-stopped:
	}
}

func (rec *recorder) recordOpaque() {
	if record := rec.listener.o.Record; record != nil {
		record(trace.Event{Seen: &trace.Seen{Caller: rec.call.Caller, Ref: rec.call.Ref, Stream: rec.call.Stream, Opaque: true}})
	}
}

func (rec *recorder) recordSeen(meta metav1.ObjectMeta, deleted bool) {
	record := rec.listener.o.Record
	if record == nil {
		return
	}
	ref := rec.call.Ref
	ref.Namespace, ref.Name = meta.Namespace, meta.Name
	record(trace.Event{Seen: &trace.Seen{
		Caller: rec.call.Caller, Ref: ref, UID: string(meta.UID), RV: meta.ResourceVersion,
		Deleted: deleted, Stream: rec.call.Stream,
	}})
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

func (l *Listener) writeError(w http.ResponseWriter, e plan.Error, r Request, path string) {
	if l.o.API == nil {
		writeStatus(w, e, r, path)
		return
	}
	contentType, body := l.o.API.Error(e.Code, fmt.Sprintf("reconciler-fuzzer fault: %s %s", r.Verb, path))
	w.Header().Set("Content-Type", contentType)
	setRetryAfter(w, e)
	w.WriteHeader(e.Code)
	w.Write(body)
}

func setRetryAfter(w http.ResponseWriter, e plan.Error) {
	if seconds := retryAfter(e); seconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
}

func retryAfter(e plan.Error) int { return int(math.Ceil(time.Duration(e.RetryAfter).Seconds())) }

// writeStatus answers with a metav1.Status, which a client decodes whatever
// content type it asked for.
func writeStatus(w http.ResponseWriter, e plan.Error, r Request, path string) {
	resource := schema.GroupResource{Group: r.Ref.Group, Resource: r.Ref.Resource}
	status := apierrors.NewGenericServerResponse(e.Code, r.Verb, resource, r.Ref.Name, "", retryAfter(e), false).ErrStatus
	status.TypeMeta = metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}
	status.Message = fmt.Sprintf("reconciler-fuzzer fault: %s %s", r.Verb, path)
	body, _ := json.Marshal(status)
	w.Header().Set("Content-Type", "application/json")
	setRetryAfter(w, e)
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
