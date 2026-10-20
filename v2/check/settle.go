package check

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

// Settled returns the unmet clauses of the settle condition at t, or none
// when it holds. The settle wait and Converge both call it.
func Settled(ix *trace.Index, t time.Duration, s Spec) []string {
	var unmet []string
	live := ix.Live(s.PrimaryGroup, s.PrimaryResource, t)
	for _, cr := range live {
		unmet = append(unmet, crClauses(cr, s)...)
	}

	since, up := ix.Up(t)
	switch {
	case !up:
		unmet = append(unmet, "the controller is down")
	case t-since < s.Stable:
		unmet = append(unmet, fmt.Sprintf("the controller has run for %v, less than stable %v", t-since, s.Stable))
	}
	if !slices.ContainsFunc(ix.Calls(trace.Controller, since, t), func(e trace.Event) bool { return judged(e) && succeeded(*e.Call) }) {
		unmet = append(unmet, fmt.Sprintf("the controller has made no successful request since it started at %v", since))
	}
	return append(unmet, quietClauses(ix, t, s, max(1, len(live)))...)
}

// crClauses says whether cr is ready and not being deleted.
func crClauses(cr trace.Version, s Spec) []string {
	name := describe(cr.Ref)
	var body map[string]any
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		return []string{fmt.Sprintf("%s has a body that is not a JSON object: %v", name, err)}
	}
	if metadata, _ := body["metadata"].(map[string]any); metadata["deletionTimestamp"] != nil {
		return []string{name + " is being deleted"}
	}
	if ready, err := s.Ready(body); err != nil {
		return []string{fmt.Sprintf("%s: ready: %v", name, err)}
	} else if !ready {
		return []string{name + " is not ready"}
	}
	return nil
}

// quietClauses holds the controller to no changed write and to its budgets
// over (t-Stable, t].
func quietClauses(ix *trace.Index, t time.Duration, s Spec, crs int) []string {
	var changed []trace.Event
	reads, writes := 0, 0
	for _, e := range ix.Calls(trace.Controller, t-s.Stable, t) {
		if !judged(e) {
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
	var unmet []string
	if len(changed) > 0 {
		first := changed[0].Call
		ref := first.Ref
		if ref.Name == "" {
			ref.Name = first.ResultName
		}
		unmet = append(unmet, fmt.Sprintf("the controller made %d changed writes in %s; first it changed %s with %s at %v",
			len(changed), window, describe(ref), first.Verb, changed[0].At))
	}
	if budget := s.Budgets.Reads * crs; reads > budget {
		unmet = append(unmet, fmt.Sprintf("the controller made %d gets and lists in %s, over the budget of %d", reads, window, budget))
	}
	if budget := s.Budgets.Writes * crs; writes > budget {
		unmet = append(unmet, fmt.Sprintf("the controller made %d writes in %s, over the budget of %d", writes, window, budget))
	}
	return unmet
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
// The condition changes only at an event or at an event plus Stable, so
// those are the only candidate times.
func FirstSettled(ix *trace.Index, from, to time.Duration, s Spec) (time.Duration, bool) {
	candidates := []time.Duration{from}
	for _, e := range ix.EventsIn(from-s.Stable-1, to) {
		for _, t := range []time.Duration{e.At, e.At + s.Stable} {
			if from <= t && t <= to {
				candidates = append(candidates, t)
			}
		}
	}
	slices.Sort(candidates)
	for _, t := range slices.Compact(candidates) {
		if len(Settled(ix, t, s)) == 0 {
			return t, true
		}
	}
	return 0, false
}
