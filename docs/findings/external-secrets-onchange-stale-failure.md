# external-secrets v2.11.0 can leave an OnChange ExternalSecret Ready=False after its Secret is synced

This draft is for reconciler-fuzzer's maintainer to file at external-secrets. Triage did not
search external-secrets' tracker, so the maintainer searches it before filing. No agent may
file it. The issue body starts below the horizontal rule.

---

🤖 Created by Claude 🤖

## Summary

An ExternalSecret with `refreshPolicy: OnChange` has synced once. Its target Secret is then
deleted, and external-secrets creates it again. If that create commits but its response is
lost, external-secrets marks the ExternalSecret `Ready=False` with reason
`SecretSyncedError`. Every later reconcile skips the refresh, because the Secret now exists
and is valid. So the condition stays `Ready=False` until the ExternalSecret's spec changes,
although the Secret holds the right data. A restart of the controller does not clear it.

## Code path

Paths are relative to external-secrets v2.11.0,
`pkg/controllers/externalsecret/externalsecret_controller.go`.

- The create fails with a transport error, such as `EOF`. `markAsFailed` sets `Ready=False`
  and leaves `status.syncedResourceVersion` and `status.refreshTime` as they were
  (`:584-585`, `:802-812`).
- The retry reads the Secret, which the lost create made. Under `OnChange`, `shouldRefresh`
  is false while `syncedResourceVersion` matches the generation (`:1256-1261`).
  `isSecretValid` is true, because the Secret carries the managed label and the data hash
  (`:1304-1325`).
- So the reconcile logs `skipping refresh` and returns before it writes any status
  (`:372-375`). Nothing else sets `Ready=True` again. `getRequeueResult` requeues after
  `refreshInterval` (`:744-775`), and that reconcile skips the refresh the same way.

Under `refreshPolicy: Periodic`, the next refresh syncs and sets `Ready=True`.

## Reproduction

reconciler-fuzzer runs external-secrets, built with `-tags vault`, against envtest and a Vault
dev server. [onchange-stale-failure.json](../../examples/external-secrets/bugs/onchange-stale-failure.json)
creates an ExternalSecret and waits until it is ready. Its next step deletes the Secret and
drops the response to external-secrets' first Secret create. From a reconciler-fuzzer checkout:

```sh
make repro-external-secrets REPRO_RUNS=3
```

Each run fails Converge: `externalsecrets.external-secrets.io <namespace>/example is not
ready`, and stays so for the step's 34 s deadline. The plan kept the ExternalSecret
`Ready=False` in 3 of 3 runs. Generated plans for seeds 24 and 921, under the fake provider's
config, did so in 3 of 3 runs each. One of them restarted the controller meanwhile. The same
plan under `Periodic` with a 2 s `refreshInterval` passed.

## Possible fix

The skip path at `:372` could mark the ExternalSecret synced when its Ready condition is
False, since the Secret it found is valid. No run tested this.

## Unknown

- No run used the released image or a real cluster.
- A lost create response needs a network fault or an API server timeout after commit. No run
  measured how often that happens.
