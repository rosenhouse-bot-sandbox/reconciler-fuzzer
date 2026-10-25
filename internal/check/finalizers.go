package check

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
)

const removedForeign = "removed another caller's finalizer"

// Finalizers fails a controller update or patch whose answer lacks a
// finalizer that another recorded caller added. DESIGN.md §12 defines it.
func Finalizers(ix *trace.Index) []Finding {
	steps := ix.Steps()
	found := map[findingKey]*Finding{}
	for _, e := range ix.Calls(trace.Controller, -1, math.MaxInt64) {
		c := e.Call
		if c.Verb != "update" && c.Verb != "patch" || !succeeded(*c) || trace.Ignored(*c) || c.ResultFinalizers == nil {
			continue
		}
		base, ok := replaced(ix, e)
		if !ok {
			continue
		}
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
			}
			if detail := fmt.Sprintf("%s: %s, added by %s", describe(base.Ref), f, adder); !slices.Contains(found[key].Details, detail) {
				found[key].Details = append(found[key].Details, detail)
			}
		}
	}
	var findings []Finding
	for _, key := range slices.SortedFunc(maps.Keys(found), func(a, b findingKey) int {
		return cmp.Or(cmp.Compare(a.step, b.step), cmp.Compare(a.kind, b.kind))
	}) {
		findings = append(findings, *found[key])
	}
	return findings
}

// replaced is the version a write replaced. A write that made its answer's
// version replaced the one before it. A write answered with an older version
// changed nothing, or emptied a deleting object's finalizers, which deletes
// the object and answers with the request's object at the old version.
func replaced(ix *trace.Index, e trace.Event) (trace.Version, bool) {
	ref, rv := e.Call.Object(), e.Call.ResultRV
	if first, found := ix.WriteCall(ref, rv); found && first.Seq == e.Seq {
		return ix.Previous(ref, rv)
	}
	return ix.VersionOf(ref, rv)
}

// firstHolder is the writer of the first of the versions, up to v, that
// all hold f.
func firstHolder(ix *trace.Index, v trace.Version, f string) trace.Caller {
	for {
		previous, found := ix.Previous(v.Ref, v.RV)
		if !found || !slices.Contains(finalizers(previous), f) {
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
