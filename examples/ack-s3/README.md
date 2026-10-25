# ACK s3-controller

This example runs the ACK s3-controller, unpatched, against moto's S3.
This project's build sandbox cannot download MinIO, and ACK needs no more
than moto. The Makefile pins the controller's module, its commit and its
go.sum hash. It builds `cmd/controller` with `go install`, which checks
the module against the sumdb. `crds/` holds the module's Bucket CRD and
the common services.k8s.aws CRDs, and `crds/SHA256SUMS` pins them.
`rbac/` is the module's `config/rbac/cluster-role-controller.yaml`.

```sh
make test-example-ack-s3   # plans, seeds, and the negative control
make hunt-ack-s3           # HUNT_SEEDS fresh seeds
```

`controls/retain.json` sets the `services.k8s.aws/deletion-policy: retain`
annotation and deletes the Bucket. ACK keeps the bucket, and Clean must
flag it.

## Tuning

- `--reconcile-default-resync-seconds=3`: the default is 10 hours, so ACK
  would never see damage. `poll: 3s` matches it.
- `budgets.reads: 60`: one sync reads ListBuckets and 24 bucket
  subresources, and a write requeues one more.
- `primary.periodic`: each sync rewrites the conditions' stamps (bug 2).
  It names only `status.conditions[*].lastTransitionTime`, so a condition
  that flips still fails clause 7.
- `ready`: ACK records no observed generation. A sync stamps its
  conditions' `lastTransitionTime`, so `ready` wants that stamp two seconds
  after every spec write in `managedFields`. One second is not enough: a
  sync that read the spec just before a write can stamp the next second.
  So each update waits for the next resync, up to 3 s.
- `ready` depends on bug 2. If ACK moved the stamp only on a transition,
  no Bucket would be ready after an update, and every plan would fail.
- `moto_server.py` gives each bucket S3's default encryption (bug 1).
- The overlay keeps `tagSet` and `versioning.status` set (harness issue 1),
  and draws one tag, since S3 rejects two tags with one key.

## Real ACK bugs

**1. A bucket without encryption panics every sync.** This defect is
latent: only an emulator triggers it. `customFindBucket`
reads `getEncryptionResponse.ServerSideEncryptionConfiguration.Rules` after
a `ServerSideEncryptionConfigurationNotFoundError`, when the response is
nil (`pkg/resource/bucket/hook.go:487`). AWS has given every bucket
default encryption since January 2023, so AWS never answers that error.
moto and other S3 servers do. With plain `moto_server`, `plans/create.json`
fails Converge clause 1 on every step, and `controller.log` shows:

```
"msg":"Observed a panic","panic":"runtime error: invalid memory address or nil pointer dereference"
github.com/aws-controllers-k8s/s3-controller/pkg/resource/bucket.(*resourceManager).addPutFieldsToSpec
	.../s3-controller@v1.12.2/pkg/resource/bucket/hook.go:487
```

**2. Each sync rewrites every condition's `lastTransitionTime`.** No
condition changes status, yet each resync patches the status with a new
time on all three conditions. Kubernetes API conventions move it only on a
transition.

**3. A Bucket without `spec.encryption` deletes the bucket's encryption
every sync.** ACK reads AWS's default encryption, compares it with the
empty spec, and calls `DeleteBucketEncryption`, which restores the default.
The sample sets the default, `AES256`, so the plans do not loop.

**4. A failed STS call at startup exits the controller.** `main.go:94`
exits when `GetCallerIdentity` fails, so one faulted request is a crash.
Kubernetes would restart it, so this is minor. `--seed 4` fails Crash on
it, and the seeds file skips that seed.

## Harness issues

Three are fixed:

- Clause 5 now holds `stable` after a polling controller's look (DESIGN.md
  §17, entry 72).
- The s3 map places only STS's `GetCallerIdentity` at `POST /` outside any
  resource.
- The s3 map gives a bucket's subresource writes no resourceVersion. Clause
  7 used to count each as a changed write, even one that changed nothing.
  This was once filed here as bug 4: after `versioning.status` is cleared,
  ACK sends `PUT ?versioning` and `DELETE ?replication` every sync. Clause
  7 now asks the observer whether a write changed the bucket (entry 74),
  and the plan passes: those writes change nothing. moto accepts the empty
  `VersioningConfiguration`; how AWS answers it was not checked.

One remains. With no observed generation, `ready` cannot see an update
that only removes fields, because the API server leaves that
`managedFields` entry's time alone. The step can then settle on the stale
body, and ACK's reaction fails "did not stay settled".
