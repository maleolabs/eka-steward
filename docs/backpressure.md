# eka-steward backpressure & bounded worker pool

`internal/loop`'s `Run` can drive candidates through a **bounded worker pool**
(`internal/pool`). The pool turns the sizing decision (`internal/sizing`) into a
hard concurrency bound and an explicit, deterministic admission policy, so the
steward's resource use is bounded by construction and saturation is observable —
never a nondeterministic drop.

This extends [`control-loop.md`](control-loop.md) (the loop and its seams) and
[`checkpoint.md`](checkpoint.md) (the write-ahead checkpoint the pooled workers
still honor). The count the pool bounds to comes from [`sizing.md`](sizing.md).

## 1. Model

A pool owns exactly three things and nothing else:

| Concern | Bound | Source |
| --- | --- | --- |
| concurrent dispatch | `MaxWorkers` | `internal/sizing.Workers(...)` (never a raw config number) |
| queued-but-not-started jobs | `QueueLimit` | operator config (`loop.Config.QueueLimit`) |
| saturation signal | `pool.Metrics` | injected seam (no-op by default) |

It holds no persistence, no clock and no randomness. Given the same jobs
submitted in the same order it admits the same set and rejects the rest, so run
outcomes are reproducible.

```
                Submit(task)                  Drain
 candidates ──►  [ bounded queue ] ──►  N × worker goroutine ──► task
 decision.Order   cap = QueueLimit        N  = MaxWorkers
                       │
                       └─ full ⇒ ErrQueueFull (stop feeding, drain)
```

## 2. `MaxWorkers` comes from sizing

`runPooled` never passes `Config.MaxWorkers` straight to the pool. It calls
`Loop.Workers()`, which reads the cgroup v2 snapshot, applies
`internal/sizing.Workers(res, {MaxWorkers, CostCeiling})` and clamps to the
configured maximum. The pool's worker count is that result, so **the pool can
never run more workers than the sizing policy permits** — even when
`Config.MaxWorkers` is set higher than the machine snapshot warrants.

Example (the test fixture): a 16-core / 16-GiB snapshot yields a base of
`min(16/2, 16GiB/4GiB) = min(8, 4) = 4`. With `Config.MaxWorkers = 8` the pool
runs **4** workers — the observed peak in-flight is 4, never 5–8.

An unknown / `max` / missing cgroup limit falls back to the 1-worker floor, both
in sizing and therefore in the pool (see [`control-loop.md`](control-loop.md)).

## 3. The deterministic overflow rule

`QueueLimit` is the pool's bounded queue capacity **and** the number of
candidates one `Run` admits. `Submit` is **non-blocking**: it returns
`pool.ErrQueueFull` the moment the queue is full. The rule the loop implements:

> `runPooled` orders the dispatchable candidates with `internal/decision.Order`
> and submits them in that exact order. The first `QueueLimit` are admitted; the
> next `Submit` returns `ErrQueueFull`, at which point the loop **stops planning
> additional items and drains** the admitted ones. Every candidate past the limit
> is left undone (not recorded done) for the next `Run`.

Consequences, all deliberate:

- **Deterministic accepted set.** The admitted set is the first `QueueLimit`
  candidates of the pinned total order — a pure function of the candidate set,
  `QueueLimit` and the persisted state. It is identical across runs and across
  any permutation of the source input (the order comes from `decision`, not from
  slice or map iteration).
- **No nondeterministic drop.** A refused candidate is *deferred*, not lost: it
  stays out of `Done`, so the next `Run`'s `Plan` re-selects it. Backpressure
  here means "defer to the next admission window", never "discard under load".
- **One admission window per `Run`.** With `QueueLimit > 0` a single `Run`
  processes at most `QueueLimit` candidates and returns when that batch is
  drained. The caller drives repeated `Run`s to quiescence (the load test does
  exactly this: 200 candidates, `QueueLimit = 8` → 25 bounded batches).

`loop.Config.QueueLimit == 0` (the default) **disables the pool** and keeps the
original sequential path (§6).

## 4. The metrics seam

`internal/pool.Metrics` is a minimal, dependency-free saturation seam — plain
`int` setters, no metrics stack, no labels, no timers. The observability work
item (`sto:steward-observability`) will adapt it; the pool itself imports only
`context`, `errors` and `sync`.

```go
type Metrics interface {
    QueueDepth(n int)   // CURRENT queued-but-not-started jobs
    InFlight(n int)     // CURRENT executing jobs
    Dispatched(n int)   // CUMULATIVE jobs started
    Rejected(n int)     // CUMULATIVE overflow events
    PeakInFlight(n int) // highest InFlight observed
}
```

The default is `pool.NopMetrics{}` (every method a no-op). `pool.Recorder` is an
in-memory implementation for tests and for a caller that wants to read the
numbers back (`Recorder.Snapshot()` also exposes `PeakQueueDepth`). The metrics
callbacks are invoked outside the pool's internal mutex, so a slow recorder can
never stall admission.

This is the seam only: the pool builds no logging, no exporter and no dashboard.

## 5. Bounded-resource guarantees

**Guaranteed (tested, `-race`):**

- **Concurrency ≤ sizing.** At most `MaxWorkers = sizing.Workers(...)` tasks run
  at once (`TestPooledRunNeverExceedsSizingWorkers` blocks every dispatch and
  asserts the observed peak equals the sizing count, never more).
- **Queue depth ≤ `QueueLimit`.** The queue is a buffered channel of exactly
  `QueueLimit`; the `PeakQueueDepth` metric never exceeds it
  (`TestPooledRunMetricsShowBoundedSaturation`).
- **Accepted set is deterministic.** Identical across 20 runs and shuffled
  inputs (`TestPooledRunDeterministicOverflow`, `TestPoolRejectsWhenFullDeterministic`).
- **Exactly-once processing.** Every admitted candidate is dispatched and applied
  exactly once, and the checkpoint ends consistent: `Done` holds every admitted
  id, `InFlight` is empty (`TestPooledRunLoadBoundedAndExactlyOnce`).
- **Checkpoint safety under concurrency.** Every `Store` mutation
  (`beginIntent`/`rollbackIntent`/`completeIntent`/`markDone`) is serialized
  under one mutex, and every `Writer` mutation under another, so overlapping
  workers never lose an intent or a `Done` entry and the single EKA write path
  is never entered concurrently. The dispatch itself is **not** serialized — the
  workers overlap on the worker/LLM seam, which is the point.

**Best-effort / out of scope:**

- The pool **only runs what it is given** and surfaces the first task error; it
  holds no retry or cancel policy of its own. Bounded retry, per-attempt
  timeout and `Loop.Cancel` are the control loop's
  ([`recovery.md`](recovery.md)), and the loop's `Retries` / `Cancellations`
  counters ride the same `pool.Metrics` seam.

## 6. Backward compatibility

- `Config.QueueLimit == 0` ⇒ the sequential path (`runSequential`): one step at
  a time, byte-identical to the pre-pool loop. Every existing
  golden/checkpoint/crash-resume test runs unchanged on this path.
- `MaxWorkers == 1` (whether pooled or sequential) ⇒ a single worker processing
  the admitted order FIFO, so `Run` emits the **same** byte-identical step
  sequence as `Plan` (the golden contract): `TestPooledRunSingleWorkerMatchesPlan`.

## Where this is enforced

| Property | Test |
| --- | --- |
| concurrency ≤ sizing | `internal/loop` `TestPooledRunNeverExceedsSizingWorkers` |
| deterministic overflow (accepted set) | `internal/loop` `TestPooledRunDeterministicOverflow`, `internal/pool` `TestPoolRejectsWhenFullDeterministic` |
| queue depth bounded, saturation observable | `internal/loop` `TestPooledRunMetricsShowBoundedSaturation`, `internal/pool` `TestPoolQueueDepthNeverExceedsLimit` |
| bounded load, exactly-once | `internal/loop` `TestPooledRunLoadBoundedAndExactlyOnce`, `internal/pool` `TestPoolRunsEveryJobExactlyOnce` |
| single worker == golden plan | `internal/loop` `TestPooledRunSingleWorkerMatchesPlan` |
