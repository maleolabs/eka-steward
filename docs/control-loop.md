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
Load(state)              # resume: skip work already done
loop:
    Plan   → deterministic step sequence over Source + state (no dispatch)
    Step   → plan + dispatch ONE, then persist the advance
Run      → Step until nothing is dispatchable
Release(lock)
```

A loop is sequential by design. Concurrency, backpressure admission and a
real worker pool are a separate work item; this package runs one step at a
time.

## Seams

The loop names three interfaces, so the effectful edges are injectable and
testable:

| Seam | Role | Production | Tests |
| --- | --- | --- | --- |
| `Source` | reads EKA **read-only**, yields `[]decision.Candidate` | `StoreSource` (api.Knowledge, one project) | fixed `fixtureSource` |
| `Dispatcher` | the worker/LLM seam — the only place a worker runs | (deferred ACP worker dispatch) | `spyDispatcher` that records calls |
| `Writer` | the single EKA write path | `EkawriteWriter` → `internal/ekawrite` | a fake / nil |

- **`StoreSource`** maps each work item to a candidate: identity is the
  qualified line form `"<namespace>/<type>:<id>"`, state is the unit's
  `execution-state`. Units that own no execution-state rank as
  non-dispatchable. Priority and created-seq stay `0` in this cut (EKA work
  items carry no such field), so ordering is by state rank then ID — still a
  deterministic total order.
- **`EkawriteWriter`** expresses the loop's one mutation as a state
  transition and routes it through `internal/ekawrite`. The loop never names
  an `api.Authoring` method itself; the boundary test
  (`cmd/boundary_arch_test.go`) would fail if it did.

## Pure `Plan` vs `Step`

- `Plan(ctx) ([]Step, error)` is a **pure selection**. It reads the Source
  and the persisted state, orders the candidates with
  `internal/decision.Order`, drops non-dispatchable states (`planned`,
  `todo` only) and returns one `Step` per remaining candidate, in order. It
  **never calls the `Dispatcher`**; the LLM-call spy test fails if it does.
  The same source set + state yields byte-identical `Step` JSON across runs
  and any input permutation (the order comes from `decision`, not from the
  slice or map iteration).
- `Step(ctx) (Step, bool, error)` plans the next candidate
  (`decision.SelectNext`), dispatches it, records the write through `Writer`
  (when `Config.DispatchState` is set) and marks it done in the state store.
  It returns `ok=false` when nothing is dispatchable.
- `Run(ctx)` drives `Step` until nothing is left or `ctx` is cancelled.

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

## State format (and what is deferred)

The loop persists a minimal resume state as versioned JSON, written
atomically (temp file in the same directory, then `rename`) so a torn write
never leaves a half-written file. `Done` is sorted and de-duplicated, so
the file is byte-stable for the same completed set:

```json
{ "version": 1, "done": ["p/sto:i-1"], "cursor": "" }
```

On start the loop `Load`s it and skips every id in `done`. A missing file is
a fresh `version: 1` state; an unsupported version is a deterministic error.

This is **persist + reload only**. Crash-safe reconciliation of a step that
was dispatched but not yet recorded (the checkpoint/resume hardening) is a
**separate work item, `sto:checkpoint-resume`**, and is deliberately not
implemented here. Until then the loop records completion after dispatch; a
crash in between re-dispatches that one item on restart.