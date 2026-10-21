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
| Fake service | A process that serves an HTTP API, such as `crudfake`, run as a `systems:` entry (§7). |
| API map | What reads a system's requests and responses as resources: Kubernetes, or `crud`. |
| Dependency | An instance of a model kind or fake service with `dependency`, which its owning CR's readiness needs. |
| Caller | Who made a request: `controller`, `harness`, `gc`, `namespace` or `model:<resource>.<group>`. |
| Instance | One life of an object, from its create to its gone. Its UID names it. |
| Owning CR, role | The CR an instance belongs to, and the part it plays for it (§11). |

## 2. Layout and packages

`cmd/reconciler-fuzzer` holds the commands (§15), `cmd/crudfake` the fake
service, `internal/` the packages below, `targets/` the toys (§16),
`examples/<name>` adopted controllers, and `docs/` the bug matrix, failures
and findings.

| Package | Job |
| --- | --- |
| `trace` | Event types, recorder, loader, index, writer attribution. `trace/tracetest` builds traces for tests. |
| `interpose` | Listeners, the Kubernetes API map, the `API` interface, request faults, Call events. |
| `crud` | The `crud` API map. |
| `system` | The `System` interface and its value types. |
| `kube` | envtest, namespace, RBAC identity, listeners, observer, harness CRUD. |
| `fake` | A fake service as a System: its process, listeners, observer, faults and damage. |
| `crudfake` | The JSON server `cmd/crudfake` runs. |
| `model` | The `gc` and `namespace` models, and stage models. |
| `launch` | The controller process. |
| `config` | Strict YAML loader, CEL, `${...}` expansion. |
| `plan` | Plan types and `plan.json` load, save and validation. |
| `owner` | Instances, creators, owning CRs, roles and target selection. |
| `run` | The step runner, the deadline, and `Run`, which wires one run. |
| `check` | Settle condition, the checks, findings, `report.md`. |
| `generate` | Draws plans from a seed. |
| `hunt` | Loops seeds, replays, shrinks, removes duplicate findings. |

`check` and `owner` use only `trace`, `plan` and `config`, and `crudfake`
none. Only `check` decides what is a bug.

## 3. The System interface

`system.System` has `Name() string`, `Start(ctx, env) (Endpoints, error)`,
`Snapshot(ctx) ([]Resource, error)`, `Pending() bool` and
`Fault(plan.Fault) (end func(), err error)`. `Endpoints` holds the
controller and harness listeners and `Vars`, which expand as
`${systems.<name>.<key>}` in launch args and env. Request faults go to a
system's controller listener. Model faults go to the stage model or fake
service that `Takes` them. Damage goes to the system the target names.
Checks read `work` events, never `Pending`.

## 4. Trace

The recorder appends events to `trace.jsonl`, one JSON object per line. It
assigns `Seq`, a total order, and `At`, the time since the run started.
Each event sets one field: `run` (the config the checks need, after the
fixtures), `phase`, `action`, `fault`, `call`, `watchEnd`, `version` (an
observed body), `work` (a model's or fake's pending work), `seen` (what a
response showed a caller, §5) or `proc` (a controller start or exit).
`trace/event.go` documents each.

A `Ref` is `{system, group, resource, namespace, name}`. The trace stores
bodies, not readiness or gone states, so `judge` rejudges an old trace
under a new config. A query at `t` sees events with `At <= t`. A version's
writer is the caller whose response carried its ref and resourceVersion,
or `cluster`. A call of **unknown outcome** (`Call.Unknown`) reached the
server, but its caller never saw the response.

## 5. Listeners, API maps and request faults

`interpose.Listen` serves plain HTTP on `127.0.0.1:0` for one caller and
forwards with its credentials. A watch's end records a `watchEnd`. A write
whose client leaves still goes on upstream.

**API maps.** A nil `Options.API` is the Kubernetes map, which reads paths,
JSON and protobuf metadata and watch frames. An `interpose.API` instead
classifies each request, reads the objects a 2xx body shows, and writes an
injected error in its own form. A request it cannot place is **unmapped**:
its call has `unmapped` set and no resource, so no check judges it, and
the report notes it.

A fault matches by verb, group, resource, subresource and name. `nth: k`
picks the k-th matching request since the fault started; `nth: 0` picks
every one. The first active fault that picks a request applies:
`error{code, retryAfter}` answers with the map's error, `delay{d}` holds
the request, and `drop` aborts it unforwarded, with status 0.
`dropResponse` forwards it, records the answer, then aborts the client's
connection. `killAfter` also kills the controller, harness-caused, before
aborting. Neither picks or counts a watch.

**Seen.** A listener records a `seen` for each object of its `Seen` kinds,
or of every kind with `SeenAll`, that a 2xx response shows its caller. An
undecodable response records an `opaque` seen of its ref. A response of
unknown outcome shows nothing. `Shown` hears every object any response
carries, unknown outcomes included.

**The `crud` map** (`crud.New(system)`) reads `GET /<c>` as `list` (its
body's `items` are shown), `POST /<c>` as `create` (`requestName` is the
body's `name`), and `GET`, `PUT`, `PATCH` and `DELETE /<c>/<id>` as `get`,
`update`, `patch` and `delete`. `<c>` is a collection the fake's listing
names; any other path or method is unmapped. A ref is `{system, resource:
c, name: id}`. A shown object needs a string `id` and a number or string
`version`, its resourceVersion; its UID is `<system>/<c>/<id>`. The id
field and the ignore list are fixed. Restore ignores `version`. An
injected error is `{"error": {"code", "message"}}`.

## 6. Kubernetes system

`kube.Start` starts envtest, the run namespace, the controller's identity,
both listeners and the observer, and creates the fixtures, namespaced ones
in the run namespace unless they name another. `Vars` holds `kubeconfig`,
`namespace` and `url`. Harness actions return once the observer has
recorded their effect. The system's poll interval is `stable`. The
observer list-watches kinds cluster-wide, from the configured kinds and
any kind a listener records a call on, and records one `version` per
(ref, resourceVersion). `Subscribe` hands each version to the models.

## 7. Models and fake services

`run.Run` starts the fake services and models after `kube` and before the
controller. A model writes through its own listener from `k.Listen`, hears
versions through `k.Subscribe`, and records `work` as its work starts and
ends. `gc` collects an instance whose owners a live read finds gone,
following Kubernetes' cross-scope, foreground and orphan rules (`gc.go`).
`namespace` finishes a terminating namespace. `model.NewStage` makes a
stage model per `models:` entry, named `trace.ModelCaller` of its kind,
which writes `Ready` True and the `status:` fields `after` a generation's
first version. `stuck` writes matching objects `Ready` False, and
`slowDelete` adds a finalizer; either's end writes `Ready` True.

**Fake services.** `fake.New` makes a System of a `systems:` entry with a
`process`. Start runs it in the config's directory with
`RECONCILER_FUZZER_ADDR=127.0.0.1:<port>`, logging to `<out>/<name>.log`,
and opens a controller and a harness listener with the entry's map, with
`SeenAll` under `dependency`. `Vars` holds `url`, the controller listener.
An exit the harness did not cause, an unreadable listing, and an id listed
again after it was gone are harness errors; the runner checks for them at
each settle poll. A fake serves this on its own address, which only the
harness uses; the controller listener answers 404 there:

- `GET /_fake/resources` answers `{"collections", "pending", "resources":
  [{"collection", "body"}]}`; `pending` says a state change is scheduled.
- `POST /_fake/faults` with `{"effect": "stuck" | "slowDelete",
  "collection", "id"}`, either empty for all, answers 201 with `{"id"}`,
  and `DELETE /_fake/faults/<id>` ends it.

The fake's observer lists every 20 ms and hears `Shown`. It records each
object's version when it is newer than the last recorded, a deleted
version when a known object leaves the listing, and `work` when `pending`
changes. `Takes` accepts a model fault whose `system` is the fake's.
`Patch` and `Delete` go through the harness listener and return once the
observer has recorded the result.

**`crudfake`** serves the `crud` map's API. Its YAML file maps each
collection to `unique` fields (a repeat answers 409), the states
`pending`, `ready` and `deleting`, `createDelay` and `deleteDelay`. A
resource is the client's object plus `id` (`<collection>-<n>`), `version`
(raised by each change) and `state`, which a client may not set. A new
resource is `pending`, then `ready`; `DELETE` answers 202 and sets
`deleting`, and the resource leaves after `deleteDelay`. `GET /<c>?f=v`
filters on a top-level string, or on another value's JSON text. Ids are
never reused. A fault on an unknown collection answers 400. Unknown ids
answer 404, writes to a
deleting resource 409. `stuck` holds pending resources, new ones included,
and `slowDelete` deleting ones, until the fault's end and a delay more.

## 8. Plans

An action's `do` is `create` (the sample, or `object`; it first waits
until the slot's previous CR is gone), `update` (a merge `patch`),
`delete` (optionally `Foreground`), `restart` (SIGTERM, then start),
`kill` (SIGKILL; the controller stays down until end faults), `damage` (a
delete, or a merge `patch`, of an instance the controller created) or
`changeFixture` (a merge `patch`, or a delete, of a fixture). `plan.go`
documents each.

A `target` has `system` (default `kube`), `group`, `resource`, `namespace`,
`name`, `role` and `nth` (default 1). Empty fields match anything.
`owner.World.Select` resolves it when the action runs: the `nth` live
instance, in creation order, that the controller (for `damage`) or the
harness created, owned by the slot's live CR when a slot is set. A target
that matches nothing records an error `no instance matches`.

Actions run in order, each at its optional `at` offset. A fault sets one
effect: a request fault (§5), or a model fault (§7) with no verb,
subresource or `nth`. A fault's `system` defaults to `kube`; the runner
refuses an unknown one. On `kube`, a model fault and a target need a
`resource`; on a fake, an empty one matches every collection. `plan.Load`
decodes strictly and validates each action's slot and fields (`plan.go`).

## 9. Step runner

`Runner.Execute` runs the plan's steps, then step `planSteps`, which
deletes every live CR, and step `planSteps + 1`, which restarts the
controller (§17, entry 4).

**Deadline.** `endFaults + settle + min(faultPeriod, backoffCap) + maxPoll`,
where `maxPoll` is the longest poll interval of the systems the step's
faults and `damage` and `changeFixture` actions name, and of every fake
when the step creates, updates or deletes a CR. A fake's poll is its
`poll`, by default `stable`. The runner gives a model fault to the first
model or fake that `Takes` it; none is a harness error. A create that
waits `settle` for its slot ends the plan after its step. Any other action
error, except an unmatched target, is a harness error.

## 10. Settle condition

`check.Settled(ix, t, spec)` returns the unmet clauses at `t`, or none. It
judges the step whose fault period started last at or before `t`.

1. Every live CR satisfies `ready`, or `halted` after an unknown-outcome
   controller call on it or on an instance only it owns, and
2. carries no `deletionTimestamp`.
3. The controller is up at `t` and has been up for at least `stable`.
4. The controller made a 2xx request, not ignored and not of unknown
   outcome, that it sent since its last start.
5. For each object the step's `damage` and `changeFixture` actions changed,
   the controller looked at it after the change: a 2xx or 404 request,
   not of unknown outcome, that covers it (`Ref.Covers`), or a watch that
   covers it, opened before the change and open at `t`. The clause also
   needs `stable` since each change, and lapses at `endFaults + 1.5 ×` the
   longest poll of the changed objects' systems.
6. No model or fake has work pending at `t` (`Index.Pending`).
7. No changed write, not ignored, ended in `(t - stable, t]` or was in
   flight at `t`. Over each system's poll interval up to `t`, the
   controller made at most `reads × max(1, live CRs)` gets and lists and
   at most `writes × max(1, live CRs)` mutating requests of that system,
   no-op writes included. Kube's poll interval is `stable`.

`ready` and `halted` are CEL over `self`, the CR; an error fails clause 1.
No check counts what `trace.Ignored` lists: Events, Leases, TokenReview,
access reviews, non-resource URLs and unmapped requests.
`check.FirstSettled` finds the earliest settle time in `[from, to]`.

## 11. Instances, owning CRs and roles

`owner.New(ix, rules)` indexes every instance the trace shows. `rules`
come from the config, or else the run event.

- An instance is a (ref, UID), from its first version until a deleted
  version, or one its system's `gone` CEL accepts.
- Its **creator** is the caller whose create, or 201 apply, answered with
  its UID, or `cluster`.
- Its **owning CR** at `t` comes from the first rule that matches
  (`owner.go`): 1, its ownerReferences reach a primary instance; 2, its
  system's `owner` CEL names a CR by UID, `namespace/name` or name; 3, a
  `primary.owns` entry over a CR's body names it; 4, otherwise, the set
  of CRs live at its creation. An empty set marks a **shared** instance.
- Its **role** is its system's `role` CEL over its first body. Without
  one, a kube instance's is its `generateName`, or else its name, and a
  fake's is empty.
- The **controller's resources** at `t` are the live instances whose
  creator is `controller`, minus the primary kind and ignored kinds (§10).
- A **group** is a (system, group, resource, namespace, owning CRs, role).

## 12. Checks

Every check is `func(*trace.Index, Spec) []Finding`. A finding's signature
is its check, summary and `kind` (`system/group/resource`). `check.Judge`
runs every check, and notes each action error, each request fault that
picked no request, each stage-model fault that held no object, and each
distinct unmapped (system, method, path). `Spec` comes from the config,
or else the run event. A step's **end** is its `confirmed` time. Clean,
Restore and Unique judge only settled steps (§17, entry 15).

**Converge.** The settle condition holds by the deadline, and still holds
at `t* + stable`. The summary names the first unmet clause, such as
`did not settle (clause 7)`. `backoff-bound` labels a finding whose
deadline `backoffCap` set. `converge.go` suggests missing `models:`.

**Crash.** A controller exit the harness did not cause fails its step.

**Clean.** At the end of a settled step with a `delete` action, a
controller resource fails when its rule 1 to 3 CR is gone, or its rule-4
set is non-empty and every CR in it is gone. At the end of step `planSteps
+ 1`, every unshared controller resource fails. `primary.retain` allows a
resource. Shared resources are judged only for growth since the first
`create`. Summary: `left resources behind`.

**Restore.** It judges a settled step with no `create`, `update`, `delete`
or `changeFixture` action after a settled step, comparing the controller's
resources at the two ends by group, without volatile fields (entries 21
and 40, and the map's). Where no `damage` touched the owning CR, each
instance must stay, with no new one and an equal body: `replaced
resources`, `changed resources`. Where one did, each damaged group must be
live again: `did not restore damaged resources` (entry 22).

**Unique.** At each settled step's end, a group holds at most one live
controller instance. Summary: `duplicate resources`.

**Honest** and Backoff judge the whole run, fault periods included
(`honest.go`, `backoff.go`). A CR's dependencies are the instances of
dependency kinds, or of a dependency fake's collections, that it owns by
rules 1 to 3. When the controller writes the CR `ready`, or raises its
observedGeneration while `ready`, its last `seen` of each dependency must
show its last write's version or a later one, which its model's or fake's
`ready` accepts: `reported ready before seeing its dependency ready`. With
`required`, it needs a dependency of each kind: `reported ready with no
dependency`. With `track: d`, a CR fails that stays `ready` `d` after the
controller saw a dependency not ready: `stayed ready after seeing its
dependency not ready`.

**Backoff.** Failing controller calls with one system, verb, object,
subresource and status form a **run**; a create's object is its
`requestName`. A success, or the CR's new generation, breaks it. With `n`
CRs live, it fails when each gap between calls `5n + 1` to `10n + 1` is
under 100 ms: `retried a failing request in a tight loop`.

**Corpus.** `check/testdata/corpus/` holds traces of known bugs and false
positives, with the findings each needs (`corpus_test.go`).

## 13. Config

One YAML file, decoded strictly; an unknown key names its line. Paths
resolve against the file's directory. `config.go` documents each key.

```yaml
launch:
  binary: ../../bin/toy-bucket
  args: [--bug=0]
  env: {KUBECONFIG: "${systems.kube.kubeconfig}", CLOUD_ENDPOINT: "${systems.cloud.url}"}
  rbac: [rbac/role.yaml]
crds: [crds/]
primary:
  kind: toy.reconciler-fuzzer/v1/Bucket
  sample: bucket.yaml
  ready: has(self.status) && self.status.ready
  halted: false                   # optional: done after an unknown outcome
  retain: false                   # optional: CEL over the deleted CR
  owns: [{kind: "cloud:buckets", name: self.status.bucketID}]   # rule 3; kube kinds are group/version/Kind
values: {overlay: {spec.region: {enum: [eu, us]}}, mutate: [spec.region]}
models: [{kind: toy.reconciler-fuzzer/v1/Gadget, after: 200ms, dependency: {track: 1s}}]
systems:
  - {name: kube, owner: "self.metadata.labels['widget-uid']"}   # kube takes only owner
  - name: cloud
    process: {command: [../../bin/crudfake, --config, cloud.yaml]}
    api: crud
    owner: self.labels['k8s-uid']           # rule 2
    role: self.labels['role']               # optional
    ready: self.state == 'READY'            # required with dependency
    gone: self.state == 'DELETING'          # optional
    dependency: {required: true}
    poll: 1s                                # defaults to stable
budgets: {reads: 2, writes: 4}                          # defaults
timeouts: {settle: 30s, stable: 3s, backoffCap: 60s}    # defaults
```

A model's kind is neither the primary's nor another model's; its stage is
`readyCondition`, and `dependency.required` defaults to true. A fake's
name matches `[a-z][a-z0-9-]*`. A `command[0]` with a slash resolves
against the file's directory. Later: `properties`, `models[].outputs`,
`.periodic`, and a fake's `token`, `caFile` and `caBundle`.

## 14. Generation, replay and shrinking

`generate.New(cfg)` reads the primary CRD's schema with the overlay, the
sample, and the fixtures. rapid draws values, and the CRD's own validation,
run in-process, rejects invalid ones. `Plan(seed)` draws 3 to 8 steps of 1
to 4 actions and at most one fault each. Step 0 creates slot `a`. About 30%
of steps hold one `restart`, `kill` or `damage`, so Restore has steps to
judge. About 30% of multi-action steps are bursts within 50 ms. A kube
fault draws its request from what `launch.rbac` grants, creates weighted
most (§17, entry 20). With `models:`, 2 of 7 fault draws are `stuck` or
`slowDelete`. With fakes, half of the fault draws and of the `damage`
draws name a fake: a request fault by verb alone, or a model fault, and a
target by system alone. `damage` only deletes. About 1 in 4 CR deletes is
`Foreground`. A fault period lasts 0.5 s to `2 × stable`, plus the longest
`track` for a model fault. `generate.go` documents the weights.

**Hunt.** `hunt.Hunt` runs each seed, keeps runs with a new signature,
replays each 5 times and shrinks it (`shrink.go`). `summary.md` lists the
findings with seed, reproduction rate and shrunk plan.

## 15. Commands

```
reconciler-fuzzer run    --config c.yaml (--plan plan.json | --seed n) --out dir [--launch-arg a]...
reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--launch-arg a]...
reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--launch-arg a]...
crudfake --config cloud.yaml [--listen addr]
```

They print the findings, and exit 0 with none, 1 with findings, and 2 on a
usage or harness error. `crudfake` listens on `--listen`, or else
`RECONCILER_FUZZER_ADDR`.

## 16. Gates, examples and testing

**Make targets.** `make help` lists them; CI and the nightly call them.
`test-generated` runs the generated tier, and `test-envtest` every other
envtest test. `bug-matrix` rewrites `docs/bug-matrix.md`.

**Toys.** `targets/toy-widget` holds the Widget configs: `gadget/` adds a
modelled Gadget, and `tenant/` and `grant/` cross scopes. In
`targets/toy-bucket`, each Bucket CR owns one bucket in the `crudfake`
system `cloud`, named after its UID and labelled `k8s-uid`. B0 adopts its
bucket after a 409, waits for `READY`, records `status.bucketID`, polls it,
recreates it when gone, and deletes it behind a finalizer. Its bugs start
at B30. `halted/` runs `--halt`, which stops after an unknown-outcome
create, under `primary.halted`. Without it, the same plan fails Converge.

Each config's `plans/` holds its gated bugs' and B0's plans. A toy's
`gate_envtest_test.go` runs `gate_test.go`'s table, which
`docs/bug-matrix.md` renders. B0 gives no findings and no notes on every
committed plan, and no findings on the first 20 seeds of each config
(toy-bucket: the first 20 that draw a fault on `cloud`).

**Examples.** `examples/<name>` adopts cert-manager and external-secrets.
Plans and listed seeds give no findings; negative controls fail Clean.

**Testing.** Red/green TDD: a test must fail when its code is mutated.
Checks and `owner` use `trace/tracetest` traces. Envtest tests, few, live
in `*_envtest_test.go` behind `//go:build envtest`. Root tests check the
README's commands, the workflows' make targets, and this file.

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
8. Clause 7's budgets count per live CR per poll interval of each system.
9. A step the trace cannot decide gets a note, not a finding.
10. The supervisor backs off between restarts, against crash loops.
11. The Kubernetes system's poll interval is `stable`.
12. Clause 5 counts an open watch, so listeners record `watchEnd`.
13. Models report pending work as `work` events, so `judge` needs no model.
14. A named kube instance's role is its name, so Restore compares it (B14).
15. Clean, Restore and Unique judge only settled steps after a settled one.
16. `dropResponse` and `killAfter` skip watches, whose responses never end.
17. An instance's creator comes from the create response's UID.
18. Shared resources are judged for growth since the first create.
19. Unknown-outcome calls satisfy neither clause 4 nor clause 5.
20. Kube faults draw what `launch.rbac` grants, weighted toward creates.
21. Restore ignores `resourceVersion`, `managedFields` and `generation`.
22. Restore lets a damaged CR rename its other resources, as a reissue does.
23. Groups split by namespace: one name in two namespaces is no duplicate.
24. The gc model removes `foregroundDeletion` and `orphan` finalizers.
25. An apply patch that answers 201 created its object, as cert-manager's do.
26. Clause 5 holds `stable` after the change, for external-secrets' repair.
27. Crash allows an exit within 1 s of a faulted Lease request.
28. Clause 7 counts a write in flight at `t`, whose version may come first.
29. The runner waits for the settle condition before step 0, for singletons.
30. Clause 4 counts only requests sent since the last start.
31. A Converge signature names the first unmet clause and its kind.
32. `values.fixtures` names the strings `changeFixture` sets per fixture.
33. M1 has no `periodic`; external-secrets draws only hour-long refreshes.
34. A stage model has one stage and an exact `after`; `outputs:` waits.
35. Listeners record `seen` for dependency kinds only, for Honest.
36. Honest ignores rule 4, which blames the wrong CR.
37. Honest's read must show the controller's last write to the dependency.
38. `stuck` writes `Ready` False rather than nothing, so B18 and B25 see it.
39. A model fault's end writes a fresh status, for a controller without a watch.
40. Restore ignores a model kind's `status`, which the model writes.
41. A model fault that held no object gets a note, as a request fault does.
42. Backoff judges calls `5n + 1` to `10n + 1`, past controller-runtime's 160 ms gap.
43. Backoff allows 409s on a kind a `holdWatch` holds, as #114 defines.
44. Converge names an unmodelled kind only with an instance that has no status.
45. Only a config with `models:` draws model faults on kube.
46. Backoff keys a create on its requested name.
47. Honest skips a deleting dependency, and counts a gone one last seen live, and one the controller saw before the observer did.
48. A restart, exit or failed CR write excuses `track`.
49. The observer watches cluster-wide, and refuses no kind for its scope (#38).
50. Every check that uses owning-CR rule 1 also uses rules 2 and 3.
51. The gc follows Kubernetes' cross-scope owner rules.
52. A `delete` may be `Foreground` (B29). `Orphan` would fail Clean.
53. A group's owner key is its CR set, not its rule.
54. A fake service is a System, so every check judges it without naming it.
55. An API map classifies requests. An unmapped one is noted, never judged.
56. A fake's `gone` CEL ends an instance, so a deleting bucket is gone.
57. A fake's default role is empty, so two buckets of one CR are duplicates.
58. The deadline's and clause 5's poll is the longest a step's systems have. A CR change touches every fake.
59. `halted` counts a CR as done only after an unknown-outcome call for it: on it, or on an instance it alone owns, by any rule. A `drop` does not count, since no instance links it to a CR.
60. A fake's model fault and damage may name no collection.
61. A fake's model fault that held nothing gets no note.
63. The ignore list names kube kinds, so no fake collection is ignored.
