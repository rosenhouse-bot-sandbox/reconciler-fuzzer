# DESIGN.md — reconciler-fuzzer

A black-box property-based and fault-injection harness for Kubernetes controllers.

This document is the governing design for the repo. Coding agents implement against it,
and the reviewers it spawns review against it. If the code and this document disagree, one of them is wrong and the PR must say
which. §15 records the decisions behind the design and the evidence for them.

---

## 1. Thesis

Most controller bugs are not logic errors in the happy path. They are failures to
reconcile correctly across partial progress, stale reads, missed events, API errors,
and restarts. Formal verification (see Anvil / Welder) can prove liveness properties
such as Eventually Stable Reconciliation (ESR), but requires rewriting the controller.

`reconciler-fuzzer` tests the same class of properties against **unmodified** controllers by:

1. Generating random but valid sequences of operations on a custom resource.
2. Observing the controller only through the Kubernetes API (no instrumentation).
3. Injecting faults at the API boundary and by restarting the controller process.
4. Checking a small set of **generic invariants** that need no per-controller
   configuration, plus optional per-controller **properties**.
5. Shrinking any failing sequence to a minimal reproducer and emitting a report.

The controller is a black box, which reconciler-fuzzer drives through the API server. The README
lists its limitations.

## 2. Non-goals

- reconciler-fuzzer is not a formal verifier. It offers no proofs and no model checker.
- reconciler-fuzzer is not a chaos platform for production clusters. It runs against test clusters
  only, envtest or kind.
- reconciler-fuzzer does not test the Kubernetes control plane itself.
- reconciler-fuzzer is not a mocking framework for reconciler unit tests. It observes only through
  the API server.
- reconciler-fuzzer is not opinionated about controller framework. controller-runtime, kube-rs,
  client-go by hand and operator-sdk targets are all equally valid.
- reconciler-fuzzer does not replace the target's admission webhook. It generates from the CRD
  schema, so rules that only a webhook enforces are constrained per target (§8.3).

## 3. Provenance

All fixtures, targets, and examples in this repo are either (a) upstream open-source
projects referenced by name and version, or (b) designs original to this repo. No
employer code, documentation, cluster topology, or customer configuration is an input
to this project. The toy target (§9) is deliberately generic and exists only to
exercise the harness. The adoption examples (§10, M4 and M7) drive cert-manager and
external-secrets, upstream open-source projects, unmodified and pinned by version.

## 4. Vocabulary

| Term | Meaning |
|---|---|
| **Target** | A controller under test plus its CRD(s) and how to launch it. |
| **Launcher** | How a target's process is started, stopped, and restarted. |
| **Proxy** | An HTTP reverse proxy between the target and the API server. Observes every request; injects faults. |
| **Observer** | Watches the cluster state directly (not via the proxy) and records object versions and events. |
| **Test cluster** | The API server a run executes against: an envtest control plane that reconciler-fuzzer starts (default), or an existing cluster given by kubeconfig (§5.8). |
| **Primary CR** | A custom resource of the kind the target declares primary. A sequence creates up to three, and each CR op acts on one by name (§7). |
| **Fixture** | An object reconciler-fuzzer applies to the run namespace before op 0, such as an Issuer. Only a fixture op changes it; not a managed object. |
| **Managed object** | An object of a kind the target declares it manages, in the run namespace, that neither reconciler-fuzzer nor the cluster created (§6). |
| **Op** | One step in a test sequence. The ops on a primary CR are `Create`, `Update`, `Delete` and `Recreate`; the fixture ops are `UpdateFixture` and `DeleteFixture`; the control ops are `Restart`, `Fault`, `Settle` and `DeleteManaged` (§5.4). A CR op may set `noSettle` to skip the Runner's implicit settle wait (§5.5). |
| **Sequence** | An ordered list of ops plus a seed. The unit of generation, replay, and shrinking. |
| **Invariant** | A generic check that applies to every target. IDs `G1..Gn`. |
| **Property** | A per-target check declared by the target. IDs `P1..Pn`. |
| **Checkpoint** | A point at which invariants and properties are evaluated: when a settle wait ends, whether converged or expired; after each `Restart`, `Fault` and `DeleteManaged` op once the following settle ends; where the teardown's recovery wait ends (§5.5); and once after the teardown deletion window. |
| **Run** | Execution of one sequence against a clean namespace. |
| **Report** | Machine- and human-readable output of a failing run. |

## 5. Architecture

```
                 ┌──────────────┐  ops (create/update/delete)  ┌──────────────┐
  Generator ───▶ │    Runner    │ ───────────────────────────▶ │  API server  │
  (rapid)        │              │ ◀────── Observer (watch) ─── │  (envtest /  │
                 │  restart/    │                              │   kind)      │
                 │  fault ops   │                              └──────▲───────┘
                 └──┬───────┬───┘                                     │
                    │       │ fault control                           │
                    ▼       ▼                                         │
            ┌───────────┐ ┌─────────┐   all target API traffic   ┌────┴────┐
            │ Launcher  │ │  Proxy  │ ◀───────────────────────── │ Target  │
            └───────────┘ └─────────┘ ──────────────────────────▶└─────────┘
                                │ request log
                                ▼
                          Invariant engine ──▶ Report
```

In envtest mode the test cluster also includes reconciler-fuzzer's garbage-collector emulation
(§5.8). It writes to the API server directly, never through the proxy, and records its
own deletes.

### 5.1 Launcher

```go
type Launcher interface {
    Start(ctx context.Context, kubeconfig string) error                        // kubeconfig points at the proxy
    Stop(ctx context.Context) error                                            // graceful: SIGTERM, then SIGKILL after a grace period
    Restart(ctx context.Context) error                                         // crash: SIGKILL, then Start
    Supervise(ctx context.Context, onExit func(exit error, restart time.Time)) // until ctx ends, restart the target whenever it exits
    Status() Status                                                            // is the target still running, and why it stopped if not
    Exited() <-chan struct{}                                                   // closed once the target has stopped and will not start again
}
```

Implementations:

- `Binary` — the primary launcher and the only one required through M6. Exec a local
  binary. It runs on reconciler-fuzzer's host, which routes to no Pod and resolves no Service name of
  the test cluster. reconciler-fuzzer writes a kubeconfig whose server is the proxy URL and whose
  context names the run namespace, and exports it as `KUBECONFIG`. It substitutes
  `$KUBECONFIG` and `$NAMESPACE`, the run namespace, in `launch.args` and in the values of
  `launch.env`.
  `launch.env` sets variables over the environment the target inherits from reconciler-fuzzer, and
  may not set `KUBECONFIG`. The target's stdout and stderr go to `target.log` in the run
  directory. `Restart` sends SIGKILL, waits for the process to be reaped, then execs
  again, so fixed ports and lock files are released. A killed process never releases a
  leader-election lease, so a target runs with leader election off. `Supervise` restarts
  the target whenever it exits on its own, as a kubelet restarts a container: at once the
  first time, then after 10 s, doubling up to 5 min. It tells the Runner why the target
  stopped and when it starts again. `Stop` and `Restart` are not exits, and `Stop` ends
  supervision. The end of the context `Supervise` was given ends it too. `Status` says
  whether a supervised target is waiting to restart, and when the process now running
  started. reconciler-fuzzer does not probe the target for health. A settle wait does not converge
  until the process now running has shown it runs (§5.5).
- `InProcess` — deferred. It may return if envtest run time becomes the bottleneck (§14).
- `Image` — run a container image against a kind cluster, with the proxy in-cluster or
  reached by port-forward. Phase 2 (§10, M8).

### 5.2 Proxy

The proxy is an `httputil.ReverseProxy` in front of the test cluster. It listens on
`127.0.0.1` over plain HTTP, so the target's traffic is HTTP/1.1; client-go negotiates
HTTP/2 only over TLS. The proxy's upstream transport comes from the test cluster's
`rest.Config` via `rest.TransportFor`, so it carries whatever that cluster uses: a client
certificate on envtest, a token or exec credential on a kubeconfig cluster. The proxy
strips any inbound `Authorization` header. The target's requests therefore carry reconciler-fuzzer's
credentials, admin on envtest, and the target's RBAC is never exercised (§14). A target's
own `Impersonate-*` headers pass through, and the API server honors them under reconciler-fuzzer's
credentials.

Responsibilities:

- **Record** every request: verb, group/version/resource, namespace, name, status code,
  latency, timestamp. This log is the primary signal for G1.
- **Stream** long-lived watch responses without buffering (`FlushInterval = -1`).
- **Hold** a request a delay applies to, and report whether it holds one and when it
  released the last. It releases a request when it forwards it, once the delay ends, or
  when the target hangs up on one without a body. Go's server notices a hangup only
  once it has read the body, so a held write is forwarded anyway. A watch counts as held
  only until it is forwarded, however long it streams.
- **Inject faults** according to an active `FaultSpec`:

```go
type FaultSpec struct {
    Match  RequestMatcher // verb, resource, name pattern, fraction
    Action FaultAction    // Error{code}, Delay{d}, Drop{}, DropWatchEvents{kinds}
    Until  Trigger        // op index, duration, or count of requests it applied to
}
```

Two details that matter for any client-go based target:

- An injected error replaces the upstream response: the proxy sets `Content-Type:
  application/json`, drops `Content-Length` and `Content-Encoding`, and writes a
  `metav1.Status` body. client-go picks its decoder from the response `Content-Type`, so
  this works for the typed clients that negotiate protobuf.
- Requests carrying an `Upgrade` header (exec, port-forward, websocket) are passed through
  unrecorded and never faulted. Controllers do not use them.

Watch-event dropping requires parsing the watch stream (chunked JSON, or length-delimited
protobuf frames for typed clients) and filtering events. This is the hardest fault and
lands in phase 2 (§10, M8). Until then, `DeleteManaged` ops simulate a missed event by
deleting an object behind the target's back, and G7 requires the target to recreate it
(§6).

### 5.3 Observer

The Observer runs independent informers on the real API server, not through the proxy,
for the target's CRD(s) and the resource kinds the target declares it manages. It records,
per object: resourceVersion history with timestamps, generation vs observedGeneration
where present, finalizers, ownerReferences, deletion timestamps, and the UID, on which
attribution (§6) and the garbage-collector emulation (§5.8) rely. It watches the run
namespace alone, so the primary and every managed kind must be namespaced (§8.1), and it
does not see a child the target creates in another namespace.

The Observer must never affect the target. It has its own credentials and never writes.
Deleting a managed object behind the target's back is a Runner control op
(`DeleteManaged`, §5.4), never an Observer action.

### 5.4 Generator

The generator is built on `pgregory.net/rapid` and produces a `Sequence`:

- Ops on the primary CRs: `Create`, `Update` (field-level mutation), `Delete`, `Recreate`.
  A sequence creates the sample first, and may create further CRs, up to
  `generate.maxCRs` in all, three by default. The second and third take the sample's name
  with `-2` and `-3` appended, and so does their value at each `generate.distinct` path,
  which must hold a string in the sample. A spec field that names a child is such a path,
  since two CRs that name one child fight over it, which is a user's mistake and not the
  target's. No draw gives a CR another CR's value at a distinct path, whether or not that
  CR is live, nor the value another CR's suffix makes. An update or a delete acts on a
  live CR, and a recreate on any CR the sequence created.
- Control ops: `Restart` (launcher), `Settle` (wait for convergence before continuing;
  used to make properties checkable mid-sequence), and `DeleteManaged{kind, index}`
  (delete one managed object behind the target's back, §7). The Runner issues
  `DeleteManaged` directly to the API server, as it does the CR ops.
- Fixture ops: `UpdateFixture` (set one string of a fixture) and `DeleteFixture` (delete a
  fixture until a later op, §7), only on the fixtures `generate.fixtures` names (§8.1).
  Built-in kinds carry no schema reconciler-fuzzer reads, so a drawn value is a word of 4, 8 or 12
  lowercase letters and digits, which a Secret's `data` also reads as base64. Generation
  restores a deleted fixture before the next op that settles, because a target may rightly
  not be ready while a fixture is gone, and every settle wait judges G4. A target that
  names no fixture draws no fixture op.
- **Op choice.** After the create, a draw makes one to `MaxOps`−1 more ops, five at most
  by default, each count with equal chance. Each op is drawn with equal chance from those
  the state allows, and so is the CR, kind, field or fixture it acts on (D98). A
  `deleteManaged` is allowed while a CR is live, and generation puts a settle wait before
  it unless the op before it settles (D99).
- **Fault injection.** After the drawn ops are checkpointed, the generator inserts at most
  one `Fault` op per sequence, drawn with probability 0.5. The fault matches one resource
  (the primary's plural or a managed kind's, looked up from the CRD or the built-in table)
  and optionally one verb. Its action is an error code (500, 503 or 429) or a delay scaled
  to the target's `timeouts.stable`. The fault starts before an eligible op (one preceded
  by a settle, and neither a `restart` nor a `deleteManaged`, D100) and ends at a settle
  the generator inserts after the span it covers.
  `generate.faults: false` disables fault generation for a target.
- **Baseline.** After the drawn sequences, `reconciler-fuzzer run` runs the baseline,
  unless `--no-baseline` is given. It creates the sample, deletes the first object of
  each managed kind, and then updates each mutable field of the sample's CR, one op per
  field. Each update sets a value the CR does not hold and the CRD accepts, drawn up to
  eight times; a field no draw changes is left out. Every op settles. A fixed draw picks
  the values, so the baseline changes only with reconciler-fuzzer, the CRDs and the target.
  A failing baseline is minimized like a drawn sequence (D97).
- **Schema-driven mutation** from the CRD's OpenAPI v3 schema: numeric ranges, enums,
  string patterns, optional-field presence, list length, map size. Generic and works on
  any CRD. reconciler-fuzzer draws no sequence for a built-in primary kind, whose schema it does not
  read, and runs only the sequences a user writes.
- **Valid by the CRD's own rules.** Every create, recreate and update the generator draws
  passes the CRD as the API server judges the CR reconciler-fuzzer wrote: defaults, value
  validations, list types and `x-kubernetes-validations` rules, transition rules included.
  The check does not see the status the controller writes, so a CRD rule that reads status
  can still refuse a draw. The generator runs the API server's own code for this (D55). A
  create keeps a drawn field only if the CRD accepts it. A refused update is drawn again up
  to eight times, then becomes a `Settle`. Judging consumes no randomness. The sample must
  pass its CRD, and so must each further CR as the sample makes it.
- Generation starts from the target's `sample` object. When the target declares
  `generate.mutate`, only those paths are mutated, and `generate.overlay` tightens the
  schema for a path (§8.3). An overlay keyword the generator does not read is a
  configuration error. For each path it may change, the generator draws up to 100 values
  into the sample until the CRD accepts one. A `generate.mutate` path is a configuration
  error too when the generator cannot draw a value for it, such as a set whose items allow
  fewer values than its `minItems` or a pattern nothing matches, or when the CRD refuses
  every value drawn. Without `generate.mutate`, reconciler-fuzzer prints each spec path it leaves
  alone, and why: its schema says too little to draw from, such as an int-or-string, the
  generator cannot draw a value for it, or the CRD refuses every value drawn for it.
- **Hand-written generators** per target are deferred (#59). No target needs one, since
  `sample`, `generate.mutate` and `generate.overlay` keep generation inside what a target
  accepts (§8.3), and one would need a public Go API.

Every sequence is serializable to JSON (§7) so it can be replayed without rapid. A seed
names a sequence for one build of reconciler-fuzzer and one target declaration. A golden test records
what the seeds the repository runs by number draw, so a change to a draw is deliberate
(D54).

### 5.5 Runner

The Runner executes one sequence:

1. Create a fresh namespace. On a kubeconfig cluster, wait for what its controller
   manager adds to the namespace (§5.8). Exclude what the namespace holds from the
   managed objects (§6). Apply the target's fixtures. Start the target via the Launcher.
2. Apply ops in order. Before an op acts, but after its instant is stamped, create again
   each fixture a `DeleteFixture` deleted until that op (§7). After each op that mutates
   a CR or a managed object, or updates a fixture, wait up to `T_settle` for convergence
   unless the op sets `noSettle`, and longer while the target is still owed time to
   recover from a fault that stopped (§6) or to delete a primary CR. The wait ends once
   the `Ready` predicate holds on every primary CR, no primary CR is being deleted, and
   neither a CR nor a managed object has changed for `T_stable`, so a checkpoint lands
   after the target's reaction, not before it. After a deletion of a primary CR, the wait
   may run until the deletion's G3 deadline, or `T_settle` past the instant the CR went if
   it went by then. A `recreate` waits `T_delete` for its old CR to go, and longer while
   the target is owed time as above. A CR still there where that wait ends is judged
   there, as an expired settle wait is. A wait in which a CR outlived a G3 deadline that
   no fault reached into is G3's to judge (§6). Any other wait that expires while no fault
   excuses it records a G4 violation, which says why from the Observer's history of the
   wait: `Ready` never held, held and then stopped, or held while the namespace kept
   changing within `T_stable`; a CR was still being deleted; or no CR was left to be
   ready. It also says where the target was waiting to restart, restarted within
   `T_stable`, had not yet shown it runs, or first did within `T_stable`, and then leaves
   out what changed. Where `Ready` held and nothing changed within `T_stable`, it says
   that. The Runner and the engine raise it with one function, so they agree. A fault
   excuses it while active, which is once the proxy has applied it and until the proxy
   stops applying it and has released every request it held (D36), and while the target
   is still owed time to recover from it (§6). A `recreate` whose old CR stays cannot create
   its CR. Where a fault excuses the target there, the op stops the run with a note, and the
   teardown follows (step 4). G3 or G4 reports any other such CR there.
   The wait after a `deleteManaged` that deleted an object does not converge before the
   target has had its time after each fault that stopped (§6), and gives up no sooner than
   `T_stable` after that.
   A request the proxy holds counts as a change until the proxy releases it (§5.2), since
   it is about to change what the checks read. A request that reached the proxy before a
   wait's time ran out holds the wait open until `T_settle` past its release. It holds a
   `recreate`'s wait for its old CR open the same way.
   Until a settle wait has converged, normally op 0's, a wait also ends where the target's
   process exits, and the Runner checks the target is running before it applies each op. A
   target that stopped then ends the run as a harness error naming the op it was at (§11):
   a bad flag or a taken port reads the same way, and the ops behind it would run against
   nothing. The error quotes the line in `target.log` that says why: the line the last Go
   panic opens with, or else the last line above the last usage text, which Go's flag
   package and cobra print below a flag error, or else the last line above any stack
   trace, since a logger's trace ends in a frame. The log holds every process a `restart`
   started, and the last one is the one that stopped. Where reconciler-fuzzer had created the CR and
   the target had requested a resource, the error says the CR may have crashed the target
   and names the run's
   `sequence.json`, unless the target wrote that its port was taken. Once a wait has
   converged, the target has shown it runs, and the Launcher supervises it (§5.1). The run
   notes each exit and the line the target wrote as it stopped. A wait does not converge
   while the target waits to restart, nor until the process now running has shown it runs.
   reconciler-fuzzer takes a target that has read a Lease with a get to elect a leader, because
   leader election reads its Lease that way to learn who holds it. A process of such a
   target shows it runs once the API server accepts a create, update or patch of a lease
   that follows the process's own get. A controller can start informers before it leads,
   and the leader that reconciler-fuzzer replaces renews its lease without a get while its renewals
   succeed. A process of any other target shows it runs by requesting a resource outside
   leader election, because a controller lists what it watches as it starts. reconciler-fuzzer has no
   other sign that a target is back. A start and that sign count as changes, so a
   restarted target runs for `T_stable` past its return before a wait converges. A target
   that exits again within `T_stable` of each return therefore never converges, even where
   it wrote its converged state first, and its wait expires as a G4 that counts the exits
   since the target last converged and quotes the last. A target that runs longer between
   exits can converge in between, until a backoff outlasts a wait. A target that converges
   after an exit passes. reconciler-fuzzer chose when to restart the target after a `Restart` op and
   after an exit a fault excuses (§6), so a wait gives it `T_settle` past its return from
   either, where it returns within `T_settle` of the restart, and `T_settle` past the
   restart where it does not. While a fault is active, only the first exit a fault excused
   during each op gets that time (§6). An exit is during the last op stamped before it. An
   op stamped before the target has had that time after an earlier exit gives it that time
   too. Before it stamps an op, the Runner waits for any restart that follows an exit a
   fault excused, so the op never lands while the target waits for it. Any other restart
   gives it no more time, and its startup requests count toward G1 where they land in a
   quiet window (§6). A target not back when a wait expires fails G4. A restart that fails
   ends the run as the harness error above.
3. Evaluate invariants and properties at each checkpoint (§4). Properties are not
   evaluated where the proxy held a request of the target's, or released one within
   `T_stable`, which may still change what they read, nor where the target was still
   starting, nor where a fault excuses the target (§6). The run notes each such checkpoint.
   A run ends at its first violation.
   More than `N_objects` (default 500) managed objects in the namespace ends the run as a
   harness limit, reported as such rather than as a finding.
4. Tear down. Clear every active fault. If the target is still owed time to recover from
   a fault, which is so for a fault the teardown just cleared, wait for convergence as
   step 2 does and checkpoint where the wait ends. This recovery wait is judged as an op's
   wait is. A run that ended at a violation or a harness error gets none. A run a
   `recreate` stopped gets it, and the checks below, as a run whose ops all ran does. Then
   wait `T_stable`, which is the last quiet window (§6). Delete every primary CR the run
   created and wait for the G3 window. A target that stopped for good, before supervision
   or because a restart failed, cleaned nothing up, so the run ends as that harness error
   rather than at a verdict on the deletion. G3 judges a target that is waiting to
   restart, and the notes carry its exits. A run that ended at a harness error judges no
   deletion either, because its ops did not all run. Then delete every remaining object in
   the namespace of a kind the target declares or a fixture has, because a later run's
   target may watch every namespace. Then force-remove any finalizer still on one, which
   the report notes (D37). G3 judged the deletion window, which closed before this. The
   deletion comes first because the API server refuses a finalizer new to an object being
   deleted, so a running target cannot put back one it owns. Stop the target if it was
   started for this run. Delete the namespace. Namespace names are never reused, so a
   namespace that never finishes terminating (envtest, §5.8) is harmless.

**An abandoned run.** The deadline and an interrupt end the run's context (§11), and that
ends every wait of the run, the teardown's included. The run is then abandoned where it
is. Its teardown waits for nothing more and judges nothing, and a violation found before
stands. It still deletes the CRs, empties the namespace, and forces off the finalizers
without noting them. It stops the target without a grace period and deletes the namespace
on a budget of its own. Supervision ends with the context, so the target does not
restart.

Cleanup between runs never restarts the API server, because a shrink pass replays a
sequence many times and starting a control plane costs seconds.

**Shrinking** is sequence-level, and reconciler-fuzzer's own pass does all of it. It removes one op at
a time and replays what is left from clean state. Where an op stays and carries a fault,
it halves `until.count`, `until.for` and `action.delay` one at a time, while the replay
still fails, but never a count below 1 or a duration below 10 ms. A candidate replaces
the sequence where its replay fails the same check, by ID. One whose replay errs does
not, and one the sequence format refuses is not replayed. The pass goes over the sequence
again until nothing simplifies. Field values are not shrunk. The pass stops at the
deadline (§11) and keeps the smallest failing sequence it found. rapid only draws
sequences, through `Example`, which does not shrink (§5.4). rapid shrinks only inside
`rapid.Check`, and it shrinks the choices a value is drawn from rather than the ops. The
pass shrinks the JSON sequence the report carries. Each replay is a whole run against the
cluster.

### 5.6 Invariant engine

The engine consumes the Proxy request log and the Observer state history. Each invariant
is a pure function over those two inputs plus the target's declaration. Invariants are
**eventual**: they have a window and a stability requirement to absorb Kubernetes'
asynchrony. They are therefore blind to transient states; a transient state is caught by
a property (§8.1) or made permanent by a `Restart` or `Fault`. Flakiness is a bug in the
harness, not the target; tune windows, don't retry.

### 5.7 Report

A failing run emits `report.json` and `report.md` containing: the minimized sequence and
how many of its ops the run reached, the violated invariant or property with the concrete
evidence (request log excerpt, object version timeline), the target and versions, the
seed, and a one-line replay command. That command repeats the target, the kubeconfig and
every launch argument the run had, quoted so that `sh` and `zsh` read each word as
written. It does not record what the target inherits from reconciler-fuzzer's environment, so a
target declares what it needs in `launch.env` (§8.1). The run directory also holds
recordings of the run (§11), so a report can be re-examined without re-running.
`report.md` also links the failed check's section of `docs/checks.md`. A
readiness verdict and a
property violation also quote the state of the objects the target managed where it failed,
in a table of its own, bounded on its own, and say how many there were: a child the
target never created has no version to quote, and the count is what a report about a
missing one turns on (D39). A G5 violation names the two states it compared and quotes, in
a table of its own, each field that differs between them: the object, the resourceVersions
compared, the path in the form `equalIgnore` takes, and the value on each side, cut to 80
runes. An object only one state holds is one row, which names no path. The table holds at
most twenty rows, taken from each object in turn (D50).
Every violation says how many entries it chose each excerpt from, because a report that
counted only what it was handed would claim every bounded excerpt was whole. It also says
the instant it judged, which aligns a timeline with the state at the verdict, and whose
history a one-object timeline is. A readiness verdict also quotes the `ready` expression,
the error evaluating it, and the name and status of the CR it read. A status holds
whatever its controller wrote, so the report bounds it: twenty conditions as a table, 200
bytes of each field, and 1000 bytes of the rest as JSON. Each code block's fence is longer
than any run of backticks inside it. A readiness verdict and a G1 name the failing request
the target repeated most in their window, with its count, because an error loop that
backs off can stay under `N_errloop` and surface only as G4 or G1 (D48).

A report's version rows carry no object body. G5's table quotes a Secret's values as the
markers `objects.jsonl` writes (§11). A report's sequence and replay command hold the sample
and the command line as given.

A report is a snapshot taken where the check failed, and the recordings beside it are
finalized when the run ends. A request still open at the snapshot, which a watch usually
is, therefore carries no latency in the report and its own in `requests.jsonl`.

### 5.8 Test cluster

reconciler-fuzzer owns the API server a run executes against.

- **envtest** (default). reconciler-fuzzer starts `kube-apiserver` and `etcd` from
  `KUBEBUILDER_ASSETS` (installed by `setup-envtest`) using
  `sigs.k8s.io/controller-runtime/pkg/envtest` inside `internal/cluster`. This is the one
  harness package allowed to import controller-runtime (§11). It starts its own control
  plane even where `USE_EXISTING_CLUSTER` is set. Before it starts them, reconciler-fuzzer looks for
  both binaries where envtest does: `TEST_ASSET_ETCD` and `TEST_ASSET_KUBE_APISERVER`,
  then `KUBEBUILDER_ASSETS`, then `/usr/local/kubebuilder/bin`. reconciler-fuzzer looks up a bare
  name on `PATH`, as envtest does. A `KUBEBUILDER_ASSETS` of `""` or `.` leaves bare names,
  such as `etcd`. reconciler-fuzzer names the variable and the path when a binary is missing.
- **kubeconfig**. An existing cluster, normally kind. Used by `make test-kind` and, in
  phase 2, by `Image` targets. reconciler-fuzzer installs the target's CRDs there, creating or
  replacing each one, and leaves them installed. The cluster runs
  `kube-controller-manager`, whose garbage collector replaces the emulation below. It also
  adds the `default` ServiceAccount and the `kube-root-ca.crt` ConfigMap to every
  namespace. A run waits up to 30 s for those of a kind it watches, and a missing one is a
  harness error. The cluster serves one reconciler-fuzzer invocation at a time: a target that watches
  every namespace acts in another invocation's run namespace too, and each invocation
  would count that work as its own target's.

envtest runs only the API server and etcd. There is no `kube-controller-manager` and no
kubelet, so nothing garbage-collects owned objects, namespaces never finish terminating, no
default service accounts appear, no pods run, and no workload's status changes.
Consequences:

- **Garbage-collector emulation.** In envtest mode reconciler-fuzzer runs a minimal collector over
  the run namespace: it deletes a managed object that has at least one ownerReference
  once every owner in its `ownerReferences` is gone; an object with none is never touched.
  Like kube's garbage collector, it maps a reference's apiVersion and kind through
  discovery, so any version the API server serves resolves. It then finds the owner by
  (group, kind, name) in the run namespace and compares the UID; a name match with a
  different UID counts as gone. An owner of a kind reconciler-fuzzer does not watch, or named at a
  version the API server does not serve, is treated as live, so the emulator never deletes
  an object whose owners it cannot resolve. The run notes each unresolved reference once
  per object that carries it (§6). The emulator is watch-driven and deletes within 1 s of
  the owner's deletion event. It does not patch dangling ownerReferences off a dependent
  that still has a live owner. `blockOwnerDeletion`, foreground and orphan policies are
  not modelled. Its writes bypass the proxy and never count as target traffic. It records
  each delete it sends in `collector.jsonl` (§11, D88): when, the object with the UID
  and resourceVersion it read, which are the delete's preconditions, each owner and
  whether it was not found or held by another UID, and how the delete ended, a 404 or a
  409 included. It stops once the Runner has emptied the namespace (§5.5, step 4), so its
  last deletes can race that cleanup. On a kubeconfig cluster it is off.
- **Self-cleanup.** The Runner empties the run namespace itself (§5.5, step 4).
- **No workloads.** A kind that runs Pods, and a claim Pods mount, keep the status they
  were created with: Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob,
  ReplicationController, Pod and PersistentVolumeClaim. No ReplicaSet or Pod appears for
  them. A `ready` that waits on that status never holds, and a target that requeues while
  it waits can hide a missed watch. A Pod bound to a node never finishes deleting, since
  only its kubelet confirms the delete, so G3 fails on one the target manages. When
  `manages` names one of these kinds, an envtest invocation says so once, before the
  control plane starts, and names `--kubeconfig` and kind. A G4 report repeats it among
  its notes. reconciler-fuzzer does not emulate these controllers (D57).
- **No finalizers that only `kube-controller-manager` removes.** reconciler-fuzzer starts the API
  server with its garbage collector off, and with the `StorageObjectInUseProtection`
  admission plugin disabled beside envtest's own `ServiceAccount`. A delete of anything
  but a Namespace therefore adds no `orphan` or `foregroundDeletion` finalizer, whatever
  its propagation policy. No PersistentVolumeClaim carries `kubernetes.io/pvc-protection`,
  and no PersistentVolume carries `kubernetes.io/pv-protection`. On a kubeconfig cluster a
  Job or ReplicationController deleted without a policy orphans its Pods, and a claim
  keeps its finalizer until no Pod uses it.
- **No admission or conversion webhooks.** reconciler-fuzzer installs no admission webhook (§8.3).
  envtest removes a CRD's conversion webhook, so the API server converts a CR between
  versions by `apiVersion` alone. A kubeconfig cluster keeps the webhook. reconciler-fuzzer deploys
  no webhook Service, so a request that needs conversion fails unless the cluster serves
  one.

## 6. Generic invariants

All windows and thresholds are configurable per target. The defaults below are sized for
real targets; the toy target sets much shorter ones (§9).

| ID | Name | Statement | Signal |
|---|---|---|---|
| **G1** | Bounded reconciliation | Once the settle wait has ended, on convergence or at `T_settle` (default 30s) or later after a fault or a deletion (§5.5), the target makes no more than `N_quiet` (default 0) API requests in `T_stable` (default 10s). Watches do not count, nor does any request to `coordination.k8s.io`, whose leases and lease candidates leader election reads as well as writes, nor any request that names no resource, such as a health probe or a discovery read. | Proxy log |
| **G2** | No churn | Once converged under a stable spec, the target changes neither the primary CRs nor the set of managed objects and their resourceVersions for `T_stable`. A change is the target's only where one of its writes explains it (attribution, below). A status write whose content is unchanged moves no resourceVersion, so G2 counts status subresource writes from the proxy log, and more than `N_quiet` of them is churn. `N_quiet` never excuses a resourceVersion that moves. | Observer + proxy log |
| **G3** | Clean deletion | After deleting a CR with no faults active, every object the target manages for it is deleted and the CR's finalizers are cleared within `T_delete` (default 60s). Nothing the target manages remains once no CR does. | Observer |
| **G4** | Convergence | Within `T_settle` after any spec change, `UpdateFixture` or restore of a deleted fixture, and after faults stop within as long as they lasted plus `T_settle`, the target's `Ready` predicate holds on every primary CR with `T_stable` of quiet behind it (§5.5). A `Restart` gives the target `T_settle` past its return. A target waiting to restart, or not back since it last started, has not converged. This is ESR as a test. | Observer + target predicate |
| **G5** | Restart-stable | Restarting the target does not change converged state. The snapshots taken before and after a `Restart` are equal under the target's equality predicate. | Observer |
| **G6** | No error loop | The target does not make the same failing request (same verb, resource, namespace, name and subresource, 4xx/5xx) more than `N_errloop` (default 10) times within `T_settle` under an unchanged spec and fixtures, with no faults. A 409 Conflict on an `update` or a `patch` does not count. | Proxy log |
| **G7** | Self-healing | An object a `DeleteManaged` op deleted exists again, by kind and name, when the settle wait after the op ends: on convergence, or at `T_settle` or later after a fault or a deletion (§5.5). Its content may differ. A kind the target lists in `notRecreated` is exempt (§8.1). | Observer |

**The quiet window.** G1 and G2 judge the `T_stable` that follows a settle wait, which
ends where the run converged or where the wait gave up (§5.5). Measuring
it from the op instead leaves it unjudged for a target that converges quickly, because the
teardown begins one `T_stable` after the settle wait ends. The `T_stable` the teardown
waits before it deletes is a window of its own, closing at the teardown boundary (§5.5
step 4). A run judges one window per op whose settle wait it saw end, plus the teardown's:
a sequence whose last op does not settle has only the teardown's, and where the last op
did settle the two overlap, so traffic in the overlap breaks both. A window a later op or
a fault reaches into is not judged, where a fault's window runs from the first request the
proxy faulted with it to the request or the instant its trigger ran out, or to the release
of the last request it held where that is later (D36). The
teardown clears every fault before its window opens, so a fault it cleared did not reach
into it. It waits for convergence first only where the target is still owed time to
recover from a fault (§5.5 step 4), so a sequence ends with an op that settles.

**Recovery from faults.** A target backs off while its requests fail, and one that doubles
its delay retries within as long as it has been failing. Once faults stop, G4 therefore
gives the target as long as they lasted plus `T_settle`. The faults are those whose
windows reach past the last settle wait that converged, because a target that converged
had recovered. They lasted from the first request the proxy faulted with any of them, or
from that convergence if it came later, to the instant the last of them stopped, or
released the last request it held where that is later. A settle wait does not give up
before that time has passed, and one that expired is excused only while a fault is
active or that time is still owed. A spec change made within that time is
judged at the later of the two deadlines. A settle wait that converged sooner ends that
time early, except after a `deleteManaged`: nothing shows what that op changed, and a
target can converge while an informer of its still backs off. So the wait after one that
deleted an object does not converge before as long past each fault that stopped by the
op as the fault lasted, and `T_settle` more, whatever converged since (D90).
A target that exits while a fault excuses it, as controller-runtime with
leader election on does when it loses its lease, then waits out the restart's backoff
(§5.1), which reconciler-fuzzer chose. G4 gives it `T_settle` past its return from that restart too,
as after a `Restart` (§5.5), and does not judge a window the exit falls in, as it does not
judge one a fault reaches into. While a fault is active, only the target's first such exit
during each op is owed that time, because owing each later exit would hold a crash loop's
wait open for as long as the fault lasts. If the target exits again during the op, the
wait can end before it restarts. No op lands while the target waits to restart after an
exit a fault excused, and an op that lands before the target has had its time after such
an exit owes it that time too (§5.5). That time ends within `2 × T_settle` of the op, so
it bounds the op's wait. Once no fault is active, every exit a fault excused is owed its
time. Only a fault excuses an exit, so a crash loop that a fault set off still fails G4,
at the latest in the wait the teardown gives the target once it has cleared the faults.
A target a fault excuses may not yet have repaired what the fault kept it from seeing or
doing, so no property is evaluated, under `checkpoint` or `end`, at a checkpoint where a
fault is active or the target is still owed time to recover from one, and the run notes
each. That includes a wait that converged under an active fault, since a target that
cannot read a kind sees no change to it. A property evaluated `always` reads every event,
a fault's included, because a fault's transient states are what it exists to catch (§5.6).

**A target still starting.** A wait converges only once the target is back since it last
started and has run for `T_stable` after that (§5.5). A wait a fault excuses can end
sooner: a later exit during the op is owed no time, and a target can take longer than
`T_settle` past its restart to win its lease back. The target is then still starting: it
is waiting to restart, restarted within `T_stable`, or has not shown it runs since it last
started (§5.5), or first did within `T_stable`. G4's statement names the same states. The
teardown's checkpoint can find the target so too. A target still starting may not yet
have acted on what changed while it was down, so no property is evaluated at that
checkpoint, under `checkpoint` or `end`, and the run notes each one.
A wait that converged saw the target back for `T_stable`, so this rule does not skip its
checkpoint, even where a `Restart` op replaced a target waiting out its backoff. A property evaluated
`always` reads every event rather than a checkpoint. No invariant needs the rule. A target
that does not come back fails G4 where a wait expires with no fault active, at the latest
in the teardown's recovery wait. G7 asks more after a restart: the target must be back
before the op. G1 and G2 judge no window a fault reaches into, so they skip the window
after such a wait where a fault is active at its end. G3 judges deadlines, G5 converged
states and G6 failing requests.

**The teardown boundary.** No invariant window reaches past the instant the Runner
begins the teardown (§5.5 step 4), because from there on reconciler-fuzzer is the one changing the
namespace and the attribution below no longer holds. A window that would close after it
is not judged, rather than judged early: judging early would hold the target to a shorter
window than §6 gives it, and where the boundary falls would depend on harness timing. G3
is the exception, since §5.5 step 4 opens its window deliberately, and §4's teardown
checkpoint still evaluates properties. A primary CR with a deletionTimestamp need not
satisfy `Ready`: it is being deleted, so G3 judges it, not G4. A settle wait waits for such
a CR to go, until its G3 deadline (§5.5), so a wait in which it outlived that deadline ends
where G3 can judge it, and G4 leaves that wait to G3. Where a fault reached into the
deletion, G3 only notes it, and G4 judges the wait.

**Attribution.** A managed object is any object of a declared managed kind in the run
namespace that neither reconciler-fuzzer nor the cluster created. Fixtures and the primary CRs are
reconciler-fuzzer's. The cluster's are what the namespace holds before the fixtures and the target,
once §5.8's wait is over. Both are excluded by name, so an object the cluster recreates
stays excluded. The namespace is private to one run, since a kubeconfig cluster serves one
invocation at a time (§5.8). Everything else in it came from the target, except what a
cluster adds later: the optional selector leaves that out. G2 counts a change to a primary
CR or a managed object only where one of the target's writes explains it: a create,
update, patch, delete or deletecollection of the object's resource in its namespace, which
names the object or, as a create does, no object. The write reached the proxy inside the
quiet window and no later than the Observer saw the change. Its subresource and its answer
do not matter. So G2 does not count a change the cluster makes, such as the garbage
collector's delete of a child (D92). The other checks judge what the namespace holds,
whoever changed it. ownerReferences refine attribution to a particular CR; they are not required for it. An
object belongs to the primary CRs its ownerReferences name, by UID, and one that names
none may belong to any CR. G3, G5, G7 and the properties attribute by that rule, so that
a run of several CRs holds each to its own objects (below). An object owned through
another, such as a ReplicaSet of a Deployment a CR owns, names no CR.

**Fixtures.** A fixture stays reconciler-fuzzer's, whatever its kind, and so does the object reconciler-fuzzer
creates again under its name, so G3 never counts one as left behind and G7 never asks for
one back. A fixture op, and the restore of a deleted fixture, is a change of reconciler-fuzzer's: G5
does not judge a restart it falls between, and G7 notes a `DeleteManaged` it precedes
before the run converged. G4 measures `T_settle` from an update or a restore of a fixture as
from a spec change, and G6 counts failing requests afresh after any fixture op or restore.
A deletion hands G4's window before it to the restore, since a target may rightly not be
ready while a fixture it reads is gone. A settle wait that ends while a fixture is gone is
judged as any other, so generation restores the fixture first (§5.4).

**Deletion.** Owned children are removed by the cluster's garbage collector (real on kind,
emulated on envtest, §5.8). G3 therefore fails on orphans, meaning children with no
ownerReference to the CR, and on finalizers that never clear. The teardown watches the
namespace until it is clean or `T_delete` expires (§5.5 step 4). The settle wait after a
`delete` op waits for the CR to go, as a `recreate` does (§5.5 step 2), so G3 judges that
deletion where the wait ends if the CR outlived `T_delete`. A namespace that came clean
satisfies G3 at that instant, which is how a target that cleans up promptly is judged
rather than left unjudged: the run stops watching long before `T_delete` is up. An object
a `DeleteManaged` op took inside the window is not cleanup: G3 notes it (D38). An object
there when a CR went belongs to the CRs it names, or to every CR there then if it names
none. The last of those CRs to go answers for it, at its own deadline: a CR still there at
another's deadline, or deleted after it, keeps the object.

**Periodic work.** A controller that resyncs on a timer makes requests after it has
converged, often a write that changes nothing. `N_quiet` is how many of those one quiet
window may hold, per target (§8.1). A tick every `interval` puts at most
`floor(T_stable / interval) + 1` ticks in one window, so `N_quiet` is that times the
requests one tick makes, summed over every timer the target runs, such as one per CR. A
write that changes something still moves a resourceVersion, which G2 reports whatever
`N_quiet` is. Any `N_quiet` above zero also lets a slow loop of that many requests per
window through G1. G6 counts a loop only while it fails more than `N_errloop` times
within `T_settle`, so a slow failing loop can pass both.

**What the proxy cannot see.** G1 and G6 observe only requests that leave the target
process. Reads served from a client-side cache are invisible, so a reconcile loop that
makes no API calls is outside what reconciler-fuzzer can detect.

**What G6 does not count.** A 409 Conflict on an `update` or a `patch` is the API
server's optimistic-concurrency contract: the target is meant to re-read and write again,
and controllers that share one object's status conflict constantly. A 409 on a `create`
is AlreadyExists, which says the object is there, so a target that keeps re-creating it is
looping and G6 counts it; so is a 409 on any other verb. G6 also counts a failing watch,
which G1 excludes: G1 ignores a watch because a watch that hangs is the target waiting,
while a watch that fails returns at once and repeating it is a loop.

**Backoff.** controller-runtime's default rate limiter doubles a failing request's delay
from 5 ms. A loop on one object therefore fails 10 times in its first 5 s, 11 times by
5.1 s, and 13 times in its densest 30 s, however long it runs. The default `N_errloop`
sees it once it has run 5.1 s under one spec. A `T_settle` of 5 s holds only 10, so it
needs an `N_errloop` of 9 or less, which is why the toy declares 5 (§9). A loop on a
fixed interval longer than `T_settle / N_errloop` escapes G6, and a readiness verdict or
G1 often names it instead (§5.7).

**Notes.** A check that could not judge something records a note naming it: G3 for a
deletion whose deadline the run did not reach, that a fault reached into, or that reconciler-fuzzer
took an object inside, G5 for a `Restart` missing a snapshot or with a change of reconciler-fuzzer's
or a fault between its snapshots, and G7 for an object a `DeleteManaged` deleted that did
not come back, where the op followed such a change before the run converged or followed a
`Restart` the target had not yet answered, or where a fault reached into the op or its
wait or the target was still owed time to recover from one where the wait ended. The
Runner carries the last checkpoint's notes out and `reconciler-fuzzer` prints them at the end of the
run, because a check that was skipped otherwise reads like one that passed. A run may
delete one CR more than once, so a G3 note names the first `delete` or `recreate` that
found the CR, or else the teardown. The Observer can record a deletion after the next op
begins, and that op finds the CR too. Where neither found it, as when an op deleted a CR
the Observer had not yet seen, the note names only the CR. G5 also notes an `equalIgnore`
path it could not follow (§8.1), since it then compares a field the target meant it to
skip. The Runner also notes each ownerReference the collector could not resolve (§5.8),
since the object that carries it stays, and G3 would report it without saying why. It
notes each fault op whose fault the proxy applied to no request, since that fault tested
nothing (D36). It notes each exit of the target it restarted (§5.5), since a run that
passes shows no other sign of it. A G4 report on envtest also notes the managed kinds
whose status envtest never changes (§5.8), since G4 may fail for that alone. reconciler-fuzzer prints
that note once, when the invocation starts, rather than with a run's notes.

**Readiness.** G3 and G6 require nothing from the target except which resource kinds it
manages. G4 needs a `Ready` predicate. G1, G2, G5 and G7 need none of their own, but they
read the settle wait, which the predicate ends. The default is
`has(status.observedGeneration) && status.observedGeneration == metadata.generation`, and
a target whose primary CR lacks that field must declare `ready` (§8.4).

**G5 evaluation.** G5 is evaluated once per `Restart`, and one violation names every object
that differs across it, judged at the snapshot after it. The Runner snapshots whenever a
settle wait converges, implicit or explicit. A `Restart` is compared against the last
converged snapshot before it and the first converged snapshot after it. If either is
missing, G5 is not evaluated for that `Restart` and the report says so. The same holds
where a fault's window reaches between them. An op reconciler-fuzzer applied between the two
snapshots that changed a CR, a managed object or a fixture leaves unjudged what it may have
changed: a `Create`, `Update`, `Delete` or `Recreate` reaches its CR, and a `DeleteManaged`
that deleted something reaches its object and the CRs that object named, or every CR where
it named none. A fixture op, or an op before which reconciler-fuzzer restored a fixture, reaches every
object, since any CR may read the fixture. What a reached CR owns, and every object that
names no CR, is reached too (§6, attribution). A difference there could be the op's. G5
compares the rest and notes what it left out, and where nothing is left it is not evaluated
for that `Restart`. A `Settle`, another `Restart`, a `DeleteManaged` that deleted nothing
or a `Fault` that faulted no request leaves the comparison standing. Snapshots are keyed by
kind and name.

**G5 equality.** The default ignores exactly `metadata.resourceVersion`, `metadata.uid`,
`metadata.creationTimestamp`, `metadata.generation`, `metadata.managedFields`,
`status.conditions[*].lastTransitionTime`, and any ownerReference whose owner no longer
exists. G5 finds an owner by group, kind and name, whatever version the reference names,
and then by UID. It compares everything else, including labels, annotations, finalizers
and the remaining ownerReferences. A target excludes further paths with `equalIgnore`,
written in the form of the paths above (§8.1). Along an ignored path, a map or a list
left empty counts as absent, so an ignored annotation that only one side carries compares
equal. An item that `[*]` names stays even when left empty, so the items still count.

**G7 evaluation.** G7 is evaluated once per `DeleteManaged` op that deleted something,
where the settle wait after it ends, which is always before the teardown boundary. Its
window is that wait: up to `T_settle` or later after a fault or a deletion, closing once
`Ready` holds with `T_stable` of quiet behind it (§5.5). Where `Ready` holds without the
object, the target therefore has `T_stable` to recreate it, or longer after a fault that
stopped. An object of the deleted one's
kind and name satisfies G7, whatever its UID and content, since a recreated object carries
a new UID. Where none exists, G7 does not judge an op where nothing asks for the object
back when the wait ends: no CR the object named, or no CR at all where it named none, is
live and not being deleted. It notes an op where reconciler-fuzzer changed a CR, a managed object or a
fixture after the last settle wait that converged, since the target may then have meant to
delete the object itself. It notes one where a fault was active during the op or its wait,
or where the wait ended while the target was still owed time to recover from a fault. That
time counts each fault that stopped before the op, whatever converged since, and the
Runner's wait does not end before it (D90). It
also notes an op that follows a restart, by a `Restart` op or by `Supervise` after an exit,
where the target had not shown it runs (§5.5) between the last restart and the op. A
settle wait that converged after the restart rules this out.
It notes an op where the target exited, or waited to restart, during the op or its wait.
Where several of these apply, the note names the first. A violation quotes the object's
history and the managed objects where the wait ended, which show an object recreated under
a new name.

## 7. Sequence format

```json
{
  "seed": 8675309,
  "target": "toy-widget",
  "ops": [
    {"i": 0, "t": "create", "obj": {"apiVersion": "...", "kind": "Widget", "spec": {"count": 3}}},
    {"i": 1, "t": "fault", "spec": {"match": {"verb": "create", "resource": "configmaps", "fraction": 0.5}, "action": {"error": 500}, "until": {"op": 3}}},
    {"i": 2, "t": "update", "patch": {"spec": {"count": 5}}, "noSettle": true},
    {"i": 3, "t": "settle"},
    {"i": 4, "t": "restart"},
    {"i": 5, "t": "settle"},
    {"i": 6, "t": "deleteManaged", "kind": "v1/ConfigMap", "index": 0},
    {"i": 7, "t": "delete"}
  ]
}
```

Details the example does not show:

- Any CR op may carry `"noSettle": true`, which skips the Runner's implicit settle wait.
- `update`, `delete` and `recreate` act on the CR that `"cr"` names, or on the sample's name
  where they name none, so a sequence of one CR named as the sample names none. A `create`
  names its CR in `obj`, and a `recreate` creates the CR it deletes. These are
  configuration errors: an `obj` with no `metadata.name`, an op on a CR that no earlier
  `create` or `recreate` creates, an `update` or `delete` of a CR deleted since it was last
  created, a `recreate` whose `obj` names another CR, a `create` of a CR that no
  `delete` has removed since, and a `create` of a CR that a `noSettle` `delete` removed
  with no op since that settles. A finalizer may still hold that CR, and the API server
  refuses the create while one does. A `recreate` waits for its old CR to go.
- A sequence ends with an op that settles, or nothing judges the state it leaves behind
  (§6, D33). That rules out a trailing `noSettle`, `restart` or `fault`.
- G5 judges a `restart` only between two converged settle waits with no fault's window
  between them, and leaves out what a CR op, a fixture op or a `deleteManaged` between
  them may have changed (§6). Put a `settle` op after a `restart`, and one before it
  unless the op before it settles. G7 judges a `deleteManaged` after a `restart` only once
  the target has shown it runs (§5.5), which a `settle` op between them waits for.
- A fault may outlast the sequence. The teardown then clears it and waits for the target
  to recover (§5.5).
- A fault's `match.verb` is one of `get`, `list`, `watch`, `create`, `update`, `patch`,
  `delete` and `deletecollection`, the verbs the proxy records. Its `match.resource` is
  the plural the API server serves, such as `configmaps`, because the proxy records that.
  The Runner asks discovery for it when it applies the fault op, so a CRD the target
  installed counts, and a name the API server does not serve ends the run as a
  configuration error. It holds no slash, so it names no group, version or subresource. A
  fault on `widgets` also matches the requests to `widgets/status`.
- A fault's `match.name` is a glob as Go's `path.Match` reads it. `match.fraction` is a
  share above 0 and up to 1, and a fault without one applies to every request it
  matches. `action.error` is a status from 400 to 599. `action.delay` is not negative, and
  0 leaves it unset. `until.count` and `until.for` are above 0, because reconciler-fuzzer would read 0
  as no trigger. `until.op` is above the fault's own index, and an index past the last op
  lets the fault outlast the sequence. Any other value is a configuration error.
- Each `fault` op adds a fault of its own, even where its spec equals another's. The proxy
  tries faults in op order, the first that applies to a request wins, and each runs out on
  its own `until`.
- `update` applies `patch` as a JSON merge patch (RFC 7386).
- `updateFixture` applies `patch` as a JSON merge patch to the fixture of `kind` and `name`,
  and settles: `{"i": 1, "t": "updateFixture", "kind": "v1/Secret", "name": "token",
  "patch": {"data": {"token": "abcd"}}}`.
- `deleteFixture` deletes the fixture of `kind` and `name` and waits up to `T_delete` for it
  to go, and reconciler-fuzzer creates it again, as it last wrote it, before the op `until` names acts:
  `{"i": 2, "t": "deleteFixture", "kind": "v1/Secret", "name": "token", "until": {"op": 3}}`.
  A fixture still there where the wait ends, held by a finalizer, ends the run as a harness
  error that names the finalizers. So does another object that takes the fixture's name
  before reconciler-fuzzer restores it. `until.op` names a later op, up to the last, and no op
  before it acts on that fixture. A `deleteFixture` does not settle.
  A fixture op names a fixture the target declares, or the run ends as a configuration
  error.
- `recreate` is a delete, a wait for the object to disappear, and a create of `obj` under
  the same name. An object still there where the wait ends is judged there, and the op
  creates nothing (§5.5).
- `deleteManaged` selects the i-th managed object of `kind`, ordered by creationTimestamp
  then name. The index is resolved at execution time against what the Observer has seen.
  An index that resolves to nothing is skipped and reported as a note, since a target that
  manages fewer objects than the sequence expected is behaving, not failing. So is an
  object already gone when reconciler-fuzzer deletes it, which the Observer had not yet seen go: the
  op deleted nothing, and the note names the object. A kind the target does not declare in
  `manages` is a configuration error. G7 judges a `deleteManaged` only once the run has
  converged since reconciler-fuzzer last changed something (§6), so put a `settle` op between a
  `noSettle` op and a `deleteManaged`.

reconciler-fuzzer refuses an op that lacks a field its type needs, or carries one it does not take.
The error names the fields the type needs.

`reconciler-fuzzer replay --target target.yaml sequence.json` re-executes exactly this. Reports
embed the minimized sequence in this format. `docs/reference.md` lists every field, op and
fault field, and its example sequence sets each one (§11).

## 8. Target contract

A target is declared in YAML. In-repo targets, including the toy, are declared the same
way. Go code can attach named hooks (§8.4) when CEL or the schema is not enough; no
phase-1 target needs one.

### 8.1 target.yaml

```yaml
# examples/cert-manager/target.yaml
name: cert-manager
version: v1.21.2                              # free text; printed in reports
crds:
  - crds/cert-manager.crds.yaml               # files or directories of CRD YAML
primary: cert-manager.io/v1/Certificate       # the resource CR ops act on
sample: certificate.yaml                      # a valid primary CR; generation mutates copies of it
fixtures:
  - issuer.yaml                               # applied to the run namespace before op 0
manages:
  - v1/Secret
  - cert-manager.io/v1/CertificateRequest
notRecreated:                                 # managed kinds G7 does not require back
  - cert-manager.io/v1/CertificateRequest
ready: >-                                     # CEL over metadata, spec, status; must yield bool
  has(status.conditions) && status.conditions.exists(c,
    c.type == "Ready" && c.status == "True"
    && has(c.observedGeneration) && c.observedGeneration == metadata.generation)
properties:                                   # optional per-target checks, IDs P1..Pn
  - id: P1
    description: A Ready Certificate's Secret exists.
    cel: >-
      !has(status.conditions)
      || !status.conditions.exists(c, c.type == "Ready" && c.status == "True")
      || managed.exists(o, o.kind == "Secret" && o.metadata.name == spec.secretName)
    when: checkpoint                          # always | checkpoint | end
generate:
  maxCRs: 3                                   # CRs a sequence may create; 3 by default
  distinct:                                   # paths at which no two CRs hold one value
    - spec.secretName
  mutate:                                     # allowlist of paths; absent means every schema path
    - spec.dnsNames
    - spec.duration
    - spec.privateKey.algorithm
    - spec.privateKey.rotationPolicy
  overlay:                                    # per-path schema tightening
    spec.dnsNames: {minItems: 1, maxItems: 3}
    spec.duration: {enum: ["1h", "24h", "2160h"]}
    spec.privateKey.rotationPolicy: {enum: ["Always"]}
launch:
  binary: bin/cert-manager-controller
  args:
    - --kubeconfig=$KUBECONFIG
    - --leader-elect=false
    - --enable-certificate-owner-ref=true
    - --metrics-listen-address=127.0.0.1:0
timeouts:                                     # optional; defaults in §6
  settle: 30s                                 # stable has to be shorter than this
  stable: 10s
  delete: 60s
thresholds:                                   # optional; defaults in §6
  errloop: 10                                 # N_errloop for G6
  quiet: 0                                    # N_quiet for G1 and G2
```

A settle wait ends once the Ready predicate holds and nothing has changed for `stable`,
within `settle` or later after a fault or a deletion (§5.5), so the target has
`settle - stable` to react before the quiet window has to open. A `stable` at least as
wide as `settle` leaves it none, and every op that writes then expires. Loading such a
target is a configuration error rather than a run that reports G4 against a target that
did nothing wrong.

A key target.yaml does not take is a configuration error. It names the key's line and
dotted path, and the key within two edits of it, or else the keys its block takes. A swap
of two adjacent letters counts as one edit. `docs/reference.md` lists every key with its
default and meaning (§11).

`generate.maxCRs` bounds the primary CRs a sequence creates, and `1` keeps a run to the
sample alone, for a controller that takes one CR per namespace. `generate.distinct` names
the spec paths at which no two CRs may hold one value, such as a field that names a
child. Each CR after the first appends `-2` or `-3` to the sample's string there (§5.4). A
distinct path the sample holds no string at is a configuration error, as is a CR so made
that its CRD refuses.

The sample must set `metadata.name`, since each further CR extends it and ops name the CR
they act on.

`crds`, `sample` and `fixtures` are relative to the directory holding target.yaml.
`launch.binary` is relative to the directory reconciler-fuzzer runs in, or a name on `PATH`, because
`launch.args` and any relative path the target opens itself resolve from there too. reconciler-fuzzer
checks that it can execute `launch.binary` before it starts a control plane.

`launch.env` sets environment variables for the target, over those it inherits from reconciler-fuzzer.
Its values and `launch.args` take two placeholders: `$KUBECONFIG`, the path of the
kubeconfig reconciler-fuzzer writes, and `$NAMESPACE`, the run namespace, which that kubeconfig also
names (§5.1). reconciler-fuzzer replaces each occurrence of either text and expands no other spelling,
such as `$(NAMESPACE)`. reconciler-fuzzer sets no namespace variable of its own, because frameworks
name it differently. An operator-sdk operator declares:

```yaml
launch:
  binary: bin/manager
  env:
    WATCH_NAMESPACE: $NAMESPACE
    POD_NAMESPACE: $NAMESPACE
```

YAML 1.1 reads an unquoted `0022` as 18 and `ON` as true. A `launch.env` name or value that
decoding would change is a configuration error, as is one that holds NUL.

`manages` names kinds as `group/version/Kind`, with `v1/Kind` for the core group. An
optional `selector` (label selector) refines attribution (§6). Paths under `generate.mutate`
and `generate.overlay` are dotted schema property names, which the CRD schema validates. A
Go hook may replace the equality predicate as `equal: go:<name>` (§8.4). A hook takes no
`equalIgnore`, since nothing would read it.

`generate.fixtures` names the files under `fixtures` whose objects generation may delete,
and under `mutate` the strings in them it may set, written as `equalIgnore` writes a path.
Each path must name a string every object in the file holds:

```yaml
generate:
  fixtures:
    secret.yaml:
      mutate:
        - data.token
```

Name only strings in which the target accepts any word §5.4 draws. G4 requires `Ready` after
each update, so a word the target rightly rejects fails a correct controller. A Secret's
`data` decodes the word to 3, 6 or 9 arbitrary bytes.

`notRecreated` lists managed kinds the target leaves deleted by design, or recreates under
a new name, which G7 does not require back (§6). Each must appear in `manages`.
cert-manager lists CertificateRequest: a request records one issuance, and a Ready
Certificate whose request is deleted issues no new one.

The primary, every managed kind and every fixture must be namespaced, because a run owns
one namespace (§5.5, D13). reconciler-fuzzer judges scopes in two checks, and each refuses every
cluster-scoped kind it knows in one error. The first runs when reconciler-fuzzer loads the target.
It knows the kinds a Kubernetes 1.37 API server serves by default and the kinds the
`crds` define. The second runs through discovery once the control plane is up, before
the first run. It knows the rest, such as a kind whose CRD the cluster holds but `crds`
does not list. On `--kubeconfig`, a target with kinds of both sorts therefore hears of
them in two errors. reconciler-fuzzer observes only the run namespace, so it does not see a child
the target creates in another.
reconciler-fuzzer creates the CR and each fixture in the run namespace. A fixture sets no
`metadata.namespace`, because the target may look for it in the namespace it names. A
check refuses such a fixture only if it found no cluster-scoped kind and knows the
fixture's kind is namespaced. The run refuses the rest. The CR may set one, which reconciler-fuzzer
replaces, because the target finds a CR by watching.

`equalIgnore` lists further paths G5 ignores (§6). A path joins keys with `.`. A key that
holds `.`, `[`, `]`, `"`, `*`, `/`, `:` or whitespace goes in brackets as a JSON string,
and `[*]` names every item of a list or value of a map:

```yaml
equalIgnore:
  - status.lastSyncTime
  - metadata.annotations["probe.example.com/started-at"]
  - status.conditions[*].lastHeartbeatTime
```

YAML gives `[`, `]`, `: ` and ` #` meanings of their own, so the list is written in block
style, and a path that starts with `[` or holds `: ` or ` #` goes in single quotes. The
loader refuses a malformed path, naming the offset. It refuses a list index such as `[0]`,
since a restart can reorder a list. It refuses a key that the dots split where it can
tell: a key outside brackets that holds `/`, such as the `io/name` of
`app.kubernetes.io/name`, and a path that goes more than one step below the `labels` or
`annotations` of any `metadata`, which map keys to strings. A key names nothing inside a
list, and only an object shows where a list is. G5 therefore notes a key that meets a list
and names the path with `[*]` in its place (§6).

A property's `when` says where it is evaluated: `always` on every Observer event before
the teardown boundary (§6), `checkpoint` at each checkpoint (§4), `end` at the last
checkpoint only. Neither of the last two judges a checkpoint §5.5 step 3 leaves
unjudged.

cert-manager v1.21.2 binds its healthz server to `0.0.0.0:9403`. The one flag that moves
it, `--internal-healthz-listen-address`, is hidden, and upstream says the prefix and the
hiding are there to discourage overriding it and that the flag may be renamed or removed.
reconciler-fuzzer does not build on it, so runs against this target are sequential. Its metrics
server does take a supported flag, and the ephemeral port above keeps it out of the way.

### 8.2 Go form

`internal/target` loads the YAML into a `Target`, and everything downstream consumes it.
`go doc github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/target Target` lists its fields, so this document
gives only the intent. `Ready` returns an error beside its verdict, and an error means not
ready (§8.4). A property's error is a configuration error. A nil `Equal` means the §6
default with the `equalIgnore` paths. An `equal` hook replaces the default and takes no
`equalIgnore` (§8.1). The Runner keys snapshots by kind and name, never by UID, so a
recreated object compares against its predecessor.

### 8.3 Generation constraints and admission webhooks

reconciler-fuzzer generates from the CRD's OpenAPI v3 schema and keeps the rules the CRD states,
`x-kubernetes-validations` included, on the CR reconciler-fuzzer writes. It cannot keep a rule that
reads the status the controller writes (§5.4). Phase 1 does not install the target's
admission webhooks. Rules that only a webhook enforces are therefore invisible to the
generator. For cert-manager these include: a Certificate needs at least one of
`commonName`, `dnsNames`, `ipAddresses`, `uris` or `emailAddresses`; `duration` must
parse as a Go duration; `renewBefore` must be shorter than `duration`; a `dnsNames` entry
must be a DNS name, which neither the CRD schema nor the API server checks, so the
overlay spells out an RFC 1123 label. The target keeps generation inside the valid subset
with `sample`, `generate.mutate` and `generate.overlay`. A rule may also span ops, which
`target.yaml` cannot state. Under `rotationPolicy: Never`, cert-manager keeps a stored key
that a later `algorithm` does not match, and waits for a user, as its CRD documents. So the
overlay leaves `Never` out, and a pinned sequence runs it (D87). A generated spec that the
target rejects, ignores or leaves for a user because it violates such a rule is a
target-declaration bug, not a finding; the journal records each rule that had to be encoded
this way. A CR op the API server refuses ends the invocation as a configuration error that
names the run and the sequence file holding the op.

### 8.4 Predicates

`ready` and each property's `cel` are CEL expressions compiled with `cel-go`. Equality is
not CEL: it is the §6 default with the `equalIgnore` paths also ignored, or a Go hook.
JSONPath is not supported anywhere.

`ready` binds `metadata`, `spec` and `status` to the corresponding top-level fields of a
primary CR as dynamic maps; a missing field binds to an empty map. `Ready` holds where it
holds on every primary CR. A property binds those three to each primary CR in turn, and
holds where it holds on each. Where no primary CR exists, as at the teardown's checkpoint,
it runs once with the three empty, over every managed object, and a violation there says
that no CR existed. It also binds `managed`, the managed objects that name that
CR in their ownerReferences or name no CR (§6), as dynamic maps, each carrying
`apiVersion`, `kind`, `metadata` and the object's other top-level fields. The standard
macros (`exists`, `all`, `has`, `map`, `filter`) and the string extensions are available. At a checkpoint the harness reads `managed` from the API server, not from the
Observer cache, so cross-informer ordering cannot produce a false finding.

A compile error or a non-boolean result is a configuration error (exit code 2, §11), never
a finding. `ready` is typed dynamically, so a non-boolean result can first appear at run
time; the run ends there. An expression is evaluated against objects that may not yet
carry the fields it reads, so it must guard optional fields with `has()`. An evaluation
error while polling for readiness means "not ready"; it becomes a G4 finding only if it
persists past `T_settle`. The finding quotes the CEL error, and the report quotes the
expression and the CR's status beside it (§5.7). An evaluation error in a property is a
configuration error.

A Go hook is a function registered under a name in `internal/target` and referenced as
`ready: go:<name>` or `equal: go:<name>`. Hooks exist for in-repo targets only. No Go
function runs reconciler-fuzzer end to end, and its packages make no compatibility promise (D81).

## 9. Toy target: `Widget`

The toy exercises every invariant and proves that the harness catches known bugs. It is
deliberately boring. It builds as the binary `bin/toy-widget` and is declared in
`targets/toy-widget/target.yaml`; `--bug` is passed through `launch.args`.

- `Widget.spec.count` (int, 0–10, required). The controller ensures exactly `count`
  ConfigMaps named `<widget>-<i>` exist, owned by the Widget, each containing `index: i`.
- `Widget.status.ready` (int) holds the number of ConfigMaps present. The controller also
  sets `status.observedGeneration`.
- The Widget carries the finalizer `widget.reconciler-fuzzer/cleanup`. On deletion the controller
  lists the children by ownerReference, deletes any that remain, and removes the finalizer
  when none remain. Children also carry ownerReferences, so the collector and the
  finalizer are two independent cleanup paths.
- The controller sets those ownerReferences **except** where a seeded bug says otherwise.
- `--cleanup-delay` holds a deleted Widget's finalizer that long past its
  deletionTimestamp before the cleanup begins. The controller stays correct, only slower.
- `ready`: `has(status.observedGeneration) && status.observedGeneration ==
  metadata.generation && has(status.ready) && status.ready == spec.count`.
- `P1`, `when: checkpoint`: `status.ready` never exceeds the number of Widget-owned ConfigMaps
  present. In CEL, `!has(status.ready) || status.ready <= managed.filter(o, o.kind ==
  "ConfigMap").size()`.
- `timeouts: {settle: 5s, stable: 2s, delete: 10s}`. The toy converges in milliseconds.
- `thresholds: {errloop: 5}`. B5's backoff repeats its failing request 10 times in the
  toy's 5 s `T_settle` (§6, backoff).
- `--resync=<duration>` requeues every Widget on that interval and writes its status each
  time, changed or not. The envtest tier runs it at 900 ms, which fails G1 under the
  default `N_quiet` and passes under `quiet: 3` (§6, periodic work).
- The toy watches only the namespace `WATCH_NAMESPACE` names, where it is set, and its
  target sets it to `$NAMESPACE` (§8.1).
- `--label-from=<name>` has each child copy `data.label` of that ConfigMap in the Widget's
  namespace, and the toy watches that ConfigMap. The target sets it to `widget-config`,
  which it declares as a fixture.
- `--lease=<duration>` has the toy elect a leader through a Lease in `WATCH_NAMESPACE`
  that lasts that long, renew it within two thirds of that, and exit when it loses the
  lease, as controller-runtime does. The envtest tier runs it at 3 s under a fault that
  fails most lease updates, so the toy can lose its lease and restart until the teardown
  clears the fault (§6, recovery from faults).
- `--index` indexes Widgets by `spec.count`, as a controller indexes a field it lists by.
  controller-runtime starts the informer an index needs before the manager leads, so with
  `--lease` the toy watches Widgets while it waits out its lease. The envtest tier runs it
  so under the same fault (§5.5).

### 9.1 Seeded bug catalog (`--bug=<id>`)

| ID | Bug | Class | Should trip |
|---|---|---|---|
| B1 | Writes `status.ready = count` before creating children, and holds that state for 3 s so P1 trips deterministically; the hold must satisfy `T_stable < hold < 2 × T_stable`, which also lands the children in the quiet window | intermediate-state | G1, G2, P1 |
| B2 | Uses `generateName` for children and never deletes surplus ones, so every reconcile adds duplicates | non-idempotent | G1, G2 |
| B3 | Omits the ownerReference on child `<widget>-0` and counts children by name, so it converges and the orphan surfaces on deletion | orphan | G3 |
| B4 | Reads `count` from `status.ready` instead of `spec.count` | stale-state | G4 |
| B5 | Treats NotFound on child Get as an error and requeues forever. The Get is an uncached read (`mgr.GetAPIReader()`), so the failing request reaches the proxy | error loop | G6, G1 |
| B6 | Writes a fresh `status.lastSyncTime` (microsecond precision, so consecutive writes differ) on every reconcile, so every write re-triggers the controller | churn | G1, G2 |
| B7 | Does not delete children on `count` decrease | scale-down | G4 |
| B8 | Does not `Own()` ConfigMaps, so a deleted child is never recreated. The toy's `ready` does not depend on the children, so G4 stays true | unobserved-state | G7 and P1 (at the checkpoint after a `DeleteManaged` op), G5 (after a `Restart` recreates the child) |
| B9 | Removes the finalizer on the first deletion reconcile, before deleting children, and omits ownerReferences on every child, so no path cleans up | cleanup-ordering | G3 |
| B10 | Writes status only from an in-memory flag set when it created children. After a `Restart` the flag is gone, so a later scale-down converges the children but leaves `status` stale (a scale-up creates a child and re-arms the flag) | intermediate-state | G4 |
| B11 | Believes a child is present from the moment it asks the API server to create it, and never asks again. The belief outlives whatever removed the child, so a refused create, a scale-down or a `DeleteManaged` leaves the toy one child short for good, with no error and no requeue | unconfirmed-write | G4 |
| B12 | Once it has written status, logs what percentage of its children are ready, dividing by `count`. It runs without controller-runtime's panic recovery, so a `count` of 0 ends the process after the toy converged. Every restart reconciles the same spec and exits again | crash loop | G4 |
| B13 | Never runs its cleanup, so a deleted Widget keeps its finalizer and its children for good | stuck-finalizer | G3 |
| B14 | Does not watch the ConfigMap `--label-from` names, so a changed label reaches no child until something else reconciles the Widget | unobserved-state | G5 (after an `UpdateFixture` and a `Restart`) |
| B15 | Names each child `widget-<i>`, after the kind, rather than `<widget>-<i>`. A second Widget finds its children owned by the first, and its every reconcile fails before it writes a status. The sample is named `widget`, so a run of one Widget cannot tell | name collision | G4 |

Three of the classes are Sieve's bug patterns (§13): intermediate-state, stale-state,
and unobserved-state. The other classes are this repo's own.

The matrix runs one `b<id>.json` sequence per bug, under that bug and again against the
toy with no bug, where no check may fire. No sequence carries a fault (§10, M3), so
B11's row scales down and back up: the belief outlives the child the toy itself deleted.
The unconfirmed write needs a fault, and §10 M6's acceptance test runs `b11-fault.json`
for it. B11 is the deterministic form of §5.6's "a transient state is made permanent by a
`Fault`", so that settle wait expires whatever the windows are. A `Restart` heals B11,
because the belief lives in the process. B12's row creates the Widget at a count of 2,
then sets 0 and settles once more. An exit before a settle wait has converged is a harness
error (§5.5), and a restart that exits again more than `T_stable` after its first request
looks converged to the wait it lands in. B14's row changes the label and then restarts the
toy, which reconciles the Widget. B15's row creates a second Widget, then deletes a child,
restarts the toy and deletes the second Widget, which the toy with no bug passes.
`fault.json`, the fault example of `docs/targets.md`, holds a fault that outlasts the
sequence. The envtest tier runs it, not the matrix: the toy with no bug recovers once the
teardown clears the fault, and B11 fails G4 there.

Acceptance for M3: a matrix in `docs/bug-matrix.md` showing which invariant or property
catches each bug, generated by CI, with no empty rows and no check firing on any sequence
against the toy with no bug. The README links to it.

## 10. Milestones

Each milestone ends with something runnable and a journal entry (§12). A milestone may
span several PRs.

**M0 — Scaffold.** Deliverables:

- Go module `github.com/rosenhouse-bot-sandbox/reconciler-fuzzer` on Go 1.26.
- `LICENSE` (Apache-2.0).
- `Makefile` with `setup`, `test`, `test-envtest`, `fmt`, `vet`.
- `setup-envtest` pinned through `ENVTEST_K8S_VERSION` and `ENVTEST_INDEX_URL`.
- `internal/cluster` starting and stopping an envtest control plane, with a unit test and an
  envtest-tagged smoke test that reads the server version, so the envtest tier is not
  empty.
- CI on PR running the unit and envtest tiers.
- A `.claude/settings.json` SessionStart hook that runs `make setup`, so a Claude Code
  web session can run envtest without manual steps.
- `README.md` in the §11 shape with a status line.
- `docs/journal.md`.
- One skill, `add-invariant`, documenting the procedure used from M3 on.

Acceptance: `make test-envtest` passes both in CI and in a fresh web session.

**M1 — Toy target.** `Widget` CRD and controller built as `bin/toy-widget` with `--bug`
and B1–B10 implemented behind it; `targets/toy-widget/target.yaml`. Plain envtest tests
for the happy path. Acceptance: `--bug=0` passes happy-path tests; each `--bug=N` is
reachable.

**M2 — Proxy, Observer, Binary launcher.** Reverse proxy with request log and streaming
watches; Observer with version history; `Binary` launcher; `target.yaml` loader with CEL
`ready` and properties; `internal/cluster` extended with garbage-collector emulation.
Acceptance: an envtest test starts the cluster, the proxy and the toy binary, creates a
Widget from its sample, and asserts that the request log shows the reconcile and the
Observer shows the ConfigMaps.

**M3 — Generic invariants and Runner.** G1–G6 and declared properties implemented as pure
functions with tests on recorded fixtures. Runner executes hand-written JSON sequences
with `Create`, `Update`, `Delete`, `Recreate`, `Settle`, `Restart` and `DeleteManaged`
(the `Restart` op is a launcher call, so it lands here; faults do not). `reconciler-fuzzer replay`.
Acceptance: every seeded bug of §9.1 with a fault-free sequence is caught by the
invariant or property that section names, and no check fires on that sequence against the
toy with no bug; `docs/bug-matrix.md` is generated by CI.

**M4 — Adoption: cert-manager.** `examples/cert-manager/` with `target.yaml`,
`crds/cert-manager.crds.yaml` (the release asset for the pinned version; the in-tree
`deploy/crds/*.yaml` are development-only), `issuer.yaml`, `certificate.yaml`,
hand-written sequences, `quickstart.sh`, and a `Makefile` that obtains the controller by
shallow-cloning the pinned tag and running `go build` (about 2 s to clone and 90 s to
build cold; cached in CI). On every PR, `make test-example` runs `quickstart.sh`, which
must pass, and then a negative control with `--enable-certificate-owner-ref=false`, which
must fail G3 naming the retained Secret. The negative control proves the harness observes
the target instead of passing vacuously. `README.md` is rewritten usage-first per §11 and
embeds `quickstart.sh`. Acceptance: the example job is green; the embedded quickstart is
byte-identical to the script, enforced by the embed test; the journal records what an
adopter has to supply.

**M5 — Generation and shrinking.** rapid-driven sequences; schema-driven mutation from
CRD OpenAPI with `generate.mutate` and `generate.overlay`; sequence-level shrinker;
`reconciler-fuzzer run --runs --seed`. Acceptance: with `--bug=B2`, the harness finds and shrinks a
failure to ≤ 3 ops without a hand-written sequence; the cert-manager example switches to
generated runs, with fixed seeds on PRs and random seeds nightly.

**M6 — Faults and report.** FaultSpec injection (Error, Delay, Drop); `report.json` and
`report.md`. Acceptance: a fault makes the toy fail an invariant it passes without the
fault, and its report names the invariant, the minimized sequence and the evidence.

**M7 — Adoption: external-secrets.** `examples/external-secrets/` with `target.yaml`,
`crds/external-secrets.yaml` (the release asset for the pinned version, a whole install
manifest of which envtest keeps the 25 CRDs), `secretstore.yaml`, `externalsecret.yaml`,
hand-written sequences, `quickstart.sh`, and a `Makefile` that obtains the controller by
shallow-cloning the pinned tag and running `go build -tags fake` at the repository root,
where its main package lives (about 2 s to clone and 150 s to build cold; cached in CI).
The second adoption is against a controller unlike the first: it binds every port
ephemerally, so its runs may overlap; it carries no `observedGeneration`, so readiness
reads a version string; and no flag makes it orphan the Secret it manages, so the
negative control is a sequence whose CR sets `spec.target.creationPolicy: Orphan`. On
every PR, `make test-example-external-secrets` runs `quickstart.sh` and the pinned
sequences, which must pass, and then that control, which must fail G3. Acceptance:
`make test-example-external-secrets` passes on a clean checkout, the orphan control fails
G3 naming the Secret, and both are green in CI.

**M8 (phase 2) — Multi-cluster target.** A multi-cluster sync controller as `Binary`
target, forcing two-API-server envtest and cross-cluster faults; watch-event dropping in
the proxy; the `Image` launcher. Separate design addendum.

## 11. Repo conventions

- **Language and pins.** Go `1.26.0` in `go.mod` (cert-manager v1.21.2 and the
  Kubernetes 0.37 libraries require it); CI uses `go-version-file: go.mod`. Library pins
  live in `go.mod` only: `k8s.io/{api,apimachinery,client-go,apiextensions-apiserver,apiserver}`
  v0.37.x,
  `sigs.k8s.io/controller-runtime` v0.25.x, `pgregory.net/rapid` v1.3.x,
  `github.com/google/cel-go` v0.30.x. Tool and target pins live in one Makefile variable
  each: `ENVTEST_K8S_VERSION`, `SETUP_ENVTEST_VERSION`, `CONTROLLER_GEN_VERSION` (which
  also pins the envtest release index), `KIND_VERSION`, `KIND_NODE_IMAGE` (by digest),
  and one trio per adopted example:
  `CERT_MANAGER_VERSION` and `EXTERNAL_SECRETS_VERSION`, each with the `_COMMIT` the tag
  must name and the `_CRDS_SHA256` of its checked-in CRDs, so a moved tag or an edited
  asset fails rather than passing quietly. Values live in the Makefile. The README's Install
  block and the CI recipe repeat the envtest pins for adopters to copy. The recipe also
  repeats go.mod's module and Go version, and the runner and action releases of
  `.github/workflows/`. The README's Install section and the tools module section of
  `docs/ci.md` quote go.mod's Go version and the k8s.io/api and controller-runtime versions
  that requiring reconciler-fuzzer forces on a module (D80).
  `make test` holds each copy to its source. Each `--deadline` in the
  recipe gives a run at least the time that the Makefile's example tiers give one. Bumps
  are their own PRs, never mixed with features.
- **controller-runtime boundary.** Only `targets/toy-widget/` and `internal/cluster` may
  import it. The rule covers the root module; the spike modules under `docs/spikes/` are
  separate and exempt. Everything else uses client-go and apimachinery.
- **Layout.** `cmd/reconciler-fuzzer/`, `internal/cluster`, `internal/proxy`, `internal/observe`,
  `internal/invariant`, `internal/generate`, `internal/run`, `internal/report`, `internal/target`,
  `targets/toy-widget/`, `examples/cert-manager/`, `examples/external-secrets/`,
  `examples/ci/`, `docs/`, `internal/reference` for the tests that read
  `docs/reference.md`, and `bin/` for git-ignored build output.
- **CLI.** reconciler-fuzzer has these commands. A test holds this block to the flags each command
  parses.

  ```
  reconciler-fuzzer run --target file [--deadline duration] [--junit file] [--kubeconfig file] [--launch-arg arg]... [--no-baseline] [--out dir] [--runs n] [--seed n] [sequence.json...]
  reconciler-fuzzer replay --target file [--deadline duration] [--junit file] [--kubeconfig file] [--launch-arg arg]... [--out dir] sequence.json
  reconciler-fuzzer version
  reconciler-fuzzer matrix --target file --sequences dir [--deadline duration] [--kubeconfig file] [--launch-arg arg]... [--out file]
  ```

  `reconciler-fuzzer matrix` generates the toy's bug matrix for `make bug-matrix`, so the top-level
  help leaves it out. `reconciler-fuzzer help` and `reconciler-fuzzer --help` print what reconciler-fuzzer does, its
  commands, the exit codes, `KUBEBUILDER_ASSETS`, the README's URL and the pages that list
  every key and explain a failure, because `go install` ships no documentation.
  `reconciler-fuzzer <command> --help` and `reconciler-fuzzer help <command>` print the command's synopsis and
  each flag with its meaning and default. `-h`, `--h`, `-help` or `--help` after a sequence
  file asks for help too. Help goes to stdout and exits 0. A bare `reconciler-fuzzer` prints reconciler-fuzzer's
  help to stderr and exits 2. A usage error, such as an unknown flag, a missing required
  flag, another flag or `--` after a sequence file or a wrong count of sequence files,
  exits 2 and prints the error and the command's synopsis to stderr. It spells a flag with
  two dashes, as the help does, but quotes a flag after a sequence file as given. No
  message reconciler-fuzzer prints cites this document or uses its symbols, such as `T_settle`. A
  message names the target.yaml key and its value instead, as in `2s (timeouts.stable)`. A
  test scans the code's string literals for them.
  `reconciler-fuzzer run` draws its sequences or runs the ones named, never both, since `--runs`
  says how many to draw and `--no-baseline` leaves out the baseline after them. The
  deadline abandons the run under way (§5.5), and the shrinker stops there and reports
  the smallest failing sequence it found. Without
  `--deadline`, reconciler-fuzzer prints and uses the longest the planned runs' waits can take at the
  target's timeouts, plus 4m to minimize a failure where reconciler-fuzzer drew the sequences
  (D64). A shrink pass can take minutes, so reconciler-fuzzer prints the failed check's ID before it
  minimizes a drawn sequence of more than one op. `--launch-arg` appends to `launch.args`
  (repeatable; a later flag wins), which is how the bug matrix selects `--bug=N`.
  `--kubeconfig` selects an existing cluster instead of envtest and installs the target's
  CRDs there (§5.8); `KUBEBUILDER_ASSETS` locates the envtest binaries. Exit codes: 0, all runs
  passed; 1, an invariant or property failed and a report was written; 2, configuration or
  harness error, or a deadline that ended a run or stopped the invocation before its last
  run. A deadline that ends minimization leaves the violation, and exit 1, standing.
  SIGINT, SIGTERM and SIGHUP interrupt the invocation. No further run starts, and the run
  under way is abandoned (§5.5). `reconciler-fuzzer run` and `reconciler-fuzzer replay` name its directory, and
  `reconciler-fuzzer matrix` names its row. A run that failed before the interrupt reports its own
  error. reconciler-fuzzer ignores SIGPIPE from then on, stops what it started, and then dies of the
  signal, so a shell reports 128 plus the signal's number. A second signal kills reconciler-fuzzer at
  once. A SIGHUP or SIGINT that reconciler-fuzzer was started ignoring, as under `nohup`, stays
  ignored.
- **Output.** `--out` defaults to `reconciler-fuzzer-out/`. Each invocation writes
  `<out>/<timestamp>-<seed>/`, taking the next free name where a second invocation of one
  seed opens a directory in the same second. Each failing run writes `run-<n>/` under it
  with `report.json`, `report.md`, `sequence.json`, `requests.jsonl`, `objects.jsonl`,
  `target.log`, the `kubeconfig` the target was given and, on envtest, `collector.jsonl`,
  plus `sequence.shrunk.json` where the deadline or an interrupt ended the shrink pass
  before its result could be run there. The `kubeconfig` names the proxy and the run
  namespace. A passing run's recordings are not kept.
  Once `run` or `replay` has read or drawn its sequences, it writes `summary.json` and
  `summary.md` into the invocation's directory. It writes them again as each run starts,
  once a run finds a violation, before it runs a minimized sequence again, and when the
  invocation ends, before it stops the cluster. Until then, the invocation and the run
  under way are `unfinished`, and the summary gives no exit code, so a SIGKILL or a crash
  leaves the runs that finished. An interrupt that arrives while the cluster stops
  rewrites them, since reconciler-fuzzer then dies of it. They give reconciler-fuzzer's version, the target, the
  seed, the `--launch-arg` values, the cluster, the deadline, the outcome (`passed`,
  `violation`, `error`, `interrupted` or `unfinished`), the exit code, and what stopped
  the invocation where no run did. Each planned run has its seed, its sequence file if it
  had one, its outcome (those five or `not run`), its duration, its ops, how many it
  applied, its ops by type, how many fault ops the proxy applied and how many requests it
  faulted, its checkpoints, the target's exits, its notes and its sequence (§7). These
  describe the run of the planned sequence. A failing run also has its `run-<n>/`,
  relative to the summary, and its error, or the violation and notes its report carries.
  Until reconciler-fuzzer writes the report, those are the run's own, and `run-<n>/` holds no report.
  reconciler-fuzzer empties `run-<n>/` before it runs the minimized sequence there, and meanwhile
  `summary.md` and the JUnit file say `run-<n>/` holds a partial run of it (D72).
  `summary.md` leaves out the ops by type, the checkpoints, what each exit said and the
  sequences. `summary.json` carries `schema: 2`, which changes when a field changes
  meaning or goes away. `--junit FILE` writes the runs as JUnit XML with the summary,
  creating the file's directory: one testsuite, a testcase per planned run, a `failure`
  typed with the check's ID for a violation, an `error` for a run that did not finish,
  `skipped` for a run that never started, and an `error` testcase named `reconciler-fuzzer` for what
  stopped the invocation where no run did, or for an invocation that has not finished. The
  testsuite of an invocation that has not finished gives no time. An invocation that stops
  before it has a directory writes that testcase alone.
  reconciler-fuzzer replaces each file whole, and warns of one it cannot write.
  `objects.jsonl` writes each value of a Secret's `data` and annotations as a marker such
  as `[redacted 6 bytes hmac-sha256:8c7ef51307f40278]`. The HMAC key is drawn per
  invocation and never written, so equal values share a marker within one invocation and a
  marker reveals only the value's length. The Observer's history keeps the values, so G5
  compares them exactly, and its report quotes the markers. Nothing else is redacted: a
  Secret's labels, every other object, `target.log`, `sequence.json`, the summary's
  sequences, and a report's sequence and replay command hold what the target, the sample
  and the command line gave them (D49). `reconciler-fuzzer matrix` writes each run to a directory
  under the system's temporary directory and deletes it once the checks have read it. A
  run that errs keeps its directory, and the error names it and the command that replays
  the run. CI uploads that directory.
- **Test tiers.** `make test` = unit, no API server. `make test-envtest` = envtest, under
  10 minutes on CI. `make test-example` and `make test-example-external-secrets` = the two
  adopted examples under envtest, each under 12 minutes on CI including obtaining the
  binary (cached). All four run on every PR. The `-nightly` target beside each example
  runs it on seeds reconciler-fuzzer draws, with the negative control. `make test-kind` = the toy
  through `--kubeconfig` against a kind cluster it creates and deletes, on demand. It
  passes `b0.json` and fixed seeds, and fails B3 on G3 and B8 on G7 as its negative
  controls. It installs the pinned kind into `bin/` and needs Docker. The nightly workflow
  runs the same runs with `make test-kind-runs`. The `must-pass` Makefile macro runs a
  command and exits make with the command's own exit code, so the nightly workflow can
  read `summary.json` to tell a find (exit 1) from a harness error (exit 2). Each
  quickstart exits 2 on its own failures (D93). `make hunt-cert-manager` and
  `make hunt-external-secrets` hunt for bugs in the adopted examples, on demand and on no
  pull request. Each runs every family in `examples/<example>/sequences/hunt/`, then up to
  `HUNT_RUNS` seeds drawn from `HUNT_SEED` on, each in an invocation of its own, until
  `HUNT_MINUTES` runs out. Seeds leave out the baseline once an invocation has passed it. Each invocation writes under `reconciler-fuzzer-out/hunt-<example>/`. A
  family is checked in only once it passes the pinned controller.
- **Triage.** A hunt run that fails is a candidate, not a bug. Triage replays it three
  times, reproduces it by hand against envtest unless only reconciler-fuzzer's proxy can inject its
  faults, and reads upstream's code path. It searches upstream's tracker where the
  maintainer allows that. A candidate that no replay reproduces, or that breaks a reconciler-fuzzer
  rule and no upstream contract, becomes a reconciler-fuzzer issue. A candidate that breaks an
  upstream contract becomes a draft under `docs/findings/`, with how many replays failed,
  for reconciler-fuzzer's maintainer to file upstream. The draft's directory holds its
  `sequence.json`, which `make test` checks against the example it names and its CRD,
  and the by-hand reproducer where there is one. One that upstream's tracker already
  holds needs no draft. A draft says whether triage searched the tracker. Where it did
  not, the maintainer searches before filing.
- **Network assumptions.** Every tier below kind reaches only `proxy.golang.org`,
  `sum.golang.org`, `github.com`, `raw.githubusercontent.com` and GitHub's release-asset
  hosts (`*.githubusercontent.com`). No tier assumes a container registry: the Claude Code
  web sandbox cannot reach `quay.io`, and the agent must be able to run every PR tier
  locally. External targets are obtained by shallow git clone at a tag plus `go build`, or
  as a GitHub release asset, and are pinned.
- **Lint.** `gofmt` and `go vet` run in CI. golangci-lint may be added in its own PR.
- **Comments.** A Go comment outside tests makes sense without this document open. It
  cites no section or decision and uses none of this document's symbols. It names the
  target.yaml key instead, as a message does. A test scans every such comment, the
  spikes' included. Tests may cite this document, since they hold the code to it (D82).
- **README.** Usage-first and short; internals live here and in `docs/`. Order: what reconciler-fuzzer
  is, in a few sentences, which say that it runs the controller against a real
  kube-apiserver and etcd; its limitations; install; a quick start, which draws runs
  on the toy and replays a bug planted in it, and runs as written in an envtest test, with
  the reconciler-fuzzer binary and the control plane that install leaves; writing `target.yaml` for your own
  controller, around the toy's `target.yaml` embedded as the worked example, with how to
  run it, a `ready` for a CR that reports a Ready condition, which `make test` evaluates,
  and the baseline, which the envtest tier runs against B7 and B8 (D97);
  reading a failure, with links to `docs/checks.md` and to what to change when
  reconciler-fuzzer exits 2; running in CI, in brief; a closing "Development and internals"
  section that links to this document, its §6, and `docs/bug-matrix.md`.
  Detail lives in pages the README links (D83, D95): `docs/reference.md` lists every key
  and field, `docs/targets.md` says how to write a target, `docs/checks.md` gives each
  check of §6 a section that says what it requires, what usually fails it and what its
  message means, `docs/failures.md` says what each file of a failure holds and what to
  change when reconciler-fuzzer exits 2, `docs/examples.md` runs the adopted examples and
  their negative controls, and `docs/ci.md` holds the CI recipe, embedded from
  `examples/ci/github-actions.yml`, a tools module that keeps reconciler-fuzzer out of an operator's
  go.mod, embedded from `examples/tools-module.sh`, which the envtest tier runs (D80), and
  a test that runs reconciler-fuzzer from `go test`, embedded from `targets/toy-widget/reconciler_fuzzer_test.go`,
  which the envtest tier runs (D81). Only the README's closing section links here, or
  cites a section, a decision or a symbol of this document. The guide pages cite none.
  None of them names a milestone. Every sequence they show loads, every link among them
  and `docs/reference.md` lands on a file and a heading. Each check that they or
  `docs/reference.md` name outside code and headings links its section of `docs/checks.md`,
  and each section quotes words its violation prints. `make test` enforces these rules. The limits
  section is a list of `- ` bullets, one per limit, with their other lines indented two
  spaces, and nothing follows the list. A bullet for a limit an issue tracks links the
  issue. Each line's text begins with a letter, `[` or `(`. The section holds no HTML,
  footnote, link definition, code or backslash, so nothing hides a limit or its link. A
  test refuses any other line, lists the limits, holds each bullet to a listed limit, and
  holds the README and this document to each. A reviewer checks that no bullet states an
  unlisted limit and that each linked issue is open (§12). A fenced block
  preceded by `<!-- embed: <path> -->` has content, excluding the two fence lines,
  byte-identical to that file including its trailing newline; `<path>` is relative to the
  repository root; `make test` enforces it. It also runs the cert-manager quickstart
  command of `docs/examples.md` against a fake session and requires the block after it to
  hold what reconciler-fuzzer prints.
- **PRs.** Every PR description, issue, review and comment a Claude session posts begins
  with the line `🤖 Created by Claude 🤖` (CLAUDE.md). The description then names the
  milestone and the invariant/property IDs it touches, and carries a "Design change"
  section whenever it edits this document.
- **Reference.** `docs/reference.md` gives every target.yaml key, sequence field, op and
  fault field a row with its meaning, and each key its default. Tests hold the rows to the
  code. The page embeds examples that set every key but `equal`, and every op and field.
  The toy must pass the example sequence with each fault applied and no check left
  unjudged.
- **No flaky-test retries in CI.** A flaky harness test is a P0 bug in the harness.
- **Seeds are always printed.** Every failure is reproducible from its sequence, and from
  its seed with the same reconciler-fuzzer build and target declaration (§5.4).

## 12. How agents work in this repo

- **Coding agent (Claude Code):** implements one milestone sub-task per PR. Reads
  this document first. Works red/green: a failing test first, then the minimum code
  (CLAUDE.md). Does not change invariant definitions without a PR that edits §6 in the
  same change.
- **Reviewers (subagents):** before claiming a change is done, the coding agent spawns
  at least one adversarial reviewer with fresh context, using the personas CLAUDE.md
  lists. A reviewer reads the diff against §6, §8 and §11, citing the section it applies,
  and it may run the code: start a cluster, drive the binary, mutate a function and check
  that a test dies. It flags any import of controller-runtime outside the two places §11
  allows, a README embed block that differs from its file, a limit the README still states
  though the PR lifts it or its issue is closed, a README bullet that states a limit the
  test does not list, a post whose first line is not `🤖 Created by Claude 🤖`, and a
  PR description that lacks the milestone, the IDs, or the "Design change" section when
  this document changed. Reviewers never merge.
- **Journal:** `docs/journal.md`, one entry per milestone, recording what the agents
  got right, what they got wrong, and which prompt, skill, or convention change fixed
  it. This is a first-class deliverable of the repo.

**Autonomy.** Decided with the maintainer on 2026-09-20 (§15, D15, D16, D24):

- The coding agent decides alone: package internals within the §11 layout; names; test
  structure; windows within the §6 defaults; CLI flags consistent with §11; patch and
  minor dependency bumps; README wording; journal entries; how a milestone splits into
  PRs.
- When this document is ambiguous or wrong, the coding agent proceeds under a stated
  assumption and amends this document in the same PR under a "Design change" heading.
  This includes invariant statements in §6.
- The coding agent stops and asks before: changing the license, the module path or other
  public names; editing `CLAUDE.md`; changing CI secrets or permissions; publishing a
  release; replacing an adoption example.
- **Merging.** The agent merges its own PR once CI is green on the head commit, with a
  squash merge. Before it merges, the PR description must record the adversarial reviews
  it ran, what they found and how each finding was addressed, so the trail is auditable
  after the fact. An unaddressed blocking finding stops the merge. It never force-pushes
  a shared branch.
- **Continuation.** Work does not wait for a human. A scheduled Routine starts a fresh
  session every hour. Each session reads this document and the journal, then looks for an
  open Claude PR. If one exists and another session touched it within the last two hours,
  the new session leaves that PR alone and starts the next independent sub-task on its own
  branch; it stands down only when no independent sub-task exists. Otherwise it drives
  that PR to merge, then takes the next incomplete milestone sub-task, and keeps going
  until the milestone is done or its context is spent.
- **Economy.** Spend is acceptable when it is well spent. A session delegates bounded
  sub-tasks and every adversarial review to subagents with fresh context, picks a cheaper
  model where the task allows (Sonnet for mechanical work, Opus for design-heavy work),
  and keeps its own coordinating context small.
- **Fallback.** cert-manager and external-secrets are both adopted (§10, M4 and M7), so
  an example that stops working under envtest for a reason on its own side leaves the
  other standing. The agent records the blocker in the journal and asks before dropping
  either.

## 13. Prior art

- Anvil / Welder (UIUC) verify controller liveness formally. ESR is the property G4
  approximates.
- Acto (UIUC, SOSP'23) tests operators from the CRD schema. It is the source of the
  generation approach and the oracle ideas.
- Sieve (UIUC, OSDI'22) injects controller faults by instrumenting the controller, and
  names the intermediate-state, stale-state and unobserved-state classes used in §9.1.
  `reconciler-fuzzer` injects at the API boundary instead.
- rapid gives Go property-based testing with shrinking.
- envtest, kind and kwok provide test control planes.

## 14. Open questions

1. Does G2 need a per-target exemption for a status field that a controller rewrites
   with a new value on a timer, such as a heartbeat? `N_quiet` admits writes that change
   nothing (D58), and a write that moves a resourceVersion is churn. external-secrets
   under `refreshPolicy: Periodic` writes such a field, and D40 answered it with a
   target-side setting. The question stands for a controller that offers no such
   setting.
2. How is a cluster-scoped primary CR (ClusterIssuer-like), managed kind or fixture
   isolated per run? Should reconciler-fuzzer observe a child the target creates in another
   namespace?
3. Should a later phase run the target's admission webhook in envtest, so that generation
   can widen beyond `generate.mutate`?
4. Is `InProcess` worth reviving for speed once envtest run time is measured?
5. G5 takes an op on one CR to change only that CR and what it owns (§6). Should it judge
   less for a controller whose CRs refer to one another, such as one CR delegating to
   another of its kind?
6. Should a target declare its RBAC and run under it (§5.2)? The proxy could send a token
   that reconciler-fuzzer requests for a ServiceAccount of the run, bound to the target's Roles and
   ClusterRoles. On envtest 1.37, such a token was refused a verb and a resource its Role
   lacked, another namespace, and an impersonation of `system:masters`.
7. What lets the README show a find in a real controller as a bug reconciler-fuzzer found: upstream
   acknowledging it, or a deterministic replay that a reading of upstream's code confirms?

## 15. Decision log

D1–D17 were taken on 2026-09-20 with the
maintainer and rest on a spike in this sandbox, written up in
`docs/spikes/2026-09-20-cert-manager-envtest.md`: envtest 1.37.0, cert-manager v1.21.2
built from source and run as a black-box binary.

- **D1 Binary is the primary launcher; InProcess is deferred.** One code path from the
  toy target onward, real crash restarts, and no controller-runtime in the harness.
- **D2 target.yaml is canonical.** The Go interface is the loaded form; hooks are
  optional and in-repo only.
- **D3 CEL for `ready` and for properties.** cert-manager's Certificate carries
  `observedGeneration` only inside conditions, so readiness needs a cross-field comparison
  JSONPath cannot express.
- **D4 envtest is the default test cluster and reconciler-fuzzer owns it.** `internal/cluster` may
  import `controller-runtime/pkg/envtest`; re-implementing envtest would be waste.
- **D5 Garbage-collector emulation and self-cleanup on envtest.** Spike: 10 s after a
  Certificate was deleted, its Secret and CertificateRequest were still present despite
  ownerReferences, and the namespace stayed `Terminating`.
- **D6 Attribution by namespace.** Everything in the run namespace that reconciler-fuzzer or a
  fixture did not create is the target's. Amended by D56 for a cluster with a controller
  manager.
- **D7 G2 covers the set of managed objects; G5 is measured within one run.** G2 as
  first written missed new objects appearing (B2). G5 as first written compared two runs,
  which random `generateName` suffixes make incomparable.
- **D8 `primary`, `sample`, `fixtures`, `generate.mutate` and `generate.overlay` join the
  target contract.** A Certificate needs an Issuer to exist first and a valid `issuerRef`
  no schema can invent; webhook-only rules need an allowlist.
- **D9 The cert-manager adoption milestone is M4, before generation.** Adoptability
  problems surface while the black-box contract is still cheap to change.
- **D10 External targets are obtained by shallow clone plus `go build`.** cert-manager's
  `cmd/controller` is a nested Go module with a `replace` directive and no separate tag,
  so `go install` refuses it; no standalone binary is published; the sandbox cannot reach
  `quay.io`.
- **D11 Pins.** Go 1.26.0; Kubernetes libraries 0.37.0; envtest 1.37.0; controller-runtime
  0.25.1; rapid 1.3.0; cel-go 0.30.0; cert-manager v1.21.2. All current on the decision
  date.
- **D12 The `Restart` op lands in M3.** With the Binary launcher it is one call.
- **D13 No metrics scraping; `DeleteManaged` before watch-stream filtering; namespaced
  primary CRs only.** Scraping controller-runtime metrics would break the black box.
- **D14 B2 trips G1 and G2, not G3; B9 omits ownerReferences as well as ordering the
  finalizer wrongly.** Garbage collection removes owned duplicates and owned children, so
  G3 cannot see either bug otherwise.
- **D15 The agent merges its own PR once CI is green on the head commit and the PR
  records the adversarial reviews it ran; it amends this document in the same PR when
  needed; a human is asked only for the items listed in §12.** Chosen by the maintainer
  for unattended progress, and amended by D24.
- **D16 License Apache-2.0; continuation by scheduled sessions, continuous rather than
  throttled.** Chosen by the maintainer, with the instruction that spend is fine when well
  spent: subagents with fresh context and cheaper models where the task allows.
- **D17 The cert-manager example ships a negative control.** With
  `--enable-certificate-owner-ref=false` the Secret is retained by design (cert-manager
  documents this), and reconciler-fuzzer must report it as G3 because the target declares Secrets as
  managed.
- **D18 `ENVTEST_INDEX_URL` pins the setup-envtest release index to a tagged
  controller-tools ref.** The default index tracks `HEAD`, so a pinned
  `ENVTEST_K8S_VERSION` alone does not make asset resolution reproducible.
- **D19 Properties are declarable in `target.yaml`.** Invariants are eventual and cannot
  see a transient state such as B1, so the toy needs P1 to catch it.
- **D20 G2 counts no-op status writes from the proxy log.** The API server short-circuits
  an update whose bytes are unchanged, so resourceVersion does not move and the Observer
  sees nothing; B6 is restated as a write that does change content.
- **D21 Equality is the §6 default plus `equalIgnore`, not a CEL expression.** A
  CEL expression would have to re-implement the per-path exemptions a path list states
  directly.
- **D22 The toy's P1 is evaluated at checkpoints.** Evaluated on every Observer event, a
  `DeleteManaged` op makes every controller violate it during its reaction time.
- **D23 A settle wait ends on quiescence, G2 covers the primary CR, thresholds are
  per target, the collector ignores ownerless objects, and `--launch-arg` selects a bug.**
  Without these, the correct controller fails P1 after `DeleteManaged`, B6 escapes G2, B5
  escapes G6 under controller-runtime's backoff, B3 and B9 lose their evidence, and the
  bug matrix needs ten target files.
- **D24 Every Claude GitHub workflow is removed, and the merge gate rests on
  the adversarial reviews instead.** They ran eleven times across two PRs and posted
  nothing, because the repository holds no API secret, leaving a red check on every
  push. A reviewer that only reads a diff is also weaker than one that starts a
  cluster and mutates the code, which is where every finding so far came from. The
  `@claude` mention workflow went with them, so nothing in CI needs an API secret.
- **D25 No invariant window reaches past the teardown boundary, and a CR under deletion
  need not be `Ready`.** The rule was in the code for G1, G2 and G6 and in no document.
  G4 and `always` properties lacked it, so the teardown's own delete made a correct
  target read as unconverged: one bug-matrix row flapped between runs.
- **D26 G1's and G2's quiet window follows the settle wait, not the op.** The window
  `[T_settle, T_settle + T_stable]` after the op closes past the teardown boundary
  whenever the target converges in less than `T_settle`, so neither invariant was judged
  for a run that converged: the cert-manager example evaluated neither. The window now
  opens where the settle wait ended, converged or expired, which is what G2 already said
  in English. B1 gains G1 and G2 in the bug matrix, because it reports its children ready,
  is judged converged on that, and only then creates them.
- **D27 G6 ignores a 409 Conflict on a write and counts a failing watch.** Six
  cert-manager controllers write one Certificate's status, which conflicted nine times in
  one run against `N_errloop` 20. A conflict is the API server telling the target to
  re-read, so counting it would fail a correct target. A watch is the other way round: G1
  ignores one because a watch that hangs is the target waiting, while a watch that fails
  returns at once and repeating it is a loop. envtest's flow control turned three watches
  away with 429 in the same run, well under the threshold.
- **D28 cert-manager's healthz port moves only under a hidden flag, so runs stay
  sequential.** `--internal-healthz-listen-address` exists, and upstream hides it to
  discourage overriding it and records that it may be renamed or removed. The spike tried
  `--healthz-listen-address`, got "unknown flag", and read that as no flag at all. reconciler-fuzzer
  does not build on a flag upstream discourages.
- **D29 G6 ignores a 409 only on an `update` or a `patch`.** D27 excluded every write.
  A create that collides returns AlreadyExists, which is not a lost race: the object is
  there, and a controller that re-creates a fixed-name object forever is the error loop
  G6 exists to catch. Thirty identical failing creates in 1.5 s reported nothing. A patch
  keeps the exclusion: a server-side-apply conflict answers 409 as well, and the proxy
  does not keep the response body that would tell the two apart.
- **D30 G1 ignores every lease request, not only a lease write, and every request that
  names no resource.** §6 excluded all lease traffic and the code excluded only writes,
  so one leader-election read inside the quiet window failed a correct target, and leader
  election is on by default in nearly every controller. A health probe, a discovery read
  and the RESTMapper refresh that follows one are not reconciliation either; G6 still
  counts them, because a failing request repeated forever is a loop wherever it points.
- **D31 A clean run namespace satisfies G3, and a check that judges nothing leaves a
  note.** The teardown stops watching as soon as the namespace empties, which is always
  before `deletion + T_delete`, so G3 was skipped in every configuration that passed: the
  example's `issue.json` was observed to 31.3 s against a 71.3 s deadline, and only the
  negative control ever reached a verdict. Emptiness decides the deletion at the instant
  it is seen. G3 and G5 now record what they could not judge, the Runner carries the last
  checkpoint's notes out, and `reconciler-fuzzer` prints them, because a skipped check reads like a
  passing one from outside.
- **D32 The teardown's `T_stable` is a quiet window of its own, and G1's statement
  follows the settle wait.** §5.5 step 4 already called that wait the last quiet window
  and nothing implemented it, so a sequence ending in a `noSettle`, a `restart` or a
  `fault` was judged on no window at all. A fault that outlives the last op takes that
  op's window with it, and the teardown clears every fault as its own window opens, so
  that shape is judged now too. The teardown does not settle before it waits, so a
  sequence that ends while the target is working is judged on that work; every sequence
  here ends with an op that settles, and a teardown that settles first, which would also
  give §6's "within `T_settle` after faults stop" somewhere to be measured, waits for M6.
  G1's row promised `T_settle` to fall quiet while the code judged the `T_stable` after
  the settle wait, which is shorter whenever the target converges early. The statement
  now says what the code does and what G2 already said: the settle wait is where reconciler-fuzzer
  judges the target converged, and B1 keeps the G1 row D26 gave it, because it reports
  itself converged and only then creates its children.
- **D33 A sequence ends with an op that settles, and generation adds the settle waits
  that leave the ops it drew judged.** D32 left this a statement about the sequences
  in the repo, and the generator of M5 then drew sequences ending in a `noSettle`, a
  `restart` or nothing at all: roughly half of them, each judged on a window that opens
  while the target is still working, so a correct target failed G4. `Sequence.Validate`
  rejects that shape now, which also keeps the shrink pass from proposing it, and
  generation appends the settle a drawn op needs. A `noSettle` in the middle of a
  sequence stands: skipping that wait is what it is for.

  Generation also wraps every drawn `restart` in settle waits, because G5 compares the
  converged state either side of one, and a change on either side leaves G5 nothing to
  judge (D42). That is a rule for generation, not for the format, because a hand-written
  sequence may mean to restart and change the spec at once, as `b10.json` does.
  `Options.MaxOps` therefore bounds the ops a draw makes, not the sequence's length: at
  most two settles join each drawn op.
- **D34 G3 reads the teardown's own observation, not a checkpoint.** The Runner records a
  teardown checkpoint only for a run that found no violation, and `cleaned` derived G3's
  "the namespace emptied" from that checkpoint, so exactly the runs where a bug fired lost
  it. G3 then fell through to its last case and reported that the run ended before a
  deadline the namespace had come clean well inside: six of the eleven bug matrix rows,
  reading as passes until D31 made notes visible. The teardown now stamps
  `Timeline.Cleaned` from what `awaitClean` saw, and G3 reads that. A run that never came
  clean leaves it zero, which is the case G3 exists to report.
- **D39 A readiness verdict quotes the children in a table of their own.**
  G4's evidence was the primary CR's version history alone, so a finding about a managed
  object left the object out. A count answers the case the child was never created in,
  since an object that does not exist has no version any table can hold, but it says
  nothing about a child that exists and is wrong. A verdict therefore carries two pieces
  of evidence: a timeline, the CR's versions nearest it, and a state, one version of each
  object the target managed at the instant it judged. Each takes a bound of its own, so
  that neither crowds the other out: a CR that writes its status twenty times in the last
  second would otherwise crowd out the child the finding is about, and a target managing
  more objects than the bound would crowd out the CR its statement names. One table of
  both also left a gap in the timeline unexplained, since a reader could not tell which
  rows were a history and which a state (#24). The children are taken one from each kind in
  turn, newest first, so that a bound too small for them drops the oldest of each kind
  rather than every object of the kinds that sort last.
- **D35 A report quotes the evidence nearest the violation, and carries the notes.**
  §5.7 asks a report to quote the request log and the version timeline and does not say
  how much. A run makes thousands of requests, and a report nobody reads is worth
  nothing. The checks already bound what they quote, at the twenty entries nearest the
  violation (`internal/invariant`), which are the ones that explain it; the report quotes what
  it is given and names the file that holds the rest. A violation the Runner raises itself
  quotes the same way, from the request log and the CR history it has in hand. The
  report's own bound is a backstop against a caller that bounds nothing. A violation also
  says how many entries it chose its excerpts from, because a report that counted only
  what it was handed would claim every bounded excerpt was whole. A check keeps a
  timeline's last entries, and the report says so rather than calling them the nearest: a
  violation is stamped where its evidence opens as often as where it closes. A state is
  bounded and counted on its own (D39). Quoted versions leave their object bodies to
  `objects.jsonl`, except that a readiness verdict quotes the CR's status its predicate
  read, bounded (D48), and G5 quotes the value of each field it found changed, bounded
  and counted in the same way (D50). The report also carries what no check could judge,
  for D31's reason: a report that omits "G3 could not be judged" reads like one where G3
  passed, and it is the artefact a human actually reads.
- **D38 G3 credits no cleanup reconciler-fuzzer performed.** A `DeleteManaged` op deletes a managed
  object behind the target's back (§5.4). Inside a CR deletion's window that deletes the
  evidence: G3 asked whether the object was gone by the deadline and never asked who
  removed it, so `b3.json` with one `deleteManaged` op added turned a reported orphan into
  "every run passed". The Runner resolves the op to an object and hands it to the engine.
  G3 notes an object reconciler-fuzzer took inside the window rather than counting it as cleaned. It
  is a note and not a violation, because the target still had until the deadline; an
  object reconciler-fuzzer took after the deadline was still there at the deadline, which G3 already
  reports. The object is matched by UID, since one the run recreated carries the same name
  and belongs to the CR that came after.
- **D37 A forced finalizer is a note, not a reason to withhold G3.** §5.5 step 4 said each
  forced removal invalidates G3 for the run, and nothing implemented it. Implementing it
  literally would have thrown away true findings. The teardown stamps the end of the
  deletion window and takes G3's checkpoint before it forces anything, and no check reads a
  version recorded after that instant, so nothing reconciler-fuzzer forced was ever credited to the
  target. What was missing is visibility: an object no check judges — a fixture, or a child
  born after the deletion — can hold a finalizer while G3 passes, and nothing said so. The
  run notes what it forced.
- **D36 A fault excuses the target over the window the proxy applied it in.** The Runner
  used to open a fault's window where it injected the spec and close it where it dropped
  the spec, and only an op-index trigger made it drop one. A `Count` or `For` trigger runs
  out inside the proxy, so the Runner went on excusing the target for the rest of the run:
  one fault op left G1 to G4 and G6 unjudged from then on, and a fault matching a resource
  the target never touches did the same. The proxy now reports what it did with each
  fault, and the window runs from the first request it faulted to the request or the
  instant the trigger ran out. A fault the proxy applied to no request has no window: it
  changed nothing, so it excuses nothing. This is the vacuity §9.1's control row exists to
  catch, one layer up: a run that reports nothing because nothing was judged.
- **D40 The external-secrets sample sets `refreshPolicy: OnChange`.** Under the CRD's
  `Periodic` the controller rewrites the ExternalSecret's status on every
  `refreshInterval`. Which check reports those writes depends on the interval. At `10s`,
  shorter than `T_stable`, the quiet window never closes: the run fails G4, not G2, with
  "the settle wait after op 0 (create) expired with no fault active", and the request log
  holds the ten status writes that kept it open. At an interval longer than `T_stable`
  the settle wait converges and G2 counts them as churn. `refreshInterval: 0s` stops the
  writes and convergence with them, because the controller then skips a spec change
  unless it renames the target Secret, so readiness never reaches the new generation.
  `OnChange` syncs when the generation, the labels or the annotations change and writes
  nothing in between. It costs coverage: nothing reconciler-fuzzer does reaches the periodic refresh
  path. This is the first real target to raise §14 question 1, and the answer taken here
  is a target-side setting rather than a per-target G2 exemption list.
- **D41 external-secrets' negative control is a sequence, not a launch flag.**
  cert-manager orphans its Secret under `--enable-certificate-owner-ref=false`, which
  `--launch-arg` carries (D17). external-secrets decides ownership per CR, in
  `spec.target.creationPolicy`, so the control is `sequences/orphan.json`, whose create
  sets `Orphan`. The Secret then carries no ownerReference, the collector of §5.8 has
  nothing to resolve, and G3 names the Secret. `deletionPolicy` is not a second control:
  its default `Retain` leaves a Secret that still carries an ownerReference, which the
  collector removes.
- **D42 G5 judges a restart only where reconciler-fuzzer changed nothing between its snapshots.**
  A `restart` does not settle, so in `b10.json` the first converged state after the
  restart follows the update to `count` 1. G5 blamed the restart for that update and
  failed the toy with no bug. G5 notes such a restart instead. A `restart` that settles
  was rejected: it would reverse D33, which lets a hand-written sequence restart and
  change the spec at once, and it would change what replaying a sequence does. G5 loses
  every restart that an op of reconciler-fuzzer's confounds, since it cannot judge those soundly. It
  also loses one next to an update that changes nothing. A fault's window between the
  snapshots confounds a restart too, so G5 leaves that restart unjudged, as every other
  check ignores a fault's window. The shrink pass matches a candidate on the check alone,
  so a candidate without the settle after a restart no longer keeps a G5 the next op
  caused. `b0.json` settles after its restart, so the control row judges G5.
- **D43 The matrix runs every sequence against the toy with no bug too.** Only `b0.json`
  ran against the correct toy, so a sequence that fails a correct controller showed up
  nowhere. Each sequence now runs a second time without its bug, and a check that fires
  there fails the matrix. B0's one run is both. A `?` there does not fail it, because the
  check found nothing: G5 leaves the restart of `b10.json` unjudged. The cost is a second
  run per sequence.
- **D44 After faults stop, a target has as long as they lasted plus `T_settle` to
  recover, and the teardown waits for it.** The README's fault example failed the toy with
  no bug: its fault outlived the sequence, the teardown cleared it and opened the quiet
  window at once, and the toy's next create landed there as G1. B11 never recovers and
  passed it, because nothing judged the time after the fault. With the fault's count at
  10, the correct toy failed G4 2.47 s after the fault stopped, because its wait ended
  `T_settle` after the update. `T_settle` alone is too short: controller-runtime doubles
  the toy's delay from 5 ms, and after a fault that spanned three expired waits its next
  create came 5.4 s after the fault was cleared. A target that doubles its delay retries
  within as long as it has been failing, so G4 gives it that span as well, starting no
  earlier than its last convergence. The Runner's waits and the engine share the rule, and
  the teardown's recovery wait is judged as an op's wait is. This is the teardown that
  settles first that D32 deferred, for faults only.
- **D45 `equalIgnore` paths have a grammar that the loader checks.** A dotted path
  cannot name an annotation key, which holds dots, and the loader accepted a path that
  named nothing. `["key"]` quotes a key as a CEL map index does, and `[*]` follows §6 and
  kubectl's JSONPath. A child created after the operator started carries no annotation
  until a restart stamps one, so an empty map or list along an ignored path counts as
  absent. `generate` keeps dotted schema property names, which the schema already checks.
  A key that meets a list is a note and not a configuration error, because `equalIgnore`
  applies to every managed kind, and one kind's list can be another kind's map.
- **D46 An owner resolves through any served version, and an unresolved one is a run
  note.** An operator that migrates between API versions can name its owner at `v1alpha1`
  while the target declares `v1`. The collector keyed an owner by its apiVersion, so it
  kept that child, G3 fired on a correct controller, and only a warning on stderr said why.
  kube's garbage collector maps a reference's apiVersion and kind through discovery and
  then compares the UID. The collector now does the same with the run's RESTMapper. A
  version the API server does not serve stays unresolved, because kube's collector cannot
  resolve it either. G5 has no mapper, so it ignores the version. Each unresolved reference
  is a run note that names the object it keeps, because G3 reports that object and the
  report is what a reader acts on.
- **D47 The proxy holds each fault op's fault by identity.** The proxy matched a held
  fault by its spec, so a spent fault displaced an equal one, and the toy with no bug failed
  G4. Each fault has an ID, and a removed fault keeps its window. The Runner drops a fault
  once its window is closed, so a request faulted just before a removal stays in it.
- **D48 A G4 says why `Ready` never held, and quotes the predicate and the CR's status.**
  A misspelled `ready`, a controller that never converges and one that never stops writing
  need different fixes, so the G4 of an expired wait names its cause. It walks `Ready`
  over the CR's versions in the wait. The Runner calls the engine's function for it, so
  the two agree, and a checkpoint sits where its wait ended, where the engine stamps the
  verdict. A wait can begin before the Observer sees the op's write, so the walk counts a
  missing CR as not holding. It says `Ready` held and then stopped only if `Ready` held
  once the op's CR had a version recorded after the op. It tells that version by when the
  Observer saw it, and the op is stamped before its first request, so a controller write
  that lands before the op's own write can still count. The window opens the watch's delay
  before the stamp and closes at the op's write, so it spans an update's read and any retry
  on a conflict, and a recreate's wait for the old CR to go. Where several CRs
  are live, the verdict quotes the one `Ready` failed on. The report quotes the CR's status, which D35 left to
  `objects.jsonl`, because a reason such as "0/10 replicas available" lives there. A
  controller can copy anything into its status, so the quote is bounded. The verdict
  quotes the CEL error, and a non-bool ends the run at its first evaluation. The same
  verdicts name a failing request the target repeated, which #46 found behind G4 and G1
  under controller-runtime's default backoff.
- **D49 A Secret's values are written as keyed markers.** CI uploads `reconciler-fuzzer-out/` when a
  tier fails, and `objects.jsonl` held the external-secrets control's token and the
  cert-manager control's private keys. A marker still shows a reader which value changed.
  An unkeyed hash would let anyone confirm a guessed value. external-secrets annotates its
  Secret with exactly such a hash, and kubectl with a copy of the data, so every annotation
  value is marked too. Labels are not, because a reader checks a `selector`'s attribution
  against them. Only core Secrets are redacted, because reconciler-fuzzer cannot tell a credential
  anywhere else from other data. No flag writes the raw values. Each example tier fails if
  its control's evidence holds its Secret's value.
- **D50 A G5 violation names each field that differs.** A G5 report named the object and
  the restart, so an adopter diffed two versions in `objects.jsonl` by hand to learn what
  changed and whether it belonged in `equalIgnore`. G5 now diffs the forms it compared,
  after the default and `equalIgnore` reductions, so nothing it ignores appears. A path
  writes a list item as `[*]` and ends at a label or an annotation, so every path pastes
  into `equalIgnore`; a list whose length changed is one row. A map only one version holds
  compares as an empty one, so a stamp on a child that had no annotations (D45) names its
  key, and pasting it leaves the other annotations compared. One violation covers
  every object a restart changed, because a run keeps its first violation and one per object
  hid the rest. The bound takes each object's rows in turn, so one noisy object does not
  crowd out the others. G5 is stamped at the state after the restart, where it judged. A
  target with its own equality hook gets one row per object, because G5 cannot see what the
  hook compared. A row of a whole object names no path, because `equalIgnore` cannot ignore
  an object. The line reconciler-fuzzer prints quotes the first row with a path, since that is what an
  adopter pastes, and names its object where the statement names another.
- **D51 A target learns the run namespace from reconciler-fuzzer.** Each run takes a fresh
  namespace (§5.5), and an operator-sdk operator watches only the namespace
  `WATCH_NAMESPACE` names. reconciler-fuzzer substituted only `$KUBECONFIG`, and its kubeconfig named
  no namespace, so such an operator watched the wrong one and every run failed G4 with 0
  managed objects. The placeholder `$NAMESPACE` and the key `launch.env` now carry the run
  namespace. The kubeconfig's context names it too, and kube-rs, clientcmd and kubectl read
  it there with no configuration. reconciler-fuzzer exports no `WATCH_NAMESPACE` of its own, because
  the name differs by framework and operator-sdk reads an empty one as every namespace. The
  target still inherits reconciler-fuzzer's environment, since it may need `PATH`, `HOME` or proxy
  settings, but the replay command does not record it. `launch.env` is in the target file,
  which the replay reads. The toy reads `WATCH_NAMESPACE`, so every toy run depends on the
  substitution. Only the bare spellings expand. Refusing `${...}` would break a `sh -c`
  script that reads `${KUBECONFIG}`. The loader refuses a `launch.env` name or value that
  YAML 1.1 decoding would change, such as `0022` or `ON`, and accepts one that decodes to
  its own text, such as `8080`.
- **D52 A configuration mistake fails before a control plane starts, where it can, and
  names the setting to change.** A new user's first mistakes surfaced late or with no hint.
  envtest reported `fork/exec /usr/local/kubebuilder/bin/etcd` and named neither
  `KUBEBUILDER_ASSETS` nor setup-envtest. A misspelled key gave `json: unknown field` with
  no line. A missing `launch.binary` and a cluster-scoped kind failed only once the control
  plane was up, and the cluster-scoped kinds one at a time. A stopped target's quote was the
  last frame of its stack trace. A fault on `configmap` matched nothing and said nothing.
  reconciler-fuzzer now looks for the control plane where envtest does, checks `launch.binary` first,
  names a bad key's line and nearest key, and quotes the line above a stack trace. It
  refuses every cluster-scoped primary, managed kind and fixture in one error per check:
  from the CRD files at load time, and through discovery before the first run for built-in
  kinds, which no file describes. D13 stands, and now covers managed kinds and fixtures. A
  fixture that sets a namespace is refused rather than moved, because the target may look
  for it there, while the target finds a moved CR by watching. `launch.binary` keeps the
  working directory as its base, because `launch.args` and the target's own relative paths
  resolve from there. The Runner checks a fault's resource when it applies the fault op,
  not when the run starts, because a target may install its CRDs itself. Amended by D78.
- **D53 reconciler-fuzzer restarts a target that exits once it has converged, and a crash loop is a
  G4.** Controllers are deployed to be restarted, and controller-runtime with leader
  election on exits on purpose when it loses its lease, which a fault can cause. A new
  invariant, "the target keeps running", would report such a controller, so reconciler-fuzzer
  restarts the target as a kubelet does, with a kubelet's backoff. A target waiting out
  the backoff has not converged, and a restart counts as a change, so a target that exits
  again soon after each restart fails G4, even where it writes the converged state first.
  A drawn sequence that finds one shrinks. An exit before any wait converged stays a
  harness error, since a bad flag, a taken port and a crash on op 0's CR look alike there.
  An exit a fault excuses owes the target `T_settle` past its restart. Otherwise a correct
  controller that exits once under each of two faults fails G4, because the second restart
  waits 10 s. Any other restart gives the target no more time, and its startup requests
  count toward G1 in a quiet window. Excusing them would need a recovery window of their
  own, and a correct controller rarely exits with no fault active.
- **D54 A golden test pins what fixed seeds draw.** Draws come from rapid's
  `Example(seed)`, which rapid documents as fit only for examples and which promises nothing
  across versions. A draw also depends on the CRD schema, the sample, `generate` and
  `manages`. The Makefile, the README and the envtest tier rely on what particular seeds
  draw. `internal/generate` records the draws of those seeds for the toy, the toy with a label
  fixture, cert-manager and external-secrets. It takes the example and kind tiers' seeds
  from the Makefile, and the fixture envtest fails unless it draws what the record holds. A
  change to generation or to rapid that moves a draw fails until the test is rerun with
  `-update`. The quickstarts of `docs/examples.md` must run the Makefile's example seed,
  and the drawn runs that page shows must start at that seed. Another test runs its
  cert-manager quickstart command as `quickstart.sh` passes it on, and fails unless the
  page shows what reconciler-fuzzer prints. The README tells CI to pin reconciler-fuzzer to a commit and to
  replay a failing `sequence.json` against the base branch.
- **D55 Generation keeps the CRD's own rules, judged by the API server's code.** The
  generator read part of the OpenAPI schema and no `x-kubernetes-validations`. With the
  rule `self.maxUnavailable <= self.count`, 15 of 100 drawn sequences broke it, and the
  first refusal ended the invocation with exit 2 and no word of where the sequence was.
  Real CRDs carry such rules: the external-secrets bundle holds 50, and cert-manager's
  Issuer states "exactly one of" in CEL. The generator now judges each drawn CR op with
  `k8s.io/apiextensions-apiserver`, as the API server does, and undoes or redraws what the
  CRD refuses. Reimplementing the rules on cel-go alone was rejected. Kubernetes CEL types
  each rule against the structural schema, escapes property names, adds its own libraries
  and sees defaults first. Each divergence would undo a valid draw or pass one the API
  server refuses. The API server's code moves no selected module version, because
  controller-runtime v0.25.1 already requires `k8s.io/apiserver` and
  `k8s.io/component-base` at v0.37.0. Naming them in `go.mod` adds seven modules to the
  module graph, none of them built, and the binary grows by 4.6 MB. An
  envtest test applies 200 drawn sequences per target to a real API server with no
  controller, so the two cannot drift apart unseen on a CR without status. With a
  controller, the API server judges an update with the stored status copied in, which the
  generator never sees. A CRD rule that reads status can therefore still refuse a draw. A
  refusal still exits 2, since a rule reconciler-fuzzer cannot keep belongs in the target declaration
  (§8.3). The message names the run and its `sequence.json`, and names a status rule as a
  possible cause. Undoing a refused field biases draws away from a rule's boundary: a
  sample that sets one of two exclusive fields never switches to the other. When the CRD
  refuses all 100 values drawn for a field into the sample, the field would never move, so
  New reports it instead of skipping it silently. Each value is judged against the sample
  alone, so New also reports a field that only another field's change makes valid. A
  sample that accepts the field fixes that. No sample accepts both of two exclusive
  fields, so `generate.mutate` names only one of them.
- **D56 A kubeconfig cluster gets the CRDs, and a run excludes what the cluster put in its
  namespace.** On kind, `--kubeconfig` installed no CRDs, so the first run failed to
  resolve the primary kind. With the CRD applied by hand, the toy with no bug failed G3 on
  `kube-root-ca.crt`: `deleteManaged` took that ConfigMap as the oldest, and
  kube-controller-manager recreated it. D6 assumed that only reconciler-fuzzer and the target write
  to the namespace. Kubernetes' e2e framework waits for the `default` ServiceAccount and
  `kube-root-ca.crt` in each test namespace, so a run waits for those of a kind it
  watches. It then excludes, by name, everything the namespace holds, before fixtures and
  the target. Without the wait, a root CA published late counts as the target's. Objects
  a cluster adds later stay attributed to the target, and `selector` leaves them out.
  reconciler-fuzzer installs the CRDs with envtest's `InstallCRDs`, which creates or replaces each
  one and waits until it is served. It leaves them, because deleting a CRD deletes every
  object of that kind in the cluster. A kubeconfig cluster serves one invocation at a time:
  two invocations on one kind cluster failed the correct toy on G1 and G6, because each toy
  reconciled the other's Widget.
  envtest also read `USE_EXISTING_CLUSTER`, which pointed reconciler-fuzzer's default mode, collector
  emulation and all, at whatever `KUBECONFIG` named. `cluster.Start` now turns that off.
  `make test-kind` runs on kind v0.33.0 and its default node image, Kubernetes 1.37.0.
- **D57 reconciler-fuzzer names the kinds envtest never moves, and envtest adds no finalizer that
  only the controller manager removes.** An operator whose `ready` waited on its
  Deployment's `availableReplicas` failed G4 on every envtest run, and reconciler-fuzzer said nothing
  about why. envtest runs no `kube-controller-manager` and no kubelet, so the Deployment's
  status stayed empty and no ReplicaSet or Pod appeared. Under the default
  `observedGeneration` predicate the same operator passed, but it requeued every 5 s while
  it waited, and so recreated a child it never watched. A seeded missed-watch bug passed.
  reconciler-fuzzer therefore names these kinds and points to kind, rather than suggest a weaker
  `ready`. The list is explicit and short: the kinds that run Pods, and
  PersistentVolumeClaim. It matches by group and kind, at any version. Running
  `kube-controller-manager` beside envtest was rejected: setup-envtest ships no such
  binary, and no Pod would run without a kubelet anyway. A claim also carried
  `kubernetes.io/pvc-protection`, which nothing on envtest removes, so a correctly owned
  claim failed G3. Removing that finalizer in the collector was rejected, because it adds
  code and timing, and on envtest no Pod ever uses a claim. reconciler-fuzzer disables the admission
  plugin instead. A Job or a ReplicationController, which a delete orphans by default, and
  any foreground delete likewise carried a finalizer that only the garbage collector
  removes, so a correctly owned Job failed G3 too. reconciler-fuzzer therefore also turns off the API
  server's garbage collector, which adds those finalizers.
- **D58 A target declares how many requests a quiet window may hold.** G1 failed a
  controller that resyncs on a timer, because one request in the `T_stable` after
  convergence was a violation, and a target could not declare the timer.
  `thresholds.quiet`, `N_quiet`, default 0, bounds G1's count of requests in one quiet
  window and G2's count of status writes in it. It mirrors `errloop`: one number keeps G1 a
  bound on the request rate. A declared resync interval would need reconciler-fuzzer to find each
  tick's burst, and an opt-out would drop G1 and G2 whole. One number covers both checks,
  because a tick that rewrites an unchanged status is both a request and a status write.
  It never excuses a resourceVersion that moves, so a heartbeat that changes a field is
  still churn (§14 question 1). An `N_quiet` above zero lets a slow loop through G1, and
  G6 catches only a loop that fails often enough (D59). The toy's `--resync` runs under
  envtest with `quiet: 3` and with the default.
- **D59 `N_errloop` defaults to 10, and the teardown deletes the fixtures.**
  controller-runtime's default backoff fails 11 times by 5.1 s and 13 times in the densest
  30 s, so G6 missed such a loop at 20. A lower default can fail a correct controller that
  retries one request fast, so the adopted examples set the bar. The worst count of one
  failing request within 30 s was 3 over 23 external-secrets runs and 2 over 13
  cert-manager runs, on drawn seeds, pinned sequences and controls. Before the teardown
  deleted the fixtures, external-secrets reached 26 by the seventh run of one invocation.
  It reconciled each earlier run's SecretStore, left in a namespace envtest never deletes,
  and the API server refused every Event it created there. A `T_settle` of 5 s still
  needs 9 or less (§6, backoff).
- **D60 G7 requires an object `DeleteManaged` deleted to come back.** The op simulates a
  missed event, and no check asked whether the target recovered from one. Under B8 with
  the toy's P1 removed, `create` then `deleteManaged v1/ConfigMap` passed every invariant,
  and G5 saw the missing child only when a `restart` followed. G7 judges each such op
  where its settle wait ends and asks for an object of the same kind and name, because a
  recreated object has a new UID and may differ in content. Comparing the converged states
  on either side, as G5 does, was rejected: cert-manager answers a deleted Secret with a
  new key and a newly named CertificateRequest. A target lists the kinds it leaves deleted
  by design in `notRecreated`, a list of its own, so that `manages` stays a list of
  strings. cert-manager lists CertificateRequest: after `create` and `deleteManaged` of
  its request, the settle wait converged on 10 s of quiet with the request still gone. Its
  Secret came back, also after a re-issue, and so did external-secrets' Secret, also after
  a rename, so neither lists Secret. G7 notes an op that follows a change of reconciler-fuzzer's
  before the run converged, because the target may have meant to delete that object
  itself, as the toy does on a scale-down. It notes one where a fault was active during
  the op or its wait, as every check ignores a fault's window, and one whose wait ended
  while the target was still owed time to recover from a fault. The Runner stops a fault
  that runs until an op just before it applies the op, and the toy's informer retried its
  list 4 s after such a fault stopped, when G7 had already failed it. A settle wait that
  converged since does not end that time for G7 (D90). G7 does not judge an op while no
  CR is live. An object that
  is back satisfies G7 before any of these, whatever the fault or the change did. A
  `Restart` gives reconciler-fuzzer no sign that the target is back, so a settle wait after one could
  converge while the target was still starting, or waiting out the lease its killed
  predecessor held. G7 then failed the correct toy behind a wrapper that delayed each
  restart by 3 s. A restart by `Supervise` after an exit is no different: G7 failed the
  correct toy whose first process was killed and whose restart was delayed by 15 s. G7
  judges an op after either restart only where the target requested a resource outside
  leader election between the two, whatever the API server answered, since only a running
  target asks. A request anywhere in the op's wait was rejected as the bar, because a
  target first heard from late in the wait has had no time to act. G7 also notes an op
  where the target exited, or waited to restart, during the op or its wait, since the
  target may then not have been running when the wait ended.
- **D61 G1 and G7 treat every `coordination.k8s.io` request as leader election.** The
  group holds only leases and lease candidates. A candidate under coordinated leader
  election creates and renews its LeaseCandidate whether or not it leads. G1 ignores those
  requests as it ignores lease requests, and G7 does not take one as a sign that a
  restarted target is back.
- **D62 A settle wait gives a CR under deletion its `T_delete`, and G3 judges a CR that
  outlives it.** The wait after a `delete` op gave the CR `T_settle` to go. The toy with a
  7 s cleanup, under a `T_settle` of 5 s and a `T_delete` of 10 s, failed G4 on a
  `create` and a `delete`, and G3 noted that the run ended before its deadline. B13, whose
  finalizer never clears, failed G4 too, where G3 names the finalizer with its evidence. The
  wait now waits for the CR to go, until its G3 deadline. Once the CR has gone, the run has
  `T_settle` to settle, as after a spec change, because the garbage collector deletes the
  CR's children after it. `T_stable` would not do: a child deleted after the CR restarts
  the quiet. G3's window stays `T_delete`. A wait in which a CR outlived that deadline is
  G3's, so G4 does not report it, whichever wait it is. Where a fault reached into the
  deletion, G3 only notes it, so G4 still judges that wait and blames the finalizers. A CR
  under deletion is not ready, whatever `Ready` says, because a wait that converged
  mid-cleanup would put the rest of the cleanup in the quiet window. The toy proves both:
  with a `--cleanup-delay` past `T_settle` it passes a `create` and a `delete`, and B13
  fails G3 alone. A `recreate` waits as long for its old CR, and a CR still there where
  that wait ends is judged there. A harness error there hid B13 from G3 on generated runs.
  The op cannot create its CR while the old one stays. Where a fault excuses the target,
  the run stops there (D91), and one that no check reports otherwise is a harness error.
- **D63 An interrupt abandons the run under way, and reconciler-fuzzer dies of the signal once it
  has stopped what it started.** Only reconciler-fuzzer takes back what it started: etcd,
  kube-apiserver, the target, the run namespace and envtest's directories in `TMPDIR`.
  envtest starts etcd and kube-apiserver in process groups of their own, so a terminal's
  Ctrl-C never reaches them. SIGINT, SIGTERM and SIGHUP end the invocation's context.
  GitHub Actions cancels a step by sending its shell SIGINT and, 7.5 s later, SIGTERM.
  The shell passes neither on, so the step must `exec` reconciler-fuzzer. The teardown's waits alone
  take `T_stable + T_delete`, so an abandoned run waits for nothing. A context whose
  deadline has passed cannot report a later interrupt, so the deadline ends the
  teardown's waits too, and a run it cuts there exits 2 (§11). A Ctrl-C also reaches the
  target, so supervision ends with the context. reconciler-fuzzer blames the interrupt only for a
  wait it ended and a target the Runner found stopped after it. A run that failed on its
  own says why, even when the interrupt comes during its teardown.
  reconciler-fuzzer dies of the signal rather than exit 2, so that a shell loop, make and `timeout`
  see an interrupt. A Ctrl-C also kills a `tee` that reads reconciler-fuzzer's output, so reconciler-fuzzer
  ignores SIGPIPE once interrupted. `signal.Notify` would undo `nohup`, so reconciler-fuzzer leaves
  alone a SIGHUP or SIGINT it was started ignoring. Go keeps no other inherited SIG_IGN.
  A SIGKILLed reconciler-fuzzer still leaves the control plane and the target running.
- **D64 Without `--deadline`, reconciler-fuzzer derives the deadline from the target's timeouts.** A
  fixed 4m stopped ten runs of a correct controller at §6's timeouts after six, each of
  which took 32 to 42 s, and a larger fixed default fails again when `--runs` grows or the
  timeouts widen. The derived deadline is the longest the planned runs' waits (§5.5) can
  take, one run after another: the start's wait for a kubeconfig cluster's namespace
  defaults (§5.8), `T_settle` per settle wait, `T_delete` per `delete` or `deleteFixture`,
  the longer of `T_delete` and `T_settle` per `recreate`, whose wait lasts as a settle
  wait does while the target is owed time, the reap after a `restart` and `T_settle` more
  for the target's return (D69), the teardown, stopping the target and deleting the
  namespace. A target that exits while a fault excuses it is owed `T_settle` past its
  return, which can come `T_settle` after a restart whose backoff can reach 5 min (§5.1),
  so from the first fault op on, each op allows one such exit (§6). Each op after it may
  first wait up to 5 min for a restart, and then owe `T_settle` past a return that can
  come `T_settle` after the op (§5.5). Faults that stopped are owed as long as they lasted
  plus `T_settle` (§6), so each time faults stop, the deadline doubles what the run had
  and allows another exit. A request a delay holds can keep a wait open past its time,
  so the deadline adds the longest delay so far and `T_settle` to each wait. A fault
  lasts until the proxy releases what it held (§5.5), so the deadline adds that delay to
  the faults' length too. Faults with no trigger stop together, at the teardown.
  Minimizing gets what the runs left plus 4m, so it may still stop early. At §6's
  timeouts, a create and an update get 3m50s, and ten drawn runs tens of minutes. A fault
  that stops before the update raises the 3m50s to 47 min. Four such faults, each before
  an update of its own, give 26 h, past GitHub Actions' 6 h job limit. reconciler-fuzzer
  prints the deadline, and a sequence with faults should set `--deadline`. An explicit
  `--deadline` must be positive and is used as given. The derived deadline ends no run the
  Runner would end on its own, unless a request hangs.
- **D65 G6 counts each namespace's requests apart.** Ten runs of external-secrets with
  no flags failed G6 at run 8: the controller repeated `create events` 34 times in 30 s.
  None went to the run namespace. envtest never finishes deleting a namespace, so each
  earlier run's objects stay, and the controller wrote two events into each of 17 such
  namespaces as it started. The API server refused each with 403. G6 ignored the
  namespace, so the count grew with every run. A request into another namespace is a
  request for another object, so G6 now tells them apart. Judging only the run namespace
  was rejected: G6 would then miss a failing cluster-wide watch, which D27 counts.
- **D66 Every invocation that planned its runs leaves a summary, and `--junit` writes
  JUnit XML.** A passing invocation deleted each run directory and printed `every run
  passed.`, so nothing recorded what a nightly exercised. A seed is no such record, because
  it names a sequence for one build and one target declaration only (D54). The summary
  keeps each run's sequence, which a replay needs, and still drops a passing run's
  recordings. reconciler-fuzzer opens the invocation's directory before it starts the cluster, so a
  cluster that does not start or refuses the target leaves a summary too. It writes the
  summary then and as each run starts, because a second signal kills reconciler-fuzzer at once, and
  GitHub Actions sends one 7.5 s after the first, which an abandoned run's teardown can
  outlast. It writes it again once a run finds a violation, because the shrink pass can
  take minutes and an OOM kill needs no second signal. An `unfinished` summary gives no
  exit code, which a reader would take for 0. Its JUnit testsuite gives no time, because
  the time to the last write would read as the invocation's. The final summary comes
  before the cluster stops, and an interrupt during the stop rewrites it, because reconciler-fuzzer
  then dies of the signal. A failing run's counts are of the run of its planned sequence,
  and its violation and notes are its report's, which may be of the minimized sequence.
  `run-<n>/` is named relative to the summary, so that it still resolves after an
  artifact upload. JUnit gets one testcase per planned run, not one per invariant,
  because a check that could not judge would read as a pass (D31). What stopped the
  invocation where no run did is an `error` testcase of its own, so that a CI server never
  shows a stopped invocation as green. An invocation that stops before it has a directory
  writes that testcase alone, since a CI server would otherwise read an earlier
  invocation's file as this one's. reconciler-fuzzer detects no CI vendor. `$GITHUB_STEP_SUMMARY`
  takes `summary.md` as it is, and GitLab and Jenkins read JUnit XML. A file reconciler-fuzzer cannot
  write leaves the exit code alone, since the code already says what the runs found.
- **D67 A matrix run that errs keeps its files.** The matrix deleted its temporary
  directory on every exit, so a harness error named a `target.log` that was gone. A run
  that errs now keeps its directory, and every other run's is deleted once the checks
  have read it. The directory stays under the system's temporary directory, because the
  matrix's `--out` names a file. A matrix that finishes leaves nothing there. CI's
  bug-matrix job points `TMPDIR` at a directory it uploads when the job fails. The error
  also prints a replay command with the run's `--bug`, because the runner's hint names
  only the run's `sequence.json`.
- **D68 The CI recipe is a workflow that runs in any repository, caches what it installs and
  keeps a failing run's evidence.** The README's recipe read Go's version from a go.mod, which
  a Rust operator's repository lacks. It said to cache the control plane as ci.yml does, but
  its setup-envtest wrote to the OS data directory. It uploaded nothing. The recipe is now
  `examples/ci/github-actions.yml`, which the README embeds and a test parses. It caches
  reconciler-fuzzer and setup-envtest with the control plane, because reconciler-fuzzer took 104 s to build cold on
  four CPUs and the 175 MB control plane took 3 s to fetch. It saves the cache before reconciler-fuzzer
  runs. `actions/cache` saves only when the job passes, so a nightly that kept finding
  something would never fill the cache that pull requests read. It uploads its `--out` however
  reconciler-fuzzer ends, which keeps the summary too (D66). Its download command restores the paths that
  the replay command in `report.md` names. The nightly's issues give the same command.
- **D69 A settle wait converges only once the target has shown it runs since it last
  started.** Nothing changes while a restarted target starts up, so a settle wait after a
  `Restart`, or after `Supervise` restarted the target, converged before the target was
  back. The correct toy behind a wrapper that delayed each restart by 5 s, or by 2.2 s
  with a `T_stable` of 1 s, was not back when the wait after a `deleteManaged` converged.
  P1 failed there, and G7 only noted the op. A wait now converges only once the process
  running now has requested a resource outside leader election, the sign G7 reads (D60),
  and that request counts as a change, so the target's startup falls inside the wait. A
  controller lists what it watches as it starts, so a correct one makes such a request.
  The rule covers the first process too, since a `ready` that holds without the target
  could otherwise end op 0's wait before the target started. reconciler-fuzzer chose to restart the
  target, so a `Restart` owes it `T_settle` past its return, as an exit a fault excuses
  does. Counting the startup against the wait's `T_settle`, as op 0 does, failed G4 on
  the toy behind a 3.5 s delay under its 5 s `T_settle`. A target must return within
  `T_settle` of the restart, so the 5 s wrapper still fails G4, which says the target was
  not back. An exit no fault excused owes nothing, so G4 also fails a crash whose restart,
  startup and `T_stable` outlast the wait. Such a wait converged before, while the target
  was not back. Leaving G1 and G2 unjudged in the window after an op G7 notes was rejected,
  because it judges less.
- **D70 A sequence changes and deletes fixtures.** No op touched a fixture, so reconciler-fuzzer
  never tried a referenced Secret that changes or an Issuer that disappears, and a
  controller that reads such an object without watching it passed. `updateFixture`
  merge-patches a fixture, and `deleteFixture` deletes one until the op its `until` names.
  It waits for the fixture to go, since a target may hold it with a finalizer, as
  external-secrets does its SecretStore, and reconciler-fuzzer cannot create it again until it has
  gone. A target names in `generate.fixtures` the fixtures generation may delete and the
  strings it may set, since built-in kinds carry no schema reconciler-fuzzer reads. A drawn value is
  a short word a Secret's `data` also reads as base64, so a target names only strings that
  take any such word. Generation restores a deleted fixture before the next op that
  settles, because a target may rightly not be ready while a dependency is gone. Excusing
  every check while a fixture is gone, as for a fault, was rejected: it judges less and
  touches every check. A fixture stays reconciler-fuzzer's, so G3 and G7 never ask anything of it. A
  fixture op is reconciler-fuzzer's change for G5 and G7, and G4 and G6 treat an update or a restore
  of a fixture as a spec change. The Runner stamps the op a restore precedes before it
  restores, so the target's reply falls in that op's window. The toy's B14 copies a label
  from a ConfigMap fixture it does not watch, and G5 finds it once a restart reconciles
  the Widget. A generated sequence finds it too, and the correct toy passes every seed up
  to 60 that draws a fixture op. A target without `generate.fixtures` draws what it drew
  before, which the golden draws pin.
- **D71 A sequence creates up to three CRs, and G3, G5 and G7 attribute objects to them
  by ownerReference.** A run acted on one CR, so reconciler-fuzzer never tried two CRs side by side.
  `update`, `delete` and `recreate` take an optional `cr`, the sample's name by default,
  so a sequence whose one CR takes the sample's name means what it did. One that creates
  its CR under another name must name it in `cr`. A CR with no `metadata.name`, and a
  sample with none, are configuration errors: ops name the CR they act on, and further CRs
  extend the sample's name. About a quarter of drawn sequences create a second CR. A CR
  made by renaming the sample keeps every spec field, including cert-manager's
  `spec.secretName` and external-secrets' `spec.target.name`, which name the Secret each
  CR owns. Two CRs that name one Secret fight over it, which is a user's mistake. Without
  `generate.distinct`, cert-manager's seed 5 drew two Certificates that name
  `example-tls`, and cert-manager made 5226 requests in 30 s and failed G4. No draw takes
  another CR's value at a distinct path, nor the value another CR's suffix makes:
  external-secrets' pattern for `spec.target.name` allows `example-secret-2`. Making
  several CRs opt-in was rejected, since generation is on by default and a target already
  narrows it where the schema says too little (§8.3). `maxCRs: 1` turns it off. Checks
  that read "the CR" were ambiguous: G3 failed the toy with no bug when one Widget went
  and another outlived its deadline. An object now belongs to the CRs its ownerReferences
  name, and one that names none to any. G3 holds it to the last of those CRs to go, at
  that CR's deadline, since the garbage collector keeps an object until its last owner
  goes, and a grandchild names no CR at all. G5 judges what reconciler-fuzzer's changes between its
  states could not have reached, which assumes one CR does not change another's objects
  (§14, question 5). B15 names its children after the kind, which a run of one Widget
  named `widget` cannot tell from correct. Seeds 1 to 30 of cert-manager passed, 11 of
  them with two Certificates or more, and so did seeds 36, 66, 157 and 178, which draw
  three. Seeds 1 to 40 of external-secrets passed, 7 of them with two. cert-manager's
  seeds 23 to 27 no longer draw a delete or a `deleteManaged`, and its pinned sequences
  hold both.
- **D72 reconciler-fuzzer empties a run's directory before it runs the minimized sequence there.** A
  run writes `requests.jsonl` and `objects.jsonl` as it ends. A SIGKILL during that run left
  the drawn run's recordings and the shrink pass's replays beside the new run's
  `sequence.json`, `kubeconfig` and `target.log`, under a summary that said the directory
  held a partial run of the minimized sequence. Naming the run each file came from was
  rejected, because a run that ends replaces the drawn run's recordings anyway.
- **D73 One page lists every key, op and fault field, and tests keep it whole.** Some
  keys and fault fields appeared only in the Go source. `docs/reference.md` gives each a
  row, and tests hold the rows to the code (§11). A fault field value that tests something
  else is refused, such as a `fraction` of 0, which reconciler-fuzzer read as every request, an
  `until.count` or `until.for` of 0, which reconciler-fuzzer read as no trigger, or an `until.op` at
  or before the fault's own op. Keeping the reference in DESIGN.md was rejected, because
  DESIGN.md mixes the contract with internals, milestones and decisions.
- **D74 reconciler-fuzzer's help and messages need no design document.** `go install` ships no
  DESIGN.md. Each command's help is generated from its flags and gives each flag's
  default, and reconciler-fuzzer's help gives the exit codes and `KUBEBUILDER_ASSETS`. A usage error
  prints the command's synopsis. A message names the target.yaml key and its value rather
  than a symbol of this document. G4 gives how long an expired wait ran beside the key
  that bounds it, since a wait can run longer: `timeouts.delete` for a wait that a
  deleted CR's deadline held open while the CR stayed, and `timeouts.settle` for the
  rest. A G3 note names what deleted the CR where reconciler-fuzzer can tell, because a run may
  delete one CR twice. It names the first op that found the CR, because a lagging
  Observer shows the CR to the op after the one that deleted it.
  Splitting this document was rejected, because the hourly Routine reads it whole.
- **D75 This document gives intent where a listing would drift.** §8.2 names `go doc`
  rather than listing `Target`'s fields, and a test holds §11's synopsis to each
  command's flags. reconciler-fuzzer shrinks with its own pass, because rapid shrinks only inside
  `rapid.Check` and shrinks the choices a sequence is drawn from rather than its ops.
  Hand-written generators wait for a target that needs one.
- **D76 The README says what reconciler-fuzzer cannot test yet.** An adopter found each limit only
  by trying, after the control plane had started. A section right after the intro states
  them. Each limit an open issue tracks is a bullet that links it. The opening states those
  no issue tracks: a target on the host, webhooks, and generation for a built-in primary
  kind. A test lists each limit with words the README and this document say of it, so a
  change that lifts one edits all three (§12). A fixed port is not listed, because the
  message reconciler-fuzzer exits 2 with names the fix in `launch.args`.
- **D77 While a fault is active, each op owes only its first exit and the exits it lands
  soon after, and no op lands while the target waits to restart.** B12, under a fault that
  never stopped, crashed six times and ran until a 3m deadline. Each exit the fault excused
  owed `T_settle` past a restart whose backoff doubled, so the wait after the update never
  ended, and the derived deadline was 2h45m. Owing one exit during each op bounds each
  wait, and a correct controller that loses its lease once during an op still gets
  `T_settle` past its return before the next op. Once no fault is active, every exit a
  fault excused is owed, and nothing excuses an exit past the faults' recovery. B12
  therefore fails G4 in the teardown's recovery wait: a minute into `b12-fault.json`, and
  5.5 min into the issue's sequence, whose fault lasts longer. Owing only the first exit
  after each fault began was rejected. Under a fault that failed 80% of its lease updates,
  the correct toy then saw a `deleteManaged` land while it waited out a later backoff, and
  P1 failed. A fault excuses G4, not a property, so the Runner waits for such a restart
  before each op whatever the faults. With that wait alone, the op lands as the toy
  restarts, before it has won its lease back, and one of two runs still failed P1. Owing
  the first exit during each op passed both. Where the toy exited twice during an op, the
  next op still landed at the second restart and owed nothing past it, in 8 of 11 replays
  that also delayed the toy's Widget patches. So an op that lands before the target has
  had its time after an exit owes that exit too. That time ends within `2 × T_settle` of
  the op, so the wait stays bounded. Counting that exit as the op's first was rejected,
  because the op's own first exit, common under such a fault, would then owe nothing. The
  derived deadline allows an exit per op from the first fault op on, and a backoff and
  `T_settle` per op after it, so it ends no run the Runner would end on its own. The toy's
  `--lease` elects a leader, so a fault on leases makes the correct toy exit as
  controller-runtime does.
- **D78 reconciler-fuzzer knows the scope of every built-in kind.** Under D52, a built-in
  cluster-scoped kind was refused only after envtest had started, or after the CRDs were
  installed on a `--kubeconfig` cluster. A target with a cluster-scoped CRD and a
  ClusterRole under `manages` heard only of the CRD. A cluster-scoped fixture that set a
  namespace was told to drop the namespace. reconciler-fuzzer now lists the scope of each kind a
  Kubernetes 1.37 API server serves by default. An envtest test compares that list with
  discovery on the pinned API server, so bumping that version means updating the list.
  Loading the target judges those kinds and the kinds of `crds`. Discovery, before the
  first run, judges the rest: a CRD that `crds` does not list, an aggregated API, or a
  kind newer than the list. Each check refuses every cluster-scoped kind it knows in one
  error. Only then does it refuse a fixture of a kind it knows to be namespaced that sets
  a namespace. The run refuses such a fixture of any other kind rather than move it. A
  target with kinds of both sorts hears of them in two errors. Merging them into one was
  rejected, because the first check refuses before reconciler-fuzzer installs CRDs on a
  `--kubeconfig` cluster.
- **D79 The README's first find is a seeded bug, and a hunt looks for real ones.** Every
  find the README shows is planted, and a find in a real controller needs a build of
  minutes. So the README says so, and its first find, after Install, replays the toy's B3.
  An envtest test runs that command as written and matches what it prints. A real
  controller may break under API faults, restarts mid-reconcile, changed or missing
  fixtures and several CRs, and generation draws no fault. So each adopted example carries
  hand-written families under `sequences/hunt/`, and `make hunt-<example>` runs them and
  then drawn seeds until a time box runs out. Each family and seed runs in an invocation
  of its own, because an invocation stops at its first failing run and a hunt keeps every
  failure. A run the time box cut is no failure, but a find reported after the box's end
  is one. The hunt runs a copy of `bin/reconciler-fuzzer`, so a rebuild during a hunt changes
  nothing. A family is checked in only once it passes the pinned controller. A run that
  fails is a candidate until triage keeps it (§11). No agent files a candidate upstream,
  because an issue there speaks for the maintainer. No pull request runs a hunt, because
  one takes hours. A `create` of a CR that a `noSettle` `delete` removed, with no op since
  that settles, is a configuration error (§7), because a finalizer may still hold the old
  CR. Letting that create wait for the old CR was rejected, because a `recreate` already
  does.
- **D80 The README keeps reconciler-fuzzer out of an operator's go.mod.** Minimal version selection
  works on the whole module graph, so requiring reconciler-fuzzer, or importing any of its packages,
  raises a module to reconciler-fuzzer's Go, Kubernetes and controller-runtime versions (D11). The
  README installs reconciler-fuzzer with `go install`, or with `examples/tools-module.sh`, which
  pins it in a module of its own under `tools/reconciler-fuzzer/`. A new directory leaves a `tools/`
  package of the operator's own, a common place for one, in the operator's module. The
  script sets the tools module's go line before `go get -tool`, so that a `go` before
  1.24, which lacks the flag, switches first. Its `go get` and its build run with
  `GOWORK=off`, because an operator's go.work would hold back that switch, take the
  raised go line, and leave the tools module out of the build. Exporting `GOWORK` was
  rejected, because a reader who pastes the script into a shell would keep it. It builds
  `bin/reconciler-fuzzer`, because `go -C tools/reconciler-fuzzer tool reconciler-fuzzer` runs reconciler-fuzzer in `tools/reconciler-fuzzer/`,
  where `launch.binary` does not resolve. The envtest tier runs the script in a fresh
  operator module with a `tools/` package and a go.work, as the oldest `go` the README
  names, with reconciler-fuzzer replaced by the checkout. It also checks that the `go` before that
  one fails. The unit tier runs the script with `go` stubbed, and checks that it leaves
  its shell's variables as they were. Each line of the script runs every time, so that
  check sees each change to a variable the script names.
- **D81 A Go test runs reconciler-fuzzer as a binary.** No Go function runs reconciler-fuzzer end to end, so
  the README's `go test` recipe runs `bin/reconciler-fuzzer`, which the tools module of D80 builds,
  and fails the test on a non-zero exit. A build tag keeps it out of a plain
  `go test ./...`. The README runs it with `-count=1`, because go test caches a pass and
  cannot see a change to the controller or `target.yaml`. The recipe sets `--deadline`
  half a minute before go test's timeout, in whole seconds, and fails the test when that
  leaves reconciler-fuzzer no time. A test that the timeout ends leaves reconciler-fuzzer to die of SIGPIPE at
  its next write, with its control plane still running. The envtest tier runs the recipe
  from a copy of the repository's layout: on the toy with no bug, under B4, with a
  timeout too short for its runs, with one that leaves reconciler-fuzzer no time, and with no
  controller to launch.
  Hooks stay in-repo (D2).
- **D82 Go comments need no design document.** Comments outside tests cited this
  document's sections about 290 times and its decisions 24 times, so a reader needed it
  open to follow the code, and a renumbered section left each citation wrong. A comment
  now says what it means and names the target.yaml key rather than a symbol, as D74 asks
  of a message. A test scans every comment outside tests for what it scans string
  literals for. Six packages each defined the same helper, which writes a kind as
  target.yaml does. `observe.KindName` replaces them, because `observe` imports no other
  reconciler-fuzzer package.
- **D83 The README walks a newcomer from install to CI, and `docs/` holds the detail.**
  Two newcomers walked the README cold. One built reconciler-fuzzer and fetched the control plane
  twice, because the first find did not use what Install had set up. Both left the README
  for `docs/reference.md` to learn that a property runs where no CR exists, and which
  fields a `deleteManaged` takes. Neither drew a `deleteManaged` in a dozen runs, so a
  controller with no watch on its children passed. A third walker's 13 drawn runs passed a
  controller that never rewrote a child after a spec change, since none updated a CR that
  had settled. The README now follows the order a newcomer needs: what reconciler-fuzzer is, what it
  cannot test, install, a first run and a first find with Install's binary, the toy's
  `target.yaml` as the worked example, reading a failure, and CI. It tells a reader to pin
  a sequence per managed kind that deletes one of its objects, and one per property that
  updates what the property reads. An envtest test runs the README's example against the
  toy, with B7 and B8. The examples, the kind caveats, generation, faults, crash loops and
  every message move to `docs/targets.md`, `docs/failures.md` and `docs/examples.md`.
  Those pages keep the README's rule against this document's vocabulary, and the tests
  that held the README to the examples' seeds and output now hold `docs/examples.md`. The
  worked example is embedded, so it cannot drift from a target the envtest tier runs.
  Keeping the cert-manager quickstart in the README was rejected, because it builds
  cert-manager from source and a newcomer needs the toy first. The walks also changed
  three messages. A property's violation where no CR existed now says so. An op that
  carries a field it does not take names the fields it needs. A target that stops on a bad
  flag has the line above its usage text quoted, since Go's flag package prints the error
  first. A walker also waited eight silent minutes for a find, twice the 4m the derived
  deadline's line seemed to promise. That line now says minimizing gets the rest of the
  deadline, and reconciler-fuzzer prints the failed check before it minimizes. The README says the
  derived deadline is a worst case, and how to size a shorter one. `reconciler-fuzzer --help` says
  reconciler-fuzzer runs a real kube-apiserver and etcd, and no longer that it injects faults, which
  drawn sequences never do.
- **D84 A settle wait outlasts the requests the proxy holds.** A delay fault held the
  toy's create of a child it had lost for 3 s, longer than its 2 s `T_stable`. Nothing
  changed while the proxy held the create, so the wait converged, and P1 failed the
  correct toy in 3 of 3 runs. A held request now counts as a change until the proxy
  releases it, so a wait converges no sooner than `T_stable` after the last release. The
  proxy releases a request when it forwards it, or when the target hangs up on one without
  a body. A watch therefore counts only until its response can start. Counting it while it
  streams would keep every wait after a delayed watch from converging. Every held request
  counts, leader election's too, because a target may manage Leases. A delay on lease
  renewals can then keep waits from converging until the fault stops. Counting only writes
  was rejected, because a held read delays the reaction it feeds as much. A request that
  reached the proxy before a wait's time ran out holds the wait open until `T_settle` past
  its release. Ending the wait sooner checks properties against a state the request is
  about to change. A `recreate`'s wait for its old CR is held open the same way. Under a
  12 s delay on the toy's Widget patches, that wait otherwise ended while the proxy held
  the patch that clears the finalizer, and P1 failed the correct toy. A request that
  arrives later does not hold the wait, so a wait under a proxy that holds one request
  after another ends no later than the longest delay and `T_settle` past its time. Such a
  wait can still end while the proxy holds a request. Under a 6 s delay on every ConfigMap
  request, the wait after a `delete` of the toy's Widget ended while the proxy held the
  toy's delete of its second child, and P1 failed the correct toy in 2 of 2 runs. So
  properties are not evaluated at a checkpoint where the proxy held a request of the
  target's, and the run notes each such checkpoint. A request released just before a wait
  ends can still change what the checks read. Under a 5 s delay, the toy's `T_settle`, on
  its ConfigMap deletes, the wait after a `delete` ended 9 ms after the proxy released the
  toy's delete of its third child. The Widget, with `status.ready` 3, was still there,
  since the toy had not yet removed its finalizer, and P1 failed the correct toy in 5 of
  10 runs. So a checkpoint where the proxy released a request within `T_stable` counts as
  held too. A wait that converged saw no release for `T_stable`, so it is never held. From
  `T_stable` after every fault's window has closed, checkpoints judge properties as
  before. A fault's window also lasts until the proxy releases what it held, because the
  target still waits on the fault. The checks therefore excuse the target over that time,
  and the time it owes runs from the release. The derived deadline allows for both. A
  `recreate`'s wait under requests held one after another can end with the old CR still
  there. Under a 5 s delay on every ConfigMap request, the toy's lists and deletes ran
  past the wait, and the run ended as a harness error. Any fault that keeps the CR past
  the wait does that, as a 500 on the toy's ConfigMap deletes does. Holding the wait open
  while the proxy holds request after request was rejected. An active fault excuses the
  target, so no check gives such a wait an end, and a target that renews a lease under a
  delay would hold it open until the derived deadline.
- **D85 No property is judged where the target is still starting.** Under a fault that
  failed most lease updates, the toy with `--lease=3s` lost its lease, and reconciler-fuzzer
  restarted it. The restarted toy requested only leader election until it won the lease
  back, in one run 5.5 s after the restart, past its 5 s `T_settle`. The wait after a
  `deleteManaged` gave it `T_settle` past the restart and ended there. P1 read the
  `status.ready` the toy wrote before reconciler-fuzzer deleted a child, and failed the correct toy
  in 3 of 3 runs. A property now skips, with a note, a checkpoint where the target was
  still starting by the measure a wait converges on (§6). That covers a target waiting out
  a backoff, where a wait can end after a later exit during the op. It covers the first
  start, because a fault can keep a target from leading before any wait converges. Judging
  a target that came back by the checkpoint, however late, was rejected: one first heard
  from just before a wait ends has had no time to act (D60, D69). Owing the target time
  until it is back was rejected, because an active fault can keep it from leading until
  the teardown clears the fault, and no bound would then cover the wait. A wait that
  converged is judged whatever the exits read, because a `Restart` op can replace a target
  waiting out its backoff, and the restart the exit scheduled never comes. Skipping
  properties at every wait a fault excuses was rejected, because properties are how reconciler-fuzzer
  sees a fault's transient states (§5.6). A target that elects a leader is back only once
  the API server accepts its create, update or patch of a lease. Some leader-election
  libraries win a Lease with a patch. controller-runtime starts the informers a field
  index asks for before the manager leads. The toy with `--index` watched Widgets while it
  waited out its lease, that watch counted as its return, the wait converged before the
  toy led, and P1 failed in 3 of 3 runs. A get of a Lease by any of its processes marks a
  target that elects, because leader election reads its Lease that way to learn who holds
  it, and an informer lists and watches instead. A restarted process reads its lease only
  once its caches sync, which a fault can delay, so its predecessor's get marks it first.
  A win counts only after the process's own get, because reconciler-fuzzer stamps a `Restart` op
  before it kills the leader, which renews without a get while its renewals succeed. A
  target that reads a Lease with a get and elects no leader therefore shows it runs only
  once it writes a lease, and fails G4 where it never does. G4 and the property read only
  the requests made by the checkpoint, as the wait did. Keeping the sign of D60 and D69
  and stating the limit was rejected, because the wait itself converged too early.
- **D86 The envtest tier's CI budget is ten minutes.** On CI, `internal/run` alone took 309 s,
  past the five minutes the tier had. The maintainer chose a larger budget over shorter
  tests or a tier split across jobs.
- **D87 The cert-manager example draws `rotationPolicy` only as `Always`.** The example
  mutated `spec.privateKey.rotationPolicy` and `spec.privateKey.algorithm`. Under `Never`,
  cert-manager keeps a stored key that a later algorithm does not match, and waits for a
  user, as its CRD documents, so G4 cannot hold. Seed 266 creates a Certificate under
  `Never` and later updates its algorithm, and it failed G4 in 3 of 3 runs. A scan of
  seeds 0-1999 finds 10 that update their way there. Hunt seed 1043 got there through a
  recreate, when cert-manager kept the deleted Certificate's Secret or wrote it back.
  Triage classified the write-back as an upstream contract break, and
  `docs/findings/cert-manager-secret-write-back.md` drafts its report. Under `Always`,
  and under `Never` with the same algorithm, the write-back still happens but G4 passes,
  so draws no longer catch it. `target.yaml` cannot state a rule across ops, so an overlay
  draws the policy only as `Always`. Of seeds 0-1999, 414 draws change, each only from
  `Never` to `Always`, and no draw's ops change. The example tier's seed 27 loses its
  `Never`, so `sequences/rotation-never.json` runs `Never` on every pull request. It
  creates a Certificate under `Never` and reissues it with the stored key. It recreates the
  Certificate under `Never` with the same algorithm, which passes whether cert-manager
  generates a key or reuses the deleted Certificate's. It deletes the Secret, after which
  `Never` lets cert-manager generate a key, and moves to ECDSA under `Always`. It passed 12
  of 12 runs. No pinned sequence reaches the branch where cert-manager waits for a user,
  because G4 fails there. A unit test refuses a pinned or hunt sequence that changes the
  algorithm under `Never`. A recreate after a delete counts as a change unless the Runner
  settled between them. Dropping `algorithm` from `mutate` was rejected, because it
  loses ECDSA and Ed25519, which 544 of the 2000 draws set, and changes the ops of 1877
  draws. Overlaying it to `RSA` was rejected, because it loses them too and changes the
  ops of 150 draws. Dropping `rotationPolicy` from `mutate` was rejected, because it gains
  nothing over the overlay and changes the ops of 1877 draws. Widening `ready` to accept
  `SecretMismatch` was rejected, because it would pass a Certificate whose key never
  matches its spec. Changing the collector was rejected, because an update reaches that
  state with nothing for the collector to delete.
- **D88 A run records each delete of reconciler-fuzzer's garbage collector.** Hunt seed 1043 failed
  where cert-manager re-pointed a Secret's ownerReference just before the collector's
  delete, which then failed its precondition. The collector's writes bypass the proxy,
  and it dropped a 404 or a 409 silently, so the evidence showed the Secret go but not
  who deleted it. On envtest a run now writes `collector.jsonl` (§5.8), a line per delete
  the collector sends, a retry included, and a report names it. Adding the lines to
  `requests.jsonl` was rejected, because the checks read it as the target's traffic.
  `objects.jsonl` holds versions, and a delete that lost its race changes none. Owner
  reads are not recorded, because a sweep reads every owner on each event and the
  delete's line says what the read found. An empty file says the collector tried no
  delete. A kubeconfig cluster runs its own garbage collector, so reconciler-fuzzer writes no file
  there. The lines are kept in memory and written as the run ends, as the other
  recordings are.
- **D89 A findings draft carries its reproducers.** Upstream readers do not run reconciler-fuzzer.
  So a draft's directory holds, beside the reconciler-fuzzer sequence, a program that reproduces
  the find on any cluster, unless only reconciler-fuzzer's proxy can inject its faults. The first
  such program needs only client-go, so it lives in the root module, where `go build`,
  `go vet` and `make test` reach it. A module of its own, as each spike has, was
  rejected: it would pin the same libraries a second time. `make test` loads each
  draft's sequence against the example it names, and judges its CRs by the example's
  CRD, as it does each example's sequences. It fails once either refuses the sequence. With
  no draft left, both checks fail and say what to change.
- **D90 The wait after a `deleteManaged` lasts until the target has had its time after
  each fault that stopped before the op.** The toy, restarted into a 4 s fault on
  ConfigMaps, failed its list three times and retried 4.4 s after the fault stopped.
  Settle waits converged in between, because the Widget stayed ready. So D44 owed nothing
  at a later `deleteManaged`, and G7 failed the correct toy in 4 of 6 runs (#74). Deferring
  only G7's verdict was not enough. With G7 silenced, P1 failed at the same checkpoint, and
  with P1 gone, G1 counted the toy's late create in the teardown's quiet window. So that
  wait does not converge before as long past each fault that stopped by the op as the
  fault lasted, and `T_settle` more, whatever converged since. It gives up no sooner than
  `T_stable` after that, and G7 judges there, so B8 still fails G7 after a fault. `Ready`
  shows what a CR op changed, and nothing shows what a `deleteManaged` changed. Owing that
  time to every wait was rejected, because it lengthens every wait after a fault. A fixture
  op and a fixture's restore keep D44's rule, though `Ready` does not show them either.
- **D91 A fault excuses a `checkpoint` or `end` property as it excuses G4, and a `recreate`
  whose old CR a fault keeps stops the run.** A 500 on the toy's Widget patches, active
  through a `recreate`, failed the patch that clears its finalizer. With `spec.count` 0,
  the old CR outlasted the recreate's wait, no check reported it, and the run ended as a
  harness error. With `spec.count` 1, P1 failed the correct toy where that wait ended: the
  toy had deleted its child and could not write `status.ready`. A `delete` in place of the
  `recreate` failed P1 at the same point. Under 500s on its ConfigMap creates, the toy
  could not replace a child a `deleteManaged` deleted, and P1 failed at a wait that
  converged under the fault. The checks excuse a target while a fault is active or it is
  still owed time to recover from one (D44), and properties did not. D85 rejected skipping
  properties at every wait a fault excuses. The maintainer chose it in #75, and the run
  notes each checkpoint skipped. A wait that converged under an active fault is skipped
  too, because a target that cannot act on a change, or cannot see it, converges on a
  stale state. A property evaluated `always` keeps every event, because a fault's
  transient states are what it exists to catch (§5.6). The rule for a held request (D84)
  now skips nothing this one does not: a held request keeps its fault's window open, and
  a release within `T_stable` leaves time owed. It stays for its more specific note. The
  harness error also skipped the teardown's recovery wait and checks. Under 500s on the
  toy's ConfigMap creates across an update and a `recreate`, it hid B13, whose finalizer
  never clears, and reconciler-fuzzer exited 2. Such a `recreate` now stops the run with a note. The
  teardown clears the fault, gives the target its time to recover and judges it. B13 then
  fails G4 in the wait after the last fault stopped, naming the finalizer, in RUNS_B13
  runs, and the correct toy passes RUNS_OK. The first sequence passes RUNS_R0 with the
  note, and the second RUNS_R1. A CR that stays where nothing excuses the target fails G3
  or G4 there. One that no check reports stays a harness error, which only checks that
  disagree reach. Creating the CR before a later op, as a deleted fixture is restored,
  was rejected: the create lands in that op's window, and a fault lasting to that op keeps
  the old CR again. Skipping the create and going on was rejected, because a later op on
  the CR then finds the old one or none, and errs. A wait that converged after the faults
  stopped is still judged, as D44 has it. After 500s on a restarted toy's ConfigMap lists
  and watches, its informer backed off past the next wait, which converged on the stale
  Widget, and P1 failed the correct toy in 5 of 6 runs. #74 meets the same limit in G7.
- **D92 G2 counts only the changes the target's writes explain.** On kind, just after the
  owner's CRD was installed, the garbage collector deleted an owned Deployment 2.3 s after
  its owner. The delete landed in the quiet window, and G2 failed 5 of 10 runs (#72). G2
  now counts a version only where the target wrote to that object, or to its collection,
  inside the window and no later than the Observer saw the version (§6). Every write of the
  target's passes the proxy, so B1, B2 and B6 still fail G2. A managed object the cluster
  changes because the target wrote another, such as a ReplicaSet of a Deployment the target
  updated, no longer counts, and G1 still counts the write. Waiting out the garbage
  collector on a kubeconfig cluster was rejected: it slows every run and still guesses the
  latency. So was a wider `stable`, the workaround the docs gave. Matching a write to a
  version by resourceVersion was rejected, because the proxy would have to decode every
  response. A bound on how long after a write its version may come was rejected as another
  guess. The settle wait still ends only after `T_stable` in which nothing changed, so a
  checkpoint follows the cluster's changes.
- **D93 The nightly reads summary.json to tell a find from an error.** `make` always
  exits 2 on any recipe failure, so the nightly workflow could not read reconciler-fuzzer's exit
  code from make. The `must-pass` macro captures it, each quickstart exits 2 on its
  own failures, and the report step reads `summary.json` to file under `nightly-find-`
  or `nightly-error-`. The step's `if:` needed `failure()` to run after a failed step.

- **D94 Generated faults inject at most one fault per sequence.** A sequence covers one
  span. The fault matches one resource plural (from the primary's CRD or a managed kind's
  built-in table) and optionally one verb, and acts as an error or a delay. The generator
  inserts the fault after `checkpointed` has placed settles, so the fault's span starts at
  an eligible op (preceded by a settle) and ends at a settle it inserts. A target may set
  `generate.faults: false` to keep its seeds stable while it does not handle faults.
- **D95 The README is short, and `docs/ci.md` holds the CI detail.** The README ran to
  3,800 words, and read as dense and spec-like to a fresh-context editorial review. It now
  runs to half that. What D54, D68, D80 and D81 say the README holds, the replay against
  the base branch, the CI recipe, the tools module and the `go test` recipe, `docs/ci.md`
  holds instead, with the tests that hold them. One table gives each check and its usual
  cause, and a test holds its rows to §6. The quick start shows reconciler-fuzzer's output from its
  first run on, without the deadline line, and says the toy's bugs are ones you switch on,
  in place of D79's statement that every find shown is planted. The limits' opening also
  states that envtest runs no Pod, beside D76's three. Minimizing and the `jq` recipe move
  to `docs/failures.md`. The limits stay before Install, so that a team learns whether its
  controller fits before it installs anything.
- **D96 `docs/checks.md` gives each check a section.** The README's table of checks moved
  there, with what `docs/failures.md` said of each check's message and the `jq` recipe for
  a property, so that every page can link a check it names. The README's limits became
  one list.
- **D97 Every invocation that draws ends with the baseline.** Of 1,000 seeds per example,
  13 to 14% drew a `deleteManaged` and 11 to 13% updated a CR that had settled. So five
  runs missed a `deleteManaged` about half the time, and a controller that does not watch
  a kind it manages, or ignores a spec change, passed. D83 had the README tell a user to
  write a sequence per managed kind and per property. The baseline does that for every
  target. It updates every mutable field, because reconciler-fuzzer cannot tell which a
  property reads. So a target without `generate.mutate` gets an update, and a settle wait,
  per spec path it can draw. The updates accumulate, as a user's edits do, and a value
  under which a later change leaves the controller waiting belongs out of the overlay, as
  it does for drawn updates. Weighting the draws toward these ops was rejected: it changes
  every golden seed and still guarantees neither. The baseline runs last, so a seed keeps
  its run number and the negative controls still fail on their first run. A hunt runs
  each seed in an invocation of its own, so seeds leave the baseline out once one
  invocation has passed it. cert-manager's `heal.json` only deleted the objects the
  baseline deletes, so it went.
- **D98 A draw picks each legal op with equal chance, and makes at least one after the
  create.** rapid's `SampledFrom` and `IntRange` favor low indices and small numbers,
  which suits rapid's own shrinking. reconciler-fuzzer shrinks with its own pass (§5.5),
  so the lean only skewed the mix. The ops listed first came up about twice as often as
  the sixth, and about a quarter of draws made no op after the create. Generation now
  draws each op, and what it acts on, from rapid's unbiased bits. Over 1,000 seeds per
  example, the seeds that draw a `deleteManaged` went from 13–14% to 18–20%. Those that
  update a CR right after an op that settles went from 16–19% to 21–23%. Those with no op
  but settles after the create went from 24–28% to 4%. `deleteManaged` was legal only
  after an op that settles, so it was still drawn about half as often as the other ops
  (D99). No seed now draws a lone create, so cert-manager's negative control runs
  `sequences/create.json`, as external-secrets' runs a file (D41). Longer draws took the
  example tiers from 8m42s and 7m36s on CI to 10m37s and 10m04s. Running four seeds saved
  under 30 s, so §11's budget for them went from ten minutes to twelve, and cert-manager's
  job timeout from 12 to 14 minutes.
- **D99 A draw may make a `deleteManaged` after any op while a CR is live.** Half of the
  CR ops a draw makes skip their settle, so a `deleteManaged` that waited for an op that
  settles was drawn half as often as the other ops. Generation now puts a settle wait
  before a `deleteManaged` that follows an op that does not settle, as it does before a
  `restart`. G7 can then judge it, and the managed objects are there to delete. Over seeds 1
  to 1,000, the seeds that draw a `deleteManaged` went from 18–20% to 32–34% for the toy
  and each example, against 34–38% for a delete and 38–40% for a recreate. Cert-manager's
  example seeds no longer draw a recreate, which `rotation-never.json` runs. The example
  tiers took 11m03s and 10m23s on CI, inside §11's twelve minutes.
- **D100 No drawn fault is active during a `deleteManaged`.** G7 does not judge a
  `deleteManaged` while a fault is active, and about a quarter of drawn `deleteManaged` ops
  had a fault that started at them. A fault no longer starts at one. One that starts
  earlier ends before it, because an op that settles precedes every `deleteManaged` (D99),
  and D90 gives the target its time after that fault. Over seeds 1 to 1,000, the seeds that
  draw a fault went from 38–41% to 34–37%: about one seed in ten had no other start. So no
  check now sees a target recreate a deleted child under a fault.
