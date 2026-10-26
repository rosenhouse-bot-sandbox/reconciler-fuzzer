package check_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/check"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/config"
	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace"
	tt "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/trace/tracetest"
)

var (
	secrets = trace.Kind{Version: "v1", Kind: "Secret", Resource: "secrets"}
	pw      = tt.Ref("secrets", "ns", "pw")
)

// secretRef is a Database's reference to the Secret its
// spec.passwordSecretRef.name names, which is ready once it has data.
func secretRef() trace.Reference {
	return trace.Reference{From: databases, To: secrets, Namespaced: true, Name: "spec.passwordSecretRef.name", Ready: "has(self.data)", Required: true}
}

// database is slot b's Database, naming the Secret secret.
func database(ready bool, secret string) map[string]any {
	body := tt.CR("b", ready)
	body["spec"] = map[string]any{"passwordSecretRef": map[string]any{"name": secret}}
	return body
}

func secret(ready bool) map[string]any {
	body := tt.Object("pw")
	if ready {
		body["data"] = map[string]any{"password": "eA=="}
	}
	return body
}

// secretRun starts step 0 of a run of Databases with ref, in which the
// harness creates the Secret pw with body, unless body is nil, and then
// slot b's Database, not ready, naming pw.
func secretRun(ref trace.Reference, body map[string]any) *tt.Script {
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{ref}
	r.Begin()
	if body != nil {
		r.Create(trace.Harness, pw, body)
	}
	r.Create(trace.Harness, databaseB, database(false, "pw"))
	return r.Act("create", "b", &databaseB)
}

func readyB(r *tt.Script) *tt.Script { return r.Update(ctl, databaseB, database(true, "pw")) }

func TestHonestPassesACRReadyAfterSeeingWhatItNamesReady(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(r.See(ctl, pw))
	requireNone(t, honest(t, r))
}

func TestHonestFailsACRReadyBeforeSeeingWhatItNamesReady(t *testing.T) {
	for name, c := range map[string]struct {
		secret map[string]any
		look   func(r *tt.Script)
		detail string
	}{
		"not seen":       {secret(true), func(*tt.Script) {}, "slot b: secrets ns/pw not seen"},
		"seen not ready": {secret(false), func(r *tt.Script) { r.See(ctl, pw) }, "slot b: secrets ns/pw seen not ready"},
		"seen ready, then not ready": {secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(false)).See(ctl, pw)
		}, "slot b: secrets ns/pw seen not ready"},
		"read opaquely, then seen not ready": {secret(false), func(r *tt.Script) {
			r.Add(opaque(tt.Ref("secrets", "ns", ""), 0)).See(ctl, pw)
		}, "slot b: secrets ns/pw seen not ready"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), c.secret)
			c.look(r)
			readyB(r)
			requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", c.detail)
		})
	}
}

func TestHonestFailsACRReadyWhileWhatItRequiresIsMissing(t *testing.T) {
	for name, c := range map[string]struct {
		secret map[string]any
		look   func(r *tt.Script)
		detail string
	}{
		"never created":     {nil, func(*tt.Script) {}, "slot b: secrets ns/pw not seen"},
		"a 404":             {nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, "slot b: secrets ns/pw seen missing"},
		"a list without it": {nil, func(r *tt.Script) { r.Call(ctl, "list", tt.Ref("secrets", "ns", ""), 200) }, "slot b: secrets ns/pw seen missing"},
		"seen deleted": {secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Delete(trace.Harness, pw).SeeDeleted(ctl, pw)
		}, "slot b: secrets ns/pw seen missing"},
		"seen missing, then created unseen": {nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(true))
		}, "slot b: secrets ns/pw seen missing"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), c.secret)
			c.look(r)
			readyB(r)
			requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", c.detail)
		})
	}
}

func TestHonestPassesACRReadyWhileWhatItNamesWithoutRequiredIsMissing(t *testing.T) {
	optional := secretRef()
	optional.Required = false
	for name, look := range map[string]func(r *tt.Script){
		"never seen": func(*tt.Script) {},
		"a 404":      func(r *tt.Script) { r.Call(ctl, "get", pw, 404) },
	} {
		r := secretRun(optional, nil)
		look(r)
		readyB(r)
		if findings := honest(t, r); len(findings) != 0 {
			t.Errorf("%s: findings = %+v, want none.", name, findings)
		}
	}
	r := secretRun(optional, secret(true))
	readyB(r)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/pw not seen")
}

// A reconcile that read the controller's cache before a watch event came
// may write ready just after the listener forwarded the event. The CR's
// next reconcile starts only once that write returns.
func TestHonestPassesAReadyWriteThatTheControllerCorrectsWithinStable(t *testing.T) {
	for name, c := range map[string]struct {
		writer       trace.Caller
		readyLatency time.Duration
		after        time.Duration
		correction   map[string]any
		fails        bool
	}{
		"not ready":                             {ctl, 0, 0, database(false, "pw"), false},
		"naming nothing":                        {ctl, 0, 0, database(true, ""), false},
		"not ready too late":                    {ctl, 0, 2 * s, database(false, "pw"), true},
		"ready again":                           {ctl, 0, 0, database(true, "pw"), true},
		"not ready, by another":                 {trace.Harness, 0, 0, database(false, "pw"), true},
		"not ready once a slow write returns":   {ctl, 1 * s, 1 * s, database(false, "pw"), false},
		"not ready too late after a slow write": {ctl, 1 * s, 2 * s, database(false, "pw"), true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			seenThenDeleted(r).UpdateInFlight(ctl, databaseB, database(true, "pw"), c.readyLatency).Tick(c.after).Update(c.writer, databaseB, c.correction)
			requireHonest(t, r, c.fails)
		})
	}
}

// The CR's next reconcile can read the CR from a cache that lacks the
// ready write, so that its write conflicts.
func TestHonestPassesACorrectionThatAFailedWriteOfTheCRDelays(t *testing.T) {
	dropped := slowCall("update", databaseB, 200)
	dropped.Call.Injected = trace.DropResponse
	for name, c := range map[string]struct {
		call trace.Event
		// sent is how long after the ready write the call is sent. after
		// is how long after the call's response the correction is sent.
		sent, after time.Duration
		fails       bool
	}{
		"a conflict":                         {slowCall("update", databaseB, 409), 0, 0, false},
		"a server error":                     {slowCall("update", databaseB, 500), 0, 0, false},
		"a conflicting patch":                {slowCall("patch", databaseB, 409), 0, 0, false},
		"an unknown outcome":                 {dropped, 0, 0, false},
		"a success":                          {slowCall("update", databaseB, 200), 0, 0, true},
		"a conflict sent stable after":       {slowCall("update", databaseB, 409), 1 * s, 0, false},
		"a conflict sent later":              {slowCall("update", databaseB, 409), 1*s + tt.Tick, 0, true},
		"a conflict, then a late correction": {slowCall("update", databaseB, 409), 0, 1100 * ms, true},
		"a failed get":                       {slowCall("get", databaseB, 500), 0, 0, true},
		"a conflict of another object":       {slowCall("update", databaseC, 409), 0, 0, true},
		"a conflict of another writer":       {byAnother(slowCall("update", databaseB, 409)), 0, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			readyB(seenThenDeleted(r)).Tick(c.sent+1*s-tt.Tick).Add(c.call).Tick(c.after).Update(ctl, databaseB, database(false, "pw"))
			requireHonest(t, r, c.fails)
		})
	}
}

// seenThenDeleted has the controller see pw and then its deletion.
func seenThenDeleted(r *tt.Script) *tt.Script {
	deletedSeen(r.See(ctl, pw))
	return r
}

// byAnother is the call e, made by the harness.
func byAnother(e trace.Event) trace.Event {
	e.Call.Caller = trace.Harness
	return e
}

// slowCall is the controller's call on ref that changes nothing and returns
// 1s after it is sent.
func slowCall(verb string, ref trace.Ref, status int) trace.Event {
	e := tt.Call(ctl, verb, ref, status, "")
	e.Call.Latency = 1 * s
	return e
}

func TestHonestPassesACorrectionThatFailedWritesInARowDelay(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(seenThenDeleted(r))
	for range 2 {
		r.Tick(1 * s).Add(slowCall("update", databaseB, 409))
	}
	r.Tick(900*ms).Update(ctl, databaseB, database(false, "pw"))
	requireNone(t, honest(t, r))
}

func TestHonestPassesACorrectionSentInTimeThatReturnsLate(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(seenThenDeleted(r)).Tick(900*ms).UpdateInFlight(ctl, databaseB, database(false, "pw"), 1*s)
	requireNone(t, honest(t, r))
}

// injected is a controller call on ref with status that a fault altered by
// effect.
func injected(ref trace.Ref, status int, effect string) trace.Event {
	e := tt.Call(ctl, "update", ref, status, "")
	e.Call.Injected = effect
	return e
}

// requireHonest fails the test unless Honest finds that slot b reported
// ready while pw was missing exactly when fails is set.
func requireHonest(t *testing.T, r *tt.Script, fails bool) {
	t.Helper()
	if findings := honest(t, r); fails {
		requireFinding(t, findings, "Honest", 0, noDependency, "kube/secrets", "slot b: secrets ns/pw seen missing")
	} else {
		requireNone(t, findings)
	}
}

// An exited controller corrects nothing until it starts again, and then it
// has stable from its start.
func TestHonestCountsACorrectionFromTheControllersRestart(t *testing.T) {
	for name, c := range map[string]struct {
		// exit is how long after the ready write's response the controller
		// exits. after is how long after its start the correction is sent.
		exit, after time.Duration
		fails       bool
	}{
		"stable after the start":          {500 * ms, 1 * s, false},
		"later":                           {500 * ms, 1*s + tt.Tick, true},
		"an exit as stable runs out":      {1 * s, 1 * s, false},
		"an exit once stable has run out": {1*s + tt.Tick, tt.Tick, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			readyB(seenThenDeleted(r))
			ready := r.Now - tt.Tick
			r.Tick(ready + c.exit - r.Now).Add(tt.Exit("signal: killed", true, "")).Tick(2 * s)
			start := r.Now
			r.Add(tt.Start(2))
			endFaults(r, 3*s)
			requireHonest(t, r.Tick(start+c.after-r.Now).Update(ctl, databaseB, database(false, "pw")), c.fails)
		})
	}
}

// A restart loses the reconcile that could have read a stale cache, so a
// look before it excuses no later ready write.
func TestHonestFailsAReadyWriteAfterARestartSinceTheLook(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	seenThenDeleted(r).Add(tt.Exit("signal: killed", true, "")).Tick(500 * ms).Add(tt.Start(2))
	readyB(r).Tick(500*ms).Update(ctl, databaseB, database(false, "pw"))
	requireHonest(t, r, true)
}

func TestHonestGivesNoTimeForAnEarlierRestart(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	r.Add(tt.Exit("signal: killed", true, "")).Tick(2 * s).Add(tt.Start(2))
	readyB(seenThenDeleted(r))
	requireHonest(t, r.Tick(r.Now+1*s-tt.Tick-r.Now).Update(ctl, databaseB, database(false, "pw")), false)
}

// A restart within a step's faults holds the controller no longer than they
// do.
func TestHonestCountsOverlappingHoldsOnce(t *testing.T) {
	for name, late := range map[string]time.Duration{"stable after the faults end": 0, "later": tt.Tick} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			r.Add(tt.Fault(r.Step, 0, false, "error(500) on resource=databases"))
			readyB(seenThenDeleted(r))
			r.Tick(500 * ms).Add(tt.Exit("signal: terminated", true, "")).Tick(1 * s).Add(tt.Start(2))
			end := r.Tick(1 * s).Now
			r.Add(tt.Fault(r.Step, 0, true, "error(500) on resource=databases"))
			endFaults(r, 3*s)
			requireHonest(t, r.Tick(end+1*s+late-r.Now).Update(ctl, databaseB, database(false, "pw")), late > 0)
		})
	}
}

// Supervision is off during a step's faults, so a controller that exits
// before them starts again only once they end.
func TestHonestCountsAnExitThatOutlastsItsStep(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(seenThenDeleted(r))
	ready := r.Now - tt.Tick
	r.Tick(ready + 500*ms - r.Now).Add(tt.Exit("exit status 2", false, "panic: nil map"))
	timeOut := ready + 2*s
	r.Add(trace.Event{Phase: &trace.Phase{Step: r.Step, Name: trace.EndFaults, Deadline: timeOut}})
	r.Tick(timeOut - r.Now).Add(tt.Phase(r.Step, trace.TimedOut)).Begin().
		Add(tt.Fault(r.Step, 0, false, "error(500) on resource=databases"))
	end := r.Tick(2 * s).Now
	r.Add(tt.Fault(r.Step, 0, true, "error(500) on resource=databases"), tt.Start(2))
	endFaults(r, 3*s)
	requireHonest(t, r.Tick(end+1*s-r.Now).Update(ctl, databaseB, database(false, "pw")), false)
}

func TestHonestCountsARestartThatFollowsAFailedWriteOfTheCR(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(seenThenDeleted(r)).Tick(900*ms).Call(ctl, "update", databaseB, 409).Tick(500 * ms).Add(tt.Exit("signal: killed", true, "")).Tick(2 * s)
	start := r.Now
	requireHonest(t, r.Add(tt.Start(2)).Tick(start+1*s-r.Now).Update(ctl, databaseB, database(false, "pw")), false)
}

// A write of the CR that failed is retried after a backoff, which the step's
// allowance bounds.
func TestHonestGivesTheStepsBackoffAfterAFailedWriteOfTheCR(t *testing.T) {
	for name, late := range map[string]time.Duration{"the backoff and stable after the failure": 0, "later": tt.Tick} {
		t.Run(name, func(t *testing.T) {
			const backoff = 3 * s
			r := secretRun(secretRef(), secret(true))
			r.Add(tt.Fault(r.Step, 0, false, "error(500) on resource=databases"))
			readyB(seenThenDeleted(r)).Tick(500*ms).Call(ctl, "update", databaseB, 409)
			end := r.Tick(1 * s).Now
			r.Add(tt.Fault(r.Step, 0, true, "error(500) on resource=databases"))
			endFaults(r, backoff)
			requireHonest(t, r.Tick(end+backoff+r.Spec.Stable+late-r.Now).Update(ctl, databaseB, database(false, "pw")), late > 0)
		})
	}
}

// A step can time out just after the ready write. The next step's faults
// then hold the controller until they end.
func TestHonestCountsACorrectionFromTheEndOfTheNextStepsFaults(t *testing.T) {
	for name, c := range map[string]struct {
		// begins is how long after the ready write's response the next step
		// begins. late is how long after the time the faults leave of stable
		// the correction is sent.
		begins, late time.Duration
		fails        bool
	}{
		"what stable leaves once they end":      {500 * ms, 0, false},
		"later":                                 {500 * ms, tt.Tick, true},
		"faults that start once it has run out": {1*s + tt.Tick, 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			readyB(seenThenDeleted(r))
			ready := r.Now - tt.Tick
			timeOut := ready + c.begins
			r.Add(trace.Event{Phase: &trace.Phase{Step: r.Step, Name: trace.EndFaults, Deadline: timeOut}})
			r.Tick(timeOut - r.Now).Add(tt.Phase(r.Step, trace.TimedOut)).Begin()
			begin := r.Now
			r.Add(tt.Fault(r.Step, 0, false, "error(500) on resource=databases"))
			end := r.Tick(2 * s).Now
			r.Add(tt.Fault(r.Step, 0, true, "error(500) on resource=databases"))
			endFaults(r, 3*s)
			due := end + r.Spec.Stable - (begin - ready)
			requireHonest(t, r.Tick(due+c.late-r.Now).Update(ctl, databaseB, database(false, "pw")), c.fails)
		})
	}
}

// A model fault and a held watch hold no request of the controller.
func TestHonestGivesNoTimeForAFaultThatHoldsNoRequest(t *testing.T) {
	for name, fault := range map[string]func(step int, ended bool) trace.Event{
		"a model's fault": func(step int, ended bool) trace.Event { return tt.ModelFault(step, 0, ended, "stuck", "volumes") },
		"a held watch": func(step int, ended bool) trace.Event {
			return tt.HoldFault(step, 0, ended, tt.Ref("secrets", "ns", ""))
		},
	} {
		for when, late := range map[string]time.Duration{"stable after the ready write": 0, "later": tt.Tick} {
			t.Run(name+", "+when, func(t *testing.T) {
				r := secretRun(secretRef(), secret(true))
				r.Add(fault(r.Step, false))
				readyB(seenThenDeleted(r))
				ready := r.Now - tt.Tick
				r.Tick(500 * ms).Add(fault(r.Step, true))
				endFaults(r, 3*s)
				requireHonest(t, r.Tick(ready+1*s+late-r.Now).Update(ctl, databaseB, database(false, "pw")), late > 0)
			})
		}
	}
}

// A reconcile that read its cache before the look may wait on requests that
// a fault holds until it ends, before it writes ready.
func TestHonestExcusesACorrectedReadyWriteOnlySoonAfterTheLook(t *testing.T) {
	for name, c := range map[string]struct {
		// faultsEnd is how long after the look the step's request faults,
		// which start before it, end, if it has any. sent is how long after
		// the look the ready write is sent.
		faultsEnd, sent time.Duration
		fails           bool
	}{
		"twice stable after the look":       {0, 2 * s, false},
		"later":                             {0, 2*s + tt.Tick, true},
		"twice stable after the faults end": {3 * s, 5 * s, false},
		"later than that":                   {3 * s, 5*s + tt.Tick, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			if c.faultsEnd > 0 {
				r.Add(tt.Fault(r.Step, 0, false, "delay(2s) on resource=databases"))
			}
			look := seenThenDeleted(r).Now - tt.Tick
			if c.faultsEnd > 0 {
				r.Tick(look + c.faultsEnd - r.Now).Add(tt.Fault(r.Step, 0, true, "delay(2s) on resource=databases"))
				endFaults(r, 3*s)
			}
			r.Tick(look+c.sent-r.Now).UpdateInFlight(ctl, databaseB, database(true, "pw"), 500*ms).
				Tick(500*ms).Update(ctl, databaseB, database(false, "pw"))
			requireHonest(t, r, c.fails)
		})
	}
}

// Faults that start after the look hold the controller while they last.
func TestHonestExcusesAReadyWriteSentWhileFaultsAfterTheLookHold(t *testing.T) {
	for name, late := range map[string]time.Duration{"twice stable, the faults aside": 0, "later": tt.Tick} {
		t.Run(name, func(t *testing.T) {
			r := secretRun(secretRef(), secret(true))
			look := seenThenDeleted(r).Now - tt.Tick
			begin := r.Tick(1 * s).Now
			r.Add(tt.Fault(r.Step, 0, false, "delay(2s) on resource=databases"))
			end := r.Tick(2 * s).Now
			r.Add(tt.Fault(r.Step, 0, true, "delay(2s) on resource=databases"))
			endFaults(r, 3*s)
			sent := look + 2*r.Spec.Stable + (end - begin) + late
			r.Tick(sent-r.Now).UpdateInFlight(ctl, databaseB, database(true, "pw"), 500*ms).
				Tick(500*ms).Update(ctl, databaseB, database(false, "pw"))
			requireHonest(t, r, late > 0)
		})
	}
}

// With no look, no stale cache explains the ready write.
func TestHonestFailsACorrectedReadyWriteBeforeAnyLook(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	readyB(r).Update(ctl, databaseB, database(false, "pw"))
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/pw not seen")
}

func TestHonestGivesACorrectionStableAfterTheReadyWriteDespiteAnEarlierFailure(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	seenThenDeleted(r).Call(ctl, "update", databaseB, 409).UpdateInFlight(ctl, databaseB, database(true, "pw"), 500*ms).
		Tick(1200*ms).Update(ctl, databaseB, database(false, "pw"))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesAReadyWriteByWhatTheControllerSawBeforeSendingIt(t *testing.T) {
	r := secretRun(secretRef(), secret(true))
	r.UpdateInFlight(ctl, databaseB, database(true, "pw"), 50*ms).See(ctl, pw)
	requireFinding(t, honest(t, r), "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/pw not seen")
}

func TestHonestPassesACRReadyWhileTheControllerLastReadWhatItNamesOpaquely(t *testing.T) {
	r := secretRun(secretRef(), secret(false))
	readyB(r.See(ctl, pw).Add(opaque(tt.Ref("secrets", "ns", ""), 0)))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesWhatEachValueNames(t *testing.T) {
	r := secretRun(secretRef(), secret(false))
	r.Update(ctl, databaseB, database(true, ""))
	requireNone(t, honest(t, r))

	keys := secretRef()
	keys.Name = "spec.keys[*].name"
	r = secretRun(keys, secret(true))
	r.Create(trace.Harness, tt.Ref("secrets", "ns", "other"), tt.Object("other"))
	body := tt.CR("b", true)
	body["spec"] = map[string]any{"keys": []any{map[string]any{"name": "pw"}, map[string]any{"name": "other"}}}
	r.See(ctl, pw).Update(ctl, databaseB, body)
	findings := honest(t, r)
	requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/secrets", "slot b: secrets ns/other not seen")
	if len(findings[0].Details) != 1 {
		t.Errorf("details = %q, want only the Secret other.", findings[0].Details)
	}

	elsewhere := secretRef()
	elsewhere.Namespace = "spec.passwordSecretRef.namespace"
	r = secretRun(elsewhere, secret(true))
	body = database(true, "pw")
	body["spec"].(map[string]any)["passwordSecretRef"].(map[string]any)["namespace"] = "elsewhere"
	r.See(ctl, pw).Update(ctl, databaseB, body)
	requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", "slot b: secrets elsewhere/pw not seen")
}

func TestHonestJudgesAClusterScopedCRsReferenceByItsNamespaceValue(t *testing.T) {
	tenants := trace.Kind{Group: "toy", Version: "v1", Kind: "Tenant", Resource: "tenants"}
	tenantB := tt.Ref("toy/tenants", "", "b")
	for _, namespace := range []string{"", "ns"} {
		r := fleetRun(1, tenants)
		r.Spec.References = []trace.Reference{{From: tenants, To: secrets, Namespaced: true,
			Name: "spec.secretRef.name", Namespace: "spec.secretRef.namespace", Ready: "true", Required: true}}
		r.Begin()
		tenant := func(ready bool) map[string]any {
			body := tt.CR("b", ready)
			body["spec"] = map[string]any{"secretRef": map[string]any{"name": "pw", "namespace": namespace}}
			return body
		}
		r.Create(trace.Harness, tenantB, tenant(false))
		r.Act("create", "b", &tenantB).Update(ctl, tenantB, tenant(true))
		if namespace == "" {
			requireNone(t, honest(t, r))
		} else {
			requireFinding(t, honest(t, r), "Honest", 0, noDependency, "kube/secrets", "slot b: secrets ns/pw not seen")
		}
	}
}

func TestHonestJudgesAReferenceToAListedKindByItsReady(t *testing.T) {
	for name, c := range map[string]struct {
		look    func(r *tt.Script)
		finding bool
	}{
		"seen not ready":                     {func(r *tt.Script) { r.See(ctl, clusterA) }, true},
		"made ready and seen":                {func(r *tt.Script) { r.Ready(clusterA).See(ctl, clusterA) }, false},
		"seen ready before the next write":   {func(r *tt.Script) { r.Ready(clusterA).See(ctl, clusterA).Update(ctl, clusterA, tt.CR("a", true)) }, false},
		"seen not ready after a ready write": {func(r *tt.Script) { r.See(ctl, clusterA).Ready(clusterA) }, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, clusters, databases)
			r.Spec.References = []trace.Reference{{From: databases, To: clusters, Namespaced: true, Name: "spec.clusterRef", Ready: r.Spec.Primaries[0].Ready, Required: true}}
			r.Begin()
			r.CreateCR("a", clusterA)
			body := tt.CR("b", false)
			body["spec"] = map[string]any{"clusterRef": "a"}
			r.Create(trace.Harness, databaseB, body)
			r.Act("create", "b", &databaseB)
			c.look(r)
			body = tt.CR("b", true)
			body["spec"] = map[string]any{"clusterRef": "a"}
			r.Update(ctl, databaseB, body)
			findings := honest(t, r)
			if !c.finding {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, beforeSeeing, "kube/toy/clusters", "slot b: clusters.toy ns/a seen not ready")
		})
	}
}

func TestHonestJudgesAReferenceOnlyForCRsOfItsFromKind(t *testing.T) {
	r := fleetRun(1, clusters, databases)
	r.Spec.References = []trace.Reference{secretRef()}
	r.Begin()
	cluster := func(ready bool) map[string]any {
		body := tt.CR("a", ready)
		body["spec"] = map[string]any{"passwordSecretRef": map[string]any{"name": "pw"}}
		return body
	}
	r.Create(trace.Harness, clusterA, cluster(false))
	r.Act("create", "a", &clusterA).Update(ctl, clusterA, cluster(true))
	requireNone(t, honest(t, r))
}

func TestHonestJudgesAReferenceToAFakesObject(t *testing.T) {
	for name, c := range map[string]struct {
		state           string
		look            bool
		summary, detail string
	}{
		"seen ready": {state: "READY", look: true},
		"not seen":   {state: "READY", summary: beforeSeeing, detail: "slot b: cloud buckets b1 not seen"},
		"gone":       {state: "DELETING", summary: noDependency, detail: "slot b: cloud buckets b1 not seen"},
		"seen gone":  {state: "DELETING", look: true, summary: noDependency, detail: "slot b: cloud buckets b1 seen missing"},
	} {
		t.Run(name, func(t *testing.T) {
			r := fleetRun(1, databases)
			r.Spec.Systems = []trace.System{{Name: "cloud", API: "crud", Gone: cloudSystem.Gone}}
			r.Spec.References = []trace.Reference{{From: databases, System: "cloud", To: trace.Kind{Resource: "buckets"},
				Name: "spec.bucket", Ready: cloudSystem.Ready, Required: true}}
			r.Begin()
			r.Create(trace.Harness, bucketA, map[string]any{"id": "b1", "state": c.state})
			if c.look {
				r.See(ctl, bucketA)
			}
			database := func(ready bool) map[string]any {
				body := tt.CR("b", ready)
				body["spec"] = map[string]any{"bucket": "b1"}
				return body
			}
			r.Create(trace.Harness, databaseB, database(false))
			r.Act("create", "b", &databaseB).Update(ctl, databaseB, database(true))
			findings := honest(t, r)
			if c.summary == "" {
				requireNone(t, findings)
				return
			}
			requireFinding(t, findings, "Honest", 0, c.summary, "cloud/buckets", c.detail)
		})
	}
}

func TestHonestJudgesAVersionTheObserverMissedByTheReferencesReady(t *testing.T) {
	anyObject := secretRef()
	anyObject.Ready = "true"
	r := secretRun(anyObject, secret(false))
	readyB(r.Add(tt.Seen(ctl, pw, r.UID(pw), "missed")))
	requireNone(t, honest(t, r))
}

// secretGoes has the controller make slot b ready after seeing pw ready,
// and then has change change pw.
func secretGoes(ref trace.Reference, change func(r *tt.Script)) *tt.Script {
	r := secretRun(ref, secret(true))
	readyB(r.See(ctl, pw))
	change(r)
	return r
}

func notReadySeen(r *tt.Script) { r.Update(trace.Harness, pw, secret(false)).See(ctl, pw) }

func deletedSeen(r *tt.Script) { r.Delete(trace.Harness, pw).SeeDeleted(ctl, pw) }

func TestHonestWithTrackFailsACRThatStaysReadyNamingWhatHoldsItBack(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	for name, c := range map[string]struct {
		change func(r *tt.Script)
		detail string
	}{
		"seen not ready": {notReadySeen, "slot b: secrets ns/pw seen not ready at"},
		"seen deleted":   {deletedSeen, "slot b: secrets ns/pw seen missing at"},
		"a 404":          {func(r *tt.Script) { r.Delete(trace.Harness, pw).Call(ctl, "get", pw, 404) }, "slot b: secrets ns/pw seen missing at"},
	} {
		t.Run(name, func(t *testing.T) {
			r := secretGoes(tracked, c.change)
			look := r.Now - tt.Tick
			findings := honest(t, r)
			requireFinding(t, findings, "Honest", 0, stayedReady, "kube/secrets", c.detail)
			if findings[0].At != look+500*ms {
				t.Errorf("at = %v, want %v", findings[0].At, look+500*ms)
			}
		})
	}
	requireNone(t, honest(t, secretGoes(secretRef(), notReadySeen)))
}

func TestHonestWithTrackPassesACRThatReactsInTime(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	optional := tracked
	optional.Required = false
	for name, c := range map[string]struct {
		ref    trace.Reference
		change func(r *tt.Script)
	}{
		"the CR turns not ready in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(ctl, databaseB, database(false, "pw"))
		}},
		"the controller sees it ready in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(trace.Harness, pw, secret(true)).See(ctl, pw)
		}},
		"the CR stops naming it in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Update(trace.Harness, databaseB, database(true, ""))
		}},
		"a not ready write of the CR is in flight in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).UpdateInFlight(ctl, databaseB, database(false, "pw"), 2*s)
		}},
		"a write of the CR has an unknown outcome in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400 * ms).Add(injected(databaseB, 200, trace.DropResponse))
		}},
		"the controller restarts in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400 * ms).Restart()
		}},
		"an opaque read follows": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Add(opaque(tt.Ref("secrets", "ns", ""), 0))
		}},
		"what is deleted is not required": {optional, deletedSeen},
		"the CR is deleted in time": {tracked, func(r *tt.Script) {
			notReadySeen(r)
			r.Tick(400*ms).Delete(trace.Harness, databaseB)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			requireNone(t, honest(t, secretGoes(c.ref, c.change)))
		})
	}
}

func TestHonestWithTrackFailsOnceAtTheFirstLookThatHeldTheCRBack(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	var first time.Duration
	r := secretGoes(tracked, func(r *tt.Script) {
		notReadySeen(r)
		first = r.Now - tt.Tick
		r.Tick(100*ms).See(ctl, pw).Tick(500*ms).Update(trace.Harness, pw, secret(true)).See(ctl, pw)
	})
	findings := honest(t, r)
	requireFinding(t, findings, "Honest", 0, stayedReady, "kube/secrets", fmt.Sprintf("slot b: secrets ns/pw seen not ready at %v", first))
	if f := findings[0]; len(f.Details) != 1 || f.At != first+500*ms {
		t.Errorf("finding = %+v, want one detail, at %v.", f, first+500*ms)
	}
}

func TestHonestWithTrackFailsACRThatReactsTooLate(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	r := secretGoes(tracked, func(r *tt.Script) {
		notReadySeen(r)
		r.Tick(600*ms).Update(ctl, databaseB, database(false, "pw"))
	})
	requireFinding(t, honest(t, r), "Honest", 0, stayedReady, "kube/secrets", "slot b: secrets ns/pw seen not ready at")
}

func TestHonestWithTrackJudgesOnlyLooksWhileTheCRLives(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 500 * ms
	r := fleetRun(1, databases)
	r.Spec.References = []trace.Reference{tracked}
	r.Begin()
	r.Create(trace.Harness, pw, secret(false))
	r.See(ctl, pw).Create(trace.Harness, databaseB, database(true, "pw"))
	r.Act("create", "b", &databaseB)
	requireNone(t, honest(t, r))
}

func TestHonestWithTrackJudgesNoLookWhoseTrackOutlastsTheTrace(t *testing.T) {
	tracked := secretRef()
	tracked.Track = 5 * s
	requireNone(t, honest(t, secretGoes(tracked, notReadySeen)))
}

func TestSettleExcusesACRNotReadyWhileAReferenceHoldsItBack(t *testing.T) {
	optional := secretRef()
	optional.Required = false
	for name, c := range map[string]struct {
		ref     trace.Reference
		secret  map[string]any
		look    func(r *tt.Script)
		excused bool
	}{
		"missing and never seen": {secretRef(), nil, func(*tt.Script) {}, true},
		"seen missing":           {secretRef(), nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, true},
		"seen not ready":         {secretRef(), secret(false), func(r *tt.Script) { r.See(ctl, pw) }, true},
		"seen missing, then created not ready": {secretRef(), nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(false))
		}, true},
		"seen not ready, then changed": {secretRef(), secret(false), func(r *tt.Script) {
			changed := secret(false)
			changed["type"] = "Opaque"
			r.See(ctl, pw).Update(trace.Harness, pw, changed)
		}, true},
		"missing until after t": {secretRef(), nil, func(r *tt.Script) {
			r.At(r.Now+time.Minute, tt.Version(pw, "u-pw", "99", secret(true)))
		}, true},
		"seen missing, then created ready": {secretRef(), nil, func(r *tt.Script) {
			r.Call(ctl, "get", pw, 404).Create(trace.Harness, pw, secret(true))
		}, false},
		"seen not ready, then made ready": {secretRef(), secret(false), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(true))
		}, false},
		"read opaquely": {secretRef(), secret(true), func(r *tt.Script) { r.Add(opaque(tt.Ref("secrets", "ns", ""), 0)) }, true},
		"seen ready":    {secretRef(), secret(true), func(r *tt.Script) { r.See(ctl, pw) }, false},
		"seen deleted":  {secretRef(), secret(true), func(r *tt.Script) { deletedSeen(r.See(ctl, pw)) }, true},
		"seen ready, then made not ready": {secretRef(), secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(false))
		}, false},
		"live and never seen":           {secretRef(), secret(true), func(*tt.Script) {}, false},
		"not ready and never seen":      {secretRef(), secret(false), func(*tt.Script) {}, false},
		"missing without required":      {optional, nil, func(*tt.Script) {}, false},
		"seen missing without required": {optional, nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, false},
	} {
		r := secretRun(c.ref, c.secret)
		c.look(r)
		r.EndFaults()
		unmet := settledAt(t, r, r.Now+5*s)
		if waits := waitsForB(unmet); waits == c.excused {
			t.Errorf("%s: unmet = %q, want the Database excused: %v.", name, unmet, c.excused)
		}
	}
}

// A controller reacts to a held-back CR's create within stable, after a
// backoff that the faults may have started, so the step must not settle
// before then.
func TestSettleExcusesAHeldBackCROnlyStableAfterTheBackoff(t *testing.T) {
	r := secretRun(secretRef(), nil).Tick(5 * s)
	if !waitsForB(settledAt(t, r, r.Now)) {
		t.Error("The Database is excused before the faults end.")
	}
	excuse := endFaults(r, 4*s)
	for at, excused := range map[time.Duration]bool{excuse - time.Millisecond: false, excuse: true} {
		if waits := waitsForB(settledAt(t, r, at)); waits == excused {
			t.Errorf("At %v, the Database is excused: %v, want %v.", at, !waits, excused)
		}
	}
}

func TestFirstSettledIsWhenTheHeldBackCRsExcuseStarts(t *testing.T) {
	r := secretRun(secretRef(), nil).Call(ctl, "get", databaseB, 200)
	from := r.Now
	excuse := endFaults(r, 4*s)
	ix := r.Index()
	spec, err := check.SpecFor(nil, ix)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := check.FirstSettled(ix, from, excuse+time.Second, spec); !ok || got != excuse {
		t.Errorf("FirstSettled = %v, %v; want %v.", got, ok, excuse)
	}
}

// endFaults ends r's step's faults now with backoff, and returns when the
// step's settle excuse starts.
func endFaults(r *tt.Script, backoff time.Duration) time.Duration {
	end := r.Now
	r.Add(trace.Event{Phase: &trace.Phase{Step: r.Step, Name: trace.EndFaults, Deadline: end + 30*s, Backoff: backoff}})
	return end + backoff + r.Spec.Stable
}

// waitsForB reports whether unmet holds that Database b is not ready.
func waitsForB(unmet []string) bool {
	return slices.ContainsFunc(unmet, func(u string) bool { return strings.HasPrefix(u, "databases.toy ns/b is not ready") })
}

func TestClause1SaysWhatTheControllerLastSawOfWhatTheCRNames(t *testing.T) {
	optional := secretRef()
	optional.Required = false
	for name, c := range map[string]struct {
		ref    trace.Reference
		secret map[string]any
		look   func(r *tt.Script)
		// shown is what the look showed, or empty for no look.
		shown string
	}{
		"seen ready": {secretRef(), secret(true), func(r *tt.Script) { r.See(ctl, pw) }, "ready"},
		"seen ready, then made not ready": {secretRef(), secret(true), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(false))
		}, "ready"},
		"seen missing without required": {optional, nil, func(r *tt.Script) { r.Call(ctl, "get", pw, 404) }, "missing"},
		"seen not ready, then made ready": {secretRef(), secret(false), func(r *tt.Script) {
			r.See(ctl, pw).Update(trace.Harness, pw, secret(true))
		}, "not ready"},
		"never seen": {secretRef(), secret(true), func(*tt.Script) {}, ""},
	} {
		r := secretRun(c.ref, c.secret)
		look := r.Now
		c.look(r)
		r.EndFaults()
		want := "databases.toy ns/b is not ready; the controller never saw secrets ns/pw"
		if c.shown != "" {
			want = fmt.Sprintf("databases.toy ns/b is not ready; the controller last saw secrets ns/pw %s at %v", c.shown, look)
		}
		if unmet := settledAt(t, r, r.Now+5*s); !slices.Contains(unmet, want) {
			t.Errorf("%s: unmet = %q, want %q.", name, unmet, want)
		}
	}
}

func TestSettleStillWaitsForACRNoReferenceHoldsBack(t *testing.T) {
	r := secretRun(secretRef(), nil)
	namesNothing := database(false, "")
	namesNothing["metadata"] = map[string]any{"name": "c"}
	r.Create(trace.Harness, databaseC, namesNothing)
	r.Act("create", "c", &databaseC).Call(ctl, "get", databaseC, 200).EndFaults()
	if unmet := settledAt(t, r, r.Now+5*s); !slices.Equal(unmet, []string{"databases.toy ns/c is not ready"}) {
		t.Errorf("unmet = %q, want only that Database c, which names nothing, is not ready.", unmet)
	}
}

func TestClause5HoldsTheControllerToLookingAtACreatedFixture(t *testing.T) {
	requireUnmet(t, damagedAt1200ms("createFixture"), 3200*ms, "has not looked at configmaps ns/c since the createFixture at 1.2s")
}

func TestSpecForReadsTheRunEventsReferences(t *testing.T) {
	run := tt.Run("toy", "widgets")
	run.Run.Primaries[0].Ready, run.Run.Stable, run.Run.Reads, run.Run.Writes = "self.ok", 4*s, 3, 5
	run.Run.References = []trace.Reference{
		{From: widgets, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "has(self.data)", Required: true, Track: s},
		{From: widgets, System: "cloud", To: trace.Kind{Resource: "buckets"}, Name: "spec.bucket", Ready: "self.ok"},
	}
	var b tt.Builder
	got, err := check.SpecFor(nil, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 2 {
		t.Fatalf("references = %+v, want 2.", got.References)
	}
	toSecret, toBucket := got.References[0], got.References[1]
	if toSecret.From != widgets || toSecret.To != (trace.Ref{System: "kube", Resource: "secrets"}) || !toSecret.Namespaced ||
		toSecret.Name != "spec.secretName" || !toSecret.Required || toSecret.Track != s {
		t.Errorf("the Secret reference = %+v", toSecret)
	}
	if toBucket.To != (trace.Ref{System: "cloud", Resource: "buckets"}) || toBucket.Namespaced || toBucket.Required || toBucket.Name != "spec.bucket" {
		t.Errorf("the bucket reference = %+v", toBucket)
	}
	if ok, err := toSecret.Ready(map[string]any{"data": map[string]any{}}); !ok || err != nil {
		t.Errorf("Ready = %v, %v; want the run event's.", ok, err)
	}
	run.Run.References[0].Ready = "self.("
	b = tt.Builder{}
	if _, err := check.SpecFor(nil, b.Add(run).Index()); err == nil {
		t.Error("SpecFor accepts a reference whose ready does not compile.")
	}
}

func TestSpecForTakesReferencesFromTheConfig(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.References = []config.Reference{{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Namespaced: true,
		Name: "spec.secretName", Ready: "has(self.data)", Required: true, Track: s}}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{{From: run.Run.Primaries[0].Kind, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "true"}}
	var b tt.Builder
	ix := b.Add(run).Index()
	got, err := check.SpecFor(c, ix)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 1 || got.References[0].To != (trace.Ref{System: "kube", Resource: "secrets"}) || !got.References[0].Required || got.References[0].Track != s {
		t.Fatalf("references = %+v, want the config's.", got.References)
	}
	if ok, _ := got.References[0].Ready(map[string]any{}); ok {
		t.Error("Ready is the run event's, want the config's.")
	}
	c.References[0].To = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	if _, err := check.SpecFor(c, ix); err == nil || !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("SpecFor = %v, want an error that names ConfigMap, whose seens the trace did not record.", err)
	}
}

func TestSpecForRefusesAConfigReferenceToAKindTheTraceDidNotSee(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{
		{From: run.Run.Primaries[0].Kind, To: secrets, Namespaced: true, Name: "spec.secretName", Ready: "true"},
		{From: run.Run.Primaries[0].Kind, System: "vault", To: trace.Kind{Resource: "secret"}, Name: "spec.path", Ready: "true"},
	}
	var b tt.Builder
	ix := b.Add(run).Index()
	for _, want := range []struct {
		to      config.Reference
		refused string
	}{
		{config.Reference{To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Namespaced: true}, ""},
		{config.Reference{System: "vault", Collection: "other"}, ""},
		{config.Reference{To: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespaced: true}, "v1/ConfigMap"},
		{config.Reference{To: schema.GroupVersionKind{Group: "other.io", Version: "v1", Kind: "Secret"}, Namespaced: true}, "other.io/v1/Secret"},
		{config.Reference{System: "cloud", Collection: "secret"}, "cloud:secret"},
	} {
		r := want.to
		r.From, r.Name, r.Ready = c.Primaries[0].Kind, "spec.name", "true"
		c.References = []config.Reference{r}
		_, err := check.SpecFor(c, ix)
		switch {
		case want.refused == "" && err != nil:
			t.Errorf("SpecFor = %v, want no error, as the run event names %+v.", err, want.to)
		case want.refused != "" && (err == nil || !strings.Contains(err.Error(), want.refused)):
			t.Errorf("SpecFor = %v, want an error that names %s.", err, want.refused)
		}
	}
	c.References[0] = config.Reference{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, Name: "spec.name", Ready: "self.("}
	if _, err := check.SpecFor(c, ix); err == nil {
		t.Error("SpecFor accepts a config reference whose ready does not compile.")
	}
}

func TestSpecForTakesAConfigReferencesResourceFromTheRunEvent(t *testing.T) {
	c, err := config.Load("../../targets/toy-widget/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c.References = []config.Reference{{From: c.Primaries[0].Kind, To: schema.GroupVersionKind{Group: "zoo.io", Version: "v1", Kind: "Moose"}, Namespaced: true, Name: "spec.moose", Ready: "true"}}
	run := tt.Run("toy.reconciler-fuzzer", "widgets")
	run.Run.References = []trace.Reference{{From: run.Run.Primaries[0].Kind, To: trace.Kind{Group: "zoo.io", Version: "v1", Kind: "Moose", Resource: "moose"}, Namespaced: true, Name: "spec.moose", Ready: "true"}}
	var b tt.Builder
	got, err := check.SpecFor(c, b.Add(run).Index())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.References) != 1 || got.References[0].To != (trace.Ref{System: "kube", Group: "zoo.io", Resource: "moose"}) {
		t.Errorf("references = %+v, want one to moose, the run event's resource for Moose.", got.References)
	}
}
