package fake

import (
	"strconv"
	"sync"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Instances numbers the lives of reused names for an API map. A UID is
// <system>/<resource>/[<namespace>/]<name>/<n>.
type Instances struct {
	mu    sync.Mutex
	lives map[trace.Ref]*life
}

type life struct {
	n           int
	live, ended bool
}

func NewInstances() *Instances { return &Instances{lives: map[trace.Ref]*life{}} }

func uid(ref trace.Ref, n int) string {
	prefix := ref.System + "/" + ref.Resource + "/"
	if ref.Namespace != "" {
		prefix += ref.Namespace + "/"
	}
	return prefix + ref.Name + "/" + strconv.Itoa(n)
}

func key(ref trace.Ref) trace.Ref {
	return trace.Ref{System: ref.System, Resource: ref.Resource, Namespace: ref.Namespace, Name: ref.Name}
}

// Live reports whether ref names a live instance.
func (in *Instances) Live(ref trace.Ref) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	l := in.lives[key(ref)]
	return l != nil && l.live
}

// Created starts an instance unless one is live, and returns the live one.
func (in *Instances) Created(ref trace.Ref) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.start(key(ref), false)
}

// Shown returns the instance a response or listing shows. A name that is
// unknown, or whose instance the observer saw gone, starts an instance.
// One deleted but not yet seen gone keeps its UID.
func (in *Instances) Shown(ref trace.Ref) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.start(key(ref), true)
}

// start begins instance n+1 when none is live, unless shown and the last
// instance is deleted but not ended. The caller holds in.mu.
func (in *Instances) start(k trace.Ref, shown bool) string {
	l := in.lives[k]
	if l == nil {
		l = &life{}
		in.lives[k] = l
	}
	if !l.live && !(shown && l.n > 0 && !l.ended) {
		l.n, l.live, l.ended = l.n+1, true, false
	}
	return uid(k, l.n)
}

// Deleted ends the live instance of ref, and returns its UID. It returns
// false when none was live.
func (in *Instances) Deleted(ref trace.Ref) (string, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	l := in.lives[key(ref)]
	if l == nil || !l.live {
		return "", false
	}
	l.live = false
	return uid(key(ref), l.n), true
}

// Ended records that the observer saw instance id of ref gone.
func (in *Instances) Ended(ref trace.Ref, id string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	l := in.lives[key(ref)]
	if l != nil && uid(key(ref), l.n) == id {
		l.live, l.ended = false, true
	}
}
