---
name: adopt-reconciler-fuzzer
description: Sets up reconciler-fuzzer for the Kubernetes controller of a kubebuilder project. Writes target.yaml and a sample custom resource, builds the controller, runs reconciler-fuzzer until it tests the controller, and reports what it finds. Use when the user wants to fuzz their kubebuilder operator or controller, or to adopt or try reconciler-fuzzer in their repository.
---

# Adopt reconciler-fuzzer

reconciler-fuzzer runs a controller against envtest, acts on its custom resources (CRs), and
checks that it converges, goes quiet and cleans up. This skill writes the target for a
kubebuilder project and runs it until reconciler-fuzzer tests the controller.

Never edit the controller's source without the user's consent.

Copy this checklist into your task list and track it:

```
- [ ] 1. Install reconciler-fuzzer and find its docs
- [ ] 2. Check that the controller fits
- [ ] 3. Choose a directory
- [ ] 4. Build the controller
- [ ] 5. Write target.yaml and the sample
- [ ] 6. Run until reconciler-fuzzer tests the controller
- [ ] 7. Report
```

## 1. Install reconciler-fuzzer and find its docs

If `reconciler-fuzzer` is not on `PATH` or in `$(go env GOPATH)/bin`, install it:

```sh
GOTOOLCHAIN=auto go install github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
```

`go install` writes to `$(go env GOPATH)/bin`. If that is not on `PATH`, prefix each command
with `PATH="$(go env GOPATH)/bin:$PATH"`. Then print the directory of the module it was built
from:

```sh
echo "$(go env GOMODCACHE)/github.com/rosenhouse-bot-sandbox/reconciler-fuzzer@$(reconciler-fuzzer version | cut -d' ' -f2)"
```

Below, `<module>` stands for that directory. If it is missing, run
`go mod download github.com/rosenhouse-bot-sandbox/reconciler-fuzzer@<version>`. If the version ends in
`+dirty`, or the download fails, reconciler-fuzzer was built from a clone, and `<module>` is
that clone.

The docs there match the installed binary. Read them rather than guessing:

- `<module>/docs/reference.md` lists every key of target.yaml, with its default.
- `<module>/docs/targets.md` says how to choose each value.
- `<module>/docs/checks.md` says what each check requires.
- `<module>/docs/failures.md` says what each file of a failure holds.

`<module>/README.md#install` installs reconciler-fuzzer, then `setup-envtest` and the envtest
control plane. Run the rest of its commands. Shell variables do not last from one command to
the next, so note the path in `KUBEBUILDER_ASSETS` and set it on each `reconciler-fuzzer`
command.

## 2. Check that the controller fits

Read `PROJECT`, `main.go`, the API types, the controllers and `config/`. Stop and tell the
user when one of these holds:

- The primary kind's CRD under `config/crd/bases/` says `scope: Cluster`. reconciler-fuzzer
  tests namespaced kinds only.
- The controller creates a cluster-scoped object, or an object in another namespace.
- The controller waits for a Pod, a Deployment, a Job or a PersistentVolumeClaim to become
  ready. envtest runs no Pod. Offer a kind cluster instead, as
  `<module>/docs/targets.md#against-a-cluster` says.
- `PROJECT` sets `conversion: true` under a resource's `webhooks`. No conversion webhook runs.

One target.yaml tests one primary kind. If `PROJECT` lists several kinds with controllers,
ask which to start with.

## 3. Choose a directory

Ask the user where the files go, and propose `test/reconciler-fuzzer/`. Add
`reconciler-fuzzer-out/` to `.gitignore`.

## 4. Build the controller

```sh
make manifests
go build -o bin/manager ./cmd
```

`make manifests` writes the CRDs under `config/crd/bases/` and `config/rbac/role.yaml`. If
`git status` then shows changes, tell the user. A kubebuilder v3 project keeps `main.go` at
its root, so build `.` there. Rebuild after each change to the controller.

## 5. Write target.yaml and the sample

This is the target of a project named guestbook. Its Guestbook, in group
`webapp.example.com`, owns ConfigMaps. reconciler-fuzzer reads `crds`, `sample`, `rbac` and
`fixtures` relative to target.yaml. It reads `launch.binary` relative to the directory it runs
in, which is the repository root here.

```yaml
name: guestbook
crds:
  - ../../config/crd/bases/
primary: webapp.example.com/v1/Guestbook
sample: guestbook.yaml
manages:
  - v1/ConfigMap
rbac:
  - ../../config/rbac/role.yaml
ready: >-
  has(status.conditions) && status.conditions.exists(c, c.type == "Ready"
  && c.status == "True" && has(c.observedGeneration)
  && c.observedGeneration == metadata.generation)
launch:
  binary: bin/manager
  args:
    - --leader-elect=false
    - --health-probe-bind-address=0
    - --metrics-bind-address=0
  env:
    ENABLE_WEBHOOKS: "false"
```

Fill each key from the project:

- `crds` also lists the CRD of each kind from another project that the controller watches or
  creates. Without it, the controller never starts.
- `primary` joins the CRD's `spec.group`, its storage version and its kind.
- `manages` lists each kind the controller creates. Look for `Owns(`, `SetControllerReference`
  and `CreateOrUpdate(` in the controller. Leave out the primary kind, Events and Leases. Write
  a core kind as `v1/Kind`, and any other as `group/version/Kind`.
- `ready` depends on how the reconciler writes status:
  - If it sets `status.observedGeneration`, leave `ready` out. The default compares it with
    `metadata.generation`.
  - If it sets a condition with `ObservedGeneration`, use the expression above with that
    condition's type.
  - If it writes no status, set `ready: "true"`. A wait then ends once nothing has changed for
    `timeouts.stable`. Tell the user.
  - Otherwise, write a `ready` that holds once the controller has finished. Tell the user that
    it cannot tell a stale status from a current one.
- `launch.args` holds kubebuilder's flags. `0` turns off the health probe and metrics
  servers, so no port collides. Drop each flag that `main.go` does not define. Set
  `ENABLE_WEBHOOKS` only where `main.go` reads it. reconciler-fuzzer sets `KUBECONFIG` for
  the controller.
- `launch.env` sets `WATCH_NAMESPACE: $NAMESPACE` where `main.go` reads `WATCH_NAMESPACE`, as
  a project scaffolded with `--namespaced` does. Its `role.yaml` then holds a Role with a
  `metadata.namespace`. Copy it beside target.yaml without that field, and list the copy
  under `rbac`.
- `fixtures` lists files of objects the controller reads and does not create, such as a
  ConfigMap the spec names. Write each beside target.yaml, with no `metadata.namespace`, under
  the name the sample's spec gives. If the controller watches a fixture, let generation change
  and delete it under `generate.fixtures`, as `<module>/docs/targets.md#fixtures` says.

Leave out `timeouts`, `thresholds` and `properties`. Their defaults suit a first run.

Copy the sample from `config/samples/`. Drop `metadata.namespace`, and replace each `TODO`
with a spec the controller accepts.

No webhook runs, so narrow generation to what the webhooks would admit. Search for
`Default(`, `ValidateCreate(` and `ValidateUpdate(`. For each field a defaulting webhook
sets, set it in the sample and make it required, because generation drops optional fields.
Keep the CRD's own required fields in that list. The guestbook's webhook defaults `pages`:

```yaml
generate:
  overlay:
    spec: {required: [message, pages]}
```

For each spec field that names another object, such as a fixture, allow only the sample's
value, as `spec.sourceRef: {enum: [source]}` under `overlay` does. A random name leaves the
controller waiting. `<module>/docs/targets.md#generated-values` says more.

## 6. Run until reconciler-fuzzer tests the controller

Run this from the repository root:

```sh
KUBEBUILDER_ASSETS=<path> reconciler-fuzzer run --target test/reconciler-fuzzer/target.yaml --seed 1 --runs 3
```

reconciler-fuzzer minimizes a failing sequence before it exits, which can take many minutes.
Run the command in the background, and wait for it to exit. Act on the exit code.

**Exit 2**: reconciler-fuzzer could not test the controller, and its message says why.
`<module>/docs/failures.md#when-reconciler-fuzzer-exits-2` lists the usual causes.
`target.log` in the run's directory holds the controller's output. Fix target.yaml or the
sample, and run again. If only a change to the controller would fix it, stop and ask the user.

**Exit 1**: a check failed. Read `report.md` in the run's directory, and the check's section
in `<module>/docs/checks.md`. Then decide whether target.yaml misdescribes the controller.
These failures can mean it does:

| Check | What target.yaml got wrong | Fix |
|---|---|---|
| G4 | `ready` reads a field or condition the controller never writes. | Fix `ready`. |
| G4 | Generation drew a spec value that the controller rejects, or that a webhook would. | Narrow generation, as step 5 says. |
| G1, G2 | The controller requeues with `RequeueAfter` by design. | Ask the user, then set `thresholds.quiet` as `<module>/docs/targets.md#thresholds` says. |
| G5 | The controller stamps a field when it starts. | Name it under `equalIgnore`, as `<module>/docs/targets.md#equalignore` says. |
| G7 | The controller leaves a deleted kind deleted, or recreates it under a new name, by design. | List the kind under `notRecreated`. |

Every other failure is a finding, such as G3 for a child without an ownerReference. Never
hide a finding to make a check pass. Do not drop a kind from `manages`, raise a threshold or
narrow generation unless the controller's design calls for it. Ask the user when in doubt.

On a finding, stop. Give the user the check, the line reconciler-fuzzer printed, the path of
`report.md`, and the replay command that `report.md` gives, with `KUBEBUILDER_ASSETS` set.

**Exit 0**: every run passed. Three runs draw few ops, so run once more with `--runs 10`, in
the background, before you report.

If five invocations in a row make no progress, stop and report what you tried.

## 7. Report

Tell the user:

- Say which files you wrote, and give the command that runs them.
- Say how the last invocation ended, and give each finding.
- Say what you assumed, such as the `ready` you chose and the values in the sample.
- Suggest writing sequences for what generation does not draw, as
  `<module>/docs/targets.md#sequences-you-write` says, and running in CI, as
  `<module>/docs/ci.md` says.
