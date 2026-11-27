# eka-steward checkpoint & resume

The control loop (`internal/loop`) is crash-safe: a steward killed at any
boundary of a step restarts, **resumes from the last checkpoint**, and never
applies an EKA write twice or re-dispatches completed work. This document
defines the checkpoint location, the write-ahead protocol, the reconcile rules
per write class, atomicity, versioning, and the exact crash-recovery
guarantees (and what stays best-effort).

It extends the loop described in [`control-loop.md`](control-loop.md); the
single write path it re-issues through is [`write-path.md`](write-path.md).

## 1. Checkpoint location

The checkpoint is the loop's persisted `State` file. It is **steward
bookkeeping**, deliberately separate from the EKA workspace store (it is not
EKA knowledge). The location is explicit and resolved deterministically:

| Precedence | Path | Source |
| --- | --- | --- |
| 1 (override) | `$EKA_STEWARD_STATE_DIR/checkpoint.json` | `StateDirEnv` |
| 2 | `$EKA_HOME/steward/checkpoint.json` | when `EKA_HOME` is set |
| 3 (default) | `<user home>/.eka/steward/checkpoint.json` | no override, no `EKA_HOME` |

`loop.StateDir()` / `loop.DefaultStatePath()` implement the precedence;
`loop.EnsureStateDir()` creates the directory. `Save` writes its temp file
**in the same directory** as the target, so the rename is atomic on one
filesystem; it does not create parent directories (a misconfigured path fails
loudly rather than silently landing somewhere else).

## 2. Write-ahead intent protocol

Every mutation is a checkpointed transaction. The intent log (`State.InFlight`)
is written **before** the worker runs and cleared **after** the write is
confirmed applied:

```
beginIntent      # persist Intent atomically (BEFORE dispatch)
   ↓  crash boundary "after-intent"
Dispatch         # the ONLY worker/LLM call (a normal failure rolls the intent back)
   ↓  crash boundary "after-dispatch"
Advance          # apply the mutation through the single write path
   ↓  crash boundary "after-apply"
completeIntent   # atomically move the intent → Done
```

An `Intent` is self-contained so resume can reconcile it without the original
dispatch:

```json
{
  "candidateId": "p/sto:i-1",
  "kind": "transition",
  "target": "p/sto:i-1",
  "to": "todo",
  "digest": "",
  "publish": null,
  "step": { "candidateId": "p/sto:i-1", "kind": "dispatch", "priority": 0, "state": "planned", "createdSeq": 0 }
}
```

`kind` selects the write class (`transition` | `publish`). A publish intent
carries its re-issuable payload (`publish`) and the intended payload `digest`.

## 3. Resume: reconcile, then dispatch

`Run` calls `Resume` **before** any new dispatch. `Resume` processes every
`InFlight` intent (in sorted candidate order — no map iteration, deterministic)
and **never dispatches**:

| Write class | Reconcile rule |
| --- | --- |
| `transition` | Read the line's current execution state. If it is **at or after** the target (`planned < todo < in-progress < in-review < done`) ⇒ **no-op**. Otherwise re-issue the transition. The core transition engine is a forward/backward state machine, so it is never re-issued blindly. |
| `publish` | Re-issue through `ekawrite`'s deterministic read-then-write **replay gate**: an identical payload is a **no-op** (`PublishNoOp`, no `max+1` instance); a changed payload is one explicit write. A recorded `digest` is verified first, so a corrupted intent is a deterministic error, never a silent mis-write. |

After a successful reconcile the intent is moved to `Done`, so the outer loop
skips it and completed work is never re-dispatched.

The reconcile lives in `internal/ekawrite` (`Writer.Reconcile`) — the **only**
legal mutation site — and the loop reaches it through its `Writer` seam. The
boundary test (`cmd/boundary_arch_test.go`) stays green: no mutation call exists
outside `internal/ekawrite`.

## 4. Crash matrix

Tests inject a kill at each boundary through the `Config.Crash` seam (a panic
on the named boundary — no real process), then build a fresh `Loop` from the
persisted state and resume:

| Boundary | State at kill | Post-resume behaviour | Exactly-once? |
| --- | --- | --- | --- |
| `after-intent` | intent persisted, worker not run, write not applied | reconcile applies the write (no re-dispatch) and completes the item | yes — one apply |
| `after-dispatch` | worker ran, write not applied | reconcile applies the write and completes the item (no re-dispatch) | yes — one apply |
| `after-apply` | write applied, intent not cleared | reconcile is a no-op (line at/after target; replay gate) and completes the item | yes — the applied write is not repeated |

For every row the final state is identical to the uninterrupted run and the
observed **step sequence is byte-identical** (the intent replays the persisted
`Step`; the log is sorted). Covered by
`internal/loop/checkpoint_test.go` (`TestCrashResumeExactlyOnce`,
`TestResumeStepSequenceMatchesUninterrupted`) and, against the real write path
and a real temp store, `TestResumeTransitionReconcileExactlyOnce` /
`TestReconcilePublishReplayGateIntegration`.

## 5. Atomicity, versioning and determinism

- **Atomic Save.** `Store.Save` writes a temp file in the target's directory,
  then `rename`s it over the target. A torn write never leaves a half-written
  checkpoint; the tests assert no leftover `.state-*.tmp` file and a complete,
  valid, newline-terminated JSON file.
- **Versioned format.** `State.Version` is `2` (adds the `inFlight` intent log).
  `Load` **upgrades** an older file to the current version through the ordered
  migration registry (`checkpointMigrations`, `{1 -> 2}`); a version the registry
  cannot reach is refused with the typed `loop.ErrCheckpointUnmigratable` — there
  is no guessed-at upgrade and no partial one. The registry, the supported
  range and the refusal semantics are in [`upgrade.md`](upgrade.md).
- **Deterministic bytes.** `Done` (sorted, de-duplicated) and `InFlight`
  (sorted by candidate id, last intent per candidate) are normalized before
  encoding, so the same logical state is byte-stable.
- **Deterministic resume.** `Resume` iterates the sorted intent log and replays
  the persisted `Step`, so a crash-then-resume run emits the same step sequence
  as an uninterrupted run.

## 6. Guarantees vs best-effort

**Guaranteed** (tested):

- No EKA write is applied twice: a re-issued transition at/after its target and
  a re-issued identical publish are both no-ops.
- Completed work is not re-dispatched.
- A crash at every boundary of a checkpointed step is recoverable to the
  correct final state.
- Resume is deterministic (sorted log, replayed steps).

**Best-effort / out of scope:**

- **Worker side effects are not checkpointed.** The dispatch result is opaque
  data; a crash *after intent, before dispatch* (`after-intent`) treats the
  intended write as owed and completes the item **without re-running the
  worker**. Only the EKA write (the conserved effect) is reconciled; it does
  not replay the LLM/worker conversation.
- **No write path, no checkpoint.** When `Config.DispatchState` is empty (or no
  `Writer` is wired) the run is plan/dispatch only; it records completion after
  dispatch and a crash in between re-dispatches that one item. Configure a
  write path to get the guarantee.
- **A normal dispatch error rolls the intent back** (the item is retried); only
  a crash leaves the intent for resume to reconcile. A persistent write error
  keeps the intent in flight and the next run retries the reconcile. Bounded
  retry (`MaxAttempts`/`Backoff`), the per-attempt timeout and `Loop.Cancel` are
  specified in [`recovery.md`](recovery.md).
- **Forward `publish` dispatch is not emitted by this cut.** The loop's forward
  mutation today is the `transition`; the `publish` intent kind is fully wired
  through `ekawrite.Reconcile` and tested store-backed, ready for a later
  publish-dispatch path.