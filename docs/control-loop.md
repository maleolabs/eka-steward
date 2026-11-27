# eka-steward control loop

`internal/loop` is the steward's deterministic control loop: the package
that binds the pure decision kernel (`internal/decision`), the single EKA
write path (`internal/ekawrite`), the cgroup-backed sizing
(`internal/sizing`) and a persisted resume state into one step-at-a-time
run.

It keeps the steward's core split (`docs/determinism.md`): **code decides
*which* / *when* / *if*; the LLM decides *what***. The loop chooses the
next candidate in code (`Plan`) and only runs the worker/LLM inside
`Dispatcher.Dispatch` — never to make a control decision.

## Lifecycle

```
Acquire(lock)            # one steward per EKA_HOME / state dir
Load(checkpoint)         # resume: reconcile in-flight intents, skip work done
Resume → reconcile each InFlight intent (no dispatch, idempotent)
loop:
    Plan   → deterministic step sequence over Source + state (no dispatch)
    Step   → plan + dispatch ONE, checkpointed (intent → dispatch → apply → done)
Run      → Resume, then Step until nothing is dispatchable
Release(lock)
```

The default loop is sequential: `Run` drives one `Step` at a time. When
`Config.QueueLimit > 0` it instead runs through the bounded worker pool
(`internal/pool`): up to `sizing.Workers(...)` steps run concurrently, the queue
is bounded, and admission overflow is deterministic. See
[`backpressure.md`](backpressure.md).

## Seams

The loop names three interfaces, so the effectful edges are injectable and
testable:

| Seam | Role | Production | Tests |
| --- | --- | --- | --- |
| `Source` | reads EKA **read-only**, yields `[]decision.Candidate`; the optional `GraphSource` capability yields the dependency graph | `StoreSource` (api.Knowledge, one project) | fixed `fixtureSource` / `graphSource` |
| `Dispatcher` | the worker/LLM seam — the only place a worker runs | (deferred ACP worker dispatch) | `spyDispatcher` that records calls |
| `Writer` | the single EKA write path | `EkawriteWriter` → `internal/ekawrite` | a fake / nil |

- **`StoreSource`** maps each work item to a candidate: identity is the
  qualified line form `"<namespace>/<type>:<id>"`, state is the unit's
  `execution-state`. Units that own no execution-state rank as
  non-dispatchable. Priority and created-seq stay `0` in this cut (EKA work
  items carry no such field), so ordering is by state rank then ID — still a
  deterministic total order.
- **`EkawriteWriter`** expresses the loop's forward mutation as a state
  transition (`Advance`) and the resume mutation as an idempotent reconcile
  (`Reconcile`), both routed through `internal/ekawrite`. The loop never names
  an `api.Authoring` method itself; the boundary test
  (`cmd/boundary_arch_test.go`) would fail if it did.

## Pure `Plan` vs `Step`

- `Plan(ctx) ([]Step, error)` is a **pure selection**. It reads the Source
  and the persisted state, orders the candidates with
  `internal/plan.Selectable` (topological over `depends-on`, then the
  container/`decision` tie-break — `docs/selection.md`), drops non-dispatchable
  states (`planned`, `todo` only) and blocked candidates, and returns one
  `Step` per remaining candidate, in order. It **never calls the `Dispatcher`**;
  the LLM-call spy test fails if it does. The same source set + state yields
  byte-identical `Step` JSON across runs and any input permutation (the order
  comes from the planner, not from the slice or map iteration). With no
  dependency edges the order is identical to `internal/decision.Order`.
- `Step(ctx) (Step, bool, error)` plans the next candidate
  (`plan.SelectNext`, the head of `Selectable`) and runs it as a
  **checkpointed transaction**: write
  the intent ahead of dispatch, dispatch, apply the write through `Writer`
  (when `Config.DispatchState` is set), then move the intent to done. It
  returns `ok=false` when nothing is dispatchable.
- `Resume(ctx)` reconciles every in-flight intent left by a previous run
  **without dispatching** (idempotent: a transition at/after its target and a
  replay of an identical publish are no-ops), then clears it.
- `Run(ctx)` runs `Resume`, then drives the loop to a stop. Sequentially (the
  default, `QueueLimit == 0`) it runs `Step` until nothing is left; with
  `QueueLimit > 0` it admits one bounded batch through the worker pool
  (`docs/backpressure.md`) and the caller drives repeated `Run`s to quiescence.

Crash-recovery, the intent log, the checkpoint location and the crash matrix
live in [`checkpoint.md`](checkpoint.md). Bounded retry, the per-attempt
timeout, `Loop.Cancel` and the failure→no-partial-write guarantees live in
[`recovery.md`](recovery.md).

`Step` is JSON-able so the exact sequence for a fixture is frozen in
`internal/loop/testdata/steps.golden`:

```json
{ "candidateId": "f", "kind": "dispatch", "priority": 10,
  "state": "planned", "createdSeq": 5 }
```

## Single-instance lock

`Acquire(path)` / `(*Lock).Release()` guard one steward per workspace. A
second `Acquire` on the same path returns `ErrLocked` (check with
`errors.Is`), so the steward can refuse to start. The primitive is split by
build tag so each OS uses its strongest crash-safe mechanism:

| OS | Build tag | Primitive | Crash behaviour |
| --- | --- | --- | --- |
| unix (linux, darwin, \*bsd) | `//go:build unix` | `flock(2)` `LOCK_EX\|LOCK_NB` on the lock file | **crash-safe**: the kernel releases the flock when the process dies, so a killed steward never wedges a restart |
| windows | `//go:build windows` | `os.OpenFile(O_CREATE\|O_EXCL)` + `os.Remove` on release | **limitation**: a crash leaves the lock file behind; a restart after a crash refuses until the file is removed manually |

The lock file's *existence* is not the lock on unix — the flock is. Release
is idempotent.

## cgroup v2 sizing

`CgroupReader` fills the `sizing.Resources` snapshot from cgroup v2 and the
loop composes it with `internal/sizing.Workers` (clamped to
`Config.MaxWorkers`). It reads only:

- `<prefix>/memory.max` — a byte count or `max` (unlimited).
- `<prefix>/cpu.max` — `"<quota> <period>"` in microseconds; `CPUCores =
  quota/period` (fractional quotas are allowed). `"max <period>"` means an
  unlimited quota.

`<prefix>` defaults to `/sys/fs/cgroup`; `Config.CgroupPrefix` overrides it
so tests point at a fixture directory. **Host totals are never read.**

Fallback is deterministic and conservative. A value that is `max`,
missing, unreadable or malformed becomes `0` for that term. A zero term
makes `internal/sizing` clamp to its **1-worker floor**, so an unknown limit
yields exactly 1 worker rather than a guessed (possibly over-large) count.

## State format

The loop persists its checkpoint as versioned JSON, written atomically (temp
file in the same directory, then `rename`) so a torn write never leaves a
half-written file. `Done` is sorted and de-duplicated and the `inFlight`
intent log is sorted by candidate id, so the file is byte-stable for the same
logical state:

```json
{
  "version": 2,
  "done": ["p/sto:i-1"],
  "inFlight": [
    { "candidateId": "p/sto:i-2", "kind": "transition",
      "target": "p/sto:i-2", "to": "todo", "step": { "candidateId": "p/sto:i-2", "kind": "dispatch" } }
  ]
}
```

On start the loop `Load`s it, reconciles every `inFlight` intent, and skips
every id in `done`. A missing file is a fresh `version: 2` state; a
`version: 1` file upgrades to `2` (it has no intent log); any other version is
a deterministic error.

The checkpoint **location**, the write-ahead intent protocol, the reconcile
rules per write class, atomicity/versioning and the crash matrix are specified
in [`checkpoint.md`](checkpoint.md).