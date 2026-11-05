# reconciler-fuzzer

reconciler-fuzzer finds bugs in Kubernetes controllers. It runs your controller on your machine
against envtest's kube-apiserver and etcd. It follows a plan of steps: CR changes, faults on the
controller's API requests, restarts, and damage to objects the controller created. It records
every request and object version in a trace, including requests to fake HTTP services. Then it
judges the trace with these checks: Converge, Crash, Clean, Restore, Unique, Honest, Ordered,
Teardown, Backoff, Finalizers and Property ([Reading a failure](#reading-a-failure)).

You write no test code. One YAML config describes your controller.

## Install

```sh
go install github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
export PATH="$(go env GOPATH)/bin:$PATH"
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 --index $index -p path)"
```

`setup-envtest` downloads etcd and kube-apiserver. Set `KUBEBUILDER_ASSETS` in each new shell.
Building takes Go 1.26.0 or later. Set `GOTOOLCHAIN=auto` if your Go is older.

From a checkout, the Makefile installs the same control plane under `bin/`. It also installs a
pinned, checksummed Vault and moto with its dependencies pinned by hash, the emulators the
examples run:

```sh
make setup
export KUBEBUILDER_ASSETS=$(make -s assets-path)
```

## Quick start

The toy controller in [targets/toy-widget](targets/toy-widget) turns each Widget into ConfigMaps.
Its `--bug=N` flag switches on bug N. From a clone:

```sh
go install ./cmd/reconciler-fuzzer
go build -o bin/toy-widget ./targets/toy-widget
reconciler-fuzzer run --config targets/toy-widget/config.yaml \
  --plan targets/toy-widget/plans/b3.json --out reconciler-fuzzer-out/b3-nobug
```

`plans/b3.json` is the plan that catches bug 3. It creates a Widget and then deletes it. With no
bug, the run prints this and exits 0:

```
No findings.
```

Bug 3 leaves the first ConfigMap without an owner reference, so it outlives its Widget:

```sh
reconciler-fuzzer run --config targets/toy-widget/config.yaml \
  --plan targets/toy-widget/plans/b3.json --launch-arg --bug=3 --out reconciler-fuzzer-out/b3
```

It exits 1 and prints this, with a new namespace and time on each run:

````
# Findings

## Clean: left resources behind

Step 1, at 15.356s.
Actions: delete slot a.

```
configmaps rf-26dbb0d4/widget-0 (rule 4, slot a)
```

# Next steps

Replay the plan:

```
reconciler-fuzzer run --config targets/toy-widget/config.yaml --plan reconciler-fuzzer-out/b3/plan.json --out reconciler-fuzzer-out/b3/replay --launch-arg --bug=3
```

[docs/failures.md](https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/blob/main/docs/failures.md) says what each finding means.
````

`--seed n` draws a plan from the config instead, and `plan` prints that plan without running
it. `hunt` runs many seeds, then replays and shrinks the plan of each seed with new findings:

```sh
reconciler-fuzzer plan --config targets/toy-widget/config.yaml --seed 6
reconciler-fuzzer run --config targets/toy-widget/config.yaml --seed 6 --out reconciler-fuzzer-out/seed-6
reconciler-fuzzer hunt --config targets/toy-widget/config.yaml --seeds 3 \
  --launch-arg --bug=3 --out reconciler-fuzzer-out/hunt
```

A toy run takes half a minute or more. `hunt` replays the plan of a seed with new findings 5
times. For each finding, it then tries up to 16 smaller plans, called shrink candidates, and
runs each at most 4 times, so this hunt takes several minutes. `--replays` and `--candidates`
change these counts. `hunt` rewrites `summary.md` while it runs. It lists each finding with its
seed, reproduction rate and shrunk plan, and each plan that a failed action ended early. `reconciler-fuzzer help hunt` describes each flag.

## Writing a config

[DESIGN.md §13](DESIGN.md#13-config) lists every key. A minimal `fuzz/config.yaml` for a
kubebuilder project:

```yaml
launch:
  binary: ../bin/manager                   # your controller, built
  args: [--health-probe-bind-address=0, --metrics-bind-address=0]
  env:
    WATCH_NAMESPACE: ${systems.kube.namespace}
  rbac: [../config/rbac/role.yaml]         # the controller runs with only these rules
crds: [../config/crd/bases/]
primary:
  kind: example.com/v1/Widget              # group/version/kind of the CR you fuzz
  sample: widget.yaml                      # a valid CR; generated CRs start from it
  ready: >-
    has(self.status) && has(self.status.observedGeneration)
    && self.status.observedGeneration == self.metadata.generation
fixtures: [settings.yaml]                  # objects created before step 0
timeouts: {settle: 30s, stable: 3s}
```

- Paths resolve against the config file's directory.
- The controller's `KUBECONFIG` defaults to `${systems.kube.kubeconfig}`, the kubeconfig the
  harness writes for it. Pass `--kubeconfig=${systems.kube.kubeconfig}` in `args` if the
  controller does not read `KUBECONFIG`, as when it calls `clientcmd.BuildConfigFromFlags`
  with an empty path.
- `args` turns off the scaffold's fixed ports, so runs cannot clash on them. If it has
  webhooks, set `ENABLE_WEBHOOKS: "false"` in `env`.
- `ready` is CEL over the CR, typed by its CRD's schema. It must be true only once the
  controller has acted on the current spec, so compare an `observedGeneration` with
  `metadata.generation`. Read a property whose name is not a CEL identifier by index, as in
  `self.spec.selector['app.kubernetes.io/name']`.
- `stable` is how long the controller must stay quiet before a step counts as settled.
- `launch.rbac` also tells generation which requests a fault can pick. A generated `damage` of
  a kube object targets an unlisted kind that `launch.rbac` lets the controller create. Faults
  favor the listed kinds. Faults and damage favor the kinds the CRs own: the kube kinds that
  `owns` names and the `models:` kinds with a `dependency`. Without owned kinds, the core kinds
  that the controller may create, such as Secrets, stand in for them. An owned kind ends that
  stand-in, even for a core kind that the controller also writes.
- `launch.gracePeriod`, 5s by default, is how long a stop of the controller waits after
  SIGTERM before it sends SIGKILL. The report notes each time the controller outlived the
  grace period. After an interrupt, such as Ctrl-C, a stop sends SIGKILL at once.
- `values` steers generated CRs: `overlay` narrows the CRD schema, `distinct` fields differ
  across a plan's CRs, `mutate` names the fields updates change, and `fixtures` names the
  fixture strings `changeFixture` sets.
- `values.deletable` lists `fixtures` files whose objects plans may delete. Half the generated
  plans then end with a step that deletes one such object, and maybe restarts or kills the
  controller. Converge fails a CR that is not ready after that step. So list a file only if the
  controller keeps every CR ready while such an object is being deleted and once it is gone.
  The files may hold no Namespace or CRD. They may hold no object of a kind that a `references`
  entry names in `to` ([References](#references)), since plans already delete such objects.

### Several kinds

A controller that serves several CRD kinds lists each under `primaries`. `primary` is shorthand
for a list of one. An entry takes `primary`'s keys, and its own `overlay`, `distinct` and
`mutate` under `values`. The top-level `values` then takes only `fixtures` and `deletable`. The
toy controller in [targets/toy-fleet](targets/toy-fleet/config.yaml) serves Clusters, Databases
and Users:

```yaml
primaries:
  - kind: toy.reconciler-fuzzer/v1/Cluster
    sample: cluster.yaml
    ready: &ready >-
      has(self.status) && has(self.status.observedGeneration)
      && self.status.observedGeneration == self.metadata.generation
      && has(self.status.phase) && self.status.phase == 'Ready'
    values: {mutate: [spec.volumes, spec.volumeSize]}
  - {kind: toy.reconciler-fuzzer/v1/Database, sample: database/database.yaml, ready: *ready}
  - {kind: toy.reconciler-fuzzer/v1/User, sample: user.yaml, ready: *ready}
fixtures: [admin-password.yaml]                     # a Secret that a Database names
```

- Every check judges the CRs of each listed kind by their own entry, and the settle condition
  waits for each. A CR that the controller creates is also judged as its owner's resource, so
  Clean fails it when it outlives that owner. A `damage` in the owner's slot can target the
  CR's resources, and Restore treats a damage under the CR as a damage under its owner.
- A plan creates only CRs of listed kinds, and `run` refuses any other create. A create
  without an object makes the first entry's sample.
- Generated plans draw each create's kind from the list. They fill a field that names another
  object, such as a Database's `clusterRef`, only when a `references` entry declares it.
- A fixture may not be a CR. An object whose ownerReferences lead to no CR, but reach a live
  object that the controller did not create, such as a fixture, belongs to that object. They may
  reach it through live objects that the controller made. `owner` and `owns` do not change this.
  Clean skips such an object while its owner lives.
- A dependency belongs to the nearest CR that owns it, even one that the controller created.
  `dependency.required` binds only CRs of the first entry.

### References

A `references` entry declares a field by which a CR names an object that it does not own. In
toy-fleet, a Database names the Cluster that it runs on and the Secret that holds its password,
and a User names a Database:

```yaml
references:
  - from: toy.reconciler-fuzzer/v1/Database
    to: toy.reconciler-fuzzer/v1/Cluster
    name: spec.clusterRef
    onDelete: block                                   # the Cluster's deletion waits for its Databases
  - from: toy.reconciler-fuzzer/v1/Database
    to: v1/Secret
    name: spec.passwordSecretRef.name
    ready: has(self.data) && has(self.data.password)   # CEL over the Secret
    required: true                                    # the default
    track: 1s
  - {from: toy.reconciler-fuzzer/v1/User, to: toy.reconciler-fuzzer/v1/Database, name: spec.databaseRef, onDelete: block}
```

- `from` is a listed kind. `to` is a kube kind, or a fake's collection written as
  `<system>:<collection>`. A kube kind needs its CRD in `crds`, unless Kubernetes serves it
  without one, as it does `v1/Secret` and `apps/v1/Deployment`.
- `ready` defaults to the `ready` of the `to` kind's entry, model or fake. A model's compares
  `observedGeneration` with `metadata.generation`, as an entry's should. So Honest fails a CR that
  the controller writes ready while the named object's status lags its spec. If the controller
  checks only the named object's condition or phase, set `ready` to that check.
- `name` and `namespace` are field paths in the CR, as in `values.mutate`, which the `from` CRD's
  schema must describe as strings. `[*]` names a field of each list item, as in
  `spec.keys[*].secretName`. Without `namespace`, a namespaced object is in the CR's namespace. An
  empty or absent value names nothing. A `name` may not be the field that an `owns` entry of the
  `from` entry reads for the same kind, since the CR owns that object.
- The controller **looks** at an object when a response or watch event gives it the object, or
  when a get, a list or the initial events of a watch show it missing. A response that gives it
  the object after a watch without a selector showed the object deleted does not count, since it
  shows a version from before the deletion. A reference **holds back** a CR while the
  controller's last look at the named object showed it not ready, or missing and `required`.
  Before any look, only a missing `required` object holds the CR back.
- Honest fails a CR that the controller writes ready while a reference holds it back, or before
  the controller looked at a live named object. It passes a write sent within 2×`stable` of a
  look, after an earlier look that let the CR be ready, with no restart since, that the
  controller retracts within `stable` of the write's response, by writing the CR not ready or
  naming another object. A reconcile may read its cache before the look reaches the cache, and
  the cache holds what an earlier look showed. The retraction window starts again at a start in it,
  since a restart loses the reconcile. A write of the CR, the ready write itself or one in the
  window, that failed or had an unknown outcome, such as a conflict, gives the controller the
  time the faults of the step that answered it lasted, at most `timeouts.backoffCap`, and
  `stable` from its response, since it retries after a backoff. A killed write gives none, since
  the controller starts again. With `track: d`, Honest fails a CR that stays ready `d` after a
  look that held it back. None of these windows counts time in which the controller **stalls**:
  while a step's faults last, or between an exit and its next start. A step's faults stall it
  only if a fault altered one of the controller's requests in them, so a model fault and a held
  watch stall it not at all.
- The settle condition does not wait for a CR that a reference holds back, if the object as it is
  now would hold the CR back too. This starts once `stable`, and the time the step's faults
  lasted, at most `timeouts.backoffCap`, have passed since the faults ended. So a controller in
  retry backoff reacts first. If the controller does not watch the `to` kind, it misses the
  object's return, and the step does not settle. While such a CR is not ready, Restore lets its
  damaged resources stay gone.
- A response that the harness cannot decode, such as a watch of Tables, may show the named object.
  If one ends after the controller's last look, or is a watch still open since, the reference is
  **unknown**. Honest then passes the CR's ready writes, and the settle condition treats the CR as
  held back.
- Generated plans write at each `name` the name of a slot's CR of the `to` kind, or of a fixture
  of it, so a CR may name an object that does not exist yet. With `required: false`, 1 write in 4
  names `missing`, which no object has. `changeFixture` may delete a kube fixture of a `to` kind,
  and `createFixture` makes it again:
  `{"do": "createFixture", "target": {"resource": "secrets", "name": "admin-password"}}`.
- The step that deletes every CR deletes those of each `from` kind before those of its `to` kind.
- `onDelete: block` says that the object's deletion waits for the CRs that name it, as
  `kubernetes.io/pvc-protection` holds a claim that a Pod uses. The controller holds the
  object, usually with a finalizer that it removes once no CR names the object. A fake's
  collection cannot take it. Without `onDelete`, the deletion need not wait for those CRs.
- Under `block`, Ordered fails an object that is released, that is gone, while a CR names it:
  `released an object that another names`. A CR names the object until the CR is gone, even
  while the CR is being deleted. A CR that moves its reference releases the old object. Ordered
  counts only a CR that the controller saw naming the object, and an object that it saw, in time
  to hold the object: `stable` before the object's deletion started, not counting stalls. If a
  step's faults stalled the controller in that time, the deletion must also start once `stable`,
  and the time those faults lasted, at most `timeouts.backoffCap`, have passed since they ended,
  since the controller may wait in retry backoff until then. A controller cannot hold an object
  whose deletion starts before it reacts, and only admission control can refuse a CR that names
  the object later.
- Under `block`, the settle condition counts a CR that waits in deletion while a live CR names it
  as settled. This starts `stable` after its deletion started, and not before `stable` and the
  time the step's faults lasted, at most `timeouts.backoffCap`, have passed since the faults
  ended. One that stays after its last naming CR is gone still fails clause 2. CRs being deleted
  that name each other, directly or through other CRs being deleted, would wait for good, so
  those names do not count. A CR that names itself is such a cycle. Ordered passes the release
  that breaks one.
- Under `block`, Honest counts a look at an object that waits in deletion as ready if the look,
  or the object's last version before its deletion started, shows it ready, since the
  controller may still use the object.
- Clause 2 judges only CRs of listed kinds, so no check fails an object of another kind, such as
  a Secret, that stays in deletion after its last naming CR is gone (#188).
- Under `block`, a generated create or `createFixture` may need a name that waits in deletion
  while a CR names it. Its wait then gives up with a `still taken` note, and the plan's later
  steps do not run. `hunt` drops the note, so the seed counts as clean (#189, #190).

[targets/toy-fleet/secret](targets/toy-fleet/secret/config.yaml) runs toy-fleet with
`--protect-secrets`, which holds each Secret while a Database names it, and blocks on the Secret
reference.

### Other namespaces and cluster scope

CRs, their children and fixtures may be cluster-scoped or in any namespace. The harness
watches the whole cluster. A fixture with `metadata.namespace` stays in that namespace, which the
harness creates; one without it goes in the run namespace.

Clean, Restore, Unique and Honest judge each object by the CR that owns it. An ownerReference
chain names that CR. A child with none, such as one in another namespace or a cluster-scoped
child of a namespaced CR, needs one of these:

```yaml
primary:                                   # or a primaries entry
  owns:                                    # a field of the CR names the child
    - {kind: rbac.authorization.k8s.io/v1/ClusterRole, name: "self.metadata.name + '-reader'"}
    - {kind: v1/Secret, name: self.spec.secretName, namespace: "'shared'"}
systems:
  - name: kube                             # takes only owner and role
    owner: self.metadata.labels['example.com/owner-uid']   # a label on the child names the CR
```

- `owns` takes CEL over the CR, typed by its CRD's schema. Without `namespace`, a namespaced
  child is in the CR's namespace. A cluster-scoped kind takes no `namespace`.
- `owner` takes CEL over the child. It yields the CR's UID, `namespace/name` or name. A bare name
  prefers a CR in the child's namespace, and names no CR when it matches CRs in two other namespaces.
  A name that CRs of two kinds share names no CR.
- Without either, Clean blames such a child on every CR that lived when it was created, except
  those that the controller created. It fails only once all of them are gone. A child made while
  none lived is shared, and fails only if the run ends with more shared children of its kind and
  [role](#roles) than the first create saw.

Kubernetes' garbage collector deletes a child only through an ownerReference chain. It never
follows a cluster-scoped child's ownerReference to a namespaced owner. So before a controller
removes its finalizer from a CR, it must delete each child that it created for the CR and that the
garbage collector does not delete. Teardown fails a controller write that removes the controller's
last finalizer from a CR being deleted while such a child is not yet being deleted: `released a CR
before deleting its resources`. It finds the CR's children through `owns`, `owner` and
ownerReferences. A child with a `deletionTimestamp`, a child whose delete succeeded or found it
gone, and a child in a namespace being deleted are being deleted. The controller need not wait
until the child is gone. But a kube child's delete counts only if the trace shows the child
deleting or gone at some time, so a dry run alone does not count. The garbage collector deletes no
fake service's object. The CR's `retain` allows its children to stay. Teardown judges a child that
the `owns` entries of several CRs name only at the release of the last of them, and passes it if
any of them retains it. To judge the order of children that the garbage collector deletes, write a
[property](#properties).

### Roles

A child's role is the part it plays for its CR. Clean, Restore and Unique group children by
kind, owner and role. Unique fails two live children of one group. Restore counts a damaged
child as restored once a child of its group lives again, whatever its name.

A kube child's role is its `generateName`, or else its name. A controller that ends each child's
name with a suffix that each process picks needs a `role` on the kube entry. That CEL runs over
every kube object, so guard it by kind. The config of the sample controller in
[targets/toy-fleet](targets/toy-fleet/config.yaml), whose Cluster CRs own Volumes, drops such a
suffix from each Volume's name:

```yaml
systems:
  - name: kube
    role: >-                               # cluster-vol-0-x7k2p plays cluster-vol-0
      self.kind == 'Volume' && self.metadata.name.matches('-vol-[0-9]+-[^-]+$')
      ? self.metadata.name.substring(0, self.metadata.name.lastIndexOf('-')) : ''
```

Where the CEL fails or yields `''`, a child keeps its `generateName` or name. When it fails on
every object of a kind the controller created, `report.md` notes the first error. A fake takes
`role` too, and a fake resource without one has an empty role. A `damage` target's `role` picks
a child of that role.

### Objects another controller sets ready

envtest runs no other controllers. If your controller creates an object, such as a Gadget, and
waits for another controller to set its status, add a `models:` entry. The harness then plays
that controller:

```yaml
models:
  - kind: example.com/v1/Gadget
    stages: readyCondition                   # sets Ready=True for each generation
    after: 200ms
    dependency: {required: true, track: 1s}
```

- `dependency` makes the Gadget a readiness dependency of the CR that owns it, by
  ownerReference, `owns` or `owner`. Honest then fails a CR that reports ready before the controller saw its
  Gadget ready. `required` (the default)
  also fails one with no Gadget. `track: d` fails a CR that stays ready longer than `d` after
  the controller saw its Gadget not ready, not counting time in which the controller stalls, as
  References defines.
- Generated plans then draw two model faults. `stuck` keeps Gadgets not ready. `slowDelete`
  holds their deletion with the finalizer `reconciler-fuzzer.dev/slow-delete`, which the model
  keeps on each Gadget only while the fault lasts. Each ends with the fault period. A create
  that waits until a deleted CR no longer exists ends them once `period` has passed. A plan step
  writes one as
  `{"match": {"group": "example.com", "resource": "gadgets"}, "stuck": true}`, and needs a
  `period` to keep it on after the step's last action returns. A generated model-fault step
  that deletes a CR also restarts or kills the controller, unless the step already holds 4
  actions. The plan schedules the restart or kill 300 ms or more after the delete, within the
  period. `slowDelete` may then still hold the deletion, and a new process may have to finish
  it.
- `finalizer: {name: example.com/cleanup, after: 500ms}` plays a controller, such as a storage
  controller, that holds each Gadget's deletion with its own finalizer. The model adds the
  finalizer to each live Gadget it hears, and removes it 500 ms after it hears the deletion.
  Finalizers fails a controller write that drops it. Keep `after` under `timeouts.stable`, or
  a step can settle before the controller reacts to the release. A step waits for every hold
  to end, so raise `timeouts.settle` for a controller that waits out holds one by one.
- `status:` adds literal fields to the model's status writes, such as a PVC's `{phase: Bound}`.
  While `stuck` lasts, the `stuckStatus:` fields, such as `{phase: Pending}`, replace them.
  `stuckStatus:` defaults to `status:`. Each write removes the fields that only the other key
  sets. A model with only `stuckStatus: {phase: Pending}` thus removes `phase` while ready.
- If a run fails to converge while an object of a kind nothing models has no status,
  `report.md` names the kind and suggests a `models:` entry for it.

[targets/toy-widget/gadget](targets/toy-widget/gadget) runs the toy with a Gadget model.
Another toy, in [targets/toy-fleet](targets/toy-fleet), reconciles Cluster, Database and User
CRs, and its config models the third-party Volumes each Cluster owns.
[targets/toy-fleet/protect](targets/toy-fleet/protect/config.yaml) also holds each Volume's
deletion with a model finalizer.

### Fake services

A controller that calls an HTTP API, such as a cloud's, calls a fake service instead. A
`systems:` entry runs the fake, and the controller gets its URL. In
[targets/toy-bucket](targets/toy-bucket), each Bucket CR owns one bucket in `crudfake`, the
harness's JSON server:

```yaml
launch:
  env: {CLOUD_ENDPOINT: "${systems.cloud.url}"}   # the fake, through the controller's listener
systems:
  - name: cloud
    process: {command: [../../bin/crudfake, --config, cloud.yaml]}
    api: crud                                # the API map that reads its requests
    owner: self.labels['k8s-uid']            # CEL over a bucket that names its CR
    ready: self.state == 'READY'
    gone: self.state == 'DELETING'
    dependency: {required: true}             # Honest judges the CR's buckets
    poll: 1s                                 # how often the controller reads a bucket
```

```yaml
# cloud.yaml
collections:
  buckets: {unique: [name], pending: CREATING, ready: READY, deleting: DELETING, createDelay: 300ms, deleteDelay: 300ms}
```

- Every check judges buckets as it judges Kubernetes objects. Clean fails a leaked bucket, and
  Unique a second bucket of one CR. Restore fails a bucket the controller never recreates after
  `damage`, and Honest a CR that reports ready while its bucket is `CREATING`.
- `crudfake` serves `POST /<c>`, `GET /<c>?<field>=<value>` and `GET`, `PUT`, `PATCH` and
  `DELETE /<c>/<id>`. A new resource is `pending` for `createDelay`, then `ready`. `DELETE` sets
  `deleting`, and the resource leaves after `deleteDelay`. A repeated `unique` field answers 409,
  and an unknown id 404.
- A plan faults the fake with `"system": "cloud"`: any request fault, and `stuck` or
  `slowDelete`. The step
  `{"period": "2s", "faults": [{"system": "cloud", "match": {"verb": "create", "resource": "buckets"}, "nth": 1, "dropResponse": true}], "actions": [{"do": "create", "slot": "a"}]}`
  drops the response to the first bucket create. Without its `period`, the fault would end as
  the CR's create returns, usually before the controller creates the bucket. `damage` takes
  `"target": {"system": "cloud"}`.
- An entry's `halted` is CEL over its CR, typed by its CRD's schema. It counts a CR as done after
  a request of unknown outcome, for a controller that stops on purpose. While such a CR is not
  ready, Restore lets its damaged resources stay gone.
  [targets/toy-bucket/halted](targets/toy-bucket/halted/config.yaml) uses it.
- Any program can be a fake. The harness runs it in the config's directory and sets
  `RECONCILER_FUZZER_ADDR`, where it serves its API and the fake contract: `GET /_fake/resources`
  answers `{"collections": [<c>], "pending": <bool>, "resources": [{"collection": <c>, "body":
  <object>}]}`, `POST /_fake/faults` takes `{"effect": "stuck" | "slowDelete", "collection": <c>,
  "id": <id>}` and answers 201 with `{"id": <fault>}`, or 400 for an unknown collection, and
  `DELETE /_fake/faults/<fault>` ends the fault. The controller cannot reach `/_fake/`.
- With `api: crud`, the fake's paths follow `crudfake`'s. Each body has a string `id`, never
  reused, and a `version`, a number or string that changes with every change. A listing the
  harness cannot read fails the run.

### Emulators

A real emulator can be a fake service too. `api: vault` reads a Vault server's KV version 2
mounts, and `api: s3` an S3 server's buckets and objects:

```yaml
systems:
  - name: vault
    api: vault
    token: root                              # required: the harness's token, and ${systems.vault.token}
    access: read                             # optional: the controller only reads it
    process:
      command: [../../bin/vault, server, -dev, -dev-root-token-id=root, "-dev-listen-address=${addr}"]
    fixtures: [vault.yaml]                   # created before step 0
```

```yaml
# vault.yaml
- {resource: secret, name: token, body: {value: s3cr3t}}   # secret/data/token
```

- The harness runs the command with `${addr}` and `${port}` expanded, and lists the emulator
  through its own API. Emulators take no `stuck` or `slowDelete`.
- Kube fixtures expand `${systems.<name>.url}` and `.token`, so a Secret fixture can hold the
  controller's credentials.
- `changeFixture` on `{"system": "vault", "resource": "secret", "name": "token"}` with a `patch`
  writes a new KV version. The patch merges into the secret's data, so `{"value": "new"}`
  sets `value`.
- With `access: read`, generated plans draw no damage and only read faults on the fake.
- A request the map cannot place gets a note `unmapped request`, and no check judges it.

### Properties

A property is CEL that must hold at each settled step's end, or with `when: always` at every
recorded version. It sees `crs`, the live CRs, and `world.<system>.<key>`, the live objects of
each system. A kube key is the kind's resource, followed by `.<group>` outside the core group,
as in `world.kube.secrets` or `world.kube['volumes.storage.thirdparty']`. A fake's key is its
collection, as in `world.vault.secret`:

```yaml
properties:
  - name: synced
    expr: >-
      crs.all(c, world.vault.secret.all(v, v.path != c.spec.data[0].remoteRef.key
        || world.kube.secrets.exists(s, s.metadata.name == c.spec.target.name
          && s.data.token == base64.encode(bytes(v.data.value)))))
```

A kind the harness watches from the start maps to an empty list while no object of it exists.
These include each installed CRD, each listed kind, each kind that a reference names as `to`, and
each kind that `models` or a fixture names.
They also include each kind that a `launch.rbac` rule names without `*`, except Events, Leases
and the other kinds the checks ignore. The harness skips a kind that the server does not list
and watch in the named group. The `run` event of `trace.jsonl` lists these kinds in `kinds`. A
fake collection that an entry's `owns` names also maps to an empty list. Any other kind or
collection has a key only if the trace holds an object of it.

Write a property for a bug that no check can see. No check fails a controller that raises
`observedGeneration` for a spec it ignored, since Honest cannot know which spec field maps to
which dependency. No check fails a CR that drops out of ready and back with no cause, since a
controller may drop ready without a spec change by design, as after a failed health probe or an
expired credential. [targets/toy-fleet/cluster](targets/toy-fleet/cluster/config.yaml) has a
property for each of these bugs. The first fails a Ready Cluster whose Volumes do not match its
spec. The second fails a Cluster whose status for its current generation says Provisioning and
counts every Volume ready. `crs` holds the live CRs of every listed kind, so each property skips
CRs of other kinds:

```yaml
properties:
  - name: volumes-match-spec
    expr: >-
      crs.all(c, c.kind != 'Cluster' || !has(c.status) || !has(c.status.phase)
        || c.status.phase != 'Ready'
        || world.kube['volumes.storage.thirdparty'].filter(v, has(v.metadata.ownerReferences)
             && v.metadata.ownerReferences.exists(o, o.uid == c.metadata.uid)
             && v.spec.size == c.spec.volumeSize).size() == c.spec.volumes)
  - name: provisioning-means-volumes-not-ready
    when: always
    expr: >-
      crs.all(c, c.kind != 'Cluster' || !has(c.status) || !has(c.status.phase)
        || c.status.phase != 'Provisioning'
        || !has(c.status.observedGeneration) || c.status.observedGeneration != c.metadata.generation
        || (has(c.status.readyVolumes) ? c.status.readyVolumes : 0) < c.spec.volumes)
```

A CR has no `status` until the controller writes one, and a Go controller leaves out an
`omitempty` field at its zero value, such as `readyVolumes: 0`. CEL fails on a missing field, so
the property `could not be evaluated`, and its details name the missing key, as in
`no such key: status`. Guard such fields with `has()`, as above.

Teardown skips a child that the garbage collector deletes through its ownerReference chain, since
the garbage collector deletes it after the CR whatever the controller's order. A `when: always`
property can require that such children are deleted first, as
[targets/toy-fleet/order](targets/toy-fleet/order/config.yaml) does:

```yaml
properties:
  - name: volumes-deleted-before-their-cluster
    when: always
    expr: >-
      world.kube['volumes.storage.thirdparty'].all(v, !has(v.metadata.ownerReferences)
        || v.metadata.ownerReferences.all(o, crs.exists(c, c.metadata.uid == o.uid)))
```

It fails at the first version that shows a Volume whose owner is no longer a live CR. A failing
property's details say when it turned false, and the writes at that time:

```
it turned false at 13.328547311s: cluster deleted clusters.toy.reconciler-fuzzer rf-c1e784db/cluster
```

Each write names its caller, or `cluster` if no recorded response carried it, as for the delete
that follows a CR's last finalizer.

`firstWrittenUnder(object, path)` names the CR that owned the object when its value at `path`
first appeared, across deletes and recreates. cert-manager's `fresh-key` property uses it. A
failing property's details say which instance and CR such a value came from.

A controller that refreshes on a timer writes its CR each period. In the CR's entry,
`periodic: {every: 2s, paths: [status.refreshTime]}` lets those writes pass the settle
condition. `[*]` names a field of every list item, as in `status.conditions[*].lastTransitionTime`.

### Kinds the controller does not restore

Generated plans damage the objects the controller creates, and Restore wants each damaged object
back. Some kinds are one-shot records that a correct controller never makes again, such as
cert-manager's CertificateRequest. `notRestored` lists them:

```yaml
notRestored: [cert-manager.io/v1/CertificateRequest]
```

Plans still damage these kinds, and every other check still judges them. Restore only stops
requiring a damaged object of such a kind, or any object of its kind under the damaged root, to
come back.

### Holding watch events

A `holdWatch` fault pauses the controller's watch events of one kind for the fault period, and
delivers them in order when it ends. The controller then acts on a stale cache, as on a busy
cluster. The settle condition counts a held watch as looking only once the hold ends, and it
holds the step for `stable` after the hold ends, since the events it delivers may show the
controller work, such as its own write:

```json
{"actions": [{"do": "delete", "slot": "a"}, {"do": "create", "slot": "a", "object": {}}],
 "period": "2s", "faults": [{"match": {"resource": "secrets"}, "holdWatch": true}]}
```

The create waits until the deleted CR no longer exists. Once `period` has passed, the wait
ends the hold, so that the controller can finish the delete.

Generated plans draw it for kinds `launch.rbac` lets the controller watch, favoring the kinds
other faults favor, in a step that deletes a CR and creates it again unchanged. About 1
cert-manager seed in 11 holds Secrets this way, so a 20-seed `make hunt-cert-manager` usually,
but not always, finds #63. A report notes a `holdWatch` while the controller had no watch of
its kind, such as one with a misspelt resource.

### Upgrading

A `restart` stops the controller and starts it again with its current binary, args and env. The
stop sends SIGTERM, and SIGKILL if the controller outlives `launch.gracePeriod`. The harness
installs `crds:` before step 0, and the CRDs stay fixed unless a restart installs new ones. A
restart that names `binary`, `args` or `crds` tests an upgrade:

```json
{"actions": [{"do": "restart", "args": ["--version=2"], "crds": ["../v2/crds/"]}]}
```

- It installs `crds` first, while the old controller still runs.
- `args` replace `launch.args`, and `[]` drops them. The `--launch-arg`s still come after the
  new args, and `${systems.<name>.<key>}` expands as in `launch.args`.
- Every later start keeps the new `binary` and `args`: restarts, crash restarts, the start after
  a `kill`, and the appended restart. Env, RBAC and `launch.gracePeriod` do not change, so
  `launch.rbac` must grant what both versions need.
- Paths resolve against the plan's directory. Before it starts, `run` refuses a `binary` that
  is not an executable file, a `crds` path that holds no CRD, and an unknown or unclosed
  `${key}`. It checks `launch.upgrades` the same way.

`launch.upgrades` lists later versions with the same keys:

```yaml
launch:
  upgrades:
    - {args: [--bug=0, --version=2], crds: [v2/crds/]}
```

Each generated plan then restarts into each version once, in order, at the end of a random step.
That step may hold a fault or come before a delete. `shrink` and `hunt` drop upgrades from the
last one back, so the versions stay in order.

The checks judge an upgrade's step as they judge any restart's, except that Restore lets the new
version change the old version's resources but not replace them. Restore judges a generated
upgrade only when its step changes no CR or fixture. Only some seeds draw such a step.
[targets/toy-fleet/upgrade](targets/toy-fleet/upgrade/config.yaml) upgrades toy-fleet to
`--version=2`, which labels each Volume with its version. Its bugs B140 to B142 mishandle the
Clusters that version 1 made.

Upgrades have these limits:

- No conversion webhook runs, and the two versions never run at once.
- For about a second after an install, the harness's watches still decode objects with the old
  schema. The trace keeps a version they record then until the object's next write.
- The harness watches each kind at the API version it first used. A restart whose CRDs stop
  serving that version ends the run with exit 2. The harness's patches and deletes use the
  kind's preferred version, which new CRDs may change.
- A property's `world` lacks a kind that only the new CRDs serve, unless an object of it exists
  during the run. `world` then lists no objects of that kind before the install.
- A default that the new CRDs add to a kind the controller makes shows as a Restore change if
  the controller first writes such an object after the upgrade's step.
- Restore reports `replaced resources` for a resource that a new version adds, deletes, renames
  or gives another owner. Adding an ownerReference can change a child's owner.
- Every plan.json that `run`, `hunt` or `shrink` writes holds absolute upgrade paths. Make them
  relative before you commit the plan.

### Uninstalling

A `stop` action lets a plan test an uninstall. It waits until the CRs the plan deleted are gone,
and then stops the controller for good:

```json
{"actions": [{"do": "delete", "slot": "a"}, {"do": "stop"}]}
```

The runner appends two steps to a plan without a stop: one deletes every CR, and one restarts
the controller. After a stop, a plan's only actions are deletes and fixture changes, and the
runner appends no steps. Clean then judges the plan's last step, but spares what a live CR owns.
No controller is left to clean up after a CR that the plan deletes after the stop, so Clean
counts that CR as live. A CR that the stop did not wait for, such as that one, may keep its
finalizers, and gets a note.

An uninstall may delete a CRD before the stop. List that CRD under `fixtures:`, not `crds:`. Its
kind must be one the config does not list. `changeFixture` without a `patch` deletes it:

```json
{"actions": [{"do": "changeFixture", "target": {"group": "apiextensions.k8s.io",
  "resource": "customresourcedefinitions", "name": "databases.example.com"}}]}
```

- A CRD under both `crds:` and `fixtures:` fails the run with `already exists`.
- A listed kind's CRD must stay in `crds:`. As a fixture alone, the config fails to load with
  `no CRD under crds and no built-in kind serves it`.
- No target matches a CRD that `crds:` or a restart installed. A `changeFixture` of one only gets
  a `no instance matches` note that says so.

[targets/toy-fleet/uninstall](targets/toy-fleet/uninstall/config.yaml) makes the Database CRD a
fixture this way.

### Examples

Each example builds its controller from a pinned upstream tag. Its plans and seeds give no
findings, and a negative control, where one exists, must fail.

- [examples/cert-manager](examples/cert-manager/config.yaml) runs cert-manager. Its
  `bugs/` plan holds the Secret watch across a recreate, and `fresh-key` finds
  [a real bug](docs/findings/cert-manager-secret-write-back.md): the new Certificate gets the
  deleted one's key. `make test-example-cert-manager` needs it in 1 of 3 runs, and
  `make repro-cert-manager` prints its reproduction rate. A hunt can report a false Restore
  finding on a CertificateRequest that a `damage` deleted after issuance, since cert-manager
  rightly leaves it gone.
- [examples/external-secrets](examples/external-secrets/config.yaml) runs external-secrets
  with its Vault provider against a Vault dev server. Its `bugs/` plan finds
  [a real bug](docs/findings/external-secrets-onchange-stale-failure.md): after a lost Secret
  create, an `OnChange` ExternalSecret stays `Ready=False`.
  [rotation](examples/external-secrets/rotation/config.yaml) rotates a Vault value under
  `refreshPolicy: Periodic`.
- [examples/ack-s3](examples/ack-s3/config.yaml) runs ACK's s3-controller against moto. Its
  [README](examples/ack-s3/README.md) lists the ACK bugs it found and the harness bugs it
  exposed.

## Reading a failure

Each finding's heading names its check:

| Check | It fails when |
| --- | --- |
| Converge | A step does not settle by its deadline, or does not stay settled. |
| Crash | The controller exits, and the harness did not cause the exit. |
| Clean | A resource that the controller made for a CR outlives the CR. |
| Restore | After a step that changes no CR or fixture, the controller's resources differ from the previous step's end, as when a damaged one stays gone. |
| Unique | At a settled step's end, two live controller resources share an owner, kind, namespace and role. |
| Honest | The controller reports a CR ready before it sees the CR's dependencies and named objects ready, or, with `track`, keeps it ready longer than `track` after it sees one not ready. |
| Ordered | An object that a CR names under `onDelete: block` is gone while the CR names it, and the controller saw both in time to hold it. |
| Teardown | The controller releases a CR while one of the CR's resources that the garbage collector does not delete is not yet being deleted. |
| Backoff | The controller repeats a failing request more than 10 times per live CR, with gaps under 100 ms. For a write that changes nothing, the limit is 20 per live CR. |
| Finalizers | A controller write drops a finalizer that a model or the garbage collector owns. |
| Property | A [property](#properties) does not hold, or cannot be evaluated. |

[docs/failures.md](docs/failures.md) explains each finding and what to change when a run exits 2.
[DESIGN.md §12](DESIGN.md#12-checks) defines the checks. The `--out` directory holds `report.md`,
the `plan.json` that ran, `trace.jsonl`, `controller.log` and `harness.log`, where client-go logs
the errors of the harness's watches. `judge` rejudges a stored trace, and `shrink` shrinks a
failing plan to `shrunk.json`:

```sh
reconciler-fuzzer judge --trace reconciler-fuzzer-out/b3
reconciler-fuzzer shrink --config targets/toy-widget/config.yaml \
  --plan reconciler-fuzzer-out/b3/plan.json --launch-arg --bug=3 --out reconciler-fuzzer-out/b3-shrunk
```

## CI

Every command exits 0 with no findings, 1 with findings, and 2 on a usage or harness error.
`hunt` and `shrink` exit 1 on any find, even after a failed seed or an interrupt.
A minimal GitHub Actions job:

```yaml
jobs:
  fuzz:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
        with: {go-version: stable}
      - run: |
          go install github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
          go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
          index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
          echo "KUBEBUILDER_ASSETS=$(setup-envtest use 1.37.0 --index $index -p path)" >> "$GITHUB_ENV"
      - run: go build -o bin/manager ./cmd/manager
      - run: |
          for plan in fuzz/plans/*.json; do
            reconciler-fuzzer run --config fuzz/config.yaml --plan "$plan" --out "out/$(basename $plan)" || exit
          done
          for seed in 1 2 3; do
            reconciler-fuzzer run --config fuzz/config.yaml --seed "$seed" --out "out/seed-$seed" || exit
          done
```

Commit each plan that once found a bug, so the job keeps checking it.

This repository's own tiers are make targets ([DESIGN.md §16](DESIGN.md#16-gates-examples-and-testing)).
`make help` lists them. [docs/bug-matrix.md](docs/bug-matrix.md) lists the toys' bugs and the
check that catches each.

## Terms

[DESIGN.md §1](DESIGN.md#1-terms) defines the design's terms, such as plan, step and slot.
The README also uses these:

| Term | Meaning |
| --- | --- |
| Target | The controller under test, with its config. |
| CR | An object of a kind that the config's `primaries` lists. A plan creates only CRs. |
| Fixture | An object the harness creates before step 0, such as a ConfigMap the controller reads. |
| envtest | A kube-apiserver and etcd with no other controllers, from controller-runtime. |
| Seed | A number that fixes a generated plan, not its timing. |
| Sample | The CR in an entry's `sample`. Generated CRs of its kind start from it. |
| `ready` | CEL over a CR that is true once the controller has acted on its spec. |
| `stable` | How long the controller must stay quiet for a step to settle. |
| `crudfake` | The harness's JSON server, a fake service for a REST JSON API. |
| Emulator | A real API's local server, such as a Vault dev server or moto for S3. |
| Unknown outcome | A request that reached the server, but whose response the caller never saw. |
