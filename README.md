# reconciler-fuzzer

reconciler-fuzzer finds bugs in Kubernetes controllers. It runs your controller on your machine
against envtest's kube-apiserver and etcd. It follows a plan of steps: CR changes, faults on the
controller's API requests, restarts, and damage to objects the controller created. It records
every request and object version in a trace. Then it judges the trace with five checks: Converge,
Crash, Clean, Restore and Unique ([DESIGN.md §12](DESIGN.md#12-checks)).

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

From a checkout, the Makefile installs the same control plane under `bin/`:

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
    KUBECONFIG: ${systems.kube.kubeconfig}   # the harness writes this file
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
- `args` turns off the scaffold's fixed ports, so runs cannot clash on them. If it has
  webhooks, set `ENABLE_WEBHOOKS: "false"` in `env`.
- `ready` is CEL over the CR. It must be true only once the controller has acted on the
  current spec, so compare an `observedGeneration` with `metadata.generation`.
- `stable` is how long the controller must stay quiet before a step counts as settled.
- `launch.rbac` also tells generation which requests a fault can pick.
- `values` steers generated CRs: `overlay` narrows the CRD schema, `distinct` fields differ
  across a plan's CRs, `mutate` names the fields updates change, and `fixtures` names the
  fixture strings `changeFixture` sets.

[examples/cert-manager](examples/cert-manager/config.yaml) and
[examples/external-secrets](examples/external-secrets/config.yaml) are real configs.

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
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v5
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
`make help` lists them. [docs/bug-matrix.md](docs/bug-matrix.md) lists the toy's bugs and the
check that catches each.

## Terms

[DESIGN.md §1](DESIGN.md#1-terms) defines the design's terms, such as plan, step and slot.
The README also uses these:

| Term | Meaning |
| --- | --- |
| Target | The controller under test, with its config. |
| CR | A custom resource. The config's `primary.kind` names the CRs a plan changes. |
| Fixture | An object the harness creates before step 0, such as a ConfigMap the controller reads. |
| envtest | A kube-apiserver and etcd with no other controllers, from controller-runtime. |
| Seed | A number that fixes a generated plan, not its timing. |
| Sample | The CR in `primary.sample`. Generated CRs start from it. |
| `ready` | CEL over a CR that is true once the controller has acted on its spec. |
| `stable` | How long the controller must stay quiet for a step to settle. |
