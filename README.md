# reconciler-fuzzer

reconciler-fuzzer finds bugs in Kubernetes controllers. It runs your controller on your machine, against
a real kube-apiserver and etcd from envtest, and records every request your controller makes.
Each run applies a random sequence of ops, drawn from a seed. The ops:

- create, update and delete your custom resources (CRs)
- restart your controller
- delete objects your controller manages
- inject API errors and delays

reconciler-fuzzer checks that your controller converges, goes quiet, cleans up after a deleted CR, and
recreates the objects that reconciler-fuzzer deletes. It also checks properties you declare. When a check
fails, reconciler-fuzzer minimizes a drawn sequence to the ops the failure needs, and writes a report.

You write no test code. One `target.yaml` describes your controller.

## Limitations

- Your controller runs on your machine, not in a Pod, even against a cluster. It cannot reach a
  Pod or a Service.
- No admission or conversion webhook of yours runs. Keep your controller and your CRs on the
  version your CRD stores, and keep drawn CRs
  [within what your webhooks admit](docs/targets.md#generated-values).
- reconciler-fuzzer generates sequences only for a custom resource whose CRD your target lists.
  If your controller reconciles a built-in kind, such as a Service,
  [write the sequences](docs/targets.md#sequences-you-write) yourself.
- envtest runs no Pod, so a Deployment, a Job or a PersistentVolumeClaim never becomes ready.
  If your controller waits on one, run reconciler-fuzzer
  [against a cluster](docs/targets.md#against-a-cluster), such as kind.
- reconciler-fuzzer tests namespaced kinds only, in one namespace per run. It misses a child your
  controller leaks into another namespace, and it cannot supply an object your controller
  reads from another namespace ([#38](https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/issues/38)).

## Install

```sh
go install github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
export PATH="$(go env GOPATH)/bin:$PATH"
index=https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 --index $index -p path)"
```

`setup-envtest` downloads etcd and kube-apiserver. Run the last three lines again in each new
shell. reconciler-fuzzer has no release yet, so `@latest` installs the tip of main.

Building reconciler-fuzzer takes Go 1.26.0 or later. Releases from Go 1.21 on download it for you. Many CI
images set `GOTOOLCHAIN=local`, which stops the download, and `go install` fails with
`requires go >= 1.26.0`. Install a newer Go, or run the commands with `GOTOOLCHAIN=auto`.

`reconciler-fuzzer --help` lists the commands, and `reconciler-fuzzer run --help` lists the flags.

## Quick start

This repository holds a toy controller with bugs you can switch on. Clone it with
`git clone https://github.com/rosenhouse-bot-sandbox/reconciler-fuzzer`, and run this in the clone. The build takes a
minute or two the first time, and the runs about half a minute:

```sh
go build -o bin/toy-widget ./targets/toy-widget
reconciler-fuzzer run --target targets/toy-widget/target.yaml --seed 1 --runs 3
```

```
run 1: seed 1, generated
run 2: seed 2, generated
run 2: P1 is not evaluated at the checkpoint after op 7 (settle): a fault was active there, or the target was still owed time to recover from one, so it may not yet have repaired what P1 reads
run 3: seed 3, generated
run 4: baseline
every run passed.
```

Each run starts the toy in a new namespace and applies a sequence of ops, drawn from the run's
seed. Run 2 drew a fault, so P1 skips a checkpoint where the toy may still be recovering. Run 4
is [the baseline](#the-baseline). The toy is correct, so every run passes.

Now switch on bug B3, which leaves a ConfigMap without an ownerReference. `--launch-arg` passes
an extra flag to the controller. `replay` runs a sequence file rather than a drawn sequence.
This one creates a Widget and deletes it:

```sh
reconciler-fuzzer replay --target targets/toy-widget/target.yaml --launch-arg --bug=3 targets/toy-widget/sequences/b3.json
```

```
run 1: seed 20260920, sequence targets/toy-widget/sequences/b3.json
run 1: G3 the v1/ConfigMap widget-0 was still there 10s (timeouts.delete) after widget, the last CR it may belong to, was deleted, orphaned: it carries no ownerReference to the CR
  at 2026-09-30T19:50:43.395112151Z; 1 version, the first v1/ConfigMap widget-0
  the evidence is in reconciler-fuzzer-out/20260930T195026Z-20260920/run-1
```

reconciler-fuzzer exits 1, and `report.md` in the evidence directory says what failed. All looks well
until the Widget is deleted and its ConfigMap stays. envtest has no garbage collector, so an
envtest suite misses this bug unless it checks each ownerReference. reconciler-fuzzer emulates the garbage
collector and checks every object your controller manages in every run.

## Your own controller

[docs/examples.md](docs/examples.md) runs reconciler-fuzzer on two real controllers, cert-manager and
external-secrets.

### With Claude Code

For a kubebuilder project, the [adopt-reconciler-fuzzer](skills/adopt-reconciler-fuzzer/SKILL.md)
skill writes `target.yaml` and runs reconciler-fuzzer until it tests your controller. After the
install, copy the skill into your repository:

```sh
mkdir -p .claude/skills
cp -r "$(go env GOMODCACHE)/github.com/rosenhouse-bot-sandbox/reconciler-fuzzer@$(reconciler-fuzzer version | cut -d' ' -f2)/skills/adopt-reconciler-fuzzer" .claude/skills/
chmod -R u+w .claude/skills/adopt-reconciler-fuzzer
```

Then ask Claude Code to set up reconciler-fuzzer, or run `/adopt-reconciler-fuzzer`.

### Write target.yaml

A target is one YAML file. This is the toy's:

<!-- embed: targets/toy-widget/target.yaml -->
```yaml
# reconciler-fuzzer reads crds, sample, fixtures and rbac relative to this file, and
# launch.binary relative to the directory it runs in.
name: toy-widget
version: dev                       # Reports print it.
crds:                              # reconciler-fuzzer installs the CRDs in these files and directories.
  - crds/
# reconciler-fuzzer creates, changes and deletes CRs of this kind.
primary: toy.reconciler-fuzzer/v1/Widget
sample: widget.yaml                # Each drawn sequence creates a variant of this CR first.
manages:                           # The controller creates objects of these kinds.
  - v1/ConfigMap
rbac:
  - rbac/role.yaml
launch:
  binary: bin/toy-widget
  args:
    - --kubeconfig=$KUBECONFIG     # reconciler-fuzzer writes a kubeconfig for its proxy.
    - --label-from=widget-config
    - --bug=0                      # A later --bug wins, so --launch-arg --bug=3 switches on B3.
  env:
    WATCH_NAMESPACE: $NAMESPACE    # reconciler-fuzzer replaces $NAMESPACE with each run's namespace.
fixtures:                          # The controller reads these objects and owns none.
  - config.yaml
ready: >-                          # This CEL holds once the controller has converged.
  has(status.observedGeneration) && status.observedGeneration == metadata.generation
  && has(status.ready) && status.ready == spec.count
properties:                        # These checks are your own.
  - id: P1
    description: status.ready never exceeds the number of ConfigMaps present.
    # A property also runs where no CR exists, so the CEL guards status.ready.
    cel: '!has(status.ready) || status.ready <= managed.filter(o, o.kind == "ConfigMap").size()'
timeouts:                          # The toy converges in milliseconds.
  settle: 5s
  stable: 2s
  delete: 10s
thresholds:
  # G6 fails a controller that repeats a failing request more than errloop
  # times. controller-runtime's backoff repeats one 10 times in a 5s settle.
  errloop: 5
```

Beyond what its comments say:

- `manages` names each kind as `group/version/Kind`, or `v1/Kind` for the core group. reconciler-fuzzer
  checks only objects of these kinds.
- reconciler-fuzzer replaces `$KUBECONFIG` and `$NAMESPACE` in `launch.args` and `launch.env`. Turn off
  leader election, and bind each listener to a free port, such as `127.0.0.1:0`.
- `ready` is CEL over the CR's `metadata`, `spec` and `status`. The default compares
  `status.observedGeneration` with `metadata.generation`.
- A property's `managed` holds the objects that the CR owns, and those that no CR owns.

For a CR whose `Ready` condition carries `observedGeneration`, use this `ready`:

```yaml
ready: >-
  has(status.conditions) && status.conditions.exists(c, c.type == "Ready"
  && c.status == "True" && has(c.observedGeneration)
  && c.observedGeneration == metadata.generation)
```

[docs/reference.md](docs/reference.md) lists every key with its default.
[docs/targets.md](docs/targets.md) says how to choose the values, such as `timeouts` for a slow
controller.

### Run it

reconciler-fuzzer resolves `launch.binary` from the directory it runs in, so run it from your repository
root:

```sh
reconciler-fuzzer run --target target.yaml --runs 5
```

reconciler-fuzzer prints each run's seed. `--seed` draws the same sequences again, as long as reconciler-fuzzer, your
CRDs and `target.yaml` are unchanged.

### The baseline

Drawn sequences may miss a managed kind, or never update a CR that has settled. So after the
drawn runs, reconciler-fuzzer runs the baseline. It creates your sample, and deletes the first
object of each managed kind, which your controller must recreate. It then changes each field
that generation may change, one at a time, and your controller must settle after each change.
`--no-baseline` leaves the baseline out.

To test what generation does not draw, write a sequence.
`reconciler-fuzzer run --target target.yaml sequences/*.json` runs your sequences.
[docs/targets.md](docs/targets.md#sequences-you-write) says how to write them.

## Reading a failure

reconciler-fuzzer exits 0 when every run passes, 1 when a check fails, and 2 when it could not test your
controller. On exit 2, the message says why, and
[docs/failures.md](docs/failures.md#when-reconciler-fuzzer-exits-2) lists the usual causes.

When a run fails, reconciler-fuzzer prints the check that failed:
[G1](docs/checks.md#g1-bounded-reconciliation) to [G7](docs/checks.md#g7-self-healing), or a
property's ID, such as P1. [docs/checks.md](docs/checks.md) says what each check requires, what
usually fails it, and what its message means. reconciler-fuzzer then minimizes a drawn
sequence, which can take minutes, and prints the run's evidence directory. Start with
`report.md` there. It says what failed, quotes what the check read, and gives the command that
replays it. [docs/failures.md](docs/failures.md) says what each file holds.

## Running in CI

In CI, `go install` reconciler-fuzzer at a commit of main, not `@latest`, and keep it
[out of your go.mod](docs/ci.md#keep-reconciler-fuzzer-out-of-your-gomod).

Run fixed seeds on a pull request, and fresh seeds nightly:

```sh
# On a pull request:
reconciler-fuzzer run --target target.yaml --seed 23 --runs 5 --deadline 10m --out reconciler-fuzzer-out
# Nightly:
reconciler-fuzzer run --target target.yaml --runs 20 --deadline 30m --out reconciler-fuzzer-out
```

[Size `--deadline`](docs/ci.md#deadlines) to your controller. Upload `reconciler-fuzzer-out/`,
so that you can replay a failure. Anyone who can read the repository can then read it, and
reconciler-fuzzer [redacts only](docs/failures.md#the-evidence) the values of a Secret's
`data` and annotations.

[docs/ci.md](docs/ci.md) holds a GitHub Actions workflow to copy. It also says how to pin
reconciler-fuzzer in a tools module, and how to run reconciler-fuzzer from `go test`.

## Development and internals

- `make setup` installs the envtest control plane, and `make help` lists every target.
- `make test`, `make test-envtest`, `make test-example` and `make test-example-external-secrets`
  are the tiers CI runs on every PR.
- `make hunt-cert-manager` and `make hunt-external-secrets` hunt for bugs in the pinned
  controllers for `HUNT_MINUTES` (default 120), over up to `HUNT_RUNS` (default 1000) seeds
  from `HUNT_SEED` (default 1000) on.
- [DESIGN.md](DESIGN.md) governs the code and docs. Its
  [§6](DESIGN.md#6-generic-invariants) states each check exactly.
- [docs/bug-matrix.md](docs/bug-matrix.md) shows which check catches each bug in the toy
  controller.

## License

Apache-2.0. See [LICENSE](LICENSE).
