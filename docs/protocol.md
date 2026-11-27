# eka-steward machine protocol

**Protocol version: `1`.** `stewardProtocolVersion = "1"` is negotiated at
`initialize`. The surface evolves **additively only**: a method, param, result
field, event type or error code may be added, but nothing is removed or
re-typed within v1. A future v2 is selected per connection behind the same
dispatcher, so an existing v1 client keeps working unchanged.

`internal/protocol` is the steward's versioned, language-agnostic machine
boundary: JSON-RPC 2.0 over a newline-delimited byte stream (stdio by default,
an optional local unix socket). It is the product boundary — the engine and the
control loop stay in `internal/*`. Every method reuses the loop owned by
`sto:steward-run-command` through the `Engine` seam (`*app.App`); this package
never wires a second loop.

- Governing decisions: `adr:steward-machine-protocol` (defines this JSON-RPC
  method surface as the product's machine boundary) and
  `adr:steward-as-eka-cli-plugin` (hosts the steward as an EKA CLI plugin, which
  is why the transport mirrors eka-mcp framing).
- Run semantics: [`control-loop.md`](control-loop.md). Crash-safe resume:
  [`checkpoint.md`](checkpoint.md). Retry/cancel/no-partial-write:
  [`recovery.md`](recovery.md). Event stream shape: [`observability.md`](observability.md).
  Approval gate: [`approval.md`](approval.md). Determinism rule:
  [`determinism.md`](determinism.md).

## 1. Transport and framing

| Aspect | Rule |
| --- | --- |
| Encoding | JSON-RPC 2.0, **one JSON object per line**, single `LF` delimiter |
| Default transport | stdio — stdin = requests, stdout = responses + notifications |
| Diagnostics | stderr only, **never** protocol bytes |
| Unix socket | `serve --socket <path>` — same framing on the connection |
| stdio alias | `serve --stdio` is an accepted explicit alias for the default |
| Line cap | 64 MiB per message (content, not the framing newline) |
| Overflow | refused deterministically with `-32600` **and the stream resynchronized at the next newline** |
| Flush | every reply is flushed immediately (flush-per-reply) |
| Blank lines | skipped |
| EOF | `Serve` returns (clean stop) |
| Batch | refused: `-32600 "batch requests are not supported"` |
| Notifications from client | a request without an `id` is fire-and-forget; no response |

The cap, resync and flush-per-reply mirror eka-mcp. One writer mutex serializes
every output line, so a reply and a concurrent notification can never
interleave bytes. The transport also exposes `Notify(method, params)` — the
writer the event stream (§4) uses.

An oversized line yields exactly this refusal, then the session continues:

```json
{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"message exceeds the 64 MiB line limit"}}
```

## 2. Lifecycle

```
initialize  →  plan / step / run / resume / cancel / approve / status / events.*  →  shutdown
```

1. `steward/initialize` negotiates the protocol version and returns
   `protocolVersion` + `capabilities` (the method list and event-type list).
2. Every other request before a successful `initialize` is refused with
   `-32001 not_initialized`.
3. `steward/shutdown` cancels every running run, answers `stopping:true`, and
   `Serve` returns after writing the reply.

**Version negotiation.** `initialize` takes `protocolVersions: []string`:

- absent / empty offer → the server assumes its highest supported version (`"1"`);
- an offer containing a supported version → the **highest supported** one in the
  offer (string comparison) is selected;
- an offer with no supported version → `-32002 version_unsupported`.

## 3. Methods

Every method dispatches to the single owned control loop. `params` are decoded
into an object; a non-object (e.g. an array or scalar) is refused with
`-32008`. Absent params behave as an empty object.

| # | Method | Params | Result | Method-specific errors |
| --- | --- | --- | --- | --- |
| 1 | `steward/initialize` | `{protocolVersions?: string[]}` | `{protocolVersion, capabilities:{methods[],events[]}}` | `-32008`, `-32002` |
| 2 | `steward/plan` | `{project*: string, idempotencyKey?}` | `{candidates:[{id,state,priority,kind,createdSeq}], selected, blocked}` | `-32008`, `-32007`, `-32010` |
| 3 | `steward/step` | `{runId*: string, idempotencyKey?}` | `{step: Step \| null}` | `-32008`, `-32007`, `-32004`, `-32010` |
| 4 | `steward/run` | `{project*: string, maxSteps?: int, dryRun?: bool, idempotencyKey?}` | `{runId}` | `-32008`, `-32007` |
| 5 | `steward/resume` | `{runId*: string, idempotencyKey?}` | `{runId, resumedFrom}` | `-32008`, `-32007`, `-32003`, `-32004` |
| 6 | `steward/cancel` | `{runId*: string, idempotencyKey?}` | `{runId, canceled:true}` | `-32008`, `-32007`, `-32003` |
| 7 | `steward/approve` | `{runId?, requestId*: string, decision*: "approve"\|"deny", idempotencyKey?}` | `{requestId, accepted:true}` | `-32008`, `-32007`, `-32010` |
| 8 | `steward/status` | `{runId?: string}` | `{runId, state, counters, lastStep, report}` | `-32008` |
| 9 | `steward/events.subscribe` | `{types?: string[], fromSeq?: uint}` | `{subscribed:true, fromSeq, subscriptionId}` | `-32008`, `-32010` |
| 10 | `steward/events.unsubscribe` | `{subscriptionId*: string}` | `{unsubscribed:true}` | `-32008` |
| 11 | `steward/shutdown` | `{graceful?: bool}` | `{stopping:true}` | `-32008` |

`*` = required (a missing/empty value is `-32008 invalid_params`). Common
errors apply to every method: `-32700` (unparseable line), `-32600` (not a
valid request object), `-32601` (unknown method), `-32001` (before
`initialize`), `-32009` (after shutdown began, except `shutdown` itself).

Notes that the code fixes:

- **`plan` is pure.** It runs the loop's `Plan` pass and never dispatches. It
  returns the dispatchable steps; `selected = len(candidates)` and `blocked` is
  always `0` (the planner yields only dispatchable candidates, so v1 plan does
  not surface blocked work). `candidateInfo` includes `kind` and `createdSeq`
  in addition to `id/state/priority`.
- **`step` runs one checkpointed candidate** (one `Plan`+`Step`). A `runId`
  that matches a run still in `running` state is refused `-32004 busy` (one
  driver per run). An **unknown** `runId` is allowed, so a consumer may drive
  its own tracked id; `step` returns `{step:null}` when nothing is dispatchable.
- **`run` is asynchronous.** It starts a goroutine and returns `runId`
  immediately; progress is observed via `status` and `steward/event`.
  `maxSteps > 0` drives at most that many steps; `dryRun:true` performs the pure
  plan only (no dispatch, no write). A run id is deterministic (§7) with an
  idempotency key, else a per-connection counter `run-1`, `run-2`, ….
- **`resume`** restarts a known run: the engine reconciles any in-flight intent
  before it dispatches (the crash-recovery entry — [`checkpoint.md`](checkpoint.md)).
  Unknown `runId` → `-32003`; already `running` → `-32004`. `resumedFrom` is the
  same run id.
- **`cancel`** is idempotent: a repeat — even on a finished run — returns the
  same `canceled:true`. Unknown `runId` → `-32003`. See §6.
- **`approve`** routes a human decision to the approval channel the loop polls;
  `decision` must be `approve` or `deny` (anything else is `-32008`). `runId`
  is accepted but not required by the handler.
- **`status`** with no `runId` reports the most recent run (`""`/`idle` when
  none). `state` ∈ `running` \| `done` \| `failed` \| `canceled` \| `idle`.
  `counters` is the observability `Snapshot`, `report` the run report
  ([`observability.md`](observability.md) §3–4), `lastStep` the last dispatched
  `Step` or `null`.
- **`events.subscribe` / `events.unsubscribe`** are specified in §4.
- **`shutdown`** cancels all running runs and the engine. `graceful` is
  accepted; v1 shutdown is always graceful (the field is advisory).

`Step` is the loop's JSON-able decision record:
`{candidateId, kind, priority, state, createdSeq}`.

## 4. Notifications

The server emits **one notification method**, `steward/event`, carrying one
stream event. It has **no `id`** and **no `runId`** — the payload is exactly
`{seq, type, data}`:

```json
{"jsonrpc":"2.0","method":"steward/event","params":{"seq":7,"type":"step","data":{...}}}
```

`capabilities.events` advertises the five real types; `gap` is a synthetic sixth
type the server emits on overflow and is deliberately **not** advertised.

| `type` | Source | `data` (redacted) |
| --- | --- | --- |
| `step` | `Observer.Step` | `{candidateId, kind, outcome, durationMs}` |
| `dispatch` | derived from `StepEvent` | `{candidateId, attempts, outcome}` |
| `write` | `Observer.Write` | `{line, kind, result, instanceVersion}` |
| `approval` | `App.Approve` → `Stream.EmitApproval` | `{requestId, decision}` |
| `metrics` | `Recorder.Snapshot()` after each step | `{steps,retries,cancellations,peakInFlight,peakQueueDepth,inFlight,queueDepth,dispatched,rejected}` |
| `gap` (synthetic) | overflow only | `{fromSeq, toSeq}` — inclusive dropped range |

**Ordering.** Every real event gets a strictly increasing, gap-free `seq`
assigned under the stream mutex; delivery to every subscriber happens under the
same mutex, so a subscriber receives events in `seq` order. `seq` is **global
across all types**: a subscriber that filters by `types` may therefore observe
`seq` gaps for the types it did not request (those are not `gap` events).

**Backpressure.** Each subscriber has a bounded buffer (64). Delivery is
**non-blocking** — the control loop never blocks on a slow client. When the
buffer is full the event is dropped and, once space frees, that subscriber
receives exactly **one** synthetic `gap` event per dropped burst recording the
inclusive range `{fromSeq, toSeq}`. The gap event's own `seq` is `fromSeq`, so it
stays ordered after the last delivered event and before the next. Gap events
bypass the type filter (they describe that subscriber's own loss).

**Replay.** `events.subscribe` with `fromSeq > 0` first replays buffered events
with `seq >= fromSeq` from a bounded 256-event ring (filtered by `types`); a
replay larger than the buffer may itself yield a `gap`. `fromSeq: 0` replays
nothing. The reply's `subscriptionId` is what `events.unsubscribe` consumes; an
unknown or empty id is `-32008`. Unsubscribe or connection close closes the
channel and the forwarding goroutine exits (no leak).

**Worked `gap` example.** A subscriber's buffer overflows, dropping real seqs
42 through 57; when space frees it receives one gap, then resumes at 58:

```json
{"jsonrpc":"2.0","method":"steward/event","params":{"seq":41,"type":"step","data":{"candidateId":"p/sto:a","kind":"dispatch","outcome":"ok","durationMs":12}}}
{"jsonrpc":"2.0","method":"steward/event","params":{"seq":42,"type":"gap","data":{"fromSeq":42,"toSeq":57}}}
{"jsonrpc":"2.0","method":"steward/event","params":{"seq":58,"type":"dispatch","data":{"candidateId":"p/sto:b","attempts":1,"outcome":"ok"}}}
```

Redaction is applied to every string field of every payload by the same
pattern-based `Redactor` of [`observability.md`](observability.md) §5; it is a
best-effort backstop, not a guarantee.

## 5. Error codes

JSON-RPC 2.0 `error.code` values. Standard codes are the spec's; `-320xx` are the
steward's deterministic refusal classes.

| Code | Name | Meaning |
| --- | --- | --- |
| `-32700` | parse error | the line is not valid JSON |
| `-32600` | invalid request | not a valid request object (never `id`-less), batch, bad `jsonrpc`, malformed/missing `method` or `id`; also the oversized-line refusal |
| `-32601` | method not found | unknown `method` |
| `-32001` | `not_initialized` | any request before a successful `initialize` |
| `-32002` | `version_unsupported` | `initialize` offered no version the server speaks |
| `-32003` | `run_not_found` | unknown `runId` (`resume`/`cancel`) |
| `-32004` | `busy` | the named run is already executing (`step`/`resume`) |
| `-32005` | `approval_timeout` | **reserved** — defined for the approval bounded-wait timeout; not emitted by v1 |
| `-32006` | `canceled` | **reserved** — defined for a canceled operation; not emitted by v1 (cancel surfaces as `canceled:true`) |
| `-32007` | `idempotency_conflict` | a key was reused with different params |
| `-32008` | `invalid_params` | malformed/missing params (non-object, empty required field, bad `decision`, unknown `subscriptionId`) |
| `-32009` | `shutdown_in_progress` | any request after shutdown began, except `shutdown` |
| `-32010` | `internal` | an engine failure (sanitized to its first line) |

An error response carries `id` (or `null`), `error:{code,message}`, no `result`.

## 6. Concurrency and cancel

- **Lifecycle gate.** Requests are serialized on one input stream; output
  (replies and notifications) is serialized on one writer mutex, so lines never
  interleave.
- **Runs are asynchronous goroutines.** `run` and `resume` launch the engine in
  a goroutine and return immediately. Each run carries its own `context` and
  cancel func. `step` refuses (`-32004`) only the `runId` that is still
  `running`; it is the "one driver per run" guard. The engine's single-instance
  lock ([`control-loop.md`](control-loop.md) §Single-instance lock) is what
  makes genuinely overlapping runs fail deterministically (a run whose lock is
  held ends in `failed`), not protocol-level serialization.
- **`cancel`** cancels the run's context and the engine's current batch
  (`eng.Cancel()`), sets the run state to `canceled`, and returns
  `{canceled:true}`. It is **idempotent** and safe to repeat on a finished run.
  Cancel is a clean stop: no retry, no partial EKA write, the candidate is left
  un-`done` and re-selectable ([`recovery.md`](recovery.md) §2–3). A cancelled
  run's terminal state is `canceled`.
- **Terminal state** is decided deterministically: `canceled` if the run was
  cancelled, else `failed` on error, else `done`.
- **`shutdown`** cancels every running run and the engine, then answers
  `stopping:true`; `Serve` returns after the reply (under `--socket`, the
  listener then stops accepting).

## 7. Idempotency

Methods that accept `idempotencyKey` — `plan`, `step`, `run`, `resume`, `cancel`,
`approve` — record their result under `(method, key)`:

- **Same key + same params** ⇒ the stored result is replayed byte-for-byte
  under the **new** request `id`, with **no second effect** (no second run, no
  second dispatch, no second write). Params are compared canonically (keys
  sorted via a JSON round-trip), so whitespace and key order do not matter.
- **Same key + different params** ⇒ `-32007 idempotency_conflict`, and no
  effect.
- **No key** ⇒ no replay; the call always executes.

The key is scoped per `(method, key)` on the connection. `run` additionally
derives a **deterministic** run id from `(project, key)`:
`run-` + first 12 hex chars of `sha256("run/1\x00" + project + "\x00" + key)`,
so a retry names the same run. Without a key the id is `run-<n>` (a
per-connection monotonic counter).

Read-only `status`, `initialize`, the `events.*` methods and `shutdown` take no
idempotency key.

## 8. Determinism guarantees

- **No timestamps in payloads.** No method result and no event `data` embeds a
  wall-clock value. `durationMs` is a measured span (an input-derived integer),
  not a time.
- **Stable ordering.** `plan` returns the loop's total order
  ([`determinism.md`](determinism.md) §3); the run report's `steps` and `writes`
  lists are sorted by a total order; subscribers receive events in `seq` order.
- **Identical request ⇒ identical response** for the same engine state (the
  only varying field is `seq`, which is monotonic).
- **Deterministic run ids** for keyed runs (§7).
- **Sanitized errors.** An engine error is reduced to its deterministic first
  line — no stack traces, no addresses.
- **Empty lists are `[]`, never `null`** in the report; `lastStep` is `null`
  when there is no step.

## 9. Integration guide — minimal non-Go client

A self-contained Python 3 client (stdlib only) that drives a full cycle over
stdio: `initialize → plan → run → status(poll) → shutdown`. It obeys the framing
rules: one JSON object per line, `LF`-terminated, flush after each write, read
one line at a time, and treat an `id`-less line as a `steward/event`
notification.

`serve` boots the real engine, so it needs an EKA project plus the steward
config and role it dispatches as (see [`config.md`](config.md)); the argv below
is the production shape.

```python
#!/usr/bin/env python3
"""Minimal stdlib-only client for the eka-steward machine protocol (v1)."""
import json
import subprocess
import sys
import time

PROJECT = "p"
SERVER = [
    "eka-steward", "serve", "--stdio",
    "--project", PROJECT,
    "--config", "steward.yaml",
    "--role", "implementer",
]


def main():
    proc = subprocess.Popen(
        SERVER, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
        text=True, bufsize=1,  # line-buffered text mode
    )
    next_id = 0

    def call(method, params):
        nonlocal next_id
        next_id += 1
        req_id = next_id
        # One compact JSON object per line; flush so the server sees it now.
        proc.stdin.write(json.dumps(
            {"jsonrpc": "2.0", "id": req_id, "method": method, "params": params},
            separators=(",", ":"),
        ) + "\n")
        proc.stdin.flush()
        while True:
            raw = proc.stdout.readline()
            if not raw:
                raise SystemExit("server closed the stream")
            msg = json.loads(raw)
            if msg.get("id") == req_id:          # our reply
                if "error" in msg:
                    raise SystemExit(f"{method} failed: {msg['error']}")
                return msg["result"]
            # No id: a steward/event notification. Handle asynchronously.
            print("event:", json.dumps(msg["params"]), file=sys.stderr)

    try:
        init = call("steward/initialize", {"protocolVersions": ["1"]})
        assert init["protocolVersion"] == "1", init
        print("capabilities:", init["capabilities"])

        plan = call("steward/plan", {"project": PROJECT})
        print("plan:", plan["selected"], "candidate(s)")

        run = call("steward/run", {
            "project": PROJECT, "idempotencyKey": "cycle-1",
        })
        run_id = run["runId"]

        # Poll to a terminal state (run is asynchronous).
        status = {"state": "running"}
        for _ in range(600):
            status = call("steward/status", {"runId": run_id})
            if status["state"] != "running":
                break
            time.sleep(0.1)
        print("status:", status["state"])

        print("shutdown:", call("steward/shutdown", {"graceful": True}))
    finally:
        proc.stdin.close()
        proc.wait()


if __name__ == "__main__":
    main()
```

Notes for other languages:

- Write the request as **one line** and flush; never pretty-print across lines.
- Read **lines**, not fixed byte counts; decode each line independently.
- Match replies by `id`; a line with no `id` and `method:"steward/event"` is a
  notification (§4) and must be handled out of band.
- `initialize` is mandatory before any other method; the version you offer is
  `protocolVersions` (an array), and the server returns the selected
  `protocolVersion`.
- `run` returns before the run finishes — poll `status` or consume events to
  reach `done`/`failed`/`canceled`.

## 10. Golden transcripts

Pinned request→response pairs live in `internal/protocol/testdata/*.golden` and
are validated against a fresh server by `TestGoldenTranscripts` in
`internal/protocol/protocol_test.go` (regenerate with `UPDATE_GOLDEN=1`). Each
file is: a comment header, the request line, the expected response line. The
`# init: true` directive hands the request an `initialize` first. These are the
authoritative, test-enforced examples — this document does not restate them all.

| File | Covers |
| --- | --- |
| `initialize.golden` | handshake + `capabilities` |
| `plan.golden` | pure plan result |
| `step.golden` | one step result |
| `run.golden` | async run id |
| `status.golden` | idle status envelope |
| `resume_notfound.golden` | `-32003` |
| `cancel_notfound.golden` | `-32003` |
| `approve.golden` | approval accepted |
| `shutdown.golden` | graceful stop |
| `malformed.golden` | `-32700` |
| `not_initialized.golden` | `-32001` |
| `version_unsupported.golden` | `-32002` |

`initialize.golden`, verbatim:

```json
{"jsonrpc":"2.0","id":1,"method":"steward/initialize","params":{"protocolVersions":["1"]}}
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"1","capabilities":{"methods":["steward/initialize","steward/plan","steward/step","steward/run","steward/resume","steward/cancel","steward/approve","steward/status","steward/shutdown","steward/events.subscribe","steward/events.unsubscribe"],"events":["step","dispatch","write","approval","metrics"]}}}
```

`status.golden`, verbatim (empty-params idle status envelope):

```json
{"jsonrpc":"2.0","id":6,"method":"steward/status","params":{}}
{"jsonrpc":"2.0","id":6,"result":{"runId":"","state":"idle","counters":{"steps":0,"retries":0,"cancellations":0,"peakInFlight":0,"peakQueueDepth":0,"inFlight":0,"queueDepth":0,"dispatched":0,"rejected":0},"lastStep":null,"report":{"steps":[],"writes":[]}}}
```

`run.golden`, verbatim (async run id):

```json
{"jsonrpc":"2.0","id":4,"method":"steward/run","params":{"project":"p"}}
{"jsonrpc":"2.0","id":4,"result":{"runId":"run-1"}}
```

## 11. Contract notes (code is authoritative)

The design draft in `/tmp/opencode/steward-protocol-contract.md` predates the
implementation; where it differed, the code wins:

- Notifications carry **no `runId`** — the params are `{seq, type, data}`.
- `events.subscribe` **does** return `subscriptionId` and takes only
  `{types?, fromSeq?}` (no `runId` filter; a `runId` param is ignored as an
  unknown field).
- Backpressure is **non-blocking drop + one `gap` event per burst**, not
  "block the emitter up to a cap then fail".
- `cancel` emits **no synthetic terminal `metrics` event**; events come only
  from `obs.Stream`. Cancel surfaces as the reply `canceled:true` and the
  `canceled` run state.
- `plan` candidates also carry `kind` and `createdSeq`; `blocked` is always `0`.
- `seq` is global per stream: a type-filtered subscriber may see `seq` gaps for
  types it did not request (distinct from `gap` events).

## Where this is enforced

| Guarantee | Test (`internal/protocol/protocol_test.go` unless noted) |
| --- | --- |
| pinned transcripts match the server | `TestGoldenTranscripts` |
| full cycle `initialize→plan→run→status→shutdown` | `TestFullCycle` |
| version selection + un-negotiated gate | `TestVersionNegotiation` |
| idempotent replay, no duplicate effect, conflict | `TestIdempotencyKey` |
| cancel mid-run, idempotent, terminal `canceled` | `TestCancelMidRun` |
| malformed JSON and oversized-line refusal + resync | `TestFrameRefusals` |
| notification line carries no `id` | `TestNotifyWritesNotificationLine` |
| `events.subscribe` notifications interleave with replies | `TestEventsSubscribeOverPipe` |
| gap policy, redaction, replay ring | `internal/obs` `TestStreamBackpressureGap`, `TestStreamRedaction`, `TestStreamExactSequence` |