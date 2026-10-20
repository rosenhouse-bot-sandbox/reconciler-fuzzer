# reconciler-fuzzer v2 design

This document governs the `v2/` tree. Issue #114 holds the proposal and its
reasons. This document states what v2 does through milestone M1. It stays
under 500 lines. Where the code documents a rule, this document names it.

## 1. Terms

| Term | Meaning |
| --- | --- |
| Plan | The list of steps for one run, stored as `plan.json`. |
| Step | One unit of a run: fault period, end faults, settle wait, confirm window. |
| Action | Something the harness does once in a step, such as creating a CR or damaging an object. |
| Slot | A letter, such as `a`, that names one CR across a plan's steps. |
| Fault | A problem the harness causes during a fault period and then ends. |
| Damage | A change the harness makes to an object the controller created. It is never undone. |
| Settle condition | The one test that ends a step's wait and decides whether the controller converged. |
| Confirm window | One more `stable` period after the settle condition holds. |
| System | Anything outside the controller that it talks to, behind the `System` interface. |
| Model | An in-process stand-in for another controller, such as the garbage collector. |
| Caller | Who made a request: `controller`, `harness`, `gc` or `namespace`. |
| Instance | One life of an object, from its create to its delete. Its UID names it. |
| Owning CR | The CR an instance belongs to, by the rules of §11. |
| Role | The part an instance plays for its owning CR (§11). |
| Settled step | A step that settled and stayed settled: Converge passed. |

## 2. Packages

Each package owns one directory under `v2/`.

| Package | Job | Ports from |
| --- | --- | --- |
| `trace` | Event types, recorder, loader, index, writer attribution. `trace/tracetest` builds traces for tests. | new |
| `interpose` | Listeners, request classification, request faults, Call events. | `internal/proxy` |
| `system` | The `System` interface and its value types. | new |
| `kube` | envtest, namespace, RBAC identity, listeners, observer, harness CRUD. | `internal/cluster` |
| `model` | The `gc` and `namespace` models. | `internal/cluster/collector.go` |
| `launch` | The controller process. | `internal/launch` |
| `config` | Strict YAML loader, CEL, `${...}` expansion. | `internal/target` |
| `plan` | Plan types and `plan.json` load, save and validation. | new |
| `owner` | Instances, creators, owning CRs, roles and target selection. | new |
| `run` | The step runner, the deadline, and `Run`, which wires one run. | new |
| `check` | Settle condition, the checks, findings, `report.md`. | new |
| `generate` | Draws plans from a seed. | `internal/generate` |
| `hunt` | Loops seeds, replays, shrinks, removes duplicate findings. | new |

`run` uses `kube`, `model`, `launch`, `owner` and `check`. `check` uses
`owner`, and both use only `trace`, `plan` and `config`. Only `kube` and
`model` import client-go. Only `check` decides what is a bug. The runner
calls the settle condition only to stop waiting early.

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
Request faults go to the controller listener. In M1 every `System.Fault`
returns `ErrUnsupported`. A model's `Endpoints` are empty, and checks read
its `work` events, never `Pending`.

## 4. Trace

The recorder appends events to `trace.jsonl`, one JSON object per line. It
assigns `Seq`, a total order, and `At`, the time since the run started.
Each event sets exactly one of these fields.

| Field | Written by | Content |
| --- | --- | --- |
| `run` | runner, after the fixtures | Namespace, primary kind, `planSteps`, and the config's `ready`, `retain`, `stable` and budgets. |
| `phase` | runner | Step and phase: `faultPeriod`, `endFaults` (with `deadline`, `backoffBound`), `settled`, `timedOut`, `confirmed`. |
| `action` | runner | Step, index, `do`, slot, the ref acted on, and any error. |
| `fault` | runner | Step, index, start or end, and a description. |
| `call` | listener | Caller, method, path, verb, ref, status, latency, injected fault, result uid, resourceVersion and name, watch `stream`. |
| `watchEnd` | listener | Caller, system and `stream` of a watch whose response ended. |
| `version` | observer | Ref, uid, resourceVersion, deleted flag, full body. |
| `work` | model | System and whether it has work pending from now on. |
| `proc` | runner | Controller start or exit, pid, exit status, `harnessCaused`, output tail. |

A `Ref` is `{system, group, resource, namespace, name}`, with no API
version. The trace stores bodies, not readiness, so `judge` rejudges an old
trace under a new config. A query at `t` sees events with `At <= t`. A
call's `At` is when its response ended, or when a watch's stream opened. A
version's writer is the caller whose response carried its ref and
resourceVersion, or `cluster`. `Index.Changed` says a successful write's
resourceVersion is new, so a no-op write is no change.

**Unknown outcome.** A call whose `injected` is `dropResponse` or
`killAfter` reached the server, but its caller never saw the response
(`Call.Unknown`). The call records the server's status and result fields.

## 5. Listeners and request faults

`interpose.Listen` serves plain HTTP on `127.0.0.1:0` for one caller and
forwards with that caller's credentials. Each watch gets a `stream` number,
and its end records a `watchEnd`. A write whose client leaves still goes
on upstream, so its call records the server's answer.

A fault matches by verb, group, resource, subresource and name. `nth: k`
picks the k-th matching request since the fault started; `nth: 0` picks
every one. The first active fault that picks a request applies:

- `error{code, retryAfter}` answers with a `metav1.Status`, 400 to 599.
- `delay{d}` holds the request for `d`, or until the fault ends.
- `drop` aborts the connection without forwarding. Status 0.
- `dropResponse` forwards the request, reads the whole response, records the
  call, then aborts the client connection with `http.ErrAbortHandler`.
- `killAfter` does what `dropResponse` does, but calls the listener's kill
  function after recording and before aborting. The runner sets it to
  `launch.Binary.Kill`, which returns once the process is reaped. The kill
  is harness-caused, and the controller stays down until end faults.

`dropResponse` and `killAfter` never pick a watch, and a watch does not
count toward their `nth`.

## 6. Kubernetes system

`kube.Start` starts envtest, the run namespace, the controller's identity,
both listeners and the observer, and creates the fixtures. `Vars` holds
`kubeconfig`, `namespace` and `url`. The models stand in for envtest's
missing garbage collector and namespace controller (§7).

**Observer.** It list-watches kinds cluster-wide and records one `version`
per (ref, resourceVersion). It starts with the primary, fixture and
`launch.rbac` kinds, and adds any kind a listener records a call on, except
ignored kinds (§10). `Subscribe` hands each recorded version to a model, in
record order, after recording it.

**Harness actions.** `Create`, `Patch` and `Delete` act through the harness
listener and return once the observer has recorded their effect. The
system's poll interval is `stable` (§18, entry 11).

## 7. Models

`model.New(k)` returns the models; `run.Run` starts them after `kube` and
before the controller. Each writes through an admin listener from
`k.Listen`, so its writes are calls with it as caller, and hears versions
through `k.Subscribe`. A model records `work{pending: true}` on a version
that can give it work, and `work{pending: false}` once a sweep finds no
write to send, every write it sent is observed, and no retry is scheduled.
A failed call retries after 250 ms.

**`gc`** ports `internal/cluster/collector.go` cluster-wide:

- It collects an instance once every ownerReference names a gone owner:
  not found, or found under another UID, by a live read, because one kind's
  watch can lag another's. An owner it cannot read or resolve is live.
- A namespaced owner kind is looked up in the dependent's namespace, a
  cluster-scoped one cluster-wide. A cluster-scoped dependent whose owner
  kind is namespaced is never touched.
- Its delete carries a UID precondition, as the real collector's does.
- A `foregroundDeletion` owner's dependents are deleted; the finalizer goes
  once no dependent with `blockOwnerDeletion` remains. An `orphan` owner
  loses its ownerReference from every dependent, then the finalizer.
- It removes gone and foreground owners from a dependent with a live owner.

**`namespace`** finishes a namespace with a `deletionTimestamp`: it deletes
every object in it, waits until the observer has recorded each gone, then
removes the `kubernetes` finalizer through the `finalize` subresource.

## 8. Plans

```json
{"seed": 1, "steps": [
  {"actions": [{"do": "create", "slot": "a"}]},
  {"actions": [{"do": "damage", "slot": "a", "target": {"resource": "configmaps"}}]},
  {"period": "1s", "faults": [{"match": {"verb": "create", "resource": "secrets"}, "nth": 1, "dropResponse": true}]},
  {"actions": [{"do": "changeFixture", "target": {"resource": "configmaps", "name": "widget-config"}, "patch": {"data": {"label": "blue"}}}]}
]}
```

| `do` | Fields | Effect |
| --- | --- | --- |
| `create` | `slot`, `object`? | Creates `object`, or the sample. Waits first until the slot's previous CR is gone. |
| `update` | `slot`, `patch` | Merge-patches the slot's CR. |
| `delete` | `slot` | Deletes the slot's CR. Does not wait. |
| `restart` | | Stops the controller with SIGTERM and starts it again. |
| `kill` | | Sends SIGKILL. The controller stays down until end faults. |
| `damage` | `target`, `slot`?, `patch`? | Deletes, or merge-patches, an instance the controller created. |
| `changeFixture` | `target`, `patch`? | Merge-patches, or deletes, a fixture. |

A `target` has `system` (default `kube`), `group`, `resource`, `name`,
`role` and `nth` (default 1). Empty fields match anything. The runner
resolves it when the action runs, with `owner.World.Select`: the `nth`
live instance, in creation order, that the controller (for `damage`) or
the harness (for `changeFixture`) created, and, for a `damage` with a
slot, whose owning CR is that slot's live CR. A target that matches nothing,
or whose object is gone or replaced before the delete, records an error
that starts `no instance matches`, and the step goes on. The action event's
`ref` is the resolved object.

Each action has an optional `at` offset, and actions run in order. A fault
sets exactly one effect. `plan.Load` decodes strictly and validates: known
`do`; slots as in M0; a `damage` slot, when set, holds a live CR; `damage`
and `changeFixture` need a `target` with a `resource`; `changeFixture`
needs a target `name`; a patch is a JSON object; only `create`, `update`,
`damage` and `changeFixture` carry their fields.

## 9. Step runner

`run.Run` starts the recorder, `kube`, the models and the supervised
controller, waits up to `settle` for the settle condition, so startup
objects precede the first CR, executes the plan (`PlanPath` or `Plan`),
judges the trace, writes `report.md` and returns the report. `Runner.Execute` runs the plan's
steps, then step `planSteps`, which deletes every live CR, and step
`planSteps + 1`, which restarts the controller. Each step runs its fault
period, ends its faults, polls `FirstSettled` every 50 ms until the
deadline, and after `settled` waits out the confirm window.

**Deadline.** `endFaults + settle + min(faultPeriod, backoffCap) + maxPoll`.
`maxPoll` is the longest poll interval of the systems the step's `damage`
and `changeFixture` actions target, matched or not, and zero otherwise.

A create that waits `settle` for its slot ends the plan after its step.
Any other action error, except an unmatched target, is a harness error.

## 10. Settle condition

`check.Settled(ix, t, spec)` returns the unmet clauses at `t`, or none. It
judges the step whose fault period started last at or before `t`.

1. Every live CR satisfies `ready`, and
2. carries no `deletionTimestamp`.
3. The controller is up at `t` and has been up for at least `stable`.
4. The controller made a 2xx request, not ignored and not of unknown
   outcome, that it sent since its last start.
5. For each object the step's `damage` and `changeFixture` actions changed,
   the controller looked at it after the change: it made a request with
   status 2xx or 404, not of unknown outcome, whose kind is the object's,
   whose namespace is the object's or none, and whose name is the object's
   or none; or it held a watch that covers the object, opened before the
   change and still open at `t` (`Index.Watching`). The clause also needs
   `stable` since each change, and lapses at `endFaults + 1.5 × poll`.
6. No model has work pending at `t` (`Index.Pending`).
7. No changed write, not ignored, ended in `(t - stable, t]` or was in
   flight at `t`. In that window the controller made at most
   `reads × max(1, live CRs)` gets and lists and at most
   `writes × max(1, live CRs)` mutating requests, no-op writes included.

`ready` is CEL over `self`, the CR; an error fails clause 1. No clause or
check counts the requests `trace.Ignored` lists: Events, Leases, TokenReview
and the access reviews, and non-resource URLs. `check.FirstSettled` finds
the earliest settle time in `[from, to]`, testing `from`, event times, event
times plus `stable`, and clause 5's lapse times.

## 11. Instances, owning CRs and roles

`owner.New(ix)` indexes every instance the trace shows.

- An instance is a (ref, UID), from its first version to its delete.
- Its **creator** is the caller of the successful create, or the 201
  apply, whose response carried its UID, or `cluster` (§18, entry 17).
- Its **owning CR** at `t` comes from the first rule that matches:
  1. Its ownerReferences at `t`, followed by UID through instances in the
     trace, gone ones included, reach an instance of the primary kind. The
     first ownerReference with `controller: true` is followed first.
  4. Otherwise, the set of primary instances live at its creation. An empty
     set marks a **shared** instance. Rules 2 and 3 arrive in M3.
- Its **role** is its `metadata.generateName` when set, or else its name.
- The **controller's resources** at `t` are the live instances whose
  creator is `controller`, minus the primary kind and ignored kinds (§10).

A group is a (system, group, resource, namespace, owning CR, role). An
owning CR's key is its rule and sorted UIDs.

## 12. Checks

Every check is `func(*trace.Index, Spec) []Finding`. A finding names its
check, step, time, labels, a one-line summary, detail lines, and `kind`
(`system/group/resource` of its object, `system/resource` for the core
group, or empty). Its signature is check, summary and kind. `check.Judge`
runs every check, and notes each action error and each fault that picked
no request. `check.Markdown` renders the report.

A step's **end** is its `confirmed` time. Clean, Restore and Unique judge
only settled steps (§18, entry 15).

**Converge.** As in M0: the settle condition holds by the deadline, and
still holds at `t* + stable`. The summary names the first unmet clause,
such as `did not settle (clause 7)`, and the kind is its object's. A
finding is labelled `backoff-bound` when `backoffCap` set the deadline.
Steps the trace cannot decide get notes.

**Crash.** Each step with a controller exit the harness did not cause, and
not within 1 s of a faulted Lease request, gives one finding. The grace
assumes leader election is off, or retries within 1 s.

**Clean.** At the end of a settled step with a `delete` action, each
controller resource live there fails when its owner is rule 1 and its CR is
gone, or rule 4 with a non-empty set whose every CR is gone. At the end of
step `planSteps + 1`, every live controller resource that is not shared
fails. Exceptions:

- `retain`: a resource whose rule-1 CR, or every rule-4 CR, makes
  `primary.retain` true over its last body is allowed.
- Shared resources are judged only for growth: at the end of step
  `planSteps + 1`, each group of shared resources holds at most `n` live
  instances, where `n` is its live count when the first `create` ran.

Clean gives one finding per step and kind, `left resources behind`, listing
each survivor and its owning CR's slot. It reports an instance once.

**Restore.** It judges a settled step with no `create`, `update`, `delete`
or `changeFixture` action whose previous step also settled. It compares the
controller's resources at the two ends, by group:

- In a group whose owning CR no `damage` touched, each
  previous instance must still be live, no new instance may appear, and
  each body must be equal without `metadata.resourceVersion`,
  `metadata.managedFields` and `metadata.generation`. Summaries:
  `replaced resources`, `changed resources`.
- In a group whose owning CR a `damage` touched, a damaged instance's group
  must have a live instance again, and each other group must keep its kind
  live. Summary: `did not restore damaged resources` (§18, entry 22).

**Unique.** At each settled step's end, a group holds at most one live
controller instance. Summary: `duplicate resources`.

**Corpus.** `check/testdata/corpus/<name>.jsonl` holds traces mined from
the old `DESIGN.md` decision log, and M1's false positives. Each
`<name>.want` has a first line `# D<n>: <sentence>` or
`# M1 <source>: <sentence>`, an optional `# skip: <reason>`, and one line
per expected finding, `<check> <step>`. `corpus_test.go` requires exactly
those findings. Each false-positive fix adds a trace.

## 13. Config

One YAML file, decoded strictly; an unknown key names its line. Paths
resolve against the file's directory.

```yaml
launch:
  binary: ../../../bin/toy-widget
  args: [--bug=0]
  env: {KUBECONFIG: "${systems.kube.kubeconfig}", WATCH_NAMESPACE: "${systems.kube.namespace}"}
  rbac: [rbac/role.yaml]
crds: [crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: widget.yaml
  ready: has(self.status) && self.status.ready == self.spec.count
  retain: false                   # CEL over the deleted CR; optional
values:
  overlay: {spec.count: {minimum: 0, maximum: 3}}
  distinct: [spec.secretName]     # differs in each CR of a plan
  mutate: [spec.count]            # updates change these
  fixtures: {config.yaml: [data.label]}   # strings changeFixture sets
fixtures: [config.yaml]
budgets: {reads: 2, writes: 4}                          # defaults
timeouts: {settle: 30s, stable: 3s, backoffCap: 60s}    # defaults
```

Later milestones add `models`, `systems`, `properties`, `primary.halted`,
`primary.owns` and `primary.periodic`.

## 14. Generation, replay and shrinking

`generate.New(cfg)` reads the primary CRD's schema with the overlay, the
sample, and the fixtures. It ports `schema.go`, `values.go` and
`validity.go` from `internal/generate`: rapid draws values, and the CRD's
own validation, run in-process, rejects invalid ones. `Plan(seed)` draws:

- 3 to 8 steps; step 0 creates slot `a`. Each step has 1 to 4 actions and
  at most one fault. About 30% of steps hold only one `restart`, `kill` or
  `damage`, so Restore has steps to judge. A fault there brings a `damage`
  of its resource, an `update` for the primary, or else a `restart`.
- Actions: `create` (the sample with drawn `mutate` values and suffixed
  `distinct` values, in a free slot, at most three slots), `update` (a
  drawn value at one `mutate` path), `delete`, `restart`, `kill`, `damage`
  (delete only), and `changeFixture` (a drawn word at one `values.fixtures`
  path).
- A burst, in about 30% of multi-action steps, puts its actions within
  50 ms: a delete then create of one slot, or two updates.
- A fault draws a kind from `error` (500, 503, 429), `delay` (0.25, 0.5 or
  1 × `stable`), `drop`, `dropResponse` and `killAfter`; `nth` from 0 to 3,
  mostly 0 or 1; and a request: a verb `launch.rbac` grants on the primary,
  a fixture or an RBAC resource, or its `status`, or `get` or `list` where
  it grants none. Creates weigh 6, other writes 2, reads 1, times 4 for the
  primary and core kinds the controller may create. `damage` targets draw
  from the resources less the primary.
- The fault period lasts 0.5 s to `2 × stable`. No rule limits where faults
  land. Every plan passes `plan.Validate`. The seed fixes the plan, not
  timing.

**Hunt.** `hunt.Hunt` runs each seed's plan into `<out>/seed-<n>`, and
removes the run if it found nothing new. It replays each new signature's
plan 5 times, counts the replays that find it, and shrinks it unless none
did. A failed seed run is listed; a failed replay or candidate does not
fail. After each seed, `summary.md` lists the findings with seed,
reproduction rate and shrunk plan. Each run logs a line to stderr.

**Shrink.** `hunt.Shrink` is greedy: it takes the first candidate of
`hunt.Candidates` that still fails, and repeats until none does. The
candidates, in order: drop a step; drop an action or a fault; halve a fault
period, down to 0.5 s, or clear a faultless step's; reset one spec field of
a create to the sample's value, or drop one field of an update patch;
rename one slot to another where the plan stays valid. A candidate fails
when one of k runs gives the same signature: k is 2, or 4 when the original
reproduced in under half its replays.

## 15. Commands

```
reconciler-fuzzer run    --config c.yaml (--plan plan.json | --seed n) --out dir [--launch-arg a]...
reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--launch-arg a]...
reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--launch-arg a]...
```

They print the findings, and exit 0 with none, 1 with findings, and 2 on a
usage or harness error. `shrink` writes `shrunk.json`. `judge` refuses a
config whose primary is not the trace's.

## 16. Gates, examples and testing

**Toy.** `v2/targets/toy-widget/config.yaml` drives `bin/toy-widget`. Its
`plans/` hold one plan per gated bug and B0's plans. `gate_envtest_test.go`
runs them through `make test-v2-envtest`.

| Plan | Steps | Bug | Caught by |
| --- | --- | --- | --- |
| b1, b2, b4–b7, b10, b11, b13, b15, b16 | as in M0 | as named | Converge |
| b12, b12-fault | as in M0 | B12 | Crash |
| b3 | create 2; delete | B3 | Clean, step 1 |
| b9 | create 2; delete | B9 | Clean, step 1 |
| b8 | create 1; damage the ConfigMap | B8 | Restore, step 1 |
| b14 | create 1; changeFixture label; restart | B14 | Restore, step 2 |
| the first 20 seeds with faults | generated | B0 | none; a fault fires in 3 |

Every committed plan gives no findings and no notes with `--bug=0`. Seed 6's
plan under B3 shrinks to at most 3 steps that still fail.

**Examples.** `v2/examples/<name>` ports cert-manager and external-secrets.
Their `plans/` and the seeds their `seeds` file lists give no findings. A
listed seed drops the response to the Secret's create, or cert-manager's
apply. Negative controls give a Clean finding on their Secret:
cert-manager's `plans/issue.json` with `--enable-certificate-owner-ref=false`,
and external-secrets' `controls/orphan.json`. CI runs
`make test-v2-example-<name>`.

**Testing.** Red/green TDD: a test must fail when its code is mutated.
Checks and `owner` use `trace/tracetest` traces. Envtest tests live in
`*_envtest_test.go` behind `//go:build envtest`; keep them few.
`size_test.go` caps v2 production code at 9k lines.

## 17. Later milestones

Issue #114's build order holds the rest: the cutover, which deletes the old
tree, then M2 (#111) to M5.

## 18. Decision log

Each entry changes a check, an exception, the `System` interface or the
ignore list.

1. `launch.rbac` holds the controller's RBAC. B16 needs a real identity.
2. CEL binds the CR as `self`, matching `owner:` and `retain:`.
3. Converge searches the trace for the first settle time, for `judge`.
4. The runner appends a delete-all step and a restart step to every plan.
5. Reads in clause 7 count gets and lists, not watches.
6. Harness actions wait for the observer, so the trace holds their changes.
7. Clause 5 covers only damaged and changed objects, not every changed CR.
8. Clause 7's budgets count per live CR per `stable` window.
9. A step the trace cannot decide gets a note, not a finding.
10. The supervisor backs off between restarts, against crash loops.
11. The Kubernetes system's poll interval is `stable`.
12. Clause 5 counts an open watch, so listeners record `watchEnd`.
13. Models report pending work as `work` events, so `judge` needs no model.
14. A named kube instance's role is its name, so Restore compares it (B14).
15. Clean, Restore and Unique judge only settled steps after a settled one.
16. `dropResponse` and `killAfter` skip watches, whose responses never end.
17. An instance's creator comes from the create response's UID.
18. Shared resources are judged for growth against their count at the first
    create. One made per restart, or after its CR's delete, fails.
19. Unknown-outcome calls satisfy neither clause 4 nor clause 5.
20. Faults draw the requests `launch.rbac` grants, weighted toward creates,
    the primary and core created kinds. Damage skips the primary.
21. Restore ignores `metadata.resourceVersion`, `managedFields` and
    `generation`, which every write changes.
22. Restore lets a damaged CR rename its other resources, as a reissue does.
23. Groups split by namespace: one name in two namespaces is no duplicate.
24. The gc model removes `foregroundDeletion` and `orphan` finalizers (§7).
25. An apply patch that answers 201 created its object, as cert-manager's do.
26. Clause 5 holds `stable` after the change, for external-secrets' repair.
27. Crash allows an exit within 1 s of a faulted Lease request.
28. Clause 7 counts a write in flight at `t`, whose version may come first.
29. The runner waits for the settle condition before step 0, for singletons.
30. Clause 4 counts only requests sent since the last start.
31. A Converge signature names the first unmet clause and its kind.
32. `values.fixtures` names the strings `changeFixture` sets per fixture.
33. M1 has no `periodic`; external-secrets draws only hour-long refreshes.
