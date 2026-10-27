# Reading a failure

`run` prints `report.md` and exits 1 when it has findings. This page says
what each finding means and what to look at first. DESIGN.md §10 and §12
define the clauses and checks.

## The output directory

`run --out <out>` writes:

| File | What it holds |
| --- | --- |
| `plan.json` | The plan that ran. With `--seed`, it is the drawn plan. |
| `trace.jsonl` | Every request, action, phase and controller start and exit (DESIGN.md §4). |
| `report.md` | The findings and notes. |
| `controller.log` | The controller's output. |
| `harness.log` | client-go's errors from the harness's watches, such as a failed watch of a CRD the plan deleted. |
| `kubeconfig` | The envtest API server's kubeconfig. The server is gone after the run. |

`hunt --out <out>` writes one `seed-<n>` directory per seed that found
something new, and `summary.md`. Each seed directory holds a run's files,
`replay-<i>` runs, and `shrunk.json`, or `shrunk-<f>.json` for finding f
past the first. `shrink-<f>-<c>-<r>` is run r of candidate c in the shrink
of finding f, and stays only when it gave the finding. `summary.md` has one
row per distinct finding: seed, check, summary, replays that reproduced it,
and the shrunk plan. `hunt` rewrites it as it goes, and a cell says
`pending` until its replays or shrink end. It also lists seeds whose run
failed.

## report.md

Each finding is one section:

````
## Restore: did not restore damaged resources

Step 1, at 16.907430335s.

```
configmaps rf-da5dd4dd/widget-0 (rule 1, slot a)
```
````

The heading is the check and its summary. The first line names the step,
counting from 0, and the time since the run started. `Before step 0` means
during startup. The runner appends two steps to every plan without a
`stop`: `Step N (delete all)` and `Step N+1 (restart)`.
A `Labels:` line may follow. The block lists the detail lines.

A `# Notes` section follows the findings. A note says what the trace could
not decide: an action that failed, a fault that picked no request, a
step the trace ended too early to judge, a CR still being deleted that a
`stop` did not wait for, or an unmapped request. Notes do
not fail a run. A fault that picked no request tested nothing, so a plan
with such notes may need a different fault.

`the slot's name is still taken after the create waited 37s` means that
a create gave up waiting until its slot's previous CR no longer existed.
The step's faults had ended by then (DESIGN.md §9). When the observer last
recorded a live CR with that name, the rest of the note names its
`deletionTimestamp` and finalizers, or says that it is not being deleted.
The run then skips to the `delete all` and `restart` steps that the runner
appends.

`cloud: unmapped request POST /v2/buckets/b-1/acl` means the system's API
map could not place a controller request on a resource. No check judges
that request, so a leak or duplicate it causes goes unseen. The `crud` map
places only `/<collection>` and `/<collection>/<id>` (DESIGN.md §5). Serve
the API at those paths, or put it behind another fake service.

Hunt and shrink match findings by check, summary and kind, so a replay
need not fail at the same step or on the same names.

## Checks

### Converge: did not settle (clause k)

The settle condition did not hold by the step's deadline. `k` is the first
unmet clause of DESIGN.md §10. The details list every unmet clause at the
deadline. The toy with `--bug=2` gives:

````
## Converge: did not settle (clause 1)

Step 0, at 11.953863534s.

```
widgets.toy.reconciler-fuzzer rf-6d6981a4/widget is not ready
model gc has work pending
the controller made 226 changed writes in (9.953863534s, 11.953863534s]; first it changed configmaps rf-6d6981a4/widget-0-5dt88 with create at 9.9584852s
the controller made 238 writes in (9.953863534s, 11.953863534s], over the budget of 4
```
````

Look at the clause first:

| Clause | Meaning | Look at |
| --- | --- | --- |
| 1 | A CR is not `ready`. | The CR's last status in `trace.jsonl`, and `ready` in the config. |
| 2 | A CR is still being deleted. | Its finalizers, and why the controller did not remove them. |
| 3 | The controller is down, or restarted within `stable`. | `controller.log`, and any Crash finding. |
| 4 | The controller made no successful request since it started. | Its startup in `controller.log`, and its RBAC. |
| 5 | The controller did not look at an object a `damage` or `changeFixture` changed. | Its watches and the kinds it reads. |
| 6 | A model, such as `gc`, has work pending. | What the model waits on. Churn under clause 7 often keeps it busy. A stage model's `finalizer:` holds each deletion for `finalizer.after`, so lower it, or raise `timeouts.settle`. |
| 7 | The controller kept changing objects, or went over the `budgets`. | The first changed write the detail names: it shows what the controller fights over. |

`did not stay settled (clause k)` means the condition held once, then
failed again within `stable`.

After a `stop`, only clauses 2, 6 and 7 apply, and clause 2 only in the
stop's step. It names a CR that was still being deleted when the stop's wait
for it gave up.

The `backoff-bound` label means `timeouts.backoffCap` set the deadline,
because the step's faults lasted longer. A controller whose retry backoff
exceeds the cap fails there. Raise the cap if that backoff is intended.

### Crash: the controller exited

The controller exited without the harness stopping it. The summary counts
the exits in the step. The details hold the first exit status and the tail
of its output, often a panic.

A Crash before step 0 whose output is a flag or usage error means the
controller never started. Fix `launch.args` or `launch.env`. Converge
clause 4 findings follow from it.

### Clean: left resources behind

A controller resource outlived its owning CR. Each line names the
resource, the owner rule of DESIGN.md §11 that tied it to a CR (1:
ownerReferences; 2: `systems[].owner`; 3: an entry's `owns`; 4: the CRs
live at its creation that the controller did not create), and that CR's
slot. A CR that the controller created has no slot, so the line names its
kind and name instead:

```
configmaps rf-9485af8b/widget-0 (rule 4, slot a)
buckets buckets-1 (rule 2, slot a)
configmaps rf-9485af8b/db-0 (rule 1, CR databases.example.com rf-9485af8b/db)
```

A rule 4 line usually means the resource lacks an ownerReference, so
garbage collection could not delete it. Its ownerReferences may also lead
to no CR: they reach only objects that the trace never saw, gone objects,
or objects that the controller made. While they name a live object that is
not a CR and that the controller did not create, such as a fixture,
directly or through live objects that the controller made, Clean skips the
resource. A rule 1 line on a cluster-scoped resource with a namespaced
owner means garbage collection ignores that invalid ownerReference. A rule
2 or 3 line usually means a finalizer forgot the resource. A resource that
should outlive its CR needs its entry's `retain`.

After a `stop`, a line names a resource whose CRs were deleted before the
stop, so an uninstall leaves it behind. A common cause is a controller
that removes a child's finalizer only after the CR is gone.

### Restore

The step changed no CR or fixture, but the controller's resources differ
from the previous step's end.

- `did not restore damaged resources`: a damaged resource was not
  recreated, or every resource of some kind under the damaged resource's
  root (DESIGN.md §11) is gone. Once a damaged resource's owner is gone, a
  CR that the controller made in the step must own a resource of the
  damaged one's kind and role under that root.
- `replaced resources`: an undamaged resource was replaced, or a new one
  appeared.
- `changed resources`: an undamaged resource's body changed. Each line
  names the fields that differ.

Each line names its resource's owner as Clean does. `rule 1, unlisted
owner` names the live object, such as a fixture, that owns the resource
instead of a CR.

### Unique: duplicate resources

Two live controller resources share a kind, namespace, owning CR or
unlisted owner, and role (DESIGN.md §11).

### Honest

A CR reported ready while a dependency was not, as the controller's own
reads showed it. A dependency stops counting once the controller sends a
successful `delete` of it, and while the controller last saw it deleted.
A `deletecollection` or an owner's cascade is not such a `delete`. Each
line names the CR, by its slot or else by its kind and name, and the
dependency:

- `reported ready before seeing its dependency ready`: `not seen` means
  the controller never read the dependency after its last write to it;
  `seen not ready` means its last read showed it not ready.
- `reported ready with no dependency`: under `dependency.required`, the
  CR owned none that counts.
- `stayed ready after seeing its dependency not ready`: the CR stayed
  ready longer than `track`.

### Backoff: retried a failing request in a tight loop

The controller repeated a failing request with gaps under 100 ms. The line
names the verb, the object, the status and the calls. Retry through a rate
limiter, such as controller-runtime's default, rather than a short
`RequeueAfter`.

### Finalizers: removed another caller's finalizer

A controller update or patch dropped a finalizer that a model added. Each
line names the object, the finalizer, the model and the removing write. A
stage model adds its `finalizer:` and, during `slowDelete`,
`reconciler-fuzzer.dev/slow-delete`. The API server adds
`foregroundDeletion` or `orphan` for a delete with that propagation policy,
and `gc` owns them. Remove only your own finalizer, as
`controllerutil.RemoveFinalizer` does. A `client.MergeFrom` patch replaces
the whole list, so it drops a finalizer added after your read. Write with
`Update`, or patch with
`client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})`.

## When a run exits 2

Exit 2 means a usage or harness error: the run judged nothing. The error
names its cause. A run that started leaves its partial trace in `--out`. Common causes:

| Error | Fix |
| --- | --- |
| `starting envtest: ... etcd: no such file or directory` | Set `KUBEBUILDER_ASSETS` to `$(make -s assets-path)`. |
| `starting <binary>: ... no such file or directory` | Build the controller to `launch.binary`. |
| `creating fixture ...` | Fix the fixture, or install the CRD it needs under `crds`. |
| `launch args: ...` or `launch env <name>: ...` | Use only `${systems.kube.kubeconfig}`, `namespace` and `url`, and a fake's `${systems.<name>.url}`. |
| `fake <name> exited ...` | Read `<out>/<name>.log`. Fix the `systems[].process` command or the fake's own config. |
| `create of slot "<s>" makes a <kind>, which primaries does not list` | List the kind under `primaries`, or make the object a fixture. |
| `create of slot "<s>" makes an object without apiVersion or kind` | Give the create's `object` both. |
| `... holds a <kind>, not a <kind>` | Point the entry's `sample` at an object of its `kind`. |

`hunt` notes a seed whose run failed in `summary.md`, and goes on. It
exits 2 when a seed failed and nothing was found, and 1 on any find.

## Next steps

Replay a failure:

```
reconciler-fuzzer run --config config.yaml --plan <out>/plan.json --out replay
```

Judge a trace again after changing `ready`, `retain`, `stable` or the
budgets, without rerunning:

```
reconciler-fuzzer judge --trace <out> --config config.yaml
```

Shrink a failing plan to a smaller plan that gives the same finding:

```
reconciler-fuzzer shrink --config config.yaml --plan <out>/plan.json --out shrink
```

`shrink` writes `shrunk.json` and `summary.md` under `--out`.
