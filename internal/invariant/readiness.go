package invariant

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/observe"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/proxy"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target"
)

// ExpiredWait is the G4 of a settle wait that expired, with why it did. The
// Runner raises it where the wait ends, and the engine at every checkpoint
// after, so the two cannot disagree. Its error is a configuration error.
func (in Input) ExpiredWait(checkpoint Checkpoint) (Violation, error) {
	began, at := checkpoint.Began, checkpoint.Time
	op, _ := in.op(checkpoint.Op)
	walk, err := in.walkReady(began, at, op)
	if err != nil {
		return Violation{}, err
	}
	stable := in.timeouts().Stable
	changes := in.versionsIn(at.Add(-stable), at)
	timeout := fmt.Sprintf("timeouts.settle is %s", in.timeouts().Settle)
	if checkpoint.Stayed || in.heldByDeletion(checkpoint) {
		timeout = fmt.Sprintf("timeouts.delete is %s", in.timeouts().Delete)
	}
	violation := Violation{
		ID: "G4",
		Statement: fmt.Sprintf("the settle wait after %s expired with no fault active: in %s (%s), %s%s%s",
			in.describeOp(checkpoint.Op), at.Sub(began).Round(time.Millisecond), timeout,
			walk.why(began, in.starting(at), churn(stable, changes)),
			in.repeated(began, at), in.exited(at)),
		At: at,
	}.quotingRequests(Recent(requestsUpTo(in.Requests, at))).
		quotingManaged(Sample(in.stateAt(at).managed(in)))
	switch {
	case (walk.held || walk.crs == 0) && len(changes) > 0:
		violation = violation.quotingVersions(Recent(changes))
	case walk.crs > 0:
		violation = violation.quotingVersions(RecentHistory(walk.cr.Key, upTo(in.History.History(walk.cr.Key), at)))
	}
	if walk.crs > 0 {
		violation.Ready = in.readiness(walk.cr, walk.err)
	}
	return violation, nil
}

// readyWalk is what Ready did over a settle wait. Ready holds at an instant
// where a primary CR is live and every live one satisfies it and is not under
// deletion.
type readyWalk struct {
	// crs is how many primary CRs were live at the end.
	crs int
	// cr is the CR the last evaluation failed on, or the first CR if Ready
	// held.
	cr   observe.Version
	held bool
	// ever is whether Ready held after the op's write.
	ever bool
	// turned is when Ready last began or stopped holding.
	turned time.Time
	// err is the last evaluation's.
	err error
}

// walkReady evaluates Ready on the CRs as the wait found them and on every
// version the Observer recorded after, up to the end. The wait can begin
// before the Observer records the op's write, so Ready counts as having held
// after it only once the op's CR has a version recorded after the op.
func (in Input) walkReady(began, end time.Time, op Op) (readyWalk, error) {
	var walk readyWalk
	versions := slices.DeleteFunc(in.versionsIn(time.Time{}, end), func(v observe.Version) bool {
		return v.GVK != in.Target.Primary
	})
	latest := map[observe.Key]observe.Version{}
	written := func() bool {
		v, found := latest[op.CR]
		return op.CR == (observe.Key{}) || found && v.Time.After(op.Time)
	}
	next := 0
	for ; next < len(versions) && !versions[next].Time.After(began); next++ {
		latest[versions[next].Key] = versions[next]
	}
	if err := walk.step(in, began, live(latest), written()); err != nil {
		return walk, err
	}
	for _, v := range versions[next:] {
		latest[v.Key] = v
		if err := walk.step(in, v.Time, live(latest), written()); err != nil {
			return walk, err
		}
	}
	return walk, nil
}

// step evaluates Ready at one instant, stopping at the first CR it fails on,
// as the wait does.
func (w *readyWalk) step(in Input, at time.Time, crs []observe.Version, written bool) error {
	held, err := len(crs) > 0, error(nil)
	if held {
		w.cr = crs[0]
	}
	for _, cr := range crs {
		held, err = in.Target.Ready(cr.Object)
		if errors.Is(err, target.ErrNotBool) {
			return err
		}
		if err != nil || !held || cr.DeletionTimestamp != nil {
			held, w.cr = false, cr
			break
		}
	}
	if held != w.held {
		w.turned = at
	}
	w.crs, w.held, w.err = len(crs), held, err
	w.ever = w.ever || held && written
	return nil
}

// why says what kept the wait from converging: Ready, the target's start, or
// else what changed.
func (w readyWalk) why(began time.Time, starting, churn string) string {
	if starting != "" {
		starting = "but " + starting
	}
	switch {
	case w.crs == 0:
		return "no CR was left to be ready, " + cmp.Or(starting, churn)
	case w.held:
		return fmt.Sprintf("ready held from %s on, %s", w.turned.Sub(began).Round(time.Millisecond), cmp.Or(starting, churn))
	case starting != "":
		return w.unready(began) + ", " + starting
	}
	return w.unready(began)
}

// unready says how Ready failed at the end of a wait.
func (w readyWalk) unready(began time.Time) string {
	switch {
	case w.cr.DeletionTimestamp != nil:
		return fmt.Sprintf("the CR %s was still being deleted, held by the finalizers %s", w.cr.Name, strings.Join(w.cr.Finalizers, ", "))
	case w.ever:
		return fmt.Sprintf("ready held until %s: %s", w.turned.Sub(began).Round(time.Millisecond), w.failure())
	default:
		return "ready never held: " + w.failure()
	}
}

func (w readyWalk) failure() string {
	if w.err != nil {
		return w.err.Error()
	}
	return "it evaluated to false"
}

// starting says how the target was still starting at at, or is empty. A wait
// does not converge on a target waiting to restart or not yet back, and a
// restart and the target's return are changes, which a wait needs
// timeouts.stable past. A target still starting may not yet have acted on the
// run.
func (in Input) starting(at time.Time) string {
	stable := in.timeouts().Stable
	for _, exit := range in.Exits {
		if !exit.At.After(at) && exit.Restart.After(at) {
			return "the target was waiting to restart"
		}
	}
	for _, exit := range in.Exits {
		if exit.Restart.After(at.Add(-stable)) && !exit.Restart.After(at) {
			return fmt.Sprintf("the target restarted in the last %s (timeouts.stable)", stable)
		}
	}
	start, named := in.lastRestart(at)
	if named == "" {
		named = "it started"
	}
	requests := requestsUpTo(in.Requests, at)
	switch back, found := Back(requests, start); {
	case !found:
		return fmt.Sprintf("the target had %s since %s", notBack(requests), named)
	case back.After(at.Add(-stable)):
		return fmt.Sprintf("the target had %s since %s until the last %s (timeouts.stable)", notBack(requests), named, stable)
	}
	return ""
}

// churn says what changed in the last stable, which a wait needs quiet.
func churn(stable time.Duration, changes []observe.Version) string {
	if len(changes) == 0 {
		return fmt.Sprintf("and nothing changed in the last %s (timeouts.stable)", stable)
	}
	last := changes[len(changes)-1]
	return fmt.Sprintf("but the namespace never held still for %s (timeouts.stable): %s in the last %s, the last to %s %s",
		stable, count(len(changes), "change"), stable, observe.KindName(last.GVK), last.Name)
}

// readiness is what a verdict quotes of the predicate on the CR.
func (in Input) readiness(cr observe.Version, err error) *Readiness {
	ready := &Readiness{Expr: in.Target.ReadyExpr, CR: cr.Name}
	if err != nil {
		ready.Error = err.Error()
	}
	ready.Status, _ = cr.Object.Object["status"].(map[string]any)
	return ready
}

// repeated names the failing request the target made most often in [from,
// to], where it made one more than once. An error loop under backoff can be
// too sparse for G6 and still be why the window failed.
func (in Input) repeated(from, to time.Time) string {
	counts := map[requestKey]int{}
	var worst requestKey
	var last proxy.Request
	for _, r := range in.Requests {
		if r.Start.Before(from) || r.Start.After(to) || !failed(r) {
			continue
		}
		key := keyOf(r)
		counts[key]++
		if key == worst || counts[key] > counts[worst] {
			worst, last = key, r
		}
	}
	if counts[worst] < 2 {
		return ""
	}
	return fmt.Sprintf("; the target repeated the failing request %s %d times, the last answered %d", worst, counts[worst], last.Status)
}

// exited counts the exits since the target last converged by at, and quotes
// the last. A target that keeps exiting never converges.
func (in Input) exited(at time.Time) string {
	since := in.lastConverged(at)
	exits := slices.DeleteFunc(slices.Clone(in.Exits), func(exit Exit) bool {
		return exit.At.Before(since) || exit.At.After(at)
	})
	if len(exits) == 0 {
		return ""
	}
	return fmt.Sprintf("; the target exited %s since it last converged, last with %s",
		count(len(exits), "time"), exits[len(exits)-1].Why)
}

func requestsUpTo(requests []proxy.Request, t time.Time) []proxy.Request {
	var before []proxy.Request
	for _, r := range requests {
		if !r.Start.After(t) {
			before = append(before, r)
		}
	}
	return before
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
