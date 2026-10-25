package check

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const removedForeign = "removed another caller's finalizer"

// Finalizers fails a controller write whose answer lacks a finalizer that
// another caller added. DESIGN.md §12 defines it. Its findings come in order
// of their first removal.
func Finalizers(ix *trace.Index, s Spec) []Finding {
	steps := ix.Steps()
	found := map[findingKey]*Finding{}
	var order []findingKey
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		c := e.Call
		if !judgedWrite(*c, s) {
			continue
		}
		base := replaced(ix, e)
		for _, f := range finalizers(base) {
			if slices.Contains(*c.ResultFinalizers, f) {
				continue
			}
			by, foreign := adder(ix, base, f)
			if !foreign {
				continue
			}
			key := findingKey{stepAt(steps, e.At), removedForeign, kind(base.Ref)}
			if found[key] == nil {
				found[key] = &Finding{Check: "Finalizers", Step: key.step, At: e.At, Summary: removedForeign, Kind: key.kind}
				order = append(order, key)
			}
			removal := c.Verb
			if c.Subresource != "" {
				removal = c.Subresource + " " + c.Verb
			}
			if detail := fmt.Sprintf("%s: %s, added by %s, removed by %s", describe(base.Ref), f, by, removal); !slices.Contains(found[key].Details, detail) {
				found[key].Details = append(found[key].Details, detail)
			}
		}
	}
	findings := make([]Finding, len(order))
	for n, key := range order {
		findings[n] = *found[key]
	}
	return findings
}

// judgedWrite says whether Finalizers judges c: an update or patch, whose
// answer's finalizers the listener recorded, of an object or of a built-in
// kind's status. A CRD's status write keeps the object's metadata, and a
// Scale answer holds no finalizers.
func judgedWrite(c trace.Call, s Spec) bool {
	switch {
	case c.Verb != "update" && c.Verb != "patch", c.ResultFinalizers == nil, trace.Ignored(c):
		return false
	case c.Subresource == "status":
		return !slices.ContainsFunc(s.StatusKinds, c.Ref.OfKind)
	}
	return c.Subresource == ""
}

// replaced is the version a write replaced, or no version when the trace
// lacks it. A write that made its answer's version replaced the one before.
// Any other write replaced its answer's version: it changed nothing, or it
// emptied a deleting object's finalizers, which deletes the object and
// answers with the request's object at the old version.
func replaced(ix *trace.Index, e trace.Event) trace.Version {
	ref, rv := e.Call.Object(), e.Call.ResultRV
	others := slices.ContainsFunc(makers(ix, ref, rv), func(c trace.Caller) bool { return c != e.Call.Caller })
	if ix.Changed(e) && !others {
		v, _ := ix.Previous(ref, rv)
		return v
	}
	v, _ := ix.VersionOf(ref, rv)
	return v
}

// makers are the callers whose answers carry ref's version rv while it was
// new to their requests (Index.Changed). Listeners record answers as they
// end, in no fixed order, so callers whose writes overlap may all be
// makers.
func makers(ix *trace.Index, ref trace.Ref, rv string) []trace.Caller {
	var callers []trace.Caller
	for _, a := range ix.Answers(ref, rv) {
		if ix.Changed(a) {
			callers = append(callers, a.Call.Caller)
		}
	}
	return callers
}

// adder is who added f, which v holds, and whether it was another caller
// than the controller. The API server adds the garbage collector's
// finalizers for a delete's propagation policy. Any other finalizer's
// adders made the first of the versions, up to v, that all hold f. f may be
// the controller's own when no recorded write made that version, or when
// the controller or the harness may have: a sample, a fixture or a plan may
// carry the controller's own finalizer.
func adder(ix *trace.Index, v trace.Version, f string) (trace.Caller, bool) {
	if f == metav1.FinalizerDeleteDependents || f == metav1.FinalizerOrphanDependents {
		return trace.GC, true
	}
	for {
		previous, _ := ix.Previous(v.Ref, v.RV)
		if !slices.Contains(finalizers(previous), f) {
			break
		}
		v = previous
	}
	callers := makers(ix, v.Ref, v.RV)
	if len(callers) == 0 || slices.ContainsFunc(callers, func(c trace.Caller) bool { return c == trace.Controller || c == trace.Harness }) {
		return "", false
	}
	return callers[0], true
}

func finalizers(v trace.Version) []string {
	var body struct {
		Metadata struct {
			Finalizers []string `json:"finalizers"`
		} `json:"metadata"`
	}
	json.Unmarshal(v.Body, &body)
	return body.Metadata.Finalizers
}
