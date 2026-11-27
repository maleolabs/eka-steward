# eka-steward retry, cancel & recovery

`internal/loop` bounds and deterministically controls what happens when a
dispatch **fails**, **times out** or is **cancelled**, and guarantees that a
failed worker leaves **no partial EKA write** behind. This document defines the
retry policy, the cancel semantics, the no-partial-write argument, the recovery
metrics, and where each property is enforced.

It extends [`control-loop.md`](control-loop.md) (the loop and its seams),
[`checkpoint.md`](checkpoint.md) (the write-ahead intent protocol this reuses)
and [`backpressure.md`](backpressure.md) (the bounded pool). It is the control
loop's answer to determinism rows 6 and 7 of
[`determinism.md`](determinism.md) (*whether / how many times to retry*,
*whether to cancel an in-flight worker*) — both pure, LLM-free decisions.

## 1. Retry policy

```go
type RetryPolicy struct {
    MaxAttempts int           // TOTAL dispatch attempts for one candidate
    Backoff     time.Duration // deterministic delay between attempts (no jitter)
}
```

- **`MaxAttempts` is the total number of attempts, not the number of retries.**
  `MaxAttempts == 1` means *try once, never retry* — the pre-retry behaviour and
  the semantic of the zero value. `MaxAttempts < 1` is clamped to `1`. `Config`
  defaults (`RetryPolicy{}`) therefore leave the loop byte-identical to before.
- **After `MaxAttempts` attempts all fail**, the candidate is **left un-`Done`**
  — *deferred, never dropped* — and the last dispatch error is returned. The
  next `Run` re-selects it.
- **`Backoff` is deterministic.** There is **no jitter and no randomness**, so
  the retry schedule is reproducible. It is applied through the injectable
  `Config.Sleep` seam (production: `time.Sleep`), so a test retries without
  wall-clock waits while still asserting the exact delay sequence. A
  non-positive `Backoff` retries immediately.
- **Retries are driven by the loop, not the pool.** The loop owns the intent
  log, so it can keep the checkpoint consistent across attempts; the pool knows
  nothing about intents and only runs the task it is handed.

### 1.1 Per-attempt timeout

`Config.AttemptTimeout > 0` bounds each attempt with
`context.WithTimeout(ctx, AttemptTimeout)`. A worker that hangs is **cancelled
at its deadline**; the resulting `context.DeadlineExceeded` is a **retryable
failure** — bounded by `MaxAttempts` like any other error. `0` (the default)
applies no per-attempt deadline (the attempt is bounded only by the caller's
`ctx`). The timeout is a retryable class; **operator cancellation is not** (§2).

### 1.2 Attempt is its own transaction

Each attempt is a self-contained write-ahead transaction
([`checkpoint.md`](checkpoint.md) §2), so the intent log only ever covers the
attempt currently running:

```
for attempt in 1..MaxAttempts:
    beginIntent        # write-ahead intent BEFORE the attempt
    dispatch           # ONE worker call under the per-attempt timeout
    ok  → advance → completeIntent → done
    fail→ rollbackIntent   # NO partial write; item deferred, clean retry
          if attempt == MaxAttempts or ctx cancelled: stop
          record retry (metrics); sleep(Backoff)
```

A failed or timed-out attempt **rolls its intent back** before the next retry,
so a crash between attempts leaves **no intent** and the item is re-dispatched
cleanly next run. Only a crash *during* an attempt leaves an intent for resume
to reconcile. The retry counter is reported exactly once per re-dispatch:
`MaxAttempts` failed attempts yield `MaxAttempts − 1` retries.

## 2. Cancel semantics

`Loop.Cancel()` **cancels the current batch**: `Run` installs a cancellable
batch context and every in-flight dispatch of that `Run` derives from it, so
`Cancel` cancels all of them (cancel-all, not per-id).

On cancel, deterministically:

- The in-flight worker's `ctx` is **cancelled**; the dispatch returns
  `context.Canceled`.
- **No retry** — cancellation is *not* a retryable class (§1.1).
- The attempt's intent is **rolled back** → **no EKA write**.
- The candidate is **left un-`Done`** (deferred) → re-selected by the next
  `Run`.
- **`Run` returns `nil`** — a cancelled batch is a *clean stop*, not an error.
  The caller distinguishes a cancel from completion by observing that work
  remains (the candidate is still selectable).

`Cancel` is **idempotent**, safe to call concurrently with `Run`, and a no-op
when no batch is active. A caller that drives `Step` directly (rather than
`Run`) cancels through **its own context**: `Step`/`Run` take a `ctx` and the
in-flight dispatch derives from it, so cancelling that `ctx` has the same
effect.

Because the cancelled candidate was never written and never marked `Done`, the
same starting state produces the **same outcome across N runs** — cancel is
deterministic.

## 3. Failure → no partial EKA write

A failed worker never leaves a partial EKA write, by construction:

| Failure point | What happens | EKA write |
| --- | --- | --- |
| dispatch fails / times out / cancelled | the attempt's write-ahead intent is rolled back | **none** |
| dispatch succeeds, `Advance` (apply) fails | the intent stays in flight; the next `Run`'s `Resume` reconciles it (idempotent) | at most one, on reconcile |
| crash after intent / after dispatch | `Resume` reconciles the persisted intent — no re-dispatch | exactly one |
| crash after apply | `Resume` reconcile is a no-op (line at/after target; publish replay gate) | exactly one (not repeated) |

The **forward mutation** is applied **only after a successful dispatch**, once,
under `writeMu`, and the item moves to `Done` only after the apply completes. A
candidate that exhausts its retries is left **un-`Done` with no intent**, so the
checkpoint is consistent (`Done` without it, empty `InFlight`) and a later run
recovers it cleanly. This is the same intent/apply/rollback protocol
[`checkpoint.md`](checkpoint.md) specifies — recovery adds no second write path.

## 4. Recovery metrics

The retry/cancel counters are reported through the shared observability seam
`pool.Metrics` (injected as `loop.Config.Metrics`):

| Method | Meaning |
| --- | --- |
| `Retries(n)` | CUMULATIVE dispatch retries the loop attempted (re-dispatch after a failed attempt) |
| `Cancellations(n)` | CUMULATIVE cancel signals delivered while a batch was active (`Loop.Cancel`) |

`pool.Recorder` exposes both via `Snapshot()`. The pool itself never calls these
— the loop reports its own policy. This is a seam only: **no logging sink, no
exporter** (that is `sto:steward-observability`).

## 5. Guarantees

**Guaranteed (tested, `-race`):**

- **Bounded retry.** At most `MaxAttempts` attempts per candidate, identical
  across runs; exhausted ⇒ un-`Done`, never dropped.
- **Deterministic retry.** Attempt count and the backoff delay sequence are a
  pure function of the inputs (no jitter, no clock in the decision).
- **Timeout is retryable and bounded.** A per-attempt deadline fails a hung
  worker as a retryable class, still bounded by `MaxAttempts`.
- **Cancel is clean.** In-flight `ctx` cancelled, no retry, intent rolled back,
  candidate un-`Done`, `Run` returns `nil`; identical across N runs.
- **No partial write.** `0` applies on failure/timeout/cancel, exactly `1` on
  eventual success (including after retries).

**Best-effort / out of scope:**

- **Worker side effects are not checkpointed.** Only the EKA write (the
  conserved effect) is protected; a retry re-runs the worker, and a crash
  *after intent, before dispatch* treats the intended write as owed (see
  [`checkpoint.md`](checkpoint.md) §6).
- **`Backoff` is not interruptible mid-sleep.** The loop re-checks the context
  at the top of the next attempt, so a cancel during backoff is honoured before
  the following attempt, not during the sleep.
- **Error classes are not differentiated yet.** Every dispatch error is
  retryable up to `MaxAttempts`; per-error-class policy is a later concern.

## Where this is enforced

| Property | Test |
| --- | --- |
| retry bounded, exhaust ⇒ un-Done | `internal/loop` `TestRetryExhaustedLeavesCandidateUndone`, `TestRetryZeroValueIsNoRetry` |
| retry succeeds, one apply | `TestRetrySucceedsOnFirstAttempt`, `TestRetrySucceedsAfterFailure` |
| retry deterministic | `TestRetryIsDeterministicAcrossRuns`, `TestRetryBackoffSequenceDeterministic` |
| timeout retryable + bounded, no write | `TestAttemptTimeoutIsRetryableAndBounded` |
| cancel: ctx cancelled, no write, deterministic | `TestCancelCancelsInFlightWithoutWrite`, `TestCancelIsDeterministicAcrossRuns`, `TestPooledCancelStopsBatch` |
| cancelled candidate reselectable | `TestCancelledCandidateReselectedOnNextRun` |
| no partial write, store-backed | `TestRetryNoPartialWriteStoreBacked` (real `internal/ekawrite` + temp store) |
