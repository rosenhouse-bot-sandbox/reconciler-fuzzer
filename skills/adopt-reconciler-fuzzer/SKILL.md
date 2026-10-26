---
name: adopt-reconciler-fuzzer
description: Sets up reconciler-fuzzer for a Kubernetes controller. Writes config.yaml, a sample custom resource and plans, builds the controller, runs reconciler-fuzzer until it tests the controller, and reports what it finds. Use when the user wants to fuzz their operator or controller, or to adopt or try reconciler-fuzzer in their repository.
---
# Adopt reconciler-fuzzer

reconciler-fuzzer runs a controller against envtest, follows a plan of CR changes, faults,
restarts and damage, and judges the trace with eight checks. This skill writes the config for
one controller and runs it until reconciler-fuzzer tests the controller.

Never edit the controller's source without the user's consent.

Copy this checklist into your task list and track it:

```
- [ ] 1. Install reconciler-fuzzer and find its docs
- [ ] 2. Check that the controller fits
- [ ] 3. Choose a directory
- [ ] 4. Write config.yaml
- [ ] 5. Write the sample
- [ ] 6. Build the controller
- [ ] 7. Run a hand-written plan
- [ ] 8. Read report.md and fix the config
- [ ] 9. Hunt seeds
- [ ] 10. Commit plans and seeds
- [ ] 11. Report
```

## 1. Install reconciler-fuzzer and find its docs

Run the commands of the README's Install section. In short:

```sh
GOTOOLCHAIN=auto go install github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
setup-envtest use 1.37.0 --index $index -p path
```

Shell variables do not last from one command to the next. Note the printed path and set
`KUBEBUILDER_ASSETS` to it on each `reconciler-fuzzer` command. Prefix commands with
`PATH="$(go env GOPATH)/bin:$PATH"` if that directory is not on `PATH`.

The docs that match the binary are in its module directory, `<module>` below:

```sh
version=$(go version -m "$(command -v reconciler-fuzzer)" | awk '$1 == "mod" {print $3}')
echo "$(go env GOMODCACHE)/github.com/rosenhouse-bot-sandbox/reconciler-fuzzer@$version"
```

If the version is `(devel)`, the binary was built from a clone, and `<module>` is that clone.
Read these rather than guessing:

- `<module>/DESIGN.md` §13 lists every config key, §12 every check, §8 the plan format.
- `<module>/docs/failures.md` explains each finding and each exit 2.
- `<module>/examples/` holds real configs and plans.

## 2. Check that the controller fits

Read the controller's `main`, API types, reconcilers and RBAC. Stop and tell the user when one
of these holds:

- It cannot run against envtest: it needs a webhook to admit its CRs, a conversion webhook, or
  nodes and Pods that do work.
- Its RBAC manifests are not files you can list, such as a Helm template you cannot render.

List the kinds it watches, reads and creates. For each, note who sets its status:

- The controller itself, or nobody: nothing to do.
- Another controller, such as a CRD's from another project: it needs a `models:` entry
  (step 4).
- A Pod's kubelet, a Job's or a PVC's controller: a model sets only a `Ready` condition and
  literal `status:` fields, such as `{phase: Bound}`. If the controller reads more than that,
  tell the user.
- A Deployment's, StatefulSet's, DaemonSet's or ReplicaSet's controller: a model cannot set
  `status.observedGeneration` or replica counts that follow the spec. Tell the user that
  reconciler-fuzzer cannot test this controller yet.

List the external HTTP APIs it calls, such as a cloud's. Each needs a fake service (step 4).
The controller must take the API's endpoint from a flag or an environment variable, and speak
plain HTTP to it. If it cannot, tell the user.

A config lists every CRD kind the controller serves, one `primaries` entry each. A CR may name
an object that it does not own, such as a Database's Cluster or the Secret that holds its
password. Declare each such field in `references` (step 4). If the controller holds a named
object's deletion until the CRs that name it are deleted, a generated plan that deletes the
object fails Converge clause 2. Hunt seeds (step 9) for such a controller with configs that list
one side alone and make each object it names a fixture. Hand-written plans can use the config
that lists every kind.

## 3. Choose a directory

Ask the user where the files go, and propose `test/reconciler-fuzzer/`. Add
`reconciler-fuzzer-out/` to `.gitignore`.

## 4. Write config.yaml

Every path resolves against the config's directory. This config is for a project whose
Guestbook, in group `webapp.example.com`, owns ConfigMaps and a Gadget from another project:

```yaml
launch:
  binary: ../../bin/manager
  args: [--leader-elect=false, --health-probe-bind-address=0, --metrics-bind-address=0]
  env:
    WATCH_NAMESPACE: ${systems.kube.namespace}
    ENABLE_WEBHOOKS: "false"
  rbac: [../../config/rbac/role.yaml]
crds: [../../config/crd/bases/, gadget-crd.yaml]
primary:
  kind: webapp.example.com/v1/Guestbook
  sample: guestbook.yaml
  ready: >-
    has(self.status) && has(self.status.conditions) && self.status.conditions.exists(c,
      c.type == "Ready" && c.status == "True"
      && has(c.observedGeneration) && c.observedGeneration == self.metadata.generation)
values:
  overlay: {spec.pages: {minimum: 1, maximum: 5}}
  distinct: [spec.configMapName]
  mutate: [spec.message, spec.pages]
  fixtures: {settings.yaml: [data.theme]}
fixtures: [settings.yaml]
models:
  - kind: gadgets.example.com/v1/Gadget
    stages: readyCondition
    after: 200ms
    dependency: {required: true, track: 1s}
```

Fill each key from the project:

- `launch.args` and `env`: drop each flag and variable the controller does not read. `0` turns
  off fixed ports, so runs cannot clash. Only `${systems.kube.kubeconfig}`, `namespace` and
  `url`, and each fake's `${systems.<name>.url}`, expand. `KUBECONFIG` defaults to
  `${systems.kube.kubeconfig}`. Pass the kubeconfig as a flag if the controller does not read
  `KUBECONFIG`, as when it calls `clientcmd.BuildConfigFromFlags` with an empty path.
- `launch.rbac`: the Roles and ClusterRoles the controller runs with. Generation draws faults
  from the requests they grant. For another namespace, use a ClusterRole, or a Role with
  `metadata.namespace`.
- `crds`: each listed kind's CRD, and the CRD of every other kind the controller watches or
  creates.
- `primary` describes one kind. For several, write `primaries`, a list of such entries, and
  move each kind's `overlay`, `distinct` and `mutate` into its entry's `values`.
- An entry's `ready`: CEL over the CR, true only once the controller acted on the current spec.
  Compare an `observedGeneration` with `metadata.generation`. If the controller writes no
  status, use `"true"` and tell the user.
- An entry's `retain`: CEL over a deleted CR that allows its resources to stay. Set it only where
  the controller keeps resources by design, such as under a `deletionPolicy: Retain`.
- `values`: no webhook runs, so `overlay` narrows the CRD schema to what the webhooks admit.
  A field naming another object gets a `references` entry, or else
  `{enum: [<sample's value>]}`.
  `distinct` names fields that must differ between CRs, such as a Secret's name. `mutate`
  names the fields updates change. `values.fixtures` names fixture strings to change.
- `fixtures`: objects the controller reads and does not create, of kinds the config does not
  list. Drop `metadata.namespace` unless the controller reads the object in another namespace;
  the harness creates that namespace.
- `references: [{from, to, name, namespace, ready, required, track}]`: one entry for each field
  by which a CR names an object that it does not own, such as an issuer, a store or a Secret.
  `to` is the named kind, or `<system>:<collection>` for a fake's. Put the `to` kind's CRD in
  `crds` unless Kubernetes serves the kind without one, such as `v1/Secret` or
  `apps/v1/Deployment`. `name` and `namespace` are field paths in the CR, with `[*]` for each
  list item; `namespace` defaults to the CR's. The `from` CRD's schema must describe each path
  as a string, and `name` may not be the field that an `owns` entry reads. `ready` is CEL over
  the named object, true once the CR may be ready; it defaults to the `ready` of the `to` kind's
  entry, model or fake. A model's `ready` compares `observedGeneration` with
  `metadata.generation`, as an entry's should; when the controller checks only the object's
  condition or phase, write `ready` without that comparison. `required: false` allows a CR to be
  ready while the object is missing. Set `track` when the controller should notice the object
  turning not ready.
- An entry's `owns`, and `systems`: a child without an ownerReference to its CR, such as one in
  another namespace, or a cluster-scoped child of a namespaced CR, needs one. Otherwise Clean
  judges it only by the CRs alive at its creation that the controller did not create.
  `owns: [{kind, name, namespace?}]` holds CEL over the CR that names the child; a
  cluster-scoped kind takes no `namespace`.
  `systems: [{name: kube, owner}]` holds CEL over a child that yields its CR's UID,
  `namespace/name` or name, such as an owner label. Prefer the UID: a bare name that
  matches CRs in two other namespaces, or CRs of two kinds, names none.
- `models`: one entry for every kind whose status another controller sets and the controller
  reads. Add `dependency` when a CR's readiness waits on it and the CR owns it: through
  ownerReferences, directly or through owned objects, or by `owns` or `owner`. Honest judges only such
  dependencies, so leave `dependency` off a kind the CR names by a field, such as an
  issuer or a store, and write a `references` entry instead. A dependency belongs only to the
  nearest CR that owns it, even one that the controller created. `required: false` allows a CR
  to be ready with none. `required` binds
  only CRs of the first entry, so list first the kind of that nearest CR. A config that does
  not list that kind leaves `dependency` out. Set `track` when the
  controller should notice a dependency going not ready. `status:` adds literal fields to each
  status write the model makes; it replaces the whole `conditions` list. `stuckStatus:`
  replaces those fields while a `stuck` fault lasts, and defaults to `status:`. Each write
  removes the fields that only the other key sets. Set `stuckStatus:` when `status:` means
  done, such as `{phase: Bound}`. Otherwise a controller that reads only those fields never
  sees the fault. For a Job, set `stuckStatus: {active: 1}`. A Job created during the fault
  then stays unfinished. The API server refuses to lower a done Job's `succeeded`, so that Job
  stays done and `Ready`. The model retries the refused write until the fault ends. Add
  `finalizer: {name, after}` when something else puts a finalizer on each object, such as
  Kubernetes' `kubernetes.io/pvc-protection` on a PVC. The model adds `name` to each live
  object, and removes it `after` past the deletion. Finalizers then fails a controller write
  that drops it. Keep `after` under `timeouts.stable`, or a step can settle before the
  controller reacts to the release.
- `budgets` and `timeouts`: leave them out at first. Raise `timeouts.settle` for a slow
  controller, or one that waits out `finalizer` holds one by one, and `stable` for one that
  requeues with a delay by design. Ask the user first.

### External APIs

Put each external API behind a fake service, a `systems:` entry that the harness runs:

```yaml
launch:
  env: {CLOUD_ENDPOINT: "${systems.cloud.url}"}
systems:
  - name: cloud
    process: {command: [crudfake, --config, cloud.yaml]}
    api: crud
    owner: self.labels['k8s-uid']
    role: self.labels['role']
    ready: self.state == 'READY'
    gone: self.state == 'DELETING'
    dependency: {required: true}
    poll: 1s
```

- For a REST JSON API, use `crudfake`, which `go install
  github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/crudfake@latest` installs. Its file
  maps each collection to `unique` fields, the states `pending`, `ready` and `deleting`, and
  `createDelay` and `deleteDelay`. `<module>/targets/toy-bucket/cloud.yaml` is one. The
  controller must use `POST /<c>`, `GET /<c>?<field>=<value>` and `/<c>/<id>`; if it uses other
  paths, tell the user.
- Otherwise the user writes a fake that serves the `/_fake/` contract of the README's "Fake
  services" section and the `crud` map's paths. Its bodies need a string `id` it never reuses
  and a `version` that changes with each change. An API that reuses ids, such as one keyed by
  name, needs a fake that adds a suffix. Ask before writing one.
- Point the controller's endpoint at `${systems.<name>.url}`, never at the real API.
- `owner` is CEL over one of the fake's resources that yields its CR's UID or name, such as a
  tag or label the controller sets. Without it, Clean judges the resource only by the CRs alive
  at its creation, and Honest ignores it.
- `role` is CEL that tells apart resources one CR owns on purpose. Without it, Unique fails a CR
  with two resources in one collection.
- `ready` and `gone` are CEL over a resource. `dependency` needs `ready`. `poll` is how often the
  controller reads the API; it lengthens each step's deadline.
- For Vault or S3, run the real emulator instead: `api: vault` with a Vault dev server, or
  `api: s3` with moto's `moto_server`. In a clone of reconciler-fuzzer, `make setup` installs
  both under `bin/`.
  `token` is the token the harness sends, and `${systems.<name>.token}` expands to it; Vault
  needs one. Set `access: read` when the controller only reads the emulator, so generated
  plans spend no damage or write faults on it.
  `fixtures` lists files of `{resource, name, body}` entries the harness writes first, such
  as a KV secret under mount `secret`. `<module>/examples/external-secrets/config.yaml` and
  `<module>/examples/ack-s3/config.yaml` are examples.
- Kube fixtures expand `${systems.<name>.url}` and `.token`, so a Secret fixture can carry the
  controller's credentials for the emulator.

### Properties and timers

- `properties: [{name, expr, when}]` holds CEL that must hold at each settled step's end, or at
  every version with `when: always`. It sees `crs` and `world.<system>.<key>`, such as
  `world.kube.secrets`. Write one when the user names an invariant the checks cannot know, such
  as "the Secret holds Vault's current value". Ask the user before adding one. A property needs
  no `'k' in world.<system>` guard for a kind that always has a key. An installed CRD always
  has one, as does each listed kind and each kind that `models` or a fixture names. A kind that
  a `launch.rbac` rule names without `*`, other than Events and Leases, has one if the server
  lists and watches it in the rule's group. A fake collection that an entry's `owns` names has
  one too. Guard any other kind or collection.
- An entry's `periodic: {every, paths}` names the CR fields the controller rewrites on a timer,
  such as `status.refreshTime`, or `status.conditions[*].lastTransitionTime` for a field of
  every list item. Name the narrowest path: a broader one also excuses real flapping. Without
  it, a controller that refreshes every few seconds never settles.

## 5. Write the sample

Write one sample per entry. Copy a CR from `config/samples/` or the docs beside `config.yaml`.
Drop `metadata.namespace`. Set every field a defaulting webhook would set, and give every
reference the name of the object it names: a fixture, or another entry's sample.

## 6. Build the controller

Build it to `launch.binary`, such as `go build -o bin/manager ./cmd`. Regenerate CRDs and RBAC
first if the project generates them. If `git status` then shows changes, tell the user.
Rebuild after each change to the controller.

## 7. Run a hand-written plan

Write `plans/create.json`. It creates the sample, updates it, and deletes it:

```json
{"steps": [
  {"actions": [{"do": "create", "slot": "a", "object": <the sample as JSON>}]},
  {"actions": [{"do": "update", "slot": "a", "patch": {"spec": {"pages": 2}}}]},
  {"actions": [{"do": "delete", "slot": "a"}]}
]}
```

With several kinds, step 0 also creates each other entry's sample, each in a slot of its own.
Delete a CR that others name only after them, since its controller may hold its deletion until
they are deleted. With a `references` entry to a fixture, add a step that deletes the fixture,
`{"do": "changeFixture", "target": {"resource": "secrets", "name": "<fixture>"}}`, and a later
one that makes it again, `{"do": "createFixture", "target": <the same target>}`. With a model,
add a step whose fault keeps it not ready, such as
`{"actions": [...], "period": "3s", "faults": [{"match": {"group": "gadgets.example.com",
"resource": "gadgets"}, "stuck": true}]}`. With a fake, add a step that drops the response
to the first create, such as `{"actions": [...], "period": "1s", "faults": [{"system": "cloud",
"match": {"verb": "create", "resource": "buckets"}, "nth": 1, "dropResponse": true}]}`, and one
that damages a resource, `{"do": "damage", "slot": "a", "target": {"system": "cloud"}}`. With a
Vault fixture, add a step that writes a new version, `{"do": "changeFixture", "target":
{"system": "vault", "resource": "secret", "name": "token"}, "patch": {"value": "new"}}`. A step
that deletes and creates slot `a` with `"faults": [{"match": {"resource": "secrets"},
"holdWatch": true}]` tests how the controller acts on a stale cache. A last step
`{"actions": [{"do": "delete", "slot": "a"}, {"do": "stop"}]}` tests an uninstall. A report
note that the `holdWatch` held no watch means the controller watches no such kind, or the match
is wrong.
A note that a `stuck` fault held no object means no object matched, or the API server refused
each stuck write. `trace.jsonl` records a refused write as a model call with status 422. The
API server refuses every stuck write to a done Job. Otherwise, check `stuckStatus:` against the
kind's status schema.
`<module>/targets/*/plans/` hold more.
Run it from the config's directory, in the background, and act on the exit code:

```sh
KUBEBUILDER_ASSETS=<path> reconciler-fuzzer run --config config.yaml --plan plans/create.json --out reconciler-fuzzer-out/create
```

## 8. Read report.md and fix the config

**Exit 2**: nothing was judged. The message names the cause, and
`<module>/docs/failures.md` lists the usual ones. `controller.log` in `--out` holds the
controller's output. Fix the config or the sample.

**Exit 1**: read `report.md` in `--out`. First decide whether the config misdescribes the
controller:

| Finding | What the config got wrong | Fix |
| --- | --- | --- |
| Converge, with a line `whose status nothing models` | Another controller sets that kind's status. | Add the `models:` entry it suggests. |
| Converge | `ready` reads a field the controller never writes. | Fix `ready`. |
| Converge, clause 7, on CR writes each refresh period | The controller refreshes on a timer. | Set the entry's `periodic`, and raise `budgets.reads` if each refresh reads more than twice. |
| Converge | A drawn value is one a webhook would refuse. | Narrow `values.overlay`. |
| Honest, `reported ready with no dependency` | The CR can be ready without that kind, or does not own it. | Set `required: false` in the first case. In the second, add `owns` or `owner`, or drop `dependency`. |
| Honest, on an object that a `references` entry names | The CR may be ready while the object is missing or, by `ready`, not ready. | Set the entry's `required: false`, or fix its `ready`. |
| Honest, `reported ready before seeing its dependency ready`, on a named object whose spec just changed | The default `ready` waits for the object's `observedGeneration`, but the controller checks only its condition or phase. | Set the entry's `ready` to the controller's check. |
| Converge, clause 1, `the controller last saw … ready` | The entry's `ready` accepts an object that the controller treats as not ready. | Fix the entry's `ready`. |
| Converge, clause 1, `the controller last saw … missing` or `not ready`, on an object that is ready now | Nothing, if the controller does not watch the named kind: it missed the change. | Report the bug (step 11). The controller should watch that kind and requeue the CRs that name the object. |
| Clean | The controller keeps resources by design. | Set the entry's `retain`. |
| Unique, on a fake | The CR owns several resources in one collection by design. | Set the system's `role`. |
| Converge or Restore, with a CR that halted after a create of unknown outcome | The controller stops on purpose and waits for a person. | Set the entry's `halted` to CEL over its halted condition. |

Every other finding is a bug in the controller. Never hide one to make a check pass, and ask
the user when in doubt. On a finding, stop and report it (step 11).

A note `unmapped request` means the API map could not read a request to a fake, so no check
judged it. Tell the user which paths the controller uses.

**Exit 0**: no findings. Go on.

If five runs in a row make no progress, stop and report what you tried.

## 9. Hunt seeds

```sh
KUBEBUILDER_ASSETS=<path> reconciler-fuzzer hunt --config config.yaml --seeds 20 --out reconciler-fuzzer-out/hunt
```

It replays and shrinks each finding, which takes many minutes. Run it in the background.
`summary.md` lists each finding with its seed and shrunk plan. Treat each as in step 8.

## 10. Commit plans and seeds

Commit `config.yaml`, the sample, fixtures, `plans/`, and a `seeds` file of the seeds that ran
clean. Copy each shrunk plan of a finding into `plans/` so CI keeps checking it. CI runs each
plan and seed with `reconciler-fuzzer run`, as the README's CI section shows.

## 11. Report

Tell the user:

- Which files you wrote, and the command that runs them.
- How the last run and hunt ended. For each finding, give its check, summary, `report.md`
  path, and a replay command with `--plan <out>/plan.json`.
- What you assumed, such as `ready`, the overlay and each model.
