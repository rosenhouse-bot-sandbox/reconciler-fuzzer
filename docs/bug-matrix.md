# Bug matrix

This file is generated from targets/toy-widget/gate_test.go by `make bug-matrix`.
`make test-envtest` proves each row: the plan, run against the bug, reports the check's finding at that step.

| Bug | Description | Plan | Check | Step |
|---|---|---|---|---|
| B0 | The correct controller. | all 23 in plans/ | none | |
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
