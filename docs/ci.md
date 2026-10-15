# Running reconciler-fuzzer in CI

This page holds a workflow to copy, and two other ways to run a pinned reconciler-fuzzer.

## A GitHub Actions workflow

Copy this workflow into `.github/workflows/`, and replace its build and `target.yaml` with your
own. Set `RECONCILER_FUZZER_VERSION` to a commit of main. reconciler-fuzzer's own steps need no go.mod, no cluster
and no registry.

<!-- embed: examples/ci/github-actions.yml -->
```yaml
name: reconciler-fuzzer

on:
  pull_request:
  schedule:
    - cron: '17 6 * * *'

env:
  RECONCILER_FUZZER_VERSION: <commit>   # Set this to a commit of main.
  SETUP_ENVTEST_VERSION: v0.25.2
  ENVTEST_K8S_VERSION: 1.37.0
  ENVTEST_INDEX_URL: https://raw.githubusercontent.com/kubernetes-sigs/controller-tools/v0.22.0/envtest-releases.yaml

permissions:
  contents: read

jobs:
  reconciler-fuzzer:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
          cache: false   # true needs a go.sum
      - id: tools
        uses: actions/cache/restore@v4
        with:
          path: |
            ~/go/bin
            bin/envtest
          key: ${{ runner.os }}-${{ runner.arch }}-reconciler-fuzzer-${{ env.RECONCILER_FUZZER_VERSION }}-${{ env.SETUP_ENVTEST_VERSION }}-${{ env.ENVTEST_K8S_VERSION }}
      - if: steps.tools.outputs.cache-hit != 'true'
        run: |
          go install "github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@$RECONCILER_FUZZER_VERSION"
          go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@$SETUP_ENVTEST_VERSION"
      - run: |
          assets=$(setup-envtest use "$ENVTEST_K8S_VERSION" --index "$ENVTEST_INDEX_URL" --bin-dir bin/envtest -p path)
          echo "KUBEBUILDER_ASSETS=$assets" >>"$GITHUB_ENV"
      # Saving before reconciler-fuzzer runs fills the cache even when it fails.
      - if: steps.tools.outputs.cache-hit != 'true'
        uses: actions/cache/save@v4
        with:
          path: |
            ~/go/bin
            bin/envtest
          key: ${{ steps.tools.outputs.cache-primary-key }}
      - run: go build -o bin/controller ./cmd/controller   # Build what launch.binary names.
      # exec hands a cancelled job's signal to reconciler-fuzzer. bash would not pass it on.
      - if: github.event_name == 'pull_request'
        run: exec reconciler-fuzzer run --target target.yaml --seed 23 --runs 5 --deadline 10m --out reconciler-fuzzer-out
      - if: github.event_name == 'schedule'
        run: exec reconciler-fuzzer run --target target.yaml --runs 20 --deadline 30m --out reconciler-fuzzer-out
      - if: always()
        run: cat reconciler-fuzzer-out/*/summary.md >>"$GITHUB_STEP_SUMMARY" || true
      # gh run download <run-id> --name reconciler-fuzzer-out --dir reconciler-fuzzer-out
      # restores the paths that the replay command in report.md names.
      - if: always()
        uses: actions/upload-artifact@v4
        with:
          name: reconciler-fuzzer-out
          path: reconciler-fuzzer-out/
```

The job caches reconciler-fuzzer and setup-envtest in `~/go/bin`, and the control plane in `bin/envtest`,
under a key of their versions. If your repository has a go.sum, set `go-version-file: go.mod`
and `cache: true` instead.

Add a step that runs your pinned sequences, such as
`exec reconciler-fuzzer run --target target.yaml --deadline 10m --out reconciler-fuzzer-out sequences/*.json`.

## Seeds

A pull request runs fixed seeds. Its runs repeat until you upgrade reconciler-fuzzer or edit your CRDs or
`target.yaml`. To tell whether a failure comes from the
change under review, replay its `sequence.json` against the base branch's controller.

The nightly run draws fresh seeds. When a scheduled run fails, GitHub notifies only the person who
last edited the schedule. reconciler-fuzzer's own [nightly.yml](../.github/workflows/nightly.yml) reads
`summary.json` to tell a failed check (exit 1) from an error (exit 2 or no output), and files an
issue under the matching label.

## Deadlines

`--deadline` caps each reconciler-fuzzer invocation, and reconciler-fuzzer stops within seconds of it. If the
deadline ends a run, or stops reconciler-fuzzer before its last run, reconciler-fuzzer exits 2. If it cuts
minimizing short, reconciler-fuzzer still exits 1 and reports the smallest failing sequence it found.

Without `--deadline`, reconciler-fuzzer assumes every wait runs to its timeout, and leaves at
least 4m to minimize a failure. A fast controller finishes far sooner.
To size a deadline, time the pull request's command once and add at least 4m. Or shorten
`timeouts`, as the toy does.

## Evidence

The job uploads `reconciler-fuzzer-out/`, which holds each run's sequence and a failing run's evidence.
Download it with the command in the workflow's last comment,
and build your controller. The replay command in `report.md` then runs as written from the
repository root.

On GitLab or Jenkins, `--junit reconciler-fuzzer.xml` writes the runs as JUnit XML for
`artifacts:reports:junit` or the `junit` step.

## Keep reconciler-fuzzer out of your go.mod

`go install` leaves your module alone. Requiring reconciler-fuzzer in your go.mod, with `go get -tool` or
by importing one of its packages, raises your module to reconciler-fuzzer's versions:

```
go: upgraded go 1.23.0 => 1.26.0
go: upgraded k8s.io/api v0.32.0 => v0.37.0
go: upgraded sigs.k8s.io/controller-runtime v0.20.0 => v0.25.1
```

To pin reconciler-fuzzer in your repository, give it a module of its own. Run this from your repository
root:

<!-- embed: examples/tools-module.sh -->
```sh
mkdir -p tools/reconciler-fuzzer
cd tools/reconciler-fuzzer
go mod init example.com/operator/tools/reconciler-fuzzer
go mod edit -go=1.26.0
GOWORK=off go get -tool github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer@latest
cd ../..
GOWORK=off go -C tools/reconciler-fuzzer build -o ../../bin/reconciler-fuzzer github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/cmd/reconciler-fuzzer
bin/reconciler-fuzzer version
```

`tools/reconciler-fuzzer/go.mod` then pins reconciler-fuzzer. Your own go.mod and go.work, and any package of yours
in `tools/`, stay as they were.

Keep `go mod edit -go` first, so that a `go` before 1.24, which lacks `-tool`, first switches
to a newer Go. Keep `GOWORK=off`, or
`go get` raises the go line of your go.work. Under
`GOTOOLCHAIN=local`, a `go` before 1.24 stops this recipe with
`flag provided but not defined: -tool`.

Run `bin/reconciler-fuzzer` as the last line does, not `go -C tools/reconciler-fuzzer tool reconciler-fuzzer`, which runs reconciler-fuzzer
in `tools/reconciler-fuzzer/`, where your `launch.binary` does not resolve.

To use it in the workflow:

- After checkout, add a step that runs the recipe's `go -C tools/reconciler-fuzzer build` line.
- Install only setup-envtest in the cached step.
- Drop `RECONCILER_FUZZER_VERSION` from the workflow and its cache key, and run `bin/reconciler-fuzzer`.
- Set setup-go's `cache: true` and `cache-dependency-path: tools/reconciler-fuzzer/go.sum` to keep the
  build's downloads.

## From go test

A Go test can run reconciler-fuzzer and fail on what it finds. This one runs it on the toy controller:

<!-- embed: targets/toy-widget/reconciler_fuzzer_test.go -->
```go
//go:build reconciler_fuzzer

package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestReconcilerFuzzer(t *testing.T) {
	fuzzer := exec.Command("bin/reconciler-fuzzer", "run", "--target", "targets/toy-widget/target.yaml",
		"--seed", "1", "--runs", "3")
	if deadline, ok := t.Deadline(); ok {
		// Stop reconciler-fuzzer before go test's -timeout, which would leave its
		// control plane running.
		left := time.Until(deadline).Truncate(time.Second) - 30*time.Second
		if left <= 0 {
			t.Fatal("go test's -timeout leaves reconciler-fuzzer no time")
		}
		fuzzer.Args = append(fuzzer.Args, "--deadline", left.String())
	}
	fuzzer.Dir = "../.." // launch.binary is relative to the repository root.
	fuzzer.Stdout, fuzzer.Stderr = os.Stdout, os.Stderr
	if err := fuzzer.Run(); err != nil {
		t.Fatalf("reconciler-fuzzer: %v", err)
	}
}
```

Build `bin/reconciler-fuzzer` with the [tools module](#keep-reconciler-fuzzer-out-of-your-gomod), build your
controller and set `KUBEBUILDER_ASSETS`. Then run `go test -count=1 -tags reconciler_fuzzer ./...`.
`go test` cannot see a change to your controller or `target.yaml`, so `-count=1` stops it from
reusing a cached pass. The build tag keeps the test out of a plain `go test ./...`. The test
stops reconciler-fuzzer before `go test`'s `-timeout`, so raise `-timeout` for more runs.

reconciler-fuzzer has no Go API to call instead.
