package check

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/owner"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

// Settled returns the unmet clauses of the settle condition at t, or none
// when it holds. The settle wait and Converge both call it.
func Settled(ix *trace.Index, t time.Duration, s Spec) []string {
	var texts []string
	for _, u := range unmetClauses(ix, t, s) {
		texts = append(texts, u.text)
	}
	return texts
}

// unmet is one unmet clause, by number, with the kind of the object it is
// about, or none.
type unmet struct {
	clause int
	kind   string
	text   string
}

func unmetClauses(ix *trace.Index, t time.Duration, s Spec) []unmet {
	var unmets []unmet
	crs := 0
	for _, p := range s.Primaries {
		live := ix.Live(p.Kind.Group, p.Kind.Resource, t)
		for _, cr := range live {
			unmets = append(unmets, crClauses(ix, p, cr, t, s)...)
		}
		crs += len(live)
	}

	since, up := ix.Up(t)
	switch {
	case !up:
		unmets = append(unmets, unmet{3, "", "the controller is down"})
	case t-since < s.Stable:
		unmets = append(unmets, unmet{3, "", fmt.Sprintf("the controller has run for %v, less than stable %v", t-since, s.Stable)})
	}
	if !slices.ContainsFunc(ix.Calls(trace.Controller, since, t), func(e trace.Event) bool {
		return judged(e) && succeeded(*e.Call) && !e.Call.Unknown() && e.At-e.Call.Latency >= since
	}) {
		unmets = append(unmets, unmet{4, "", fmt.Sprintf("the controller has made no successful request since it started at %v", since)})
	}
	unmets = append(unmets, lookClauses(ix, t, s)...)
	for _, model := range ix.Pending(t) {
		unmets = append(unmets, unmet{6, "", "model " + model + " has work pending"})
	}
	return append(unmets, quietClauses(ix, t, s, max(1, crs))...)
}

// lookClauses holds the controller to looking at each object that the
// damage and changeFixture actions of the latest step changed, until the
// step's lapse time. It also holds the step for Stable after each change,
// after a holdWatch released the watch that saw it, and, without a watch
// across the change, after the first request that looked, so that clause 7
// sees the controller's reaction. A held watch does not look.
func lookClauses(ix *trace.Index, t time.Duration, s Spec) []unmet {
	step, found := latestStep(ix, t)
	if !found || step.Reached[trace.EndFaults] && t >= lapse(ix, step, s) {
		return nil
	}
	var unmets []unmet
	for _, change := range changes(ix, step.Step) {
		ref := *change.Action.Ref
		watched := ix.Watching(trace.Controller, ref, change.At, t)
		released, holding, held := ix.Released(ref, change.At, t)
		watched = watched && !holding
		calls := ix.Calls(trace.Controller, change.At, t)
		first := slices.IndexFunc(calls, func(e trace.Event) bool { return looksAt(*e.Call, ref) })
		switch {
		case t < change.At+s.Stable:
			unmets = append(unmets, unmet{5, kind(ref), fmt.Sprintf("%s was changed at %v, less than stable ago", describe(ref), change.At)})
		case watched && held && t < released+s.Stable:
			unmets = append(unmets, unmet{5, kind(ref), fmt.Sprintf("a holdWatch released the controller's watch of %s at %v, less than stable ago", describe(ref), released)})
		case !watched && first >= 0 && t < calls[first].At+s.Stable:
			unmets = append(unmets, unmet{5, kind(ref), fmt.Sprintf("the controller looked at %s at %v, less than stable ago", describe(ref), calls[first].At)})
		case !watched && first < 0:
			unmets = append(unmets, unmet{5, kind(ref), fmt.Sprintf("the controller has not looked at %s since the %s at %v", describe(ref), change.Action.Do, change.At)})
		}
	}
	return unmets
}

// latestStep is the step whose fault period started last at or before t.
func latestStep(ix *trace.Index, t time.Duration) (trace.StepSpan, bool) {
	var latest trace.StepSpan
	found := false
	for _, step := range ix.Steps() {
		if step.Reached[trace.FaultPeriod] && step.Start <= t {
			latest, found = step, true
		}
	}
	return latest, found
}

// lapse is when clause 5 stops holding step to its changes: 1.5 times the
// longest poll interval of the changed objects' systems after end faults.
// Faults change no object, so their systems do not count.
func lapse(ix *trace.Index, step trace.StepSpan, s Spec) time.Duration {
	poll := s.Stable
	if changed := changes(ix, step.Step); len(changed) > 0 {
		poll = 0
		for _, e := range changed {
			poll = max(poll, s.poll(e.Action.Ref.System))
		}
	}
	return step.EndFaults + poll*3/2
}

// changes are the step's damage and changeFixture actions that changed an
// object without error.
func changes(ix *trace.Index, step int) []trace.Event {
	var changed []trace.Event
	for _, e := range ix.Actions(step) {
		if (e.Action.Do == "damage" || e.Action.Do == "changeFixture") && e.Action.Ref != nil && e.Action.Error == "" {
			changed = append(changed, e)
		}
	}
	return changed
}

// looksAt says whether c shows the controller the current state of ref.
func looksAt(c trace.Call, ref trace.Ref) bool {
	return (succeeded(c) || c.Status == 404) && !c.Unknown() && c.Ref.Covers(ref)
}

// crClauses says whether cr, a CR of p's kind, is done at t, ready or
// halted, and not being deleted.
func crClauses(ix *trace.Index, p Primary, cr trace.Version, t time.Duration, s Spec) []unmet {
	name, k := describe(cr.Ref), kind(cr.Ref)
	var body map[string]any
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		return []unmet{{1, k, fmt.Sprintf("%s has a body that is not a JSON object: %v", name, err)}}
	}
	if metadata, _ := body["metadata"].(map[string]any); metadata["deletionTimestamp"] != nil {
		return []unmet{{2, k, name + " is being deleted"}}
	}
	if ready, err := p.Ready(body); err != nil {
		return []unmet{{1, k, fmt.Sprintf("%s: ready: %v", name, err)}}
	} else if !ready && !halted(ix, p, cr, body, t, s) {
		return []unmet{{1, k, name + " is not ready"}}
	}
	return nil
}

// halted reports whether p.Halted accepts cr's body and the controller
// made a call of unknown outcome, by t, on cr or on an instance cr owns: by
// rules 1 to 3, or by rule 4 as the one CR alive at its creation.
func halted(ix *trace.Index, p Primary, cr trace.Version, body map[string]any, t time.Duration, s Spec) bool {
	if p.Halted == nil {
		return false
	}
	if ok, err := p.Halted(body); err != nil || !ok {
		return false
	}
	var w *owner.World
	return slices.ContainsFunc(ix.Calls(trace.Controller, -1, t), func(e trace.Event) bool {
		c := e.Call
		switch {
		case !c.Unknown():
			return false
		case c.Object() == cr.Ref:
			return true
		case c.ResultUID == "":
			return false
		}
		if w == nil {
			w = owner.New(ix, s.Owners)
		}
		i, found := w.Instance(c.ResultUID)
		if !found {
			return false
		}
		o := w.Owner(i, t)
		return slices.Equal(o.CRs, []string{cr.UID})
	})
}

// quietClauses holds the controller to no changed write over (t-Stable, t],
// and to its budgets over each system's poll interval up to t. A changed
// write sent by t counts even if its response ends later, because the
// observer may record its version first.
func quietClauses(ix *trace.Index, t time.Duration, s Spec, crs int) []unmet {
	var changed []trace.Event
	reads, writes := map[string]int{}, map[string]int{}
	window := s.Stable
	for _, poll := range s.Polls {
		window = max(window, poll)
	}
	for _, e := range ix.Calls(trace.Controller, t-window, math.MaxInt64) {
		if !judged(e) {
			continue
		}
		if e.At > t {
			if e.At-e.Call.Latency <= t && ix.Changed(e) && !s.periodicWrite(ix, e) {
				changed = append(changed, e)
			}
			continue
		}
		system := e.Call.Ref.System
		inPoll := e.At > t-s.poll(system)
		switch verb := e.Call.Verb; {
		case (verb == "get" || verb == "list") && inPoll:
			reads[system]++
		case trace.Mutating(verb):
			if inPoll {
				writes[system]++
			}
			if e.At > t-s.Stable && ix.Changed(e) && !s.periodicWrite(ix, e) {
				changed = append(changed, e)
			}
		}
	}
	var unmets []unmet
	if len(changed) > 0 {
		first := changed[0].Call
		ref := first.Ref
		if ref.Name == "" {
			ref.Name = first.ResultName
		}
		unmets = append(unmets, unmet{7, kind(ref), fmt.Sprintf("the controller made %d changed writes in (%v, %v]; first it changed %s with %s at %v",
			len(changed), t-s.Stable, t, describe(ref), first.Verb, changed[0].At)})
	}
	for _, system := range slices.Sorted(maps.Keys(reads)) {
		if budget := s.Budgets.Reads * crs; reads[system] > budget {
			unmets = append(unmets, unmet{7, "", fmt.Sprintf("the controller made %d gets and lists%s in %s, over the budget of %d", reads[system], of(system), s.pollWindow(system, t), budget)})
		}
	}
	for _, system := range slices.Sorted(maps.Keys(writes)) {
		if budget := s.Budgets.Writes * crs; writes[system] > budget {
			unmets = append(unmets, unmet{7, "", fmt.Sprintf("the controller made %d writes%s in %s, over the budget of %d", writes[system], of(system), s.pollWindow(system, t), budget)})
		}
	}
	return unmets
}

// of names a fake, and is empty for kube.
func of(system string) string {
	if system == "kube" {
		return ""
	}
	return " of " + system
}

func (s Spec) pollWindow(system string, t time.Duration) string {
	return fmt.Sprintf("(%v, %v]", t-s.poll(system), t)
}

// judged says whether a clause counts the call.
func judged(e trace.Event) bool { return !trace.Ignored(*e.Call) }

func succeeded(c trace.Call) bool { return c.Status >= 200 && c.Status <= 299 }

// describe names an object as "resource.group namespace/name", and a
// fake's as "system collection id".
func describe(ref trace.Ref) string {
	if ref.System != "kube" && ref.System != "" {
		return ref.System + " " + ref.Resource + " " + ref.Name
	}
	kind := ref.Resource
	if ref.Group != "" {
		kind += "." + ref.Group
	}
	name := ref.Name
	if ref.Namespace != "" {
		name = ref.Namespace + "/" + name
	}
	return kind + " " + name
}

// FirstSettled is the earliest time in [from, to] at which Settled holds.
// The condition changes only at an event, an event plus Stable, or a
// clause 5 lapse, so those are the only candidate times.
func FirstSettled(ix *trace.Index, from, to time.Duration, s Spec) (time.Duration, bool) {
	candidates := []time.Duration{from}
	for _, step := range ix.Steps() {
		if t := lapse(ix, step, s); from <= t && t <= to {
			candidates = append(candidates, t)
		}
	}
	for _, e := range ix.EventsIn(from-s.Stable-1, to) {
		for _, t := range []time.Duration{e.At, e.At + s.Stable} {
			if from <= t && t <= to {
				candidates = append(candidates, t)
			}
		}
	}
	slices.Sort(candidates)
	for _, t := range slices.Compact(candidates) {
		if len(unmetClauses(ix, t, s)) == 0 {
			return t, true
		}
	}
	return 0, false
}
