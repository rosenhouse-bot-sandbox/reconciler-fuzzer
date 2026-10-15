package invariant

import (
	"fmt"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// BoundedReconciliation is G1: once the settle wait has ended, the target
// makes no more API requests in timeouts.stable than thresholds.quiet allows.
func BoundedReconciliation(in Input) (Result, error) {
	out := Result{ID: "G1"}
	allowed := in.quietAllowance()
	for _, window := range in.quietWindows() {
		noisy := in.requestsIn(window, reconciles)
		if len(noisy) <= allowed {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the target made %s in %s, where thresholds.quiet allows %d%s",
				count(len(noisy), "API request"), window, allowed, in.repeated(window.start, window.end)),
			At: noisy[allowed].Start,
		}.quotingRequests(Recent(noisy)))
	}
	return out, nil
}

// requestsIn returns the requests keep accepts that the target started inside
// the window.
func (in Input) requestsIn(window quiet, keep func(proxy.Request) bool) []proxy.Request {
	var inside []proxy.Request
	for _, r := range in.Requests {
		if r.Start.Before(window.start) || r.Start.After(window.end) || !keep(r) {
			continue
		}
		inside = append(inside, r)
	}
	return inside
}

// reconciles reports whether a request counts towards the rate G1 bounds. A
// watch is the target waiting, leader election is it holding or awaiting
// leadership, and a request that names no resource is a health probe or a
// discovery read.
func reconciles(r proxy.Request) bool {
	if r.Watch || r.Verb == "watch" || r.Resource == "" {
		return false
	}
	return !leaderElection(r)
}

// leaderElection reports whether a request is to the group of leases and lease
// candidates, which a target reads and writes however quiet it is.
func leaderElection(r proxy.Request) bool { return r.Group == "coordination.k8s.io" }
