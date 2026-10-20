# reconciler-fuzzer design

Issue #114 holds the proposal and its reasons. This document states what
the code does. It stays under 500 lines. Where the code documents a rule,
this document names it.

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
| Model | An in-process stand-in for another controller: the garbage collector, or a stage model of a `models:` kind. |
| Dependency | An instance of a model kind with `dependency`, which its owning CR's readiness needs. |
| Caller | Who made a request: `controller`, `harness`, `gc`, `namespace` or `model:<resource>.<group>`. |
| Instance | One life of an object, from its create to its delete. Its UID names it. |
| Owning CR, role | The CR an instance belongs to, and the part it plays for it (§11). |

## 2. Layout and packages

`cmd/reconciler-fuzzer` holds the commands (§15), `internal/` the packages
below, `targets/toy-widget` the toy (§16), `examples/<name>` adopted
controllers, and `docs/` the bug matrix, failures and findings.

| Package | Job |
| --- | --- |
| `trace` | Event types, recorder, loader, index, writer attribution. `trace/tracetest` builds traces for tests. |
| `interpose` | Listeners, request classification, request faults, Call events. |
| `system` | The `System` interface and its value types. |
| `kube` | envtest, namespace, RBAC identity, listeners, observer, harness CRUD. |
| `model` | The `gc` and `namespace` models, and stage models. |
| `launch` | The controller process. |
| `config` | Strict YAML loader, CEL, `${...}` expansion. |
| `plan` | Plan types and `plan.json` load, save and validation. |
| `owner` | Instances, creators, owning CRs, roles and target selection. |
| `run` | The step runner, the deadline, and `Run`, which wires one run. |
| `check` | Settle condition, the checks, findings, `report.md`. |
| `generate` | Draws plans from a seed. |
| `hunt` | Loops seeds, replays, shrinks, removes duplicate findings. |

`run` uses `kube`, `model`, `launch`, `owner` and `check`. `check` uses
`owner`, and both use only `trace`, `plan` and `config`. Only `kube` and
`model` import client-go. Only `check` decides what is a bug; the runner
calls the settle condition only to stop waiting early.

## 3. The System interface

`system.System` has `Name() string`, `Start(ctx, env) (Endpoints, error)`,
`Snapshot(ctx) ([]Resource, error)`, `Pending() bool` and
`Fault(plan.Fault) (end func(), err error)`. `Endpoints` holds the
controller and harness listeners and `Vars`, which expand as
`${systems.<name>.<key>}` in launch args and env. Request faults go to the
controller listener, and model faults to the stage model's `Fault`. Checks
read a model's `work` events, never `Pending`.

## 4. Trace

The recorder appends events to `trace.jsonl`, one JSON object per line. It
assigns `Seq`, a total order, and `At`, the time since the run started.
Each event sets exactly one of these fields.

| Field | Written by | Content |
| --- | --- | --- |
| `run` | runner, after the fixtures | Namespace, primary kind, `planSteps`, the config's `ready`, `retain`, `stable`, budgets and models, and `statusKinds`. |
| `phase` | runner | Step and phase: `faultPeriod`, `endFaults` (with `deadline`, `backoffBound`), `settled`, `timedOut`, `confirmed`. |
| `action` | runner | Step, index, `do`, slot, the ref acted on, and any error. |
| `fault` | runner | Step, index, start or end, a description, and the `model` that took a model fault. |
| `call` | listener | Caller, method, path, verb, ref, status, latency, injected fault, result uid, resourceVersion and name, a create's `requestName`, watch `stream`. |
| `watchEnd` | listener | Caller, system and `stream` of a watch whose response ended. |
| `version` | observer | Ref, uid, resourceVersion, deleted flag, full body. |
| `work` | model | System and whether it has work pending from now on. |
| `seen` | listener | Caller, ref, uid, resourceVersion, `deleted` and `opaque` of what a response showed the caller (§5). |
| `proc` | runner | Controller start or exit, pid, exit status, `harnessCaused`, output tail. |

A `Ref` is `{system, group, resource, namespace, name}`. The trace stores
bodies, not readiness, so `judge` rejudges an old trace under a new config.
A query at `t` sees events with `At <= t`. A call's `At` is when its
response ended, or a watch opened. A version's writer is the caller whose
response carried its ref and resourceVersion, or `cluster`. `Index.Changed`
says a write's resourceVersion is new. A call of **unknown outcome**
(`Call.Unknown`) reached the server, but its caller never saw the response.

## 5. Listeners and request faults

`interpose.Listen` serves plain HTTP on `127.0.0.1:0` for one caller and
forwards with its credentials. A watch's end records a `watchEnd`. A write
whose client leaves still goes on upstream.

A fault matches by verb, group, resource, subresource and name. `nth: k`
picks the k-th matching request since the fault started; `nth: 0` picks
every one. The first active fault that picks a request applies:

- `error{code, retryAfter}` answers with a `metav1.Status`. `delay{d}`
  holds the request. `drop` aborts it unforwarded, with status 0.
- `dropResponse` forwards the request, records the server's answer, then
  aborts the client connection. `killAfter` also kills the controller,
  harness-caused, before aborting. Neither picks or counts a watch.

**Seen.** A listener records a `seen` for each object of its `Seen` kinds
(for the controller, the dependency kinds) that a 2xx response shows its
caller, before forwarding it: a get's or write's object, each list item and
each watch event's. An undecodable response records an `opaque` seen of its ref.

## 6. Kubernetes system

`kube.Start` starts envtest, the run namespace, the controller's identity,
both listeners and the observer, and creates the fixtures. `Vars` holds
`kubeconfig`, `namespace` and `url`. `StatusKinds` lists the installed CRDs'
kinds with a status subresource. Harness actions return once the observer
has recorded their effect. The system's poll interval is `stable`.
**Observer.** It list-watches kinds cluster-wide and records one `version`
per (ref, resourceVersion). It starts with the primary, fixture, model and
`launch.rbac` kinds, and adds any kind a listener records a call on, except
ignored kinds (§10). `Subscribe` hands each recorded version to the models.

## 7. Models

`run.Run` starts the models after `kube` and before the controller. Each
writes through its own listener from `k.Listen`, so its writes are calls
with it as caller, and hears versions through `k.Subscribe`. A model
records `work{pending: true}` when it has work, and `work{pending: false}`
once it has no write to send, every write it sent is observed, and no retry
is scheduled. A failed call retries after 250 ms.

**`gc`** works cluster-wide:

- It collects an instance once a live read finds every owner gone: not
  found, or found under another UID. An owner it cannot read or resolve is
  live. Its delete carries a UID precondition.
- It looks up a namespaced owner kind in the dependent's namespace, and
  never touches a cluster-scoped dependent of a namespaced owner kind.
- It deletes a `foregroundDeletion` owner's dependents, then removes the
  finalizer once no dependent with `blockOwnerDeletion` remains. It removes
  an `orphan` owner from every dependent, then the finalizer.
- It removes gone and foreground owners from a dependent with a live owner.

**`namespace`** finishes a namespace with a `deletionTimestamp`: it deletes
every object in it, waits until the observer has recorded each gone, then
removes the `kubernetes` finalizer through the `finalize` subresource.

**Stage models.** `model.NewStage` makes one per `models:` entry, named
`trace.ModelCaller` of its kind. Its stage, `readyCondition`, writes each
live object not being deleted, `after` after a generation's first version,
the `status:` fields and `Ready` True for the generation, which
`config.ReadyConditionCEL` accepts. Model faults match objects by
`match.name`, in any namespace, or all of them. `stuck` writes them `Ready`
False, reason `Stuck`. `slowDelete` adds the finalizer
`reconciler-fuzzer.dev/slow-delete` to those not being deleted. At either's
end, the model removes the finalizer and writes them `Ready` True, `message:
fault <n> ended`, so a controller without a watch gets an event.

## 8. Plans

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
`role` and `nth` (default 1). Empty fields match anything.
`owner.World.Select` resolves it when the action runs: the `nth` live
instance, in creation order, that the controller (for `damage`) or the
harness (for `changeFixture`) created, owned by the slot's live CR when a
slot is set. A target that matches nothing, or whose object is gone or
replaced before the delete, records an error that starts `no instance
matches`, and the step goes on. The action's `ref` is the resolved object.

Actions run in order, each at its optional `at` offset. A fault sets one
effect: a request fault (§5), or a model fault (§7), whose `match` names a
`resource` and no verb or subresource, with no `nth`. `plan.Load` decodes
strictly and validates: known `do`; a `create` names a free slot and no
other live CR's name; `update` and `delete` name a filled slot; a `damage`
slot holds a live CR; `damage` and `changeFixture` need a target
`resource`, and `changeFixture` a `name`; a patch is a JSON object; only
`create`, `update`, `damage` and `changeFixture` carry their fields.

## 9. Step runner

`run.Run` starts the recorder, `kube`, the models and the supervised
controller, waits up to `settle` for the settle condition, executes the
plan, judges the trace and writes `report.md`. `Runner.Execute` runs the
plan's steps, then step `planSteps`, which deletes every live CR, and step
`planSteps + 1`, which restarts the controller. Each step runs its fault
period, ends its faults, polls `FirstSettled` every 50 ms until the
deadline, and after `settled` waits out the confirm window.

**Deadline.** `endFaults + settle + min(faultPeriod, backoffCap) + maxPoll`,
where `maxPoll` is the longest poll interval of the systems the step's
`damage` and `changeFixture` actions target. The runner gives a model fault
to the first stage model that `Takes` it; none is a harness error. A create
that waits `settle` for its slot ends the plan after its step. Any other
action error, except an unmatched target, is a harness error.

## 10. Settle condition

`check.Settled(ix, t, spec)` returns the unmet clauses at `t`, or none. It
judges the step whose fault period started last at or before `t`.

1. Every live CR satisfies `ready`, and
2. carries no `deletionTimestamp`.
3. The controller is up at `t` and has been up for at least `stable`.
4. The controller made a 2xx request, not ignored and not of unknown
   outcome, that it sent since its last start.
5. For each object the step's `damage` and `changeFixture` actions changed,
   the controller looked at it after the change: a 2xx or 404 request,
   not of unknown outcome, that covers it (`Ref.Covers`), or a watch that
   covers it, opened before the change and open at `t`. The clause also
   needs `stable` since each change, and lapses at `endFaults + 1.5 × poll`.
6. No model has work pending at `t` (`Index.Pending`).
7. No changed write, not ignored, ended in `(t - stable, t]` or was in
   flight at `t`. In that window the controller made at most
   `reads × max(1, live CRs)` gets and lists and at most
   `writes × max(1, live CRs)` mutating requests, no-op writes included.

`ready` is CEL over `self`, the CR; an error fails clause 1. No check
counts what `trace.Ignored` lists: Events, Leases, TokenReview, access
reviews, non-resource URLs. `check.FirstSettled` finds the earliest settle
time in `[from, to]`, testing `from`, event times, event times plus
`stable`, and clause 5's lapse times.

## 11. Instances, owning CRs and roles

`owner.New(ix)` indexes every instance the trace shows.

- An instance is a (ref, UID), from its first version to its delete.
- Its **creator** is the caller whose create, or 201 apply, answered with
  its UID, or `cluster`.
- Its **owning CR** at `t` comes from the first rule that matches:
  1. Its ownerReferences at `t`, followed by UID, controller first, reach
     an instance of the primary kind.
  4. Otherwise, the set of primary instances live at its creation. An empty
     set marks a **shared** instance. Issue #114's build order adds rules 2
     and 3 in M3, and holds M3 to M5.
- Its **role** is its `metadata.generateName` when set, or else its name.
- The **controller's resources** at `t` are the live instances whose
  creator is `controller`, minus the primary kind and ignored kinds (§10).
- A **group** is a (system, group, resource, namespace, owning CR, role).

## 12. Checks

Every check is `func(*trace.Index, Spec) []Finding`. A finding names its
check, step, time, labels, a summary, details, and `kind` (`system/group/
resource`, or `system/resource` for the core group). Its signature is
check, summary and kind. `check.Judge` runs every check, and notes each
action error and each fault that picked no request or held no object.
`check.Markdown` renders the report. `Spec.Models` come from the config,
or else the run event. A step's **end** is its `confirmed` time. Clean,
Restore and Unique judge only settled steps (§17, entry 15).

**Converge.** The settle condition holds by the deadline, and still holds
at `t* + stable`. The summary names the first unmet clause, such as
`did not settle (clause 7)`, and the kind is its object's. `backoff-bound`
labels a finding whose deadline `backoffCap` set. A finding at `t`
suggests a `models:` entry for each kind in `statusKinds`, or Job, Pod or
PVC, that is no primary or model kind, that the controller read by `t` but
whose status it never wrote, with a live instance without status at `t`.

**Crash.** Each step with a controller exit the harness did not cause, not
within 1 s of a faulted Lease request, gives one finding.

**Clean.** At the end of a settled step with a `delete` action, a
controller resource fails when its rule-1 CR is gone, or its rule-4 set is
non-empty and every CR in it is gone. At the end of step `planSteps + 1`,
every controller resource that is not shared fails. A resource whose CRs
make `primary.retain` true is allowed. Shared resources are judged only
for growth: each group holds at most its live count at the first `create`.
Summary: `left resources behind`, listing each survivor's slot, once.

**Restore.** It judges a settled step with no `create`, `update`, `delete`
or `changeFixture` action whose previous step also settled. It compares the
controller's resources at the two ends, by group:

- In a group whose owning CR no `damage` touched, each previous instance
  must still be live, no new instance may appear, and each body must be
  equal without `resourceVersion`, `managedFields`, `generation` and a
  model kind's `status`. Summaries: `replaced resources`, `changed resources`.
- In a group whose owning CR a `damage` touched, a damaged instance's group
  must have a live instance again, and each other group must keep its kind
  live. Summary: `did not restore damaged resources` (§17, entry 22).

**Unique.** At each settled step's end, a group holds at most one live
controller instance. Summary: `duplicate resources`.

**Honest** and Backoff judge the whole run, fault periods included; a
finding's step is the one whose fault period started last by its time. A
CR's dependencies at `t` are the instances of dependency kinds it owns by
rule 1: live ones not being deleted, and unreplaced gone ones the controller
last saw live. A **trigger** is a CR version the controller wrote that becomes
`ready`, or raises observedGeneration (`status.observedGeneration`, or else
the highest in `status.conditions`) while `ready`. `t0` is its call's start.

- At a trigger, the controller's last `seen` of each dependency must show
  its last changing or unknown-outcome write's version or a later one
  (`Index.Later`, or by time when the trace lacks one), and one its
  model's `ready` accepts: `reported ready before seeing its dependency
  ready`. With `required`, a trigger needs a dependency of each kind:
  `reported ready with no dependency`, of the primary's kind.
- With `track: d`, a CR fails at `s + d` when the controller's `seen` at
  `s` of a dependency not being deleted, and each later one until `s + d`,
  showed it live and not ready, and the CR stayed `ready`. A controller
  start or exit, or a failed or unknown-outcome write to the CR, in
  `(s, s + d]` excuses it: `stayed ready after seeing its dependency not
  ready`.
- An `opaque` seen that covers a dependency after its last `seen` excuses it.

**Backoff.** Judged, failing controller calls, not watches or of unknown
outcome, with one system, verb, object, subresource and status form a
**run**; a failed create's object is its `requestName`. A success of that
system, verb, object and subresource breaks it, as does a CR's new
`metadata.generation` unless another CR owns the object by rule 1. With `n`
CRs live during the run, at least 1, it fails when each gap between the
starts of its calls `5n + 1` to `10n + 1` is under 100 ms: `retried a
failing request in a tight loop`, one per step and kind. A 409 on a kind a
`holdWatch` holds joins no run.

**Corpus.** `check/testdata/corpus/<name>.jsonl` holds known bugs and false
positives. Its `.want` (see `corpus_test.go`) lists the findings it needs.
Source `D<n>` is an entry of `git show bee85633:DESIGN.md`'s log.

## 13. Config

One YAML file, decoded strictly; an unknown key names its line. Paths
resolve against the file's directory.

```yaml
launch:
  binary: ../../bin/toy-widget
  args: [--bug=0]
  env: {KUBECONFIG: "${systems.kube.kubeconfig}", WATCH_NAMESPACE: "${systems.kube.namespace}"}
  rbac: [rbac/role.yaml]
crds: [crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Widget
  sample: widget.yaml
  ready: has(self.status) && self.status.ready == self.spec.count
  retain: false                   # CEL over the deleted CR; optional
values: {overlay: {spec.count: {minimum: 0, maximum: 3}}, mutate: [spec.count]}   # config.Values
fixtures: [widget-config.yaml]
models:
  - kind: toy.reconciler-fuzzer/v1/Gadget
    stages: readyCondition        # the default; status: adds literal fields
    after: 200ms
    dependency: {required: true, track: 1s}   # required defaults to true
budgets: {reads: 2, writes: 4}                          # defaults
timeouts: {settle: 30s, stable: 3s, backoffCap: 60s}    # defaults
```

A model's kind is neither the primary's nor another model's. Later: `systems`,
`properties`, `models[].outputs`, `primary.halted`, `.owns` and `.periodic`.

## 14. Generation, replay and shrinking

`generate.New(cfg)` reads the primary CRD's schema with the overlay, the
sample, and the fixtures. rapid draws values, and the CRD's own validation,
run in-process, rejects invalid ones. `Plan(seed)` draws 3 to 8 steps of 1
to 4 actions and at most one fault each. Step 0 creates slot `a`. About 30%
of steps hold one `restart`, `kill` or `damage`, so Restore has steps to
judge. About 30% of multi-action steps are bursts within 50 ms. A fault
draws its request from what `launch.rbac` grants, creates weighted most
(§17, entry 20). With `models:`, 2 of 7 fault draws are `stuck` or
`slowDelete` on a drawn model kind. `damage` only deletes. A fault period
lasts 0.5 s to `2 × stable`, plus the longest `track` for a model fault.
The seed fixes the plan, not timing. `generate.go` documents the weights.

**Hunt.** `hunt.Hunt` runs each seed into `<out>/seed-<n>`, keeps runs
with a new signature, replays each 5 times and shrinks it unless no replay
found it. A failed seed run is listed; a failed replay or candidate is not.
`summary.md` lists the findings with seed, reproduction rate and shrunk plan.

**Shrink.** `hunt.Shrink` takes the first of `hunt.Candidates` that still
fails, until none does: drop a step; drop an action or a fault; halve a
fault period, down to 0.5 s, or clear a faultless step's; reset one create
field to the sample's, or drop one update field; rename a slot where the
plan stays valid. A candidate fails when one of k runs gives the same
signature: k is 2, or 4 when the original reproduced in under half its
replays.

## 15. Commands

```
reconciler-fuzzer run    --config c.yaml (--plan plan.json | --seed n) --out dir [--launch-arg a]...
reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--launch-arg a]...
reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--launch-arg a]...
```

They print the findings, and exit 0 with none, 1 with findings, and 2 on a
usage or harness error. `hunt` exits 1 on any find, even after a failed
seed or an interrupt. `shrink` writes `shrunk.json`. `judge` refuses a
config whose primary is not the trace's.

## 16. Gates, examples and testing

**Make targets.** CI and the nightly call these; `make help` lists all.
`fmt`, `vet` and `verify-generate` lint and catch stale generated toy code.
`test` runs `go test ./...`. `test-envtest` runs every envtest test but the
generated tier, which `test-generated` runs: the toy's generated plans under
B0 and one shrink under B3. `test-example-<name>` runs `examples/test.sh`:
plans, listed seeds and the negative control. `hunt-<name>` hunts
`HUNT_SEEDS` seeds from `HUNT_FROM`, a fresh one by default, for at most
`HUNT_TIMEOUT`, and `nightly.yml` reports its `exit-status`: 1 a find, 2 an
error. `bug-matrix` rewrites `docs/bug-matrix.md`. No tier runs on kind.

**Toy.** `targets/toy-widget/config.yaml` drives `bin/toy-widget`, with
fixture `widget-config.yaml`. `gadget/config.yaml` adds `--gadget`: each
Widget owns a Gadget named after it, and reports `status.gadgetReady`,
which `ready` requires; its model has `dependency: {track: 1s}`. Each
config's `plans/` holds its gated bugs' and B0's plans. `gate_envtest_test.go`
runs `gate_test.go`'s table, and `TestBugMatrixIsCurrent` requires
`docs/bug-matrix.md` to render it (`-update`). B0 gives no findings and no
notes on every committed plan, and no findings on the first 20 seeds with
(model) faults. Seed 4's plan under B3 shrinks to at most 3 steps.

**Examples.** `examples/<name>` adopts cert-manager and external-secrets.
Plans and listed seeds give no findings; negative controls fail Clean.

**Testing.** Red/green TDD: a test must fail when its code is mutated.
Checks and `owner` use `trace/tracetest` traces. Envtest tests, few, live
in `*_envtest_test.go` behind `//go:build envtest`.
`readme_test.go` checks the README's commands, `workflows_test.go` the
workflows' make targets, and `design_test.go` this file's length and §2.

## 17. Decision log

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
    So Converge, not Unique, catches B2, which never settles.
16. `dropResponse` and `killAfter` skip watches, whose responses never end.
17. An instance's creator comes from the create response's UID.
18. Shared resources are judged for growth since the first create. One
    made per restart, or after its CR's delete, fails.
19. Unknown-outcome calls satisfy neither clause 4 nor clause 5.
20. Faults draw the requests `launch.rbac` grants, weighted toward creates,
    the primary and core created kinds. Damage skips the primary.
21. Restore ignores `resourceVersion`, `managedFields` and `generation`,
    which every write changes.
22. Restore lets a damaged CR rename its other resources, as a reissue does.
23. Groups split by namespace: one name in two namespaces is no duplicate.
24. The gc model removes `foregroundDeletion` and `orphan` finalizers (§7).
25. An apply patch that answers 201 created its object, as cert-manager's do.
26. Clause 5 holds `stable` after the change, for external-secrets' repair.
27. Crash allows an exit within 1 s of a faulted Lease request, assuming
    leader election is off or retries within 1 s.
28. Clause 7 counts a write in flight at `t`, whose version may come first.
29. The runner waits for the settle condition before step 0, for singletons.
30. Clause 4 counts only requests sent since the last start.
31. A Converge signature names the first unmet clause and its kind.
32. `values.fixtures` names the strings `changeFixture` sets per fixture.
33. M1 has no `periodic`; external-secrets draws only hour-long refreshes.
34. A stage model has one stage and an exact `after`; `outputs:` waits.
35. Listeners record `seen` for dependency kinds only, for Honest. A
    write's result is a read. An undecodable response is `opaque`.
36. Honest uses rule 1 only, since rule 4 blames the wrong CR.
37. Honest's read must show the controller's last write to the dependency,
    by version, since a watch can show it before the write's response.
38. `stuck` writes `Ready` False rather than nothing, so B18 and B25 see it.
39. A model fault's end writes a fresh status, for a controller without a watch.
40. Restore ignores a model kind's `status`, which the model writes.
41. A model fault that held no object gets a note, as a request fault does.
42. Backoff judges calls `5n + 1` to `10n + 1` of a run: controller-runtime's
    sixth gap is 160 ms, and `n` CRs' backoffs of one request interleave.
43. Backoff allows 409s on a kind a `holdWatch` holds, as #114 defines.
44. Converge names an unmodelled kind only with an instance that has no
    status. A model cannot track a Deployment's `observedGeneration`.
45. Only a config with `models:` draws model faults, so others draw as before.
46. Backoff keys a create on its requested name, and a CR's generation does
    not break a run on another CR's object.
47. Honest skips a dependency being deleted, which no stage makes ready, and
    counts a gone one the controller last saw live, so a stale read passes.
48. A restart, exit or failed CR write excuses `track`, since it may stop an
    honest controller from reporting.
