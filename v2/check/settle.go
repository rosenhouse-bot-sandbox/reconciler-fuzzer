package check

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
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
	live := ix.Live(s.PrimaryGroup, s.PrimaryResource, t)
	for _, cr := range live {
		unmets = append(unmets, crClauses(cr, s)...)
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
	return append(unmets, quietClauses(ix, t, s, max(1, len(live)))...)
}

// lookClauses holds the controller to looking at each object that the
// damage and changeFixture actions of the latest step changed, until the
// step's lapse time. It also holds the step for Stable after each change,
// so that clause 7 sees the controller's reaction.
func lookClauses(ix *trace.Index, t time.Duration, s Spec) []unmet {
	step, found := latestStep(ix, t)
	if !found || step.Reached[trace.EndFaults] && t >= lapse(step, s) {
		return nil
	}
	var unmets []unmet
	for _, change := range changes(ix, step.Step) {
		ref := *change.Action.Ref
		looked := ix.Watching(trace.Controller, ref, change.At, t) ||
			slices.ContainsFunc(ix.Calls(trace.Controller, change.At, t), func(e trace.Event) bool { return looksAt(*e.Call, ref) })
		switch {
		case t < change.At+s.Stable:
			unmets = append(unmets, unmet{5, kind(ref), fmt.Sprintf("%s was changed at %v, less than stable ago", describe(ref), change.At)})
		case !looked:
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

// lapse is when clause 5 stops holding step to its changes: 1.5 poll
// intervals of the Kubernetes system, which is Stable, after end faults.
func lapse(step trace.StepSpan, s Spec) time.Duration { return step.EndFaults + s.Stable*3/2 }

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

// crClauses says whether cr is ready and not being deleted.
func crClauses(cr trace.Version, s Spec) []unmet {
	name, k := describe(cr.Ref), kind(cr.Ref)
	var body map[string]any
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		return []unmet{{1, k, fmt.Sprintf("%s has a body that is not a JSON object: %v", name, err)}}
	}
	if metadata, _ := body["metadata"].(map[string]any); metadata["deletionTimestamp"] != nil {
		return []unmet{{2, k, name + " is being deleted"}}
	}
	if ready, err := s.Ready(body); err != nil {
		return []unmet{{1, k, fmt.Sprintf("%s: ready: %v", name, err)}}
	} else if !ready {
		return []unmet{{1, k, name + " is not ready"}}
	}
	return nil
}

// quietClauses holds the controller to no changed write and to its budgets
// over (t-Stable, t]. A changed write sent by t counts even if its response
// ends later, because the observer may record its version first.
func quietClauses(ix *trace.Index, t time.Duration, s Spec, crs int) []unmet {
	var changed []trace.Event
	reads, writes := 0, 0
	for _, e := range ix.Calls(trace.Controller, t-s.Stable, math.MaxInt64) {
		if !judged(e) {
			continue
		}
		if e.At > t {
			if e.At-e.Call.Latency <= t && ix.Changed(e) {
				changed = append(changed, e)
			}
			continue
		}
		switch verb := e.Call.Verb; {
		case verb == "get" || verb == "list":
			reads++
		case trace.Mutating(verb):
			writes++
			if ix.Changed(e) {
				changed = append(changed, e)
			}
		}
	}
	window := fmt.Sprintf("(%v, %v]", t-s.Stable, t)
	var unmets []unmet
	if len(changed) > 0 {
		first := changed[0].Call
		ref := first.Ref
		if ref.Name == "" {
			ref.Name = first.ResultName
		}
		unmets = append(unmets, unmet{7, kind(ref), fmt.Sprintf("the controller made %d changed writes in %s; first it changed %s with %s at %v",
			len(changed), window, describe(ref), first.Verb, changed[0].At)})
	}
	if budget := s.Budgets.Reads * crs; reads > budget {
		unmets = append(unmets, unmet{7, "", fmt.Sprintf("the controller made %d gets and lists in %s, over the budget of %d", reads, window, budget)})
	}
	if budget := s.Budgets.Writes * crs; writes > budget {
		unmets = append(unmets, unmet{7, "", fmt.Sprintf("the controller made %d writes in %s, over the budget of %d", writes, window, budget)})
	}
	return unmets
}

// judged says whether a clause counts the call.
func judged(e trace.Event) bool { return !trace.Ignored(*e.Call) }

func succeeded(c trace.Call) bool { return c.Status >= 200 && c.Status <= 299 }

// describe names an object as "resource.group namespace/name".
func describe(ref trace.Ref) string {
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
		if t := lapse(step, s); from <= t && t <= to {
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
