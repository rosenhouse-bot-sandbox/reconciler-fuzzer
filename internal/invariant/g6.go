package invariant

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// NoErrorLoop is G6: the target does not make the same failing request (verb,
// resource, namespace, name and subresource) more than thresholds.errloop times
// within timeouts.settle under an unchanged spec and fixtures, with no faults.
func NoErrorLoop(in Input) (Result, error) {
	threshold := in.errLoop()
	out := Result{ID: "G6"}
	for _, repeated := range in.repeatedFailures() {
		burst := densest(repeated.requests, in.timeouts().Settle)
		if len(burst) <= threshold {
			continue
		}
		out.violate(Violation{
			Statement: fmt.Sprintf("the target repeated the failing request %s %d times within %s (timeouts.settle), where thresholds.errloop allows %d",
				repeated.key, len(burst), in.timeouts().Settle, threshold),
			At: burst[0].Start,
		}.quotingRequests(Recent(burst)))
	}
	return out, nil
}

// failure is the failing requests of one verb, resource, namespace, name and
// subresource in one stretch of unchanged spec and fixtures.
type failure struct {
	key      requestKey
	requests []proxy.Request
}

type requestKey struct{ verb, group, resource, namespace, name, subresource string }

func (k requestKey) String() string {
	resource := k.resource
	if k.group != "" {
		resource = k.group + "/" + resource
	}
	path := fmt.Sprintf("%s %s/%s", k.verb, resource, k.name)
	if k.subresource != "" {
		path += "/" + k.subresource
	}
	return path
}

// repeatedFailures groups the failing requests by what the target asked for,
// within each stretch of unchanged spec and fixtures, in a stable order.
func (in Input) repeatedFailures() []failure {
	grouped := map[time.Time]map[requestKey][]proxy.Request{}
	for _, r := range in.Requests {
		if !failed(r) || in.faulted(r.Start, r.Start) {
			continue
		}
		key := keyOf(r)
		since := in.specSetAt(r.Start)
		if grouped[since] == nil {
			grouped[since] = map[requestKey][]proxy.Request{}
		}
		grouped[since][key] = append(grouped[since][key], r)
	}
	var failures []failure
	for _, keyed := range grouped {
		for key, requests := range keyed {
			slices.SortStableFunc(requests, func(a, b proxy.Request) int { return a.Start.Compare(b.Start) })
			failures = append(failures, failure{key: key, requests: requests})
		}
	}
	slices.SortFunc(failures, func(a, b failure) int {
		return cmp.Or(
			a.requests[0].Start.Compare(b.requests[0].Start),
			strings.Compare(a.key.String(), b.key.String()),
			strings.Compare(a.key.namespace, b.key.namespace),
		)
	})
	return failures
}

func keyOf(r proxy.Request) requestKey {
	return requestKey{verb: r.Verb, group: r.Group, resource: r.Resource, namespace: r.Namespace, name: r.Name, subresource: r.Subresource}
}

// failed reports whether the API server refused the target, rather than the
// proxy refusing it or the target losing a race.
func failed(r proxy.Request) bool {
	return r.Status >= 400 && !conflicted(r) && r.Fault == ""
}

// conflicted reports whether the request lost an optimistic-concurrency race,
// which tells the target to re-read and write again rather than to stop. Only
// an update and a patch lose that race: a 409 on a create is AlreadyExists,
// and repeating it is a loop.
func conflicted(r proxy.Request) bool {
	return r.Status == http.StatusConflict && (r.Verb == "update" || r.Verb == "patch")
}

// specSetAt is when the run last gave the target a new spec or fixture, which
// opens the stretch G6 counts within. Teardown closes the last one.
func (in Input) specSetAt(t time.Time) time.Time {
	var since time.Time
	for _, op := range in.Ops {
		if (op.Type.touchesCR() || op.Type.onFixture() || op.Restored) && !op.Time.After(t) {
			since = op.Time
		}
	}
	if in.tornDown(t) && in.Teardown.After(since) {
		since = in.Teardown
	}
	return since
}

// densest returns the most repetitions of one request that fall within the
// window, which are in the order the proxy logged them.
func densest(requests []proxy.Request, window time.Duration) []proxy.Request {
	var burst []proxy.Request
	first := 0
	for last := range requests {
		for requests[last].Start.Sub(requests[first].Start) > window {
			first++
		}
		if last-first+1 > len(burst) {
			burst = requests[first : last+1]
		}
	}
	return burst
}
