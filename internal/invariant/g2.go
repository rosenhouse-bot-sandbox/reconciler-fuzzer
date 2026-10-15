package invariant

import (
	"fmt"
	"slices"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// NoChurn is G2: once converged under a stable spec, the target changes
// neither the primary CR nor the set of managed objects and their
// resourceVersions for timeouts.stable. A status write whose content is
// unchanged moves no resourceVersion, so the proxy log supplies that half,
// which thresholds.quiet bounds.
func NoChurn(in Input) (Result, error) {
	out := Result{ID: "G2"}
	allowed := in.quietAllowance()
	for _, window := range in.quietWindows() {
		if moved := in.changesIn(window); len(moved) > 0 {
			out.violate(Violation{
				Statement: fmt.Sprintf("the target changed %s in %s, where a converged target changes nothing",
					count(len(moved), "object"), window),
				At: moved[0].Time,
			}.quotingVersions(Recent(moved)))
		}
		if written := in.requestsIn(window, writesStatus); len(written) > allowed {
			out.violate(Violation{
				Statement: fmt.Sprintf("the target made %s in %s, where thresholds.quiet allows %d",
					count(len(written), "status write"), window, allowed),
				At: written[allowed].Start,
			}.quotingRequests(Recent(written)))
		}
	}
	return out, nil
}

// changesIn returns the versions of the primary CR and of managed objects the
// Observer recorded inside the window that a write of the target's explains.
func (in Input) changesIn(window quiet) []observe.Version {
	written := in.requestsIn(window, func(r proxy.Request) bool { return writes(r.Verb) })
	var moved []observe.Version
	for _, v := range in.versions() {
		if v.Time.Before(window.start) || v.Time.After(window.end) {
			continue
		}
		if in.attributes(v) && in.explains(written, v) {
			moved = append(moved, v)
		}
	}
	return moved
}

// explains reports whether one of the writes could have made the version: a
// write to its object, or to its collection, which names no object, that
// reached the proxy no later than the version reached the Observer. A write
// counts whatever its answer, which the Observer can see first.
func (in Input) explains(written []proxy.Request, v observe.Version) bool {
	resource := in.History.Resource(v.GVK)
	return slices.ContainsFunc(written, func(r proxy.Request) bool {
		return !r.Start.After(v.Time) && r.Group == resource.Group && r.Resource == resource.Resource &&
			r.Namespace == v.Namespace && (r.Name == "" || r.Name == v.Name)
	})
}

// attributes reports whether the version is the target's work: the primary CR
// or a managed object.
func (in Input) attributes(v observe.Version) bool {
	return v.GVK == in.Target.Primary || in.History.IsManaged(v.Key)
}

func writesStatus(r proxy.Request) bool { return r.Subresource == "status" && writes(r.Verb) }

func writes(verb string) bool {
	switch verb {
	case "create", "update", "patch", "delete", "deletecollection":
		return true
	}
	return false
}
