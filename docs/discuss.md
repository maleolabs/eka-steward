# eka-steward discussion phase

`internal/discuss` is the steward's **discussion phase**: a bounded, resumable,
deterministic turn loop that drives a worker to produce decisions for one work
item, records those decisions as EKA knowledge through the one write path, and
derives the next (execute) phase's input by **reading the recorded knowledge
back** — never from in-memory session state.

It keeps the steward's core split ([`determinism.md`](determinism.md)): **code
owns the orchestration — how many rounds, which role, when to stop — the worker
owns the content**. The bounded context handed to the worker is assembled by
[`internal/render`](context.md); the checkpoint is `internal/loop`'s write-ahead
intent log (see [`checkpoint.md`](checkpoint.md)); every mutation goes through
[`internal/ekawrite`](write-path.md).

## Lifecycle

```
Discussion.Run
  Resume            # reconcile in-flight publish intents (no dispatch, idempotent)
  loop until Rounds recorded:
    build context   # render.Render: instructions + subject brief + prior recorded decisions
    dispatch        # ContextDispatcher.DispatchContext(payload)  ← the ONLY worker call
    begin intent    # persist the publish intent (AFTER the dispatch, BEFORE the publish)
    record          # Recorder.Reconcile → ekawrite replay gate (dec- publish)
    complete intent # intent → Done
```

`Discussion.Step` runs exactly one pending round; `Run` drives `Step` to
completion. The round count is `Config.Rounds`, clamped to `[1, MaxRounds]`
(`MaxRounds = 64`) — the deterministic stop is **code-owned**, never a worker
signal.

## Code vs LLM split

| Decision | Owner | Where |
| --- | --- | --- |
| How many rounds | CODE | `Config.Rounds`, `clampRounds` |
| Which role | CODE | `Config.Role` (validated against `internal/role` when `Config.Roles` is set) |
| When to stop | CODE | the round count; no worker-driven stop |
| Context assembly / budget | CODE | `internal/render.Render`, `Config.MaxBytes` |
| The decision text | WORKER | `ContextDispatcher.DispatchContext` |
| Recorded artifact shape | CODE | `decisionContent` → dec- sections |

The worker never chooses the next round, the role, or the stop condition; its
output is captured as data and recorded.

The worker seam is the existing `loop.Dispatcher` extended with the rendered
payload, exactly as `internal/loop` extended it for review
(`loop.ReviewDispatcher`):

```go
type ContextDispatcher interface {
    loop.Dispatcher
    DispatchContext(ctx, candidate, payload []byte) (loop.DispatchResult, error)
}
```

## Recorded knowledge: a `dec-` decision record

A round's decision is recorded as a **`dec-` (decision record) EKA artifact**,
published through `internal/ekawrite` (`NewDraft` + `Publish`) reached via the
`loop.Writer` seam (`loop.EkawriteWriter`). Each round is one deterministic unit:

```
namespace = subject namespace
type      = "dec"
dimension = "decisions"                 # conformance rule 6 (Architecture)
id        = "<subject-id>-dec-r<NN>"    # zero-padded: lexical order == round order
content   = { context, decision, consequences, alternativesConsidered }
```

**Why `dec-` and not a note?** A discussion produces *decisions*, and `dec-` is
exactly the artifact that is a decision record (Context / Decision / Consequences
/ Alternatives Considered). Its creation goes through `ekawrite.Publish`'s
**deterministic read-then-write replay gate**: re-issuing an identical payload is
a `PublishNoOp` — no `max+1` instance. A `cmt-` note (via `NoteDraft`) is a
comment on a line, and its draft id is generated, so it has no equivalent replay
gate; it would be the wrong artifact and a weaker idempotency story.

## Resumability

Each round is a checkpointed transaction. Because the recorded content is
**worker-produced**, the intent is persisted *after* the dispatch and *before*
the publish (the dispatch output is only known then):

```
DispatchContext      # worker produces the decision text
   ↓  crash boundary "after-dispatch"   (no intent yet → restart re-dispatches)
beginIntent          # persist the publish intent (payload + section content)
   ↓  crash boundary "after-intent"     (restart reconciles the persisted payload)
Reconcile (publish)  # ekawrite.Publish through the replay gate
   ↓  crash boundary "after-apply"      (the record applied, intent not cleared)
completeIntent       # atomically move the intent → Done
```

| Boundary | State at kill | Post-resume behaviour | Exactly-once record? |
| --- | --- | --- | --- |
| `after-dispatch` | worker ran; intent not persisted | resume has no intent for the round → re-dispatches it, then records once | yes — one published decision |
| `after-intent` | intent persisted; publish not run | `Resume` reconciles the persisted payload (no re-dispatch) | yes — one published decision |
| `after-apply` | publish applied; intent not cleared | `Resume` reconciles; the replay gate is a no-op | yes — the applied record is not repeated |

Worker side effects are deliberately **not** checkpointed (documented in
[`checkpoint.md`](checkpoint.md)): only the conserved EKA write is reconciled.
The `after-dispatch` row therefore re-runs the worker for that one round, but
never records a second decision. The checkpoint itself is the loop's versioned
`State` (`Done` = recorded rounds, `InFlight` = in-flight publish intents),
persisted atomically by `loop.Store`; `internal/discuss` reuses the same
`loop.Intent` / `loop.State` / `loop.Store` types and the same
begin/complete protocol.

## Next-phase input from the store

`BuildExecuteInput(ctx, knowledge, subjectLine, maxBytes)` takes **only the read
seam and the subject line** — never a `Discussion` value. It:

1. reads the subject through `api.Resolver` (`StoreKnowledge.Subject`);
2. reads the recorded `dec-` units back through `api.Knowledge.Search`
   (read-only; `StoreKnowledge.Decisions`);
3. orders them by round through the pure `internal/decision.Order` kernel;
4. renders the bounded execute-phase payload with `internal/render`.

So the next phase's input is reconstructed from the store after a restart, not
carried across in memory. The store-backed test builds it from a **fresh**
`StoreKnowledge` after the session has finished, proving there is no shared
in-memory struct.

## Guarantees vs best-effort

**Guaranteed (tested):**

- A discussion session produces recorded EKA knowledge: one `dec-` unit per
  round, published through the single write path
  (`TestFullDiscussionTurn`, `TestStoreBackedDiscussionTurn`).
- The phase is resumable: a crash at every boundary of a round resumes to the
  correct final state with exactly one recorded decision per round and a
  deterministic round sequence (`TestResumeCrashMatrix`).
- The next-phase input is derived from the recorded knowledge, not ephemeral
  state (`TestNextPhaseInputSurvivesRestart`,
  `TestStoreBackedDiscussionTurn`).
- Determinism: identical fixtures yield byte-identical recorded content and
  next-phase input across ≥ 20 runs (`TestDeterminismAcrossRuns`).
- Bounded rounds / deterministic stop (`TestRoundsAreBoundedAndStopDeterministic`).

**Best-effort / out of scope:**

- The worker/LLM conversation is not replayed on the `after-dispatch` boundary
  (only the EKA write is conserved); a non-deterministic worker could produce a
  different decision text on the re-dispatch.
- The worker dispatch itself is the deferred ACP client (`docs/acp-spike.md`);
  `internal/discuss` only owns the deterministic orchestration around it.