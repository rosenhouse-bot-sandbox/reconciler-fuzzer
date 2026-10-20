# reconciler-fuzzer v2 design

This document governs the `v2/` tree. Issue #114 holds the full proposal and
its reasons. This document states what v2 does, precisely for milestone M0
and briefly for later milestones. It stays under 500 lines.

## 1. Terms

| Term | Meaning |
| --- | --- |
| Plan | The list of steps for one run, stored as `plan.json`. |
| Step | One unit of a run: fault period, end faults, settle wait, confirm window. |
| Action | Something the harness does once in a step, such as creating a CR or restarting the controller. |
| Slot | A letter, such as `a`, that names one CR across a plan's steps. |
| Fault | A problem the harness causes during a fault period and then ends. |
| Fault period | The first phase of a step, when its actions and faults run. |
| Settle condition | The one test that ends a step's wait and decides whether the controller converged. |
| Confirm window | One more `stable` period after the settle condition holds. |
| System | Anything outside the controller that it talks to, behind the `System` interface. |
| Trace | The single log of a run, `trace.jsonl`. |
| Caller | Who made a request: `controller`, `harness`, or later a model. |
| Listener | An address the harness serves for one caller in front of one system. |
| Finding | One failed check. |

## 2. Packages

Each package owns one directory under `v2/`. Arrows point at imports.

```
cmd/reconciler-fuzzer ─▶ run ─▶ check ─▶ trace
                          │       └────▶ config
                          ├──▶ kube ─▶ system ─▶ interpose ─▶ trace, plan
                          ├──▶ launch
                          └──▶ plan
```

| Package | Job | Ports from |
| --- | --- | --- |
| `trace` | Event types, recorder, loader, index, writer attribution. `trace/tracetest` builds traces for tests. | new |
| `interpose` | Listeners, request classification, request faults, Call events. | `internal/proxy` |
| `system` | The `System` interface and its value types. | new |
| `kube` | envtest, namespace, RBAC identity, listeners, observer, harness CRUD. | `internal/cluster` |
| `launch` | The controller process. | `internal/launch` |
| `config` | Strict YAML loader, CEL for `ready`, `${...}` expansion. | `internal/target` |
| `plan` | Plan types and `plan.json` load, save and validation. | new |
| `run` | The step runner, the deadline, and `Run`, which wires a whole run. | new |
| `check` | Settle condition, Converge, Crash, findings, `report.md`. | new |

Only `kube` imports controller-runtime. Only `check` decides what is a bug.
The runner makes no correctness decision. It calls the settle condition only
to stop waiting early.

`Index.Writer`, `System.Snapshot`, `System.Pending` and `System.Fault` have
no production caller in M0. They are scaffolding for M1's checks and models.
The runner reaches `kube` through its own narrow `Cluster` interface.

## 3. The System interface

```go
type System interface {
    Name() string
    Start(ctx context.Context, env *Env) (Endpoints, error)
    Snapshot(ctx context.Context) ([]Resource, error)
    Pending() bool
    Fault(f plan.Fault) (end func(), err error)
}
```

`Endpoints` holds the controller listener, the harness listener and `Vars`.
Each var expands as `${systems.<name>.<key>}` in launch args and env.
Request faults go to the controller listener. `System.Fault` starts faults
inside the system, and in M0 every system returns `ErrUnsupported`.

## 4. Trace

The recorder appends events to `trace.jsonl`, one JSON object per line. Any
goroutine may call `Record`. The recorder assigns `Seq`, a total order, and
`At`, the time since the run started by a monotonic clock. Each event sets
exactly one of these fields.

| Field | Written by | Content |
| --- | --- | --- |
| `run` | runner, after the fixtures | Run namespace, the primary kind's group and resource, `planSteps`, and the config's `ready`, `stable`, `reads` and `writes`. |
| `phase` | runner | Step number and phase: `faultPeriod`, `endFaults` (with `deadline` and `backoffBound`), `settled`, `timedOut`, `confirmed`. |
| `action` | runner | Step, index, `do`, slot, the object's ref, and any error. |
| `fault` | runner | Step, index, start or end, and a description. |
| `call` | listener | Caller, method, path, verb, ref, status, latency, injected fault, and the uid, resourceVersion and name from a mutating response. |
| `version` | observer | Ref, uid, resourceVersion, deleted flag, full body. |
| `proc` | runner | Controller start or exit, pid, exit status, `harnessCaused`, output tail. |

A `Ref` is `{system, group, resource, namespace, name}`. It omits the API
version, so every version of a kind names the same object.

The trace stores bodies, not readiness. `judge` judges under the spec the
`run` event records, or under a config passed to it.

**Time.** A query at time `t` sees events with `At <= t`. A call's `At` is
when its response ended. A watch call's `At` is when its stream opened.

**Writer attribution.** A version's writer is the caller of the call whose
response carried the same ref and resourceVersion. A create joins by the
response's name, so generateName works. A version no response carried
belongs to `cluster`.

**Changed writes.** A successful mutating call changed its object when its
response resourceVersion appears in no earlier version or response for that
object. A no-op write returns the old resourceVersion, so it is no change. A
successful mutating call whose response carried no resourceVersion counts as
a change.

## 5. Listeners

`interpose.Listen` serves plain HTTP on `127.0.0.1:0` for one caller and
forwards to one upstream with that caller's credentials. It drops any
incoming `Authorization` header. Watch responses stream with no buffering.
Upgraded connections pass through unrecorded.

`kube` opens two listeners:

- The controller listener forwards with the controller's ServiceAccount
  token. The controller's kubeconfig names it and carries no credentials.
- The harness listener forwards as admin. Every harness action and fixture
  goes through it.

The observer and `Snapshot` reach the API server directly as admin. They
record no calls.

**Classification.** A listener reads `/api/<v>` or `/apis/<g>/<v>`, an
optional `/namespaces/<ns>`, then `<resource>[/<name>[/<subresource>]]`.
`status` and `finalize` after a namespace name are subresources of the
namespace. The verb follows the method and path: POST is `create`, PUT
`update`, PATCH `patch`, DELETE `delete` or `deletecollection`, GET `get`,
`list` or `watch`. A non-resource URL takes the lowercased method.

**Response decoding.** For a mutating request with a 2xx JSON or Kubernetes
protobuf response, the listener reads `metadata.uid`,
`metadata.resourceVersion` and `metadata.name`. A response of another
content type records none of them.

**Request faults.** A fault matches by verb, group, resource, subresource and
name; an empty field matches anything. `nth: k` picks the k-th matching
request since the fault started, counting from 1. `nth: 0` picks every
matching request. The first active fault that picks a request applies:

- `error{code, retryAfter}` answers with a `metav1.Status` of that code, from
  400 to 599, and a `Retry-After` header when set. The request never reaches the server.
- `delay{d}` holds the request for `d`, then forwards it. Ending the fault
  releases held requests at once.
- `drop` never forwards the request and aborts the connection. The call
  records status 0.

The call records the fault's description in `injected`, such as `error(500)`.

## 6. Kubernetes system

`kube.Start` does this, in order:

1. Starts envtest with the config's CRDs. It sets `UseExistingCluster` to
   false and disables the `StorageObjectInUseProtection` admission plugin,
   as `internal/cluster` does.
2. Creates namespace `rf-<8 hex>`.
3. Creates ServiceAccount `reconciler-fuzzer-controller`, binds each Role
   from `launch.rbac` in the namespace and each ClusterRole cluster-wide, and
   requests a seven-day token. ClusterRoles get a per-run name suffix.
4. Opens the two listeners and writes `kubeconfig`, pointing at the
   controller listener with the run namespace as its default.
5. Starts the observer.
6. Creates each fixture through the harness listener, in the namespace when
   the fixture names none.
7. Creates the primary sample with a server-side dry run, as admin. The API
   server's first create of a new kind can take 2 s, which would otherwise
   lengthen step 0's fault period.

`Vars` holds `kubeconfig` (a path), `namespace` and `url`.

**Observer.** The observer list-watches kinds across all namespaces. It
starts with the primary kind, the fixture kinds and the kinds the
`launch.rbac` rules name. It adds a kind the first time any listener records
a call on it, except ignored kinds (§10). A kind it cannot map is tried again
on the next call. For each
add, modify and delete it records a `version` event with the full body. It
records a version once per (ref, resourceVersion). A relist after a watch
error records only versions not seen before.

`Snapshot` lists every observed kind as admin. `Pending` is false.

**Harness actions.** `Create`, `Patch` (JSON merge patch), `Delete` and
`WaitGone` act through the harness listener. `Create` and `Patch` return
once the observer has recorded the version their response carried, and
`Delete` once it has recorded the object deleted or being deleted. So the
trace holds every harness change before the step's `endFaults`. `WaitGone`
returns once the object is absent, finalizers included, and returns any Get
error other than NotFound.

envtest runs no garbage collector and no namespace controller. M0 needs
neither: Converge judges only CRs.

## 7. Launcher

`launch.Binary` runs one controller process. It passes stdout and stderr
through one pipe to `controller.log` and keeps the last 4 KiB as the tail.

- `Start` execs the binary with the expanded args and env.
- `Stop` sends SIGTERM, then SIGKILL after 5 s, and waits until the process
  is reaped. The exit is harness-caused.
- `Kill` sends SIGKILL and waits until the process is reaped. The exit is
  harness-caused.
- `Supervise(true)` starts a process that is down at once, and restarts it
  after any exit the harness did not cause. A restart waits 100 ms, doubling
  up to 5 s, as kubelet does. A harness start resets the delay.
  `Supervise(false)` stops restarting.

Every start and exit calls `OnStart` or `OnExit`, which the runner turns
into `proc` events.

## 8. Plans

```json
{"seed": 1, "steps": [
  {"actions": [{"do": "create", "slot": "a", "object": {"apiVersion": "...", "kind": "Widget",
     "metadata": {"name": "widget"}, "spec": {"count": 1}}}]},
  {"period": "1s",
   "faults": [{"match": {"verb": "create", "resource": "configmaps"}, "nth": 1, "error": {"code": 500}}],
   "actions": [{"do": "update", "slot": "a", "patch": {"spec": {"count": 2}}}]}
]}
```

| `do` | Fields | Effect |
| --- | --- | --- |
| `create` | `slot`, `object`? | Creates `object`, or the sample. Waits first until the slot's previous CR is gone. |
| `update` | `slot`, `patch` | Merge-patches the slot's CR. |
| `delete` | `slot` | Deletes the slot's CR. Does not wait. |
| `restart` | | Stops the controller with SIGTERM and starts it again. |
| `kill` | | Sends SIGKILL. The controller stays down until the fault period ends. |

Each action has an optional `at` offset from the start of the fault period.
Actions run in order; an action whose offset has passed runs at once. A
fault's `system` defaults to `kube`. Durations are Go duration strings.

`plan.Load` decodes strictly and validates: every `do` is known, `update`
and `delete` name a slot an earlier action created, `create` names a slot
that holds no CR, and no two live slots give their CR the same name, the
sample's included. Each fault sets exactly one of `error`, `delay` and
`drop`, an error code from 400 to 599, and a `system` of `kube` or none.

## 9. Step runner

`run.Run` loads the config and plan, writes `plan.json`, starts the recorder
and `kube`, records the `run` event, starts the controller with
`Supervise(true)`, executes the plan, closes the trace, judges it, and writes
`report.md`. A harness error, or an interrupt, keeps `trace.jsonl` and
`controller.log` and names the directory.

`Runner.Execute` runs the plan's steps, then two more: step `planSteps`
deletes every live CR, and step `planSteps + 1` restarts the controller into
the empty namespace. Each step runs four phases:

1. **Fault period.** Record `faultPeriod`. Call `Supervise(false)`. Start
   every fault. Apply the actions at their offsets. The period lasts
   `period`, or until the last action returns if that is later.
2. **End faults.** End every fault. Call `Supervise(true)`, which starts a
   controller that is down. Compute the deadline and record `endFaults`.
3. **Settle wait.** Every 50 ms, index new events and call `FirstSettled`
   over the time since the last poll, from `endFaults` and up to the
   deadline. Record `settled` when it finds a time, or `timedOut` at the
   deadline. So the wait and Converge find the same settle time.
4. **Confirm.** After `settled`, wait `stable` and record `confirmed`. After
   `timedOut`, go straight to the next step.

**Deadline.** `endFaults + settle + min(faultPeriod, backoffCap) + maxPoll`.
`maxPoll` is the longest `poll` of any system the step touched, and is zero
in M0 because the Kubernetes system is watch-driven. `backoffBound` is set
when `backoffCap < faultPeriod`.

A harness error, such as a failed create, ends the run with an error. A
controller that never settles is a finding, not a harness error.
A create that waits `settle` for its slot's previous CR to go records the
action's error, skips the step's later actions and ends the plan after that
step. The appended steps still run. `report.md` notes every action error.

## 10. Settle condition

`check.Settled(ix, t, spec)` returns the unmet clauses at `t`, or none. M0
implements these clauses. Numbers follow the issue.

1. Every CR of the primary kind that exists at `t` satisfies `ready`, and
2. carries no `deletionTimestamp`. A deleted CR therefore holds the
   condition open until its delete version arrives. It is not also reported
   as not ready.
3. The controller is up at `t` and has been up for at least `stable`.
4. The controller made a 2xx request, not ignored, since its last start.
7. In `(t - stable, t]`, the controller made no changed write, not ignored.
   It made at most `reads × max(1, live CRs)` gets and lists, and at most
   `writes × max(1, live CRs)` mutating requests, no-op writes included,
   not ignored.

Clause 5 (the controller looked at damaged or changed objects) arrives with
`damage` and `changeFixture` in M1. A watch-based controller learns of a
harness change from a watch it opened earlier, so clause 5 must count an
open watch on the kind, which needs the listener to record when a watch
closes, and must lapse 1.5 poll intervals after end faults. Clause 6 (no
system has pending work) arrives with models in M1. A `ready` error fails
clause 1 and quotes the error.

`ready` is CEL with one variable, `self`, bound to the CR's body. A missing
field fails only where the expression reads it, so `has()` guards work.

**Ignored requests.** `trace.Ignored` lists the calls that no clause and no
check counts, and that the observer never watches:

- core and `events.k8s.io` Events;
- `coordination.k8s.io` Leases;
- TokenReview, SubjectAccessReview, SelfSubjectAccessReview,
  LocalSubjectAccessReview and SelfSubjectRulesReview;
- non-resource URLs, such as `/api`, `/apis`, `/openapi/v3` and `/version`.

`check.FirstSettled(ix, from, to, spec)` returns the earliest `t` in
`[from, to]` at which the condition holds. The condition changes only at an
event time or at an event time plus `stable`, so it tests only those times
and `from`. It finds the events in `[from - stable, to]` by binary search.

## 11. Checks

Every check is `func(*trace.Index, Spec) []Finding`. A finding names its
check, its step, its time, labels, a one-line summary and detail lines.

**Converge.** For each step that reached `endFaults`:

- Let `to` be the deadline, or the step's `confirmed` time when that is
  earlier. Let `end` be the step's recorded end: its `confirmed` time, the
  next step's start, or the trace's last event. Let `t*` be
  `FirstSettled(endFaults, min(to, end))`.
- No `t*` and `end < to`: the trace stopped early, and the step gets a note.
- No `t*`: the finding says "did not settle" and lists the unmet clauses at
  `to`.
- `t* + stable` beyond `end`: the step gets a note. Only a config with a
  longer `stable` than the run's can cause it.
- Otherwise the settle condition must hold at `t* + stable`. If it fails,
  the finding says "did not stay settled" and lists the unmet clauses.
- A finding gets the label `backoff-bound` when the step's `endFaults` set
  `backoffBound`.

The confirm test needs no extra rule: clause 7 at `t* + stable` covers the
whole window, and the harness acts in no window.

**Crash.** Each step with a controller exit not caused by the harness gives
one finding. It counts the exits and quotes the first exit's status and
output tail. An exit outside any step belongs to step -1. The issue's
exception for an exit within 1 s of a Lease fault is not in M0, so M0
targets run with leader election off.

`check.Judge` runs Converge then Crash and returns a `Report`: the findings,
and notes on action errors and on steps the trace cannot decide. A note is
not a finding. `check.Markdown` renders `report.md`: one section per
finding, or "No findings.", then the notes. It names the appended steps
"delete all" and "restart".

## 12. Config

One YAML file. Decoding is strict, and an unknown key is an error that names
its line. Paths resolve against the file's directory. M0 accepts these keys.

```yaml
launch:
  binary: ../../../bin/toy-widget
  args: [--bug=0]
  env: {KUBECONFIG: "${systems.kube.kubeconfig}", WATCH_NAMESPACE: "${systems.kube.namespace}"}
  rbac: [rbac/role.yaml]          # Roles and ClusterRoles for the controller's identity
crds: [crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget   # group/version/Kind, or version/Kind
  sample: widget.yaml
  ready: has(self.status) && has(self.status.ready) && self.status.ready == self.spec.count
fixtures: [config.yaml]
budgets: {reads: 2, writes: 4}    # defaults
timeouts: {settle: 30s, stable: 3s, backoffCap: 60s}   # defaults
```

`config.Expand` replaces `${key}` from a var map, and an unknown key is an
error. Later milestones add `values`, `models`, `systems`, `properties`,
`primary.halted`, `primary.retain`, `primary.owns` and `primary.periodic`.

## 13. Commands

```
reconciler-fuzzer run --config c.yaml --plan plan.json --out dir [--launch-arg a]...
reconciler-fuzzer judge --trace dir[/trace.jsonl] [--config c.yaml]
```

`--launch-arg` appends to the config's launch args, so `--launch-arg
--bug=3` switches on B3. Both commands print the findings. They exit 0 with
no findings, 1 with findings, and 2 on a usage or harness error. `judge`
needs no cluster and runs in milliseconds.

## 14. Toy target and the M0 gate

`v2/targets/toy-widget/config.yaml` drives the existing toy binary,
`bin/toy-widget`, built from `targets/toy-widget`. It reuses that
directory's CRD, sample, RBAC and fixture. `plans/` holds one plan per gated
bug, plus `fault.json`, `drop.json`, `delay.json` and `burst.json`, which only
B0 runs.

| Plan | Steps | Bug | Caught by, at step: detail |
| --- | --- | --- | --- |
| b1 | create 3 | B1 | Converge did not stay settled, 0: changed writes |
| b2 | create 1 | B2 | Converge did not settle, 0: changed writes |
| b4 | create 0; update 2 | B4 | Converge did not settle, 1: not ready |
| b5 | create 1 | B5 | Converge did not settle, 0: not ready |
| b6 | create 1 | B6 | Converge did not settle, 0: changed writes |
| b7 | create 2; update 1 | B7 | Converge did not settle, 1: not ready |
| b10 | create 3; restart; update 1 | B10 | Converge did not settle, 2: not ready |
| b11 | create 2; update 1; update 2 | B11 | Converge did not settle, 2: not ready |
| b11-fault | create 0; update 1 with the first ConfigMap create failing | B11 | Converge did not settle, 1: not ready |
| b12 | create 2; update 0 | B12 | Crash, 1: exit status 2 |
| b12-fault | create 2; update 0 with Widget patches delayed | B12 | Crash, 1: exit status 2 |
| b13 | create 1; delete | B13 | Converge did not settle, 1: being deleted |
| b15 | create two Widgets; delete the second | B15 | Converge did not settle, 0: widget-2 not ready |
| b16 | create 1 | B16 | Converge did not settle, 0: not ready |
| b0 | create; update; restart; kill; delete | none | none |

The gate: each plan catches its bug with the named finding, step and
detail, and every plan gives no findings and no notes with `--bug=0`. `gate_envtest_test.go` runs it, through
`make test-v2-envtest`. `make test-v2` runs the tests that need no cluster.
B3, B8, B9 and B14 wait for M1's Clean, Restore and damage.

Two M0 deliverables of the issue are deferred: a corpus of traces mined from
the old decision log, which the gate replays, and the experiment that
measures Converge's false positives on cert-manager and external-secrets.
Both should run before M1 builds on clause 7's budget unit (§17, entry 8).

## 15. Testing

- Red/green TDD. A test must fail when the code it covers is mutated.
- Cluster-free tests build traces with `trace/tracetest` and run in
  seconds. Checks are tested only this way.
- Tests that need envtest live in `*_envtest_test.go` files with
  `//go:build envtest`. Keep them few: each starts a control plane.
- Fixtures shared within a package live in its `testdata/`. A stored trace
  fixture is a `.jsonl` file that `trace.Load` reads.
- Tests of `interpose` use `httptest` upstreams. Tests of `launch` run
  `/bin/sh` scripts.
- `check/testdata/b0.jsonl` and `b16.jsonl` are traces recorded from toy
  runs. Judging them needs no cluster.

## 16. Later milestones

- **M1, matrix parity.** Generator, greedy shrinker, replay 5 times, the `gc`
  and `namespace` models, Clean, Restore, Unique, `damage` and
  `changeFixture` actions, `dropResponse` and `killAfter`, settle clauses 5
  and 6. The old tree is deleted.
- **M2, #111.** Stage models, `stuck` and `slowDelete`, Honest, Backoff,
  `conditionTrue()`, observe events for the controller's reads.
- **M3, #38.** Cluster-wide judging, owning-CR rules 2 and 3, cross-scope
  garbage collector rules.
- **M4, external HTTP.** Fake services, API maps, `crudfake`, `poll`,
  `halted`, `gone`, `role`.
- **M5, real external targets.** Vault and S3 maps, `holdWatch`.

`size_test.go` caps v2 production code at 9k lines.

## 17. Decision log

Each entry changes a check, an exception, the `System` interface or the
ignore list.

1. `launch.rbac` holds the controller's RBAC, which keeps ten top-level
   config keys. B16 needs a real identity.
2. CEL binds the CR as `self`, matching the issue's `owner:` and `retain:`
   expressions.
3. Converge searches the trace for the first settle time instead of trusting
   the runner's poll. A changed config rejudges correctly.
4. The runner appends a delete-all step and a restart step to every plan.
5. Reads in clause 7 count gets and lists. Watches are long-lived and do not
   count.
6. Harness actions wait for the observer. Without the wait, the settle
   condition held at `endFaults` on a CR whose delete the observer had not
   yet recorded, and the controller's cleanup then failed the confirm window.
7. Clause 5 waits for M1. As built first, it held a watch-based controller
   to a new request on every changed CR, and such a controller reads its
   cache instead. It failed correct controllers that skip a no-op status
   write or filter metadata-only updates.
8. Clause 7's budgets count per live CR per `stable` window, not per poll
   period, because the Kubernetes system has no poll. A controller that
   does uncached Gets on a `RequeueAfter` shorter than `stable / 2` exceeds
   the default read budget.
9. A step the trace cannot decide gets a note, not a finding: the trace
   ended before the deadline, or the confirm window is not recorded. An
   interrupted run or a changed `stable` causes these, not the controller.
10. The supervisor backs off between restarts, so a crash loop does not
   fill the trace.
