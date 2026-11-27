# eka-steward observability

`internal/obs` is the steward's observability surface. It turns the loop's
observability seams into a structured **step log**, the **metrics**, and a
deterministic **run report**, and it **redacts secrets** before any log or
report byte is written. It is stdlib-only (`log/slog`, `encoding/json`,
`sync`) and adapts the pool's saturation seam (`internal/pool.Metrics`) — the
pool and the loop take no logging dependency.

This extends [`control-loop.md`](control-loop.md) (the loop's seams),
[`backpressure.md`](backpressure.md) (the saturation metrics) and
[`recovery.md`](recovery.md) (retry/cancel counters). It is enforced by tests,
not on the honour system.

## 1. Wiring: one Recorder

`obs.Recorder` implements **both** seams, so it is wired into both fields of
the loop's config:

```go
rec := obs.NewRecorder(obs.Config{
    Driver: "claude",             // driver label (operator config)
    Role:   "author",             // worker-role label
    Secrets: []string{"$GITHUB_TOKEN-value"}, // optional deny-list
}, obs.NewLogger(os.Stderr))      // slog JSON; nil = metrics/report only

loop.New(src, disp, writer, state, loop.Config{
    Metrics:  rec,                // pool.Metrics: saturation + retry/cancel
    Observer: rec,                // loop.Observer: step + write events
    Now:      clock,              // optional deterministic clock
})
```

`loop.Config.Observer` is notified **once per completed or failed step**
(`Step` event) and **once per applied EKA write** (`Write` event). It is an
observability seam: it never feeds a control decision and is safe for
concurrent use (the pooled path calls it from worker goroutines).
`loop.Config.Now` is the injectable clock used to stamp each step's duration;
nil uses `time.Now`. A test injects a deterministic clock so the run report is
byte-stable.

## 2. Log record shape

The `Logger` writes newline-delimited JSON via `log/slog`'s JSON handler. One
record per loop step:

```json
{"time":"2026-11-27T10:00:00Z","level":"INFO","msg":"loop step",
 "driver":"claude","role":"author","outcome":"ok",
 "candidateId":"p/sto:i-1","stepKind":"dispatch",
 "attempt":1,"durationMs":12}
```

| Field | Meaning |
| --- | --- |
| `driver` | driver label (operator config; the loop owns no driver registry) |
| `role` | worker-role label |
| `outcome` | `ok` (completed) · `failed` (returned an error) · `cancelled` (context cancellation/deadline = the deterministic cancel of `recovery.md`) |
| `candidateId` | the qualified candidate line (`<ns>/<type>:<id>`) |
| `stepKind` | `dispatch` (the only step kind this cut emits) |
| `attempt` | dispatch attempts the step made (0 if it failed before dispatching) |
| `durationMs` | measured with `loop.Config.Now` (injectable) |

The `slog` record also carries `time`, `level` and `msg`; the fields above are
the shipped payload.

## 3. Metric names

`Recorder.Snapshot()` reads the counters. The saturation numbers come from the
pool through `pool.Metrics`; the step count comes from the `Observer`; the
loop reports `Retries`/`Cancellations` through the same `pool.Metrics` seam
(`loop.Config.Metrics`).

| Snapshot field | JSON key | Kind | Source |
| --- | --- | --- | --- |
| `Steps` | `steps` | cumulative | `Observer.Step` |
| `Retries` | `retries` | cumulative | loop → `pool.Metrics.Retries` |
| `Cancellations` | `cancellations` | cumulative | loop → `pool.Metrics.Cancellations` |
| `PeakInFlight` | `peakInFlight` | high-water | pool → `pool.Metrics.PeakInFlight` |
| `QueueDepth` | `queueDepth` | current | pool → `pool.Metrics.QueueDepth` |
| `PeakQueueDepth` | `peakQueueDepth` | high-water | pool → `pool.Metrics.QueueDepth` |
| `InFlight` | `inFlight` | current | pool → `pool.Metrics.InFlight` |
| `Dispatched` | `dispatched` | cumulative | pool → `pool.Metrics.Dispatched` |
| `Rejected` | `rejected` | cumulative | pool → `pool.Metrics.Rejected` |

## 4. Run report shape

`Recorder.Report()` returns the run report: the steps the run took and the EKA
writes it applied. Both lists are **sorted by a total order** (no map
iteration), so the same input yields byte-identical JSON across runs.

```json
{"steps":[
   {"candidateId":"p/sto:i-1","kind":"dispatch","outcome":"ok",
    "role":"author","driver":"claude","durationMs":12}
 ],
 "writes":[
   {"line":"p/sto:i-1","type":"sto","id":"i-1",
    "instanceVersion":0,"kind":"transition","result":"applied"}
 ]}
```

- `Report.MarshalJSON()` → compact bytes; `Report.Write(io.Writer)` → the same
  bytes plus a trailing newline. A nil list is emitted as `[]`, never `null`.
- Step order: `candidateId`, `kind`, `outcome`, `driver`, `role`, `durationMs`.
- Write order: `line`, `kind`, `result`, `instanceVersion`, `type`, `id`.

The writes are the loop's EKA mutations observed through the single write path
([`write-path.md`](write-path.md)): a forward `transition` (`result` =
`applied`) and a review `note` (`result` = the verdict); a resume reconcile
reports the reconcile status (`applied` / `no-op`). `instanceVersion` is `0`
for a transition (a transition re-points the line without bumping the
instance); the true instance version of a publish is produced by
`internal/ekawrite`, outside the loop.

## 5. Redaction rule

Every string field the `Recorder` logs or reports is passed through
`Redactor.Redact` first. The mask is a fixed marker, `■■■`, so the output
leaks no secret length and is deterministic.

Masked:

| Shape | Example |
| --- | --- |
| OpenAI-style key | `sk-<8+ chars>` |
| Google API key | `AIza<8+ chars>` |
| GitHub token | `ghp_<8+ chars>` |
| Bearer token | `Bearer <token>` |
| deny-list literal | any non-empty value in `Config.Secrets` (e.g. an env-var value that must never be logged) |

**Residual risk — pattern-based, not a guarantee.** Redaction catches only
the shapes above and the exact deny-list literals. A secret that does not
match one of those shapes and is not on the deny-list passes through
unchanged. In particular, **a secret embedded in model/worker output is only
masked if it matches one of those patterns** — the steward cannot know which
substrings of free-form LLM text are secret. Treat the worker environment as
the primary containment (`guardrails.md`: no credential variables are
inherited) and redaction as a best-effort backstop.

## 6. Live event stream (`events.subscribe`)

`obs.Stream` is a second loop `Observer` (alongside the `Recorder`) that fans
the seam out to live subscribers as ordered, monotonic, **redacted** events. The
app composes the two with `obs.Composite{rec, stream}` and wires it as
`loop.Config.Observer`, so one loop feeds both the report and the stream.

Event types (`capabilities.events`): `step` · `dispatch` · `write` ·
`approval` · `metrics`.

| Type | Source | Payload (redacted) |
| --- | --- | --- |
| `step` | `Observer.Step` | `{candidateId, kind, outcome, durationMs}` |
| `dispatch` | derived from `StepEvent` | `{candidateId, attempts, outcome}` |
| `write` | `Observer.Write` | `{line, kind, result, instanceVersion}` |
| `approval` | `App.Approve` → `Stream.EmitApproval` | `{requestId, decision}` |
| `metrics` | `Recorder.Snapshot()` after each step | the snapshot counters |

The protocol surface:

| method | params | result |
| --- | --- | --- |
| `steward/events.subscribe` | `{types?, fromSeq?}` | `{subscribed, fromSeq, subscriptionId}` |
| `steward/events.unsubscribe` | `{subscriptionId}` | `{unsubscribed}` |

Each `Event` is delivered as a `steward/event` notification, params
`{seq, type, data}` (no id). One forwarding goroutine per subscription writes
each notification under the server's writer mutex; `seq` preserves order
because the Stream assigns it and delivers under one mutex. `unsubscribe` or
connection close cancels the subscription and closes its channel, so the
goroutine exits (no leak).

**Ordering.** Every real event gets a strictly increasing, gap-free `seq`
assigned under the stream mutex. `seq` is global across all types: a subscriber
that filters by `types` may observe `seq` gaps for the types it did not
request.

**Backpressure (bounded, never blocks the loop).** Each subscriber has a
bounded buffer (`defaultBuffer` = 64). Delivery is **non-blocking**: when the
buffer is full the event is dropped and, once space frees, the subscriber
receives exactly **one** synthetic `gap` event per dropped burst recording the
range `{fromSeq, toSeq}`. The gap event's own `seq` is `fromSeq`, keeping it
ordered after the last delivered event and before the next. Gap events bypass
the type filter (they describe that subscriber's own loss). `fromSeq` replays
from a bounded ring (256 events); a replay larger than the buffer may itself
yield a `gap`.

**Residual risk.** Redaction is the same pattern-based `Redactor` of §5: every
string field of every event payload is passed through it, but a secret that
matches no pattern and is not on the deny-list still passes through.

## Where this is enforced

| Guarantee | Test |
| --- | --- |
| one structured record per step (driver/role/outcome) | `internal/obs` `TestStepEmitsStructuredLog`, `internal/loop` `TestLoopStepEmitsStructuredRecord` |
| metrics reflect steps/retries/cancellations/saturation | `internal/obs` `TestSnapshotCounters`, `internal/loop` `TestObservabilityCountersReflectRun` |
| report lists steps and EKA writes (deterministic; store-backed) | `internal/obs` `TestReportListsStepsAndWrites`, `internal/loop` `TestObservabilityStoreBackedWriteListing` |
| report is byte-identical across runs | `internal/obs` `TestReportDeterministicAcrossRuns`, `internal/loop` `TestObservabilityReportDeterministicAcrossRuns` |
| secret-like input never appears in logs or the report | `internal/obs` `TestRedactionNeverLeaksSecrets` |
| exact, monotonic event sequence for a fake run | `internal/obs` `TestStreamExactSequence` |
| every event payload is redacted | `internal/obs` `TestStreamRedaction` |
| overflow yields one deterministic gap marker (bounded, no hang) | `internal/obs` `TestStreamBackpressureGap` |
| cancel closes the subscriber channel (no goroutine leak) | `internal/obs` `TestStreamCancelClosesChannel` |
| `events.subscribe` emits `steward/event` notifications interleaved with replies | `internal/protocol` `TestEventsSubscribeOverPipe` |