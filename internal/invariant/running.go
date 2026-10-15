package invariant

import (
	"slices"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
)

// Back is when the target first made a request after since that shows it
// running, and whether it has. reconciler-fuzzer has no other sign that a
// process it started is running. A process starting up can request leader
// election and paths that name no resource, such as discovery. A process that
// elects a leader can start its informers before it leads, so only a lease it
// won shows it running. It gets the lease before it wins it, while the leader
// it replaces renews without a get until a renewal fails.
func Back(requests []proxy.Request, since time.Time) (time.Time, bool) {
	if !electing(requests) {
		return first(requests, since, func(r proxy.Request) bool { return r.Resource != "" && !leaderElection(r) })
	}
	if got, found := first(requests, since, gotLease); found {
		return first(requests, got, won)
	}
	return time.Time{}, false
}

// first is when the first request after since that matches started, and
// whether one did.
func first(requests []proxy.Request, since time.Time, matches func(proxy.Request) bool) (time.Time, bool) {
	var earliest time.Time
	for _, r := range requests {
		if r.Start.After(since) && matches(r) && (earliest.IsZero() || r.Start.Before(earliest)) {
			earliest = r.Start
		}
	}
	return earliest, !earliest.IsZero()
}

// notBack says what a target that is not back had not done.
func notBack(requests []proxy.Request) string {
	if electing(requests) {
		return "won no lease"
	}
	return "requested no resource outside leader election"
}

// electing reports whether the target read a lease with a get, as leader
// election does to learn who holds it. An informer lists and watches Leases
// instead. A process whose caches have not synced has yet to elect, but an
// earlier one shows it will.
func electing(requests []proxy.Request) bool { return slices.ContainsFunc(requests, gotLease) }

func gotLease(r proxy.Request) bool { return isLease(r) && r.Verb == "get" }

// won reports whether a request won the target a lease: the API server
// accepted its create, update or patch of one.
func won(r proxy.Request) bool {
	return isLease(r) && slices.Contains([]string{"create", "update", "patch"}, r.Verb) && r.Status/100 == 2
}

func isLease(r proxy.Request) bool { return leaderElection(r) && r.Resource == "leases" }

// settledBy is when a target reconciler-fuzzer restarted at restart must have
// converged: timeouts.settle past its return where it returned within
// timeouts.settle, or else timeouts.settle past the restart.
func (in Input) settledBy(restart time.Time) time.Time {
	settle := in.timeouts().Settle
	if back, found := Back(in.Requests, restart); found && back.Before(restart.Add(settle)) {
		return back.Add(settle)
	}
	return restart.Add(settle)
}

// restartOwed is settledBy of the last Restart op before t, or zero.
func (in Input) restartOwed(t time.Time) time.Time {
	var owed time.Time
	for _, op := range in.Ops {
		if op.Type == OpRestart && op.Time.Before(t) {
			owed = in.settledBy(op.Time)
		}
	}
	return owed
}

// lastRestart names the target's last start before t, by a Restart op or by
// the supervisor after an exit, or is empty where it has not restarted.
func (in Input) lastRestart(t time.Time) (time.Time, string) {
	var start time.Time
	var named string
	for _, op := range in.Ops {
		if op.Type == OpRestart && op.Time.Before(t) {
			start, named = op.Time, describe(op)
		}
	}
	for _, exit := range in.Exits {
		if exit.Restart.Before(t) && exit.Restart.After(start) {
			start, named = exit.Restart, "the restart after its exit during "+describe(in.opBy(exit.At))
		}
	}
	return start, named
}
