# reconciler-fuzzer

reconciler-fuzzer finds bugs in Kubernetes controllers. It runs your controller on your machine
against envtest's kube-apiserver and etcd. It follows a plan of steps: CR changes, faults on the
controller's API requests, restarts, and damage to objects the controller created. It records
every request and object version in a trace, including requests to fake HTTP services. Then it judges the trace with eight checks: Converge,
Crash, Clean, Restore, Unique, Honest, Backoff and Finalizers ([DESIGN.md §12](DESIGN.md#12-checks)).

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

Step 1, at 14.814449459s.

```
configmaps rf-cd767d99/widget-0 (rule 4, slot a)
```
````

`--seed n` draws a plan from the config instead. `hunt` runs many seeds, replays each new
finding, and shrinks its plan:

```sh
reconciler-fuzzer run --config targets/toy-widget/config.yaml --seed 6 --out reconciler-fuzzer-out/seed-6
reconciler-fuzzer hunt --config targets/toy-widget/config.yaml --seeds 3 \
  --launch-arg --bug=3 --out reconciler-fuzzer-out/hunt
```

A toy run takes half a minute or more, and `hunt` replays and shrinks each finding, so this
hunt takes several minutes. It writes `summary.md`, which lists each finding with its seed,
reproduction rate and shrunk plan.

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
- `ready` is CEL over the CR. It must be true only once the controller has acted on the
  current spec, so compare an `observedGeneration` with `metadata.generation`.
- `stable` is how long the controller must stay quiet before a step counts as settled.
- `launch.rbac` also tells generation which requests a fault can pick. A generated `damage` of
  a kube object targets an unlisted kind that `launch.rbac` lets the controller create. Faults
  and damage favor the listed kinds, the `models:` kinds and the kube kinds that `owns` names.
  Without models and kube `owns`, they favor the listed kinds and the core kinds that the
  controller may create, such as Secrets.
- `values` steers generated CRs: `overlay` narrows the CRD schema, `distinct` fields differ
  across a plan's CRs, `mutate` names the fields updates change, and `fixtures` names the
  fixture strings `changeFixture` sets.

### Several kinds

A controller that serves several CRD kinds lists each under `primaries`. `primary` is shorthand
for a list of one. An entry takes `primary`'s keys, and its own `overlay`, `distinct` and
`mutate` under `values`:

```yaml
primaries:
  - kind: example.com/v1/Cluster
    sample: cluster.yaml
    ready: &ready >-
      has(self.status) && has(self.status.observedGeneration)
      && self.status.observedGeneration == self.metadata.generation
    values: {mutate: [spec.size]}
  - {kind: example.com/v1/Database, sample: database.yaml, ready: *ready}
fixtures: [settings.yaml]
values: {fixtures: {settings.yaml: [data.theme]}}   # with primaries, only fixtures
```

- Every check judges the CRs of each listed kind by their own entry, and the settle condition
  waits for each. A CR that the controller creates is also judged as its owner's resource, so
  Clean fails it when it outlives that owner. A `damage` in the owner's slot can target the
  CR's resources, and Restore treats a damage under the CR as a damage under its owner.
- A plan creates only CRs of listed kinds, and `run` refuses any other create. A create
  without an object makes the first entry's sample.
- Generated plans draw each create's kind from the list. They do not yet fill a field that
  names another CR, such as a Database's `clusterRef`. So
  [targets/toy-fleet](targets/toy-fleet)'s `database/config.yaml` lists only Database, and
  makes the Cluster it names a fixture.
- A fixture may not be a CR. An object whose ownerReferences lead to no CR, but reach a live
  object that the controller did not create, such as a fixture, belongs to that object. They may
  reach it through live objects that the controller made. `owner` and `owns` do not change this.
  Clean skips such an object while its owner lives.
- A dependency belongs to the nearest CR that owns it, even one that the controller created.
  `dependency.required` binds only CRs of the first entry.

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
  - name: kube                             # takes only owner
    owner: self.metadata.labels['example.com/owner-uid']   # a label on the child names the CR
```

- `owns` takes CEL over the CR. Without `namespace`, a namespaced child is in the CR's namespace.
  A cluster-scoped kind takes no `namespace`.
- `owner` takes CEL over the child. It yields the CR's UID, `namespace/name` or name. A bare name
  prefers a CR in the child's namespace, and names no CR when it matches CRs in two other namespaces.
  A name that CRs of two kinds share names no CR.
- Without either, Clean blames such a child on every CR that lived when it was created, except
  those that the controller created. It fails only once all of them are gone. A child made while
  none lived is shared, and fails only if the run ends with more shared children of its kind and
  role than the first create saw.

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
  the controller saw its Gadget not ready.
- Generated plans then draw two model faults. `stuck` keeps Gadgets not ready, and `slowDelete`
  holds their deletion. Each ends with the fault period. A create that waits until a deleted CR
  no longer exists ends them once `period` has passed. A plan step writes one as
  `{"match": {"group": "example.com", "resource": "gadgets"}, "stuck": true}`.
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
- A plan faults the fake with `"system": "cloud"`: any request fault, such as
  `{"system": "cloud", "match": {"verb": "create", "resource": "buckets"}, "nth": 1, "dropResponse": true}`,
  and `stuck` or `slowDelete`. `damage` takes `"target": {"system": "cloud"}`.
- An entry's `halted` is CEL over its CR. It counts a CR as done after a request of unknown
  outcome, for a controller that stops on purpose. [targets/toy-bucket/halted](targets/toy-bucket/halted/config.yaml)
  uses it.
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
each system, such as `world.kube.secrets` or `world.vault.secret`:

```yaml
properties:
  - name: synced
    expr: >-
      crs.all(c, world.vault.secret.all(v, v.path != c.spec.data[0].remoteRef.key
        || world.kube.secrets.exists(s, s.metadata.name == c.spec.target.name
          && s.data.token == base64.encode(bytes(v.data.value)))))
```

A kind the harness watches from the start maps to an empty list while no object of it exists.
These include each installed CRD, each listed kind, and each kind that `models` or a fixture names.
They also include each kind that a `launch.rbac` rule names without `*`, except Events, Leases
and the other kinds the checks ignore. The harness skips a kind that the server does not list
and watch in the named group. The `run` event of `trace.jsonl` lists these kinds in `kinds`. A
fake collection that an entry's `owns` names also maps to an empty list. Any other kind or
collection has a key only if the trace holds an object of it.

`firstWrittenUnder(object, path)` names the CR that owned the object when its value at `path`
first appeared, across deletes and recreates. cert-manager's `fresh-key` property uses it. A
failing property's details say which instance and CR such a value came from.

A controller that refreshes on a timer writes its CR each period. In the CR's entry,
`periodic: {every: 2s, paths: [status.refreshTime]}` lets those writes pass the settle
condition. `[*]` names a field of every list item, as in `status.conditions[*].lastTransitionTime`.

### Holding watch events

A `holdWatch` fault pauses the controller's watch events of one kind for the fault period, and
delivers them in order when it ends. The controller then acts on a stale cache, as on a busy
cluster. The settle condition counts a held watch as looking only once the hold ends:

```json
{"actions": [{"do": "delete", "slot": "a"}, {"do": "create", "slot": "a", "object": {}}],
 "period": "2s", "faults": [{"match": {"resource": "secrets"}, "holdWatch": true}]}
```

The create waits until the deleted CR no longer exists. Once `period` has passed, the wait
ends the hold, so that the controller can finish the delete.

Generated plans draw it for kinds `launch.rbac` lets the controller watch, favoring the kinds
other faults favor, in a step that deletes a CR and creates it again unchanged. About 1
cert-manager seed in 12 holds Secrets this way, so a 20-seed `make hunt-cert-manager` usually,
but not always, finds #63. A report notes a `holdWatch` while the controller had no watch of
its kind, such as one with a misspelt resource.

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

### Examples

Each example builds its controller from a pinned upstream tag. Its plans and seeds give no
findings, and a negative control, where one exists, must fail.

- [examples/cert-manager](examples/cert-manager/config.yaml) runs cert-manager. Its
  `bugs/` plan holds the Secret watch across a recreate, and `fresh-key` finds
  [a real bug](docs/findings/cert-manager-secret-write-back.md): the new Certificate gets the
  deleted one's key. `make test-example-cert-manager` needs it in 1 of 3 runs, and
  `make repro-cert-manager` prints its reproduction rate.
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

[docs/failures.md](docs/failures.md) explains each finding and what to change when a run exits 2.
The `--out` directory holds `report.md`, the `plan.json` that ran, `trace.jsonl` and
`controller.log`. `judge` rejudges a stored trace, and `shrink` shrinks a failing plan to
`shrunk.json`:

```sh
reconciler-fuzzer judge --trace reconciler-fuzzer-out/b3
reconciler-fuzzer shrink --config targets/toy-widget/config.yaml \
  --plan reconciler-fuzzer-out/b3/plan.json --launch-arg --bug=3 --out reconciler-fuzzer-out/b3-shrunk
```

## CI

Every command exits 0 with no findings, 1 with findings, and 2 on a usage or harness error.
`hunt` exits 1 on any find, even after a failed seed.
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
