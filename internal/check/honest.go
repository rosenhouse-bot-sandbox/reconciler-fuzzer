package check

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Honest fails a CR that reports Ready on a dependency the controller did
// not see ready, or, with track, that stays Ready too long after the
// controller saw a dependency not ready. DESIGN.md §12 defines it.
func Honest(ix *trace.Index, s Spec) []Finding {
	var deps []Model
	for _, m := range s.Models {
		if m.Dependency != nil {
			deps = append(deps, m)
		}
	}
	if len(deps) == 0 {
		return nil
	}
	h := &honesty{ix: ix, s: s, w: owner.New(ix, s.Owners), steps: ix.Steps(), deletes: controllerDeletes(ix), found: map[findingKey]*Finding{}}
	order, crs := crVersions(ix, s)
	for _, uid := range order {
		versions := crs[uid]
		p, _ := s.primary(versions[0].Version.Ref)
		h.judgeTriggers(uid, p, versions, deps)
		if events := ix.Events(); len(events) > 0 {
			h.judgeTrack(uid, p, versions, deps, events[len(events)-1].At)
		}
	}
	keys := slices.SortedFunc(maps.Keys(h.found), func(a, b findingKey) int {
		return cmp.Or(cmp.Compare(a.step, b.step), cmp.Compare(a.summary, b.summary), cmp.Compare(a.kind, b.kind))
	})
	findings := make([]Finding, len(keys))
	for n, k := range keys {
		findings[n] = *h.found[k]
	}
	return findings
}

type honesty struct {
	ix      *trace.Index
	s       Spec
	w       *owner.World
	steps   []trace.StepSpan
	deletes map[trace.Ref][]time.Duration
	found   map[findingKey]*Finding
}

type findingKey struct {
	step          int
	summary, kind string
}

// crVersions returns each CR's versions, by UID, in Seq order, and the
// UIDs in order of first version.
func crVersions(ix *trace.Index, s Spec) ([]string, map[string][]trace.Event) {
	var order []string
	versions := map[string][]trace.Event{}
	for _, e := range ix.Events() {
		if v := e.Version; v != nil && s.listed(v.Ref) {
			if versions[v.UID] == nil {
				order = append(order, v.UID)
			}
			versions[v.UID] = append(versions[v.UID], e)
		}
	}
	return order, versions
}

// judgeTriggers judges each version of the CR, of p's kind, that the
// controller wrote that becomes ready, or raises observedGeneration while
// ready. Only a CR of the first entry's kind needs a required dependency.
func (h *honesty) judgeTriggers(uid string, p Primary, versions []trace.Event, deps []Model) {
	requires := p.Kind == h.s.Primaries[0].Kind
	wasReady, wasObserved := false, 0.0
	for _, e := range versions {
		v := e.Version
		body := decode(v.Body)
		isReady, observed := satisfies(p.Ready, body), observedGeneration(body)
		trigger := isReady && (!wasReady || observed > wasObserved)
		wasReady, wasObserved = isReady, observed
		if !trigger || h.ix.Writer(*v) != trace.Controller {
			continue
		}
		call, _ := h.ix.WriteCall(v.Ref, v.RV)
		t0 := call.At - call.Call.Latency
		for _, m := range deps {
			owned := h.dependencies(uid, m, t0)
			if len(owned) == 0 && m.Dependency.Required && requires {
				h.add(t0, noDependency, kind(v.Ref), h.slot(uid))
			}
			for _, dep := range owned {
				if why := h.unseen(dep, m, t0); why != "" {
					h.add(t0, beforeSeeing, kind(dep.Ref), fmt.Sprintf("%s: %s %s", h.slot(uid), describe(dep.Ref), why))
				}
			}
		}
	}
}

const (
	beforeSeeing = "reported ready before seeing its dependency ready"
	noDependency = "reported ready with no dependency"
	stayedReady  = "stayed ready after seeing its dependency not ready"
)

// dependencies are the instances of m's kind at t whose rule 1 to 3 owning CR
// is uid. An instance counts from when it appeared until the controller
// deletes it, and not while the controller last saw it deleted. A live one
// counts unless it is being deleted, and a gone one only if the controller
// last saw it live and nothing has replaced it.
func (h *honesty) dependencies(uid string, m Model, t time.Duration) []owner.Instance {
	var owned []owner.Instance
	for _, i := range h.w.All() {
		if !m.covers(i.Ref) || h.appeared(i) > t || h.controllerDeleted(i, t) {
			continue
		}
		seen, found := lastSeen(h.ix.Seen(trace.Controller, i.UID), t)
		if found && seen.Seen.Deleted {
			continue
		}
		if i.Gone != 0 && i.Gone <= t {
			if latest, _ := h.w.Latest(i.Ref, t); !found || !i.Live(seen.At) || latest.UID != i.UID {
				continue
			}
		} else if deleting(h.w.Body(i, t)) {
			continue
		}
		if cr, named := h.w.Owner(i, max(t, i.Created)).Named(); named && cr == uid {
			owned = append(owned, i)
		}
	}
	return owned
}

// appeared is when the trace first shows i: its first version, or the
// controller's first sight of it if that is earlier.
func (h *honesty) appeared(i owner.Instance) time.Duration {
	if seens := h.ix.Seen(trace.Controller, i.UID); len(seens) > 0 {
		return min(i.Created, seens[0].At)
	}
	return i.Created
}

// controllerDeleted reports whether the controller sent a `delete` of i's
// ref, with no subresource, by t, and the server answered with a 2xx. A
// delete sent before i appeared does not count, as it may have hit an
// earlier instance of i's ref.
func (h *honesty) controllerDeleted(i owner.Instance, t time.Duration) bool {
	appeared := h.appeared(i)
	return slices.ContainsFunc(h.deletes[i.Ref], func(sent time.Duration) bool { return sent >= appeared && sent <= t })
}

// controllerDeletes returns, by ref, the send times of the controller's
// successful `delete`s with no subresource.
func controllerDeletes(ix *trace.Index) map[trace.Ref][]time.Duration {
	sent := map[trace.Ref][]time.Duration{}
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		if c := e.Call; c.Verb == "delete" && c.Subresource == "" && succeeded(*c) {
			sent[c.Object()] = append(sent[c.Object()], e.At-c.Latency)
		}
	}
	return sent
}

func deleting(body json.RawMessage) bool {
	metadata, _ := decode(body)["metadata"].(map[string]any)
	return metadata["deletionTimestamp"] != nil
}

// unseen says why the controller's last look at dep by t0 does not show it
// ready, or returns empty.
func (h *honesty) unseen(dep owner.Instance, m Model, t0 time.Duration) string {
	seen, found := lastSeen(h.ix.Seen(trace.Controller, dep.UID), t0)
	write, wrote := h.lastWrite(dep.Ref, t0)
	since := time.Duration(-1)
	if found {
		since = seen.At
	}
	switch {
	case h.ix.Opaque(trace.Controller, dep.Ref, since, t0):
		return ""
	case !found:
		return "not seen"
	case wrote && !h.seenSince(seen, write):
		return "not seen since the controller's write"
	case !h.ready(seen, m):
		return "seen not ready"
	}
	return ""
}

func lastSeen(seens []trace.Event, t time.Duration) (trace.Event, bool) {
	n, _ := slices.BinarySearchFunc(seens, t, func(e trace.Event, t time.Duration) int {
		return cmp.Compare(e.At, t+1)
	})
	if n == 0 {
		return trace.Event{}, false
	}
	return seens[n-1], true
}

// lastWrite is the controller's last changing or unknown-outcome mutating
// call on ref by t. A no-op write leaves a read current.
func (h *honesty) lastWrite(ref trace.Ref, t time.Duration) (trace.Event, bool) {
	var last trace.Event
	found := false
	for _, e := range h.ix.Calls(trace.Controller, -1, t) {
		if c := e.Call; trace.Mutating(c.Verb) && c.Object() == ref && (h.ix.Changed(e) || c.Unknown()) {
			last, found = e, true
		}
	}
	return last, found
}

// seenSince reports whether seen shows write's version or a later one. A
// watch event can show a write's version before its response ends, so
// versions are compared by order, and by time only when the trace lacks one.
func (h *honesty) seenSince(seen, write trace.Event) bool {
	if later, found := h.ix.Later(seen.Seen.Ref, seen.Seen.RV, write.Call.ResultRV); found {
		return later
	}
	return seen.At > write.At
}

// ready reports whether a Seen event shows a live version m's ready accepts.
func (h *honesty) ready(seen trace.Event, m Model) bool {
	v, found := h.ix.VersionOf(seen.Seen.Ref, seen.Seen.RV)
	return found && !seen.Seen.Deleted && satisfies(m.Ready, decode(v.Body))
}

// judgeTrack fails a CR, of p's kind, that stays ready for track after the
// controller saw one of its dependencies not ready, and saw nothing better
// since.
func (h *honesty) judgeTrack(uid string, p Primary, versions []trace.Event, deps []Model, end time.Duration) {
	for _, m := range deps {
		d := m.Dependency.Track
		if d == 0 {
			continue
		}
		for _, dep := range h.allOf(m) {
			for _, seen := range h.ix.Seen(trace.Controller, dep.UID) {
				at := seen.At + d
				if at > end || h.ready(seen, m) || seen.Seen.Deleted || h.seenDeleting(seen) ||
					!h.streak(dep, m, seen.At, at) || !readyThrough(p, versions, seen.At, at) || h.excused(uid, seen.At, at) ||
					h.ix.Opaque(trace.Controller, dep.Ref, seen.At, at) {
					continue
				}
				if cr, named := h.w.Owner(dep, seen.At).Named(); !named || cr != uid {
					continue
				}
				h.add(at, stayedReady, kind(dep.Ref), fmt.Sprintf("%s: %s seen not ready at %v", h.slot(uid), describe(dep.Ref), seen.At))
				break
			}
		}
	}
}

func (h *honesty) seenDeleting(seen trace.Event) bool {
	v, found := h.ix.VersionOf(seen.Seen.Ref, seen.Seen.RV)
	return found && deleting(v.Body)
}

// excused reports whether, in (from, to], the controller started or exited,
// or a write of its to the CR failed or had an unknown outcome. Either can
// keep an honest controller from reporting a dependency's change.
func (h *honesty) excused(uid string, from, to time.Duration) bool {
	if slices.ContainsFunc(h.ix.Procs(), func(e trace.Event) bool { return e.At > from && e.At <= to }) {
		return true
	}
	cr, found := h.w.Instance(uid)
	return found && slices.ContainsFunc(h.ix.Calls(trace.Controller, from, to), func(e trace.Event) bool {
		c := e.Call
		return trace.Mutating(c.Verb) && c.Object() == cr.Ref && (c.Status < 200 || c.Status > 299 || c.Unknown())
	})
}

func (h *honesty) allOf(m Model) []owner.Instance {
	return slices.DeleteFunc(h.w.All(), func(i owner.Instance) bool {
		return !m.covers(i.Ref)
	})
}

// streak reports whether dep lives through to, the controller has not
// deleted it by then, and every Seen of it after from until to shows it live
// and not ready.
func (h *honesty) streak(dep owner.Instance, m Model, from, to time.Duration) bool {
	if !dep.Live(to) || h.controllerDeleted(dep, to) {
		return false
	}
	return !slices.ContainsFunc(h.ix.Seen(trace.Controller, dep.UID), func(e trace.Event) bool {
		return e.At > from && e.At <= to && (e.Seen.Deleted || h.ready(e, m))
	})
}

// readyThrough reports whether the CR's version at from, and every later
// one until to, satisfies p's ready.
func readyThrough(p Primary, versions []trace.Event, from, to time.Duration) bool {
	current := false
	for _, e := range versions {
		if e.At > to {
			break
		}
		ok := !e.Version.Deleted && satisfies(p.Ready, decode(e.Version.Body))
		if e.At <= from {
			current = ok
		} else if !ok {
			return false
		}
	}
	return current
}

func (h *honesty) slot(uid string) string { return crName(h.w, uid) }

// add adds a detail to the finding of t's step, summary and kind.
func (h *honesty) add(t time.Duration, summary, kind, detail string) {
	key := findingKey{stepAt(h.steps, t), summary, kind}
	f := h.found[key]
	if f == nil {
		f = &Finding{Check: "Honest", Step: key.step, At: t, Summary: summary, Kind: kind}
		h.found[key] = f
	}
	f.At = min(f.At, t)
	if !slices.Contains(f.Details, detail) {
		f.Details = append(f.Details, detail)
	}
}

func decode(body json.RawMessage) map[string]any {
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	return obj
}

func satisfies(ready func(map[string]any) (bool, error), obj map[string]any) bool {
	ok, err := ready(obj)
	return err == nil && ok
}

// observedGeneration is status.observedGeneration, or else the highest
// observedGeneration in status.conditions.
func observedGeneration(obj map[string]any) float64 {
	status, _ := obj["status"].(map[string]any)
	if og, ok := status["observedGeneration"].(float64); ok {
		return og
	}
	highest := 0.0
	conditions, _ := status["conditions"].([]any)
	for _, c := range conditions {
		condition, _ := c.(map[string]any)
		if og, ok := condition["observedGeneration"].(float64); ok {
			highest = max(highest, og)
		}
	}
	return highest
}
