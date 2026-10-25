# reconciler-fuzzer design

Issue #114 holds the proposal and its reasons. This document states what the
code does. It stays under 530 lines. Where the code documents a rule, this
document names it.

## 1. Terms

| Term | Meaning |
| --- | --- |
| Plan | The list of steps for one run, stored as `plan.json`. |
| Step | One unit of a run: fault period, end faults, settle wait, confirm window. |
| Action | Something the harness does once in a step, such as creating a CR or damaging an object. |
| CR | An object of a kind that the config's `primaries` lists (§13). |
| Slot | A letter, such as `a`, that names one CR across a plan's steps. |
| Fault | A problem the harness causes during a fault period and then ends. |
| Damage | A change the harness makes to an object the controller created. It is never undone. |
| Settle condition | The one test that ends a step's wait and decides whether the controller converged. |
| Confirm window | One more `stable` period after the settle condition holds. |
| System | Anything outside the controller that it talks to, behind the `System` interface. |
| Model | An in-process stand-in for another controller: the garbage collector, or a stage model of a `models:` kind. |
| Fake service | A process that serves an HTTP API, run as a `systems:` entry (§7): `crudfake`, or an emulator such as a Vault dev server or moto. |
| API map | What reads a system's requests and responses as resources: Kubernetes, `crud`, `vault` or `s3`. |
| Property | A config CEL expression over every system's resources that must hold (§12). |
| Dependency | An instance of a model kind or fake service with `dependency`, which its owning CR's readiness needs. |
| Caller | Who made a request: `controller`, `harness`, `gc`, `namespace` or `model:<resource>.<group>`. |
| Instance | One life of an object, from its create to its gone. Its UID names it. |
| Owning CR, role | The CR an instance belongs to, and the part it plays for it (§11). |

## 2. Layout and packages

`cmd/reconciler-fuzzer` holds the commands (§15), `cmd/crudfake` the fake
service, `internal/` the packages below, `targets/` the toys (§16), `examples/`
adopted controllers, and `docs/` the bug matrix, failures and findings.

| Package | Job |
| --- | --- |
| `trace` | Event types, recorder, loader, index, writer attribution. `trace/tracetest` builds traces for tests. |
| `interpose` | Listeners, the Kubernetes API map, the `API` interface, request faults, Call events. |
| `crud` | The `crud` API map. |
| `vault` | The `vault` API map: KV version 2 mounts, and their listing. |
| `s3` | The `s3` API map: buckets and objects, and their listing. |
| `system` | The `System` interface and its value types. |
| `kube` | envtest, namespace, RBAC identity, listeners, observer, harness CRUD. |
| `fake` | A fake service as a System: its process, listeners, observer, fixtures, faults and damage. |
| `crudfake` | The JSON server `cmd/crudfake` runs, with asynchronous states and strict 404 and 409 answers. |
| `model` | The `gc` and `namespace` models, and stage models. |
| `launch` | The controller process. |
| `config` | Strict YAML loader, CEL, `${...}` expansion. |
| `plan` | Plan types and `plan.json` load, save and validation. |
| `owner` | Instances, creators, owning CRs, roles and target selection. |
| `run` | The step runner, the deadline, and `Run`, which wires one run. |
| `check` | Settle condition, the checks, findings, `report.md`. |
| `generate` | Draws plans from a seed. |
| `hunt` | Loops seeds, replays, shrinks, removes duplicate findings. |

`check` uses only `trace`, `plan`, `config` and `owner`, and `crudfake` none.
Only `check` decides what is a bug.

## 3. The System interface

`system.System` has `Name() string`, `Start(ctx, env) (Endpoints, error)`,
`Snapshot(ctx) ([]Resource, error)`, `Pending() bool` and `Fault(plan.Fault)
(end func(), err error)`. `Endpoints` holds the controller and harness
listeners and `Vars`, which expand as `${systems.<name>.<key>}` in launch args
and env, and kube fixtures. Request faults go to a system's controller
listener. Model faults go to the stage model or fake service that `Takes` them.
Damage goes to the system the target names. Checks read `work` events, never
`Pending`.

## 4. Trace

The recorder appends events to `trace.jsonl`, one JSON object per line. It
assigns `Seq`, a total order, and `At`, the time since the run started. Each
event sets one field: `run` (the config the checks need, after the fixtures),
`phase`, `action`, `fault`, `call`, `watchEnd`, `version` (an observed body),
`work` (a model's or fake's pending work), `seen` (what a response showed a
caller, §5) or `proc` (a controller start or exit). `event.go` documents each.

A `Ref` is `{system, group, resource, namespace, name}`. The trace stores
bodies, not readiness or gone states, so `judge` rejudges an old trace under a
new config. A query at `t` sees events with `At <= t`. A version's writer is
the caller whose response carried its ref and resourceVersion, or `cluster`. A
call of **unknown outcome** (`Call.Unknown`) reached the server, but its caller
never saw the response.

## 5. Listeners, API maps and request faults

`interpose.Listen` serves plain HTTP on `127.0.0.1:0` for one caller and
forwards with its credentials. A watch's end records a `watchEnd`. A write
whose client leaves still goes on upstream.

**API maps.** `config.APIMaps` holds each map's Restore Ignore, model faults
and token need. A nil `Options.API` is the Kubernetes map, which reads paths,
JSON and protobuf metadata and watch frames. An `interpose.API` instead
classifies each request, reads the objects a 2xx response's header and body
show, and writes an injected error in its own form. A request it cannot place
is **unmapped**: its call has `unmapped` set and no resource, so no check
judges it, and the report notes it. A request it places outside any resource,
such as Vault's `sys/` and `auth/` paths, has no resource and is ignored. A
**partial** object names its instance without its whole body, as an S3 `PUT`
answer does: it gives the call's result, but no `seen` and no version. A fake's
listeners forward the caller's `Authorization`; the kube listeners drop it.

A fault matches by verb, group, resource, subresource and name. An empty field
matches anything. Subresource `-` matches only a request without one. `nth: k`
picks the k-th match since the fault started; `nth: 0` picks every one. The
first active fault that picks a request applies: `error{code, retryAfter}`
answers with the map's error, `delay{d}` holds the request, and `drop` aborts
it unforwarded, with status 0. `dropResponse` forwards it, records the answer,
then aborts the client's connection. `killAfter` also kills the controller,
harness-caused, before aborting. Neither picks or counts a watch. `holdWatch`
names a kube kind by `group` (empty is the core group) and `resource`, and
picks no request: while it lasts, every controller watch of the kind forwards
nothing, and at its end each stream delivers what it held, in order.

**Seen.** A listener records a `seen` for each object of its `Seen` kinds, or
of every kind with `SeenAll`, that a 2xx response shows its caller. An
undecodable response records an `opaque` seen of its ref. A response of unknown
outcome shows nothing. `Shown` hears every object any response carries, unknown
outcomes included.

**The `crud` map** reads `GET /<c>` as `list`, `POST /<c>` as `create`
(`requestName` is the body's `name`), and `GET`, `PUT`, `PATCH` and `DELETE
/<c>/<id>` as `get`, `update`, `patch` and `delete`, for each collection `<c>`
the listing names. A ref is `{system, resource: c, name: id}`; an object needs
a string `id` and a `version`, its resourceVersion; its UID is
`<system>/<c>/<id>`. Restore ignores `version`. `crud.go` documents its error.

**The `vault` and `s3` maps** read a Vault server's KV version 2 mounts, and an
S3 server's buckets and objects. Their package comments define them. Paths and
names are reused, so each numbers a name's instances: a UID is
`<system>/<resource>/[<namespace>/]<name>/<n>`, and `n` rises after a 2xx
delete that ends the instance, or after the observer records it gone
(`fake.Ender`). A write is `create` when the map holds no live instance of its
name, and `update` otherwise.

## 6. Kubernetes system

`kube.Start` starts envtest, the run namespace, the controller's identity, both
listeners and the observer, and creates the fixtures. Fixtures expand
`${systems.<fake>.<key>}`. `Vars` holds `kubeconfig`, `namespace` and `url`.
Harness actions return once the observer has recorded their effect. The
observer list-watches cluster-wide each installed CRD, each served kind the
config or a fixture names, and each served kind `launch.rbac` names that is not
ignored (§10). These are the run event's `kinds`. It then watches any kind a
listener records a call on, and records one `version` per
(ref, resourceVersion).

## 7. Models and fake services

`run.Run` starts the fakes, `kube`, the models, then the controller. A model
writes through its own listener from `k.Listen`, hears versions through
`k.Subscribe`, and records `work` as its work starts and ends. `gc` collects an
instance whose owners a live read finds gone, following Kubernetes'
cross-scope, foreground and orphan rules (`gc.go`). `namespace` finishes a
terminating namespace. `model.NewStage` makes a stage model per `models:`
entry, named `trace.ModelCaller` of its kind, which writes `Ready` True and the
`status:` fields `after` a generation's first version. `stuck` writes matching
objects `Ready` False and the `stuckStatus:` fields instead, and `slowDelete`
adds a finalizer. Either's end writes `Ready` True and the `status:` fields.

**Fake services.** `fake.New` makes a System of a `systems:` entry with a
`process`. Start runs it in the config's directory with
`RECONCILER_FUZZER_ADDR=127.0.0.1:<port>`, and `${addr}` and `${port}` in its
command expanded, logging to `<out>/<name>.log`. It opens a controller and a
harness listener with the entry's map, with `SeenAll` under `dependency`, and
creates the entry's fixtures through the harness listener. `Vars` holds `url`,
the controller listener, and `token`. An exit the harness did not cause, an
unreadable listing, and an id listed again after it was gone are harness
errors; the runner checks for them at each settle poll. A `crud` fake serves
`fake.ResourcesPath` and `fake.FaultsPath` on its own address, which only the
harness uses; the controller listener answers 404 there.

An emulator's map is a `fake.Lister`, which lists it through its own API. The
observer lists every 20 ms and hears `Shown`. It records each object's version
when newer than the last recorded, a deleted version when a known object leaves
the listing, and `work` when `pending` changes. `Takes` accepts a model fault
on a `crud` fake. `Patch` and `Delete` send the map's `Write` through the
harness listener, and return after an observer pass that started after the
answer. An answer the map's `Gone` accepts is `system.ErrGone`.

## 8. Plans

An action's `do` is `create`, `update`, `delete` (optionally `Foreground`),
`restart`, `kill` (the controller stays down until the fault period ends or a
slot wait restarts it, §9), `stop` (for good), `damage` (of an instance the
controller created) or `changeFixture` (of a fixture, on any system). After a
`stop`, a plan may only `delete` and `changeFixture`. `plan.go` documents each.

A `target` has `system` (default `kube`), `group`, `resource`, `namespace`,
`name`, `role` and `nth` (default 1). Empty fields match anything.
`owner.World.Select` resolves it when the action runs: the `nth` live instance,
in creation order, that the controller (for `damage`) or the harness created,
whose root (§11) holds the slot's newest live CR when a slot is set. A target
that matches nothing records the error `no instance matches`, which names the
slot's kind when the slot holds an object that is not a CR.

Actions run in order, each at its optional `at` offset. A fault sets one
effect: a request fault or `holdWatch` (§5), or a model fault (§7) with no
verb, subresource or `nth`. A fault's `system` defaults to `kube`; the runner
refuses an unknown one. On `kube`, a model fault and a target need a
`resource`; on a fake, an empty one matches every collection. `plan.Load`
decodes strictly and validates each action's slot and fields (`plan.go`). Two
slots' CRs clash only with equal group, kind, namespace and name. `run.Run`
refuses a plan that creates an object of an unlisted kind (`ValidateKinds`).

## 9. Step runner

`Runner.Execute` runs the plan's steps. Unless a `stop` ran, it then runs step
`planSteps`, which deletes each slot's live CR, and step `planSteps + 1`, which
restarts the controller (§17, entry 4).

**Deadline.** `endFaults + settle + min(faults, backoffCap) + maxPoll`.
`endFaults` is the end of the fault period. `faults` runs from the step's start
until then, or until a create's slot wait ends the faults sooner. `maxPoll` is
the longest poll interval of the systems the step's faults and `damage` and
`changeFixture` actions name, and of every fake when the step changes a CR. The
runner gives a model fault to the first model or fake that `Takes` it; none is a
harness error. A create into an emptied slot waits until its previous CR no
longer exists. Once `period` has passed, the wait ends the step's faults and
restarts a killed controller. It gives up `deadline - endFaults` after the later
of the faults' end and the slot's delete, and the plan ends after the step. A
`stop` waits the same way for each CR that a `delete` removed, then sends
SIGTERM even if one stays. Any other action error, except an unmatched target,
is a harness error.

## 10. Settle condition

`check.Settled(ix, t, spec)` returns the unmet clauses at `t`, or none. It
judges the step whose fault period started last at or before `t`. After a
`stop`, only clauses 2, 6 and 7 apply, and clause 2 only to CRs being deleted
at the stop.

1. Every live CR satisfies its kind's `ready`, or `halted` after an
   unknown-outcome controller call on it or on an instance only it owns, and
2. carries no `deletionTimestamp`.
3. The controller is up at `t` and has been up for at least `stable`.
4. The controller made a 2xx request, not ignored and not of unknown
   outcome, that it sent since its last start.
5. For each object the step's `damage` and `changeFixture` actions changed,
   the controller looked at it after the change: a 2xx or 404 request,
   not of unknown outcome, that covers it (`Ref.Covers`), or a watch that
   covers it, opened before the change and open at `t`. The clause also
   needs `stable` since each change, and lapses at `endFaults + 1.5 ×` the
   longest poll of the changed objects' systems. Without a watch across
   the change, it needs `stable` since the first request that looked; a
   held watch, `stable` since its `holdWatch` ended.
6. No model or fake has work pending at `t` (`Index.Pending`).
7. No changed write, not ignored, ended in `(t - stable, t]` or was in
   flight at `t`. Over each system's poll interval up to `t`, the
   controller made at most `reads × max(1, live CRs)` gets and lists and
   at most `writes × max(1, live CRs)` mutating requests of that system,
   no-op writes included. Kube's poll interval is `stable`. A controller
   write of a CR that changes only its kind's `periodic.paths`, and comes at
   least `every / 2` after the previous one of that CR, is not a changed
   write; the budgets still count it. A path's `[*]` names every list item.
   A fake's write without a resourceVersion is changed only if the
   observer records a new version within 200 ms (`Index.Changed`).

`ready` and `halted` are CEL over `self`, the CR; an error fails clause 1. No
check counts what `trace.Ignored` lists: Events, Leases, TokenReview, access
reviews, non-resource URLs and unmapped requests.

## 11. Instances, owning CRs and roles

`owner.New(ix, rules)` indexes every instance the trace shows. `rules` come
from the config, or else the run event.

- An instance is a (ref, UID), from its first version until a deleted
  version, or one its system's `gone` CEL accepts.
- Its **creator** is the caller whose create, or 201 apply, answered with
  its UID, or `cluster`.
- Its **owning CR** at `t` comes from the first rule that matches (`owner.go`):
  1, its ownerReferences reach a CR; 2, its system's `owner` CEL names a CR by
  UID, `namespace/name` or name, unless CRs of two kinds share that name; 3, an
  `owns` entry over a CR of its entry's kind names it; 4, otherwise, the set of
  other CRs live at its creation that the controller did not create (entry 79).
  While its ownerReferences reach a live instance that the controller did not
  create, directly or through live instances that it did create, and none
  reaches a CR, rule 1 gives that **unlisted owner** and no CR (entry 80). An
  instance with no CR is **shared**. Its **root** is its owner, or, when that
  names one CR that the controller created, that CR's root.
- Its **role** is its system's `role` CEL over its first body. Without
  one, a kube instance's is its `generateName`, or else its name, and a
  fake's is empty.
- The **controller's resources** at `t` are its live instances, minus
  ignored kinds (§10).
- A **group** is a (system, group, resource, namespace, owning CRs or
  unlisted owner, role).

## 12. Checks

Every check is `func(*trace.Index, Spec) []Finding`. A finding's signature is
its check, summary and `kind` (`system/group/resource`). `check.Judge` runs
every check, and notes each action error, each request fault that picked no
request, each `holdWatch` while the controller had no watch of its kind, each
stage-model fault that held no object, and each distinct unmapped (system,
method, path). `Spec` comes from the config, or else the run event. A step's
**end** is its `confirmed` time. Clean, Restore and Unique judge only settled
steps (§17, entry 15).

**Converge.** The settle condition holds by the deadline, and still holds at
`t* + stable`. The summary names the first unmet clause, such as `did not
settle (clause 7)`. `backoff-bound` labels a finding whose deadline
`backoffCap` set. `converge.go` suggests missing `models:`. A step whose record
ends too early gets a note that names that end: its confirm window's end, the
next step's start, or the trace's last event. So does a CR whose deletion began
after a `stop` and has not ended by the trace's last event.

**Crash.** A controller exit the harness did not cause fails its step.

**Clean.** At the end of a settled step with a `delete` action, a controller
resource fails when its rule 1 to 3 CR is gone, or its rule-4 set is non-empty
and every CR in it is gone. At the end of step `planSteps + 1`, every unshared
controller resource fails. After a `stop`, the plan's last step is the run's
last, and fails one whose CRs are gone. Its CRs' `retain` allows a resource.
Shared resources are judged only for growth since the first `create`, and those
of an unlisted owner not at all. Summary: `left resources behind`.

**Restore.** It judges a settled step with no `create`, `update`, `delete` or
`changeFixture` action after a settled step, comparing the controller's
resources at the two ends by group, without volatile fields (entries 21 and 40,
the map's, and a CR kind's `periodic.paths`). Where no `damage` touched the
root, each instance must stay, with no new one and an equal body:
`replaced resources`, `changed resources`. Where one did, each group's kind
must stay under the root. A damaged group must be live again while its owner
lives, or else a CR made in the step must own its kind and role there:
`did not restore damaged resources` (entries 22 and 82).

**Unique.** At each settled step's end, a group holds at most one live
controller instance. Summary: `duplicate resources`.

**Honest** and Backoff judge the whole run (`honest.go`, `backoff.go`). A CR's
dependencies are the dependency instances it owns by rules 1 to 3 (entries 47
and 77). When the controller writes the CR `ready`, or raises its
observedGeneration while `ready`, its last `seen` of each dependency must show
its last write's version or a later one, which its `ready` accepts: `reported
ready before seeing its dependency ready`. With `required`, a CR of the first
`primaries` entry needs a dependency of each kind: `reported ready with no
dependency` (entry 81). With `track: d`, a CR fails that stays `ready` `d` after
the controller saw a dependency not ready: `stayed ready after seeing its
dependency not ready`.

**Backoff.** Failing controller calls with one system, verb, object,
subresource and status form a **run**; a create's object is its `requestName`.
A success breaks it. So does a new generation of its object, or of the
object's rule 1 to 3 CR. When the object is no CR and has no such CR, any CR's
new generation breaks it. With `n` CRs live, it fails when each gap between
calls `5n + 1` to `10n + 1` is under 100 ms: `retried a failing request in a
tight loop`. It skips 409s on a held kind until `stable / 4` after the hold.

**Property.** A `step` property is judged at each settled step's end, and an
`always` one at each `version` event. Its CEL sees `crs`, the live CRs' bodies,
and `world.<system>.<key>`, the live instances' bodies, where a key is
`<resource>` or `<resource>.<group>` on kube and the collection on a fake. Each
kind in the run event's `kinds` or in an entry's `owns` has a key.
`firstWrittenUnder(object, path)` takes a kube body and a list of keys. It
finds the earliest version of the object's ref, of any instance, with the
object's value at the path, and returns the UID of that version's owning CR by
rules 1 to 3, or `""`; a finding's details say where such values came from. The
string and base64 CEL extensions apply. Summaries: `property <name> does not
hold`, `could not be evaluated`.

**Corpus.** `check/testdata/corpus/` holds traces of known bugs and false
positives (`corpus_test.go`).

## 13. Config

One YAML file, decoded strictly; an unknown key names its line. Paths resolve
against the file's directory. `config.go` documents each key.

```yaml
launch:
  binary: ../../bin/toy-bucket
  args: [--bug=0]
  env: {CLOUD_ENDPOINT: "${systems.cloud.url}"}   # KUBECONFIG defaults to ${systems.kube.kubeconfig}
  rbac: [rbac/role.yaml]
crds: [crds/]
primaries:                                  # every kind the controller serves
  - kind: toy.reconciler-fuzzer/v1/Bucket
    sample: bucket.yaml
    ready: has(self.status) && self.status.ready
    owns: [{kind: "cloud:buckets", name: self.status.bucketID}]   # rule 3; kube kinds are group/version/Kind
    periodic: {every: 1h, paths: [status.refreshTime]}           # optional: clause 7 allows these writes
    values: {overlay: {spec.region: {enum: [eu, us]}}, mutate: [spec.region]}
  - {kind: toy.reconciler-fuzzer/v1/Policy, sample: policy.yaml, ready: has(self.status)}
values: {fixtures: {kv.yaml: [value]}}              # with primaries, only fixtures
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
  - {name: vault, api: vault, token: root, fixtures: [kv.yaml], access: read,  # api: crud, vault or s3
     process: {command: [../../bin/vault, server, -dev, "-dev-listen-address=${addr}"]}}
properties: [{name: fresh-key, expr: "crs.all(c, ...)", when: step}]   # or always
budgets: {reads: 2, writes: 4}                          # defaults
timeouts: {settle: 30s, stable: 3s, backoffCap: 60s}    # defaults
```

`primary` is shorthand for a one-entry `primaries`, whose `overlay`,
`distinct` and `mutate` sit in the top-level `values`. An entry also takes
`retain` and `halted`. No kind appears twice among `primaries` and `models`,
and no kube fixture is a CR. A model's stage is `readyCondition`.
`dependency.required` defaults to true, and `stuckStatus` to `status`. A fake's
name matches `[a-z][a-z0-9-]*`. A `command[0]` with a slash resolves against
the file's directory. `vault` needs a `token`. `access: read` says the
controller only reads the fake. `values.fixtures` may name a system's fixtures
file, whose entries' body paths `changeFixture` sets.

## 14. Generation, replay and shrinking

`generate.New(cfg)` reads each listed kind's CRD schema with its overlay, its
sample, and the fixtures. rapid draws values, and the CRD's own validation, run
in-process, rejects invalid ones. `Plan(seed)` draws 3 to 8 steps of 1 to 4
actions and at most one fault each. A create draws its kind uniformly from
`primaries`, and each CR takes its kind's `values`. Step 0 creates slot `a`.
About 30% of steps hold one `restart`, `kill` or `damage`, so Restore has steps
to judge. About 30% of multi-action steps are bursts within 50 ms: two updates
of a CR, or a delete and a create of its slot and kind. A kube fault draws its
request from what `launch.rbac` grants, creates weighted most (§17, entry 20).
2 of 7 kube fault draws (of 9 with `models:`, 2 of which are `stuck` or
`slowDelete`) hold a kind `launch.rbac` lets the controller watch, the listed
and `owns` kinds weighted most, with the traffic `holdTraffic` documents. With
fakes, half of the fault draws and of the `damage` draws name a fake: a request
fault by verb alone, or a model fault on a `crud` fake, and a target by system
alone. A read-only fake takes no damage and only read faults. `changeFixture`
draws kube and system fixtures alike. `damage` only deletes. About 1 in 4 CR
deletes is `Foreground`. A fault period lasts 0.5 s to `2 × stable`, plus the
longest `track` for a model fault. `generate.go` documents the weights.

**Hunt.** `hunt.Hunt` runs each seed, keeps runs with a new signature, replays
each 5 times and shrinks it (`shrink.go`), trying at most 16 candidates.
`summary.md` lists the findings with seed, reproduction rate and shrunk plan.

## 15. Commands

```
reconciler-fuzzer run    --config c.yaml (--plan plan.json | --seed n) --out dir [--launch-arg a]...
reconciler-fuzzer judge  --trace dir[/trace.jsonl] [--config c.yaml]
reconciler-fuzzer hunt   --config c.yaml --seeds n [--from s] --out dir [--launch-arg a]...
reconciler-fuzzer shrink --config c.yaml --plan plan.json --out dir [--launch-arg a]...
crudfake --config cloud.yaml [--listen addr]
```

They exit 0 with no findings, 1 with some, and 2 on a usage or harness error.

## 16. Gates, examples and testing

**Make targets and toys.** `make help` lists the targets, which CI and the
nightly call. A toy is a directory in `targets/` with a `main.go`. `build`,
`generate`, `test-generated` and `bug-matrix` run every toy.
`targets/toy-widget` holds the Widget configs: `gadget/` adds a modelled Gadget,
and `tenant/` and `grant/` cross scopes. In `targets/toy-bucket`, each Bucket CR
owns one bucket in the `crudfake` system `cloud`; its bugs start at B30, and
`halted/` runs `--halt` under `primary.halted`. In `targets/toy-fleet`, a
Cluster owns modelled third-party Volumes, a Database names a Cluster, and a
User a Database. Its bugs start at B100. `config.yaml` lists all three kinds.
The generated test fuzzes `cluster/`, which lists Clusters alone, and
`database/`, which lists Databases on a fixture Cluster. `uninstall/` makes
the Database CRD a fixture, which a plan may delete.

Each config's `plans/` holds its gated bugs' and B0's plans. A toy's
`gate_envtest_test.go` runs `gate_test.go`'s rows, and its `bugmatrix_test.go`
renders them in `docs/bug-matrix.md`. B0 gives no findings and no notes on the
plans of the configs that `configs` lists, and no findings on the 20 faulted
seeds that the toy's generated test runs for each config it fuzzes.

**Examples.** `examples/<name>` adopts cert-manager, external-secrets on Vault,
and ACK's s3-controller on moto. Plans and listed seeds give no findings,
negative controls fail Clean or a property, and `bugs/` plans find known real
bugs, such as cert-manager's #63; `repro-<name>` prints their rates.

**Testing.** Red/green TDD: a test must fail when its code is mutated. Envtest
tests, few, live in `*_envtest_test.go` behind `//go:build envtest`. Root tests
check the README's commands, the Makefile, the workflows and this file.

## 17. Decision log

Each entry changes a check, exception, `System` interface or ignore list.

1. `launch.rbac` holds the controller's RBAC. B16 needs a real identity.
4. The runner appends a delete-all step and a restart step to every plan without a `stop` (#138).
7. Clause 5 covers only damaged and changed objects, not every changed CR.
10. The supervisor backs off between restarts, against crash loops.
15. Clean, Restore and Unique judge only settled steps after a settled one.
20. Kube faults draw what `launch.rbac` grants, weighted toward creates.
21. Restore ignores `resourceVersion`, `managedFields` and `generation`.
22. Restore lets a damaged CR rename its other resources, as a reissue does.
25. An apply patch that answers 201 created its object, as cert-manager's do.
26. Clause 5 holds `stable` after the change, for external-secrets' repair.
27. Crash allows an exit within 1 s of a faulted Lease request.
29. The runner waits for the settle condition before step 0, for singletons.
34. A stage model has one stage and an exact `after`; `outputs:` waits.
38. `stuck` writes `Ready` False rather than nothing, so B18 and B25 see it.
39. A model fault's end writes a fresh status, for a controller without a watch.
40. Restore ignores a model kind's `status`, which the model writes.
41. A model fault that held no object gets a note, as a request fault does.
43. Backoff allows 409s on a kind a `holdWatch` holds, as #114 defines.
44. Converge names an unmodelled kind only with an instance that has no status.
47. Honest skips a deleting dependency, and counts a gone one last seen live, and one the controller saw before the observer did.
48. A restart, exit or failed CR write excuses `track`.
52. A `delete` may be `Foreground` (B29). `Orphan` would fail Clean.
64. `holdWatch` holds whole streams of one kind and delivers them in order, so it reorders nothing (#63).
65. An API map reads response headers and may show partial objects, for S3.
66. The `vault` and `s3` maps number incarnations, since paths and names are reused.
67. Emulators list through their own API (`fake.Lister`), and take no model faults.
68. moto, not MinIO, emulates S3: MinIO's downloads are blocked, and ACK needs no more.
69. Properties judge settled step ends; `firstWrittenUnder` follows a value across instances, for #63.
70. `primary.periodic` lets a timer's CR writes pass clause 7, for an external-secrets rotation.
71. Fakes start before kube, so kube fixtures can name their urls.
72. Clause 5 holds `stable` after a polling controller's look, for ACK's repair.
73. A held watch looks only `stable` after its hold, and Backoff allows its 409s until `stable / 4` after.
74. A fake's write without a resourceVersion is a change only if the observer saw one, for ACK.
75. Restore ignores Vault's `current_version` and `versions`, and S3's `versionId`.
76. `primary.periodic` paths name list items with `[*]`, so ACK exempts only condition stamps.
77. Honest skips a dependency the controller deleted with a `delete`, or last saw deleted, whatever the observer's lag, so a scale-down passes (#124).
78. A property sees each kind in the run event's `kinds` or `owns` as an empty list while it has no object (#148).
79. Every check judges each kind `primaries` lists, and a CR the controller creates is also its owner's resource (#126). Rule 4 sets leave such CRs out, so a resource remade after one keeps its group.
80. A live unlisted owner, such as a fixture, keeps what it owns, directly or through the controller's live objects, from rules 2 to 4, so deleting a CR blames none of it (#128). An object the controller made is never an unlisted owner.
81. `required` binds only the first entry's CRs, so the kinds that own no dependency pass.
82. A `damage` touches each group under its root. An undamaged touched group may empty while its kind lives under that root, and a damaged one whose owner is gone while a CR made in the step owns its kind and role there. So a damage under a CR that the controller made touches that CR's owner, and the controller may remake that CR.
