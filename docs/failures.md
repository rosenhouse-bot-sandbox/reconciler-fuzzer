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
something new or whose run failed, and `summary.md`. Each seed directory
holds a run's files, `replay-<i>` runs, and `shrunk.json`, or
`shrunk-<f>.json` for finding f past the first. A shrink that keeps no
candidate saves the plan unchanged. `shrink-<f>-<c>-<r>` is run r of
candidate c in the shrink of finding f, and stays only when it gave the
finding. `summary.md` has one row per distinct finding: seed, check,
summary, replays that reproduced it, and the shrunk plan. It also lists
seeds whose run failed. `hunt` rewrites it while it runs, and a cell says
`pending` until its replays or shrink end. When an interrupt cuts a
finding's replays or shrink short, or a harness error cuts its shrink
short, its row says `(cut short)`. The row names the smallest failing plan
so far, if the hunt saved one. Run `shrink` on that plan, or on the seed's
`plan.json`, to finish.

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

A `# Notes` section follows the findings. A note says what the checks
could not judge, or names a config mistake: an action that failed, a fault
that picked no request, a step the trace ended too early to judge, a CR
still being deleted that a `stop` did not wait for, an unmapped request, or
a `role` that failed on every resource of a kind. A note also names each
time the controller outlived `launch.gracePeriod`. Notes do not fail a run.
A fault that picked no request tested nothing, so a plan with such notes
may need a different fault. A step without a `period` ends its faults by
the time its last action returns. Such a fault's note says how long the
fault lasted, unless the fault ended while the controller was down, or it
is a `holdWatch` of a kind that the controller does not watch later in
that step.

`the slot's name is still taken after the create waited 37s` means that
a create gave up waiting until its slot's previous CR no longer existed.
`the name of secrets ns/pw is still taken after the createFixture waited
37s` says the same of a `createFixture` and the fixture it makes again.
The step's faults had ended by then (DESIGN.md §9). When the observer last
recorded a live object with that name, the rest of the note names its
`deletionTimestamp` and finalizers, or says that it is not being deleted.
The run then skips to the `delete all` and `restart` steps that the runner
appends.

`Step 1, action 0 (changeFixture) failed ...: no instance matches
resource=secrets name=token: the harness's live secrets are rf-da5dd4dd/pw`
means that the action's target matched no live object. The note names the
target. It then says what the action's slot holds, or names up to five
live objects of the target's kind, in the slot if one is set. A `damage`
matches only objects the controller created, and a `changeFixture` only
objects the harness created, such as fixtures. Neither matches a CRD that
`crds:` or a restart installed. The note of a `changeFixture` that targets
CRDs says so.

`cloud: unmapped request POST /v2/buckets/b-1/acl` means the system's API
map could not place a controller request on a resource. No check judges
that request, so a leak or duplicate it causes goes unseen. The `crud` map
places only `/<collection>` and `/<collection>/<id>` (DESIGN.md §5). Serve
the API at those paths, or put it behind another fake service.

`kube/storage.thirdparty/volumes: the role failed on every resource of this
kind the controller created: evaluating "self.metadata.labels['x']": no such
key: x` names the first error of the system's `role`. The checks then group
those resources as if the system had no `role`. One note per kind catches a
`role` that a kind guard applies to that kind alone. Fix the CEL.

`Step 1, at 8.6s: the controller did not exit within launch.gracePeriod of
SIGTERM, so the harness sent SIGKILL.` means that a stop of the controller
for a `restart` or a `stop` waited `launch.gracePeriod` after SIGTERM and
then killed it. Such a stop tested a crash, not a clean shutdown. A note
that begins `After the last step` names the stop at the run's end. Raise
`launch.gracePeriod` if the controller needs longer to shut down.

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
| 5 | The controller did not look at an object a `damage`, `changeFixture` or `createFixture` changed. | Its watches and the kinds it reads. |
| 6 | A model, such as `gc`, has work pending. | What the model waits on. Churn under clause 7 often keeps it busy. A stage model's `finalizer:` holds each deletion for `finalizer.after`, so lower it, or raise `timeouts.settle`. |
| 7 | The controller kept changing objects, or went over the `budgets`. | The first changed write the detail names: it shows what the controller fights over. |

`did not stay settled (clause k)` means the condition held once, then
failed again within `stable`.

Clauses 1 and 2 skip a CR that waits in deletion while a live CR names it
under `onDelete: block`, from `stable` after its deletion started. After
faults, the skip also waits until `stable`, and the time the step's faults
lasted, at most `timeouts.backoffCap`, have passed since they ended. Until
then, clause 2's line says `waits in deletion, which counts as settled only
from <t>`, or `only once the step's faults end` while they go on. If the
controller holds a named CR's deletion by design, and clause 2 names that
CR, set the reference's `onDelete: block`.

CRs being deleted that name each other, directly or through other CRs
being deleted, would wait for good, so those names do not count. A CR
that names itself is such a cycle. If clause 2 names CRs on a cycle, the
controller should release one of them. Ordered passes that release.

Clause 1 skips a CR that a reference holds back, if the named object as it
is now would hold the CR back too, or that a reference leaves unknown. It
does so once `stable`, and the time the step's faults lasted, at most
`timeouts.backoffCap`, have passed since the faults ended.

Clause 1's line says what the controller last saw of each object that the
CR names, such as `; the controller last saw secrets ns/admin-password
ready at 7.24s`. If it saw the object ready, the reference's `ready`
accepts an object that the controller treats as not ready. If it saw the
object missing under `required: false`, the controller treats the object
as required. If it saw the object missing or not ready, and the object is
ready now, the controller missed the change, as when it does not watch the
named kind. `never saw` means the controller never read the object.

A finding at a last step that deletes a `values.deletable` fixture may come
from that delete, which the line does not name. Run the plan a few times
without that step's restarts and kills. If every run passes, a restart or
kill caused the finding, and the controller has a bug. If a run fails, the
delete alone left the CR not ready. Drop the file from `values.deletable` if
the controller does that by design. Otherwise the controller has a bug.

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
from the previous step's end. A restart changes no CR, even one that
upgrades the controller. See [README Upgrading](../README.md#upgrading) for
the findings an upgrade can cause.

- `did not restore damaged resources`: a damaged resource was not
  recreated, or every resource of some kind under the damaged resource's
  root (DESIGN.md §11) is gone. Once a damaged resource's owner is gone, a
  CR that the controller made in the step must own a resource of the
  damaged one's kind and role under that root. A resource recreated under
  another name counts when its system's `role` gives it the damaged one's
  role. A resource may stay gone while each of its CRs is not ready at the
  step's end, but halted or held back by a reference.
- `replaced resources`: an undamaged resource disappeared or changed owner,
  or a new one appeared.
- `changed resources`: an undamaged resource's body changed in a step
  whose restart did not upgrade the controller. Each line names the fields
  that differ.

Each line names its resource's owner as Clean does. `rule 1, unlisted
owner` names the live object, such as a fixture, that owns the resource
instead of a CR.

### Unique: duplicate resources

Two live controller resources share a kind, namespace, owning CR or
unlisted owner, and role (DESIGN.md §11). A kube resource's role is its
system's `role`, or else its `generateName` or name. A fake's resource
without a `role` has an empty role. Each line starts with the role:

```
role "cluster-vol-0": volumes.storage.thirdparty rf-ba88ef99/cluster-vol-0-bhltd (rule 1, slot a), volumes.storage.thirdparty rf-ba88ef99/cluster-vol-0-iomkw (rule 1, slot a)
```

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
  ready longer than `track`, not counting time in which the controller
  stalled (README.md, References).

A `references` entry's findings take the same summaries. Each line names
the CR and the object that its field names. The finding takes the object's
kind:

- `reported ready with no dependency`: the object was missing and
  `required`. `seen missing` means the controller's last look showed it
  missing; `not seen` means the controller never looked.
- `reported ready before seeing its dependency ready`: `seen not ready`
  means the controller's last look showed the object not ready; `not seen`
  means the controller never looked at the live object.
- `stayed ready after seeing its dependency not ready`: the CR stayed
  ready, naming the object, for `track` after the look at the time the
  line gives, not counting time in which the controller stalled, and each
  later look by then held it back.

Under `onDelete: block`, Honest counts a look at an object that waits in
deletion as ready if the look, or the object's last version before its
deletion started, shows it ready.

### Ordered: released an object that another names

An object that a `references` entry with `onDelete: block` names was
released, that is gone, while a CR named it. Each line names the object and
the CR, by its slot or else by its kind and name:

```
secrets rf-1b0b8a35/admin-password: slot a names it
```

The controller saw both in time to hold the object: `stable` before the
object's deletion started, not counting time in which it stalled. If a
step's faults stalled it in that time, the deletion also started once
`stable`, and the time those faults lasted, at most `timeouts.backoffCap`,
had passed since they ended. Check that it adds its finalizer to each
object that a CR names. Before it removes the finalizer, it should list the
CRs live, since its cache can miss a new one. If the controller does not
hold such objects by design, drop the entry's `onDelete`.

### Backoff: retried a failing request in a tight loop

The controller repeated a failing request with gaps under 100 ms. The line
names the verb, the object, the status and the calls. Retry through a rate
limiter, such as controller-runtime's default, rather than a short
`RequeueAfter`.

### Backoff: repeated a no-op write in a tight loop

The controller repeated a write that changed nothing, with gaps under 100 ms.
The line names the verb, the object, the status and the calls. A delete of an
object that is already being deleted is such a write. So is a
`deletecollection` sent again before anything in the cluster changed. Skip a
write that the cached object already shows, and wait for a watch event rather
than a short `RequeueAfter`.

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
| `the controller never started: ...` | No request got a 2xx answer within `timeouts.settle`. Requests that no check judges, such as discovery, Leases and Events, do not count. Read the output tail and `controller.log`. Fix `launch.args`, `launch.env` or `launch.rbac`. Raise `timeouts.settle` for a controller that starts slowly. |
| `creating fixture ...` | Fix the fixture, or install the CRD it needs under `crds`. |
| `creating fixture CustomResourceDefinition ...: ... already exists` | List the CRD under `crds` or under `fixtures`, not both. |
| `mapping <group>/<version>, Kind=<kind>: no matches for kind ...` | Install the listed kind's CRD under `crds`. |
| `steps[i].actions[j]: ...` or `launch.upgrades[i]: ...` that names a `binary` or `crds` path | Point `binary` at an executable file and `crds` at valid CRD files. A plan file's paths resolve against its directory, and `launch.upgrades` paths against the config's. |
| `installing CRDs: the <resource> CRD stops serving <version>, which the harness watches` | Keep that version served in the restart's `crds`. |
| `installing CRDs: ...` | Fix the CRDs that a restart's `crds` names. |
| `launch args[i]: ...`, `launch env <name>: ...`, `launch.upgrades[i].args[k]: ...` or `steps[i].actions[j].args[k]: ...` | Use only `${systems.kube.kubeconfig}`, `namespace` and `url`, and a fake's `${systems.<name>.url}`. Close each `${` with `}`. |
| `fake <name> exited ...` | Read `<out>/<name>.log`. Fix the `systems[].process` command or the fake's own config. |
| `create of slot "<s>" makes a <kind>, which primaries does not list` | List the kind under `primaries`, or make the object a fixture. |
| `create of slot "<s>" makes an object without apiVersion or kind` | Give the create's `object` both. |
| `... holds a <kind>, not a <kind>` | Point the entry's `sample` at an object of its `kind`. |

`hunt` lists each seed whose run failed in `summary.md`, by the first line
of its error, and goes on. It prints the whole error to stderr. It
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

`shrink` writes `shrunk.json` and a `summary.md` like `hunt`'s under `--out`.
