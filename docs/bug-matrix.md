# Bug matrix

`make bug-matrix` generates this file from the gate_test.go of each toy in targets/.
`make test-envtest` proves each row: the plan, run against the bug, reports the check's finding at that step, or no finding for check none.

| Bug | Description | Plan | Check | Step |
|---|---|---|---|---|
| B0 | The correct controller. | all 48 committed plans | none | |
| B1 | Reports children ready before it creates them. | b1 | Converge | 0 |
| B2 | Creates children with generated names, so it duplicates them. | b2 | Converge | 0 |
| B3 | Leaves child 0 without an owner reference. | b3 | Clean | 1 |
| B4 | Reads the child count from the status, not the spec. | b4 | Converge | 1 |
| B5 | Never creates a child that an uncached read cannot find. | b5 | Converge | 0 |
| B6 | Writes a fresh lastSyncTime on every reconcile. | b6 | Converge | 0 |
| B7 | Never deletes surplus children on scale-down. | b7 | Converge | 1 |
| B8 | Does not watch its children, so a deleted one goes unnoticed. | b8 | Restore | 1 |
| B9 | Owns no child and drops the finalizer before deleting children. | b9 | Clean | 1 |
| B10 | Keeps in memory which Widgets it created children for, so a restart loses status updates. | b10 | Converge | 2 |
| B11 | Never retries a child create the API server refused. | b11 | Converge | 2 |
| B11 | Never retries a child create the API server refused. | b11-fault | Converge | 1 |
| B12 | Divides by a count of zero and crashes. | b12 | Crash | 1 |
| B12 | Divides by a count of zero and crashes. | b12-fault | Crash | 1 |
| B13 | Never cleans up a deleted Widget. | b13 | Converge | 1 |
| B14 | Does not watch the label source, so a changed label reaches no child. | b14 | Restore | 2 |
| B15 | Names children after the kind, so two Widgets share them. | b15 | Converge | 0 |
| B16 | Writes status with Update, which its RBAC does not allow. | b16 | Converge | 0 |
| B17 | Reports its Gadget ready without reading the Gadget's status. | gadget/b17 | Honest | 0 |
| B18 | Drops a Widget's Gadget events once it saw the Gadget not ready, so it never notices recovery. | gadget/b18 | Converge | 1 |
| B19 | Never reports its Gadget not ready once it was ready. | gadget/b19 | Honest | 1 |
| B20 | Creates no Gadget and reports it ready. | gadget/b20 | Honest | 0 |
| B21 | Updates its Gadget on every reconcile and requeues after 100 ms. | gadget/b21 | Converge | 0 |
| B22 | Orphans its Gadget on delete instead of deleting it. | gadget/b22 | Clean | 1 |
| B23 | Gives up on cleanup for good when its Gadget outlives the delete, so the finalizer stays. | gadget/b23 | Converge | 1 |
| B24 | Never recreates a missing Gadget once it reported it ready. | gadget/b24 | Restore | 1 |
| B25 | Retries a forbidden Gadget status patch every 10 ms while the Gadget has Ready False. | gadget/b25 | Backoff | 1 |
| B26 | Gives a Tenant's ClusterRole no owner reference, so it outlives the Tenant. | tenant/b26 | Clean | 1 |
| B27 | Forgets a Grant's ConfigMap in rf-shared when the finalizer cleans up. | grant/b27 | Clean | 1 |
| B28 | Leaves a Grant's ClusterRole to garbage collection, which ignores its namespaced owner. | grant/b28 | Clean | 1 |
| B29 | Never releases a Tenant's children while the Tenant is being deleted, so only a Foreground delete wedges. | tenant/b29 | Converge | 1 |
| B29 | Never releases a Tenant's children while the Tenant is being deleted, so only a Foreground delete wedges. | tenant/b29-background | none | |

## Bucket toy

Each Bucket owns one bucket in the `crudfake` system `cloud` (targets/toy-bucket).

| Bug | Description | Plan | Check | Step |
|---|---|---|---|---|
| B0 | The correct controller. | all 8 committed plans | none | |
| B0 | The correct controller. With --halt, it stops after a create of unknown outcome. | halted/halt | none | |
| B0 | The correct controller. With --halt and no primary.halted, it never settles. | halted/halt | Converge | 0 |
| B30 | Leaks the bucket on CR delete. | b30 | Clean | 1 |
| B31 | Retries a create of unknown outcome under a new random name and keeps both buckets. | b31 | Unique | 0 |
| B31 | Retries a create of unknown outcome under a new random name and keeps both buckets. | b31 | Clean | 2 |
| B31 | Retries a create of unknown outcome under a new random name and keeps both buckets. | b31 | Honest | 0 |
| B32 | Names its bucket with a per-process nonce, so a restart after the create makes a second one. | b32 | Unique | 0 |
| B32 | Names its bucket with a per-process nonce, so a restart after the create makes a second one. | b32 | Clean | 2 |
| B32 | Names its bucket with a per-process nonce, so a restart after the create makes a second one. | b32 | Honest | 0 |
| B33 | Reports ready right after the create, while the bucket is CREATING. | b33 | Honest | 0 |
| B34 | Never recreates a missing bucket. | b34 | Restore | 1 |
| B35 | Retries a 503 every 10 ms, past the rate limiter. | b35 | Backoff | 0 |

## Fleet toy

Each Cluster owns Volumes of a third party's kind, which a model sets ready. A Database names its Cluster and maybe a Secret, and a User its Database (targets/toy-fleet).

| Bug | Description | Plan | Check | Step |
|---|---|---|---|---|
| B0 | The correct controller. | all 23 plans of config.yaml, database/config.yaml, protect/config.yaml | none | |
| B0 | The correct controller. | uninstall/crd-first-stop | none | |
| B0 | The correct controller. | uninstall/kill-recreate | none | |
| B0 | The correct controller. | uninstall/recreate | none | |
| B0 | The correct controller. | uninstall/stop-after-delete | none | |
| B0 | The correct controller. | uninstall/stop-first | none | |
| B123 | Readies a Database on a Cluster that is not Ready. | create-together | Honest | 0 |
| B124 | Readies a Database without checking its Secret. | secret-late | Honest | 0 |
| B125 | Keeps a Ready Database ready after its Secret goes. | secret-deleted-while-down | Honest | 1 |
| B125 | Keeps a Ready Database ready after its Secret goes. | secret-recreate | Honest | 1 |
| B150 | Strips its finalizers from deleting CRs when it shuts down. | database/restart-cluster-delete | Converge | 1 |
| B151 | Puts a finalizer on each Volume, and releases a deleted Cluster before its Volumes, whose finalizers it strips only afterwards. | uninstall/kill-recreate | Converge | 1 |
| B151 | Puts a finalizer on each Volume, and releases a deleted Cluster before its Volumes, whose finalizers it strips only afterwards. | uninstall/stop-after-delete | Clean | 1 |
| B152 | Errors when a dependent kind is not served, as after its CRD is deleted, so the parent's deletion never ends. | uninstall/crd-first-stop | Converge | 2 |
