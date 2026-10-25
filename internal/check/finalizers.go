package check

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const removedForeign = "removed another caller's finalizer"

// Finalizers fails a controller update or patch, without a subresource,
// whose answer lacks a finalizer that another recorded caller added.
// DESIGN.md §12 defines it. Its findings come in order of their first
// removal.
func Finalizers(ix *trace.Index) []Finding {
	steps := ix.Steps()
	found := map[findingKey]*Finding{}
	var order []findingKey
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		c := e.Call
		if c.Verb != "update" && c.Verb != "patch" || c.Subresource != "" || trace.Ignored(*c) || c.ResultFinalizers == nil {
			continue
		}
		base := replaced(ix, e)
		for _, f := range finalizers(base) {
			if slices.Contains(*c.ResultFinalizers, f) {
				continue
			}
			adder := firstHolder(ix, base, f)
			if adder == trace.Controller || adder == trace.Cluster {
				continue
			}
			key := findingKey{stepAt(steps, e.At), removedForeign, kind(base.Ref)}
			if found[key] == nil {
				found[key] = &Finding{Check: "Finalizers", Step: key.step, At: e.At, Summary: removedForeign, Kind: key.kind}
				order = append(order, key)
			}
			if detail := fmt.Sprintf("%s: %s, added by %s", describe(base.Ref), f, adder); !slices.Contains(found[key].Details, detail) {
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

// replaced is the version a write replaced, or no version when the trace
// lacks it. A write that changed its object replaced the version before its
// answer's. One that did not replaced its answer's version: it changed
// nothing, or it emptied a deleting object's finalizers, which deletes the
// object and answers with the request's object at the old version.
func replaced(ix *trace.Index, e trace.Event) trace.Version {
	ref, rv := e.Call.Object(), e.Call.ResultRV
	if ix.Changed(e) {
		v, _ := ix.Previous(ref, rv)
		return v
	}
	v, _ := ix.VersionOf(ref, rv)
	return v
}

// firstHolder is the writer of the first of the versions, up to v, that
// all hold f.
func firstHolder(ix *trace.Index, v trace.Version, f string) trace.Caller {
	for {
		previous, _ := ix.Previous(v.Ref, v.RV)
		if !slices.Contains(finalizers(previous), f) {
			return ix.Writer(v)
		}
		v = previous
	}
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
