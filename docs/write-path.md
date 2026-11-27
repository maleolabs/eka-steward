# eka-steward write path

This document states how `eka-steward` writes EKA knowledge, and the
invariants the write path guarantees. Everything here is enforced by
tests — the rules are not on the honour system.

## One write path: `internal/ekawrite`

`eka-steward` is a **consumer** of `eka-core`; its only supported contract
is `github.com/maleolabs/eka-core/v2/api`. Every EKA **mutation** the
steward performs goes through the single package **`internal/ekawrite`**.
No other non-test package may call the mutating surface of an
`api.Authoring` value:

| Method | Class |
| --- | --- |
| `NewDraft` | draft scaffold |
| `Publish`, `PublishBatch` | unit publish |
| `Transition` | state transition |
| `Relate` | relationship write |
| `NoteDraft` | note draft |
| `DiscardDraft` | draft discard |
| `Sync`, `SyncAdopt` | transport / provenance |

Reading EKA (`api.Knowledge`, `api.Resolver`, `api.Workspace`,
`api.Integrity`, `api.Contexts`) stays allowed everywhere.

Enforced by `cmd/boundary_arch_test.go`
(`TestEKAMutationsOnlyInEkawrite`): every non-test `.go` file is parsed
and any call to a mutating method **on an `api.Authoring` value** — a
direct `rt.Authoring().Publish(...)` or a method on a value typed (or
assigned from) `api.Authoring` — outside `internal/ekawrite` fails the
test. The allowed package is pinned and a stale pin (ekawrite stops
calling the surface) fails too, mirroring the `internal/escapehatch` pin.

## Attribution is deterministic

A `Writer` is built for exactly one actor and stamps that actor on every
write:

- `NewSteward(rt)` → author kind **`agent`**, name `eka-steward`;
- `NewWorker(rt)` → author kind **`worker`**, name `eka-worker`.

`Publish` passes the identity as the draft's `By`; the persisted unit's
`Author` field therefore carries the kind. `Transition` and `Note` set
their `By`/`ByKind` from the Writer, so a caller cannot attribute a write
to a different actor. Names are display metadata and deterministic; they
do not enter identity.

## Replay safety: no-op on replay (the read-then-write gate)

`api.Authoring.Publish` is **forward-only**: an explicit
`PublishOptions.InstanceVersion` is refused unless it exceeds the line's
current maximum, and `api` exposes no `store.PutUnit`. A byte-identical
re-issue therefore **cannot** be a store-level `PutUnit` no-op from a
consumer. The steward reconstructs `PutUnit`-equivalent replay safety at
its own layer with a deterministic **read-then-write gate**:

> **api has no PutUnit; steward guarantees no-op-on-replay via a
> deterministic read-then-write gate; the auto `max+1` path is never used
> implicitly.**

Algorithm (`Writer.Publish`):

1. **Intended digest.** Compute a version-independent content digest of
   the unit the input would produce (`contentDigest`): SHA-256 over
   `identity (ns, type, id) · revision · author(kind, name) · compiled
   content payload · normalized relationships`. The content is
   normalized exactly as the draft pipeline compiles it — required
   section placeholders filled, then the canonical `ContentJSON`
   encoding — so the intended digest equals the persisted unit's digest
   for identical input. The digest **excludes** the instance version
   (that is what makes a re-issue a no-op), the change log and the
   timestamps (date-derived, so they would defeat replay stability across
   a day boundary).
2. **Read the line first.** `rt.Resolver().ResolveLine(ns, type, id)`
   and take its **current** (highest-version) instance.
3. **No-op check.** If that instance carries the *same author* and the
   *same content digest*, return `PublishNoOp` — **no draft is
   scaffolded, `Publish` is never called, and no `max+1` version is
   allocated**.
4. **Explicit write.** Otherwise scaffold the draft (`NewDraft`) and call
   `Publish` with an **explicit, deterministic**
   `InstanceVersion = max(existing)+1`. The auto-assignment path is never
   used implicitly.
5. **Race → deterministic conflict.** If `Publish` still refuses because
   a concurrent writer took that version, the gate re-reads the line; if
   the line advanced to `>= next`, the refusal is surfaced as
   `ErrConcurrentWrite` — a deterministic conflict, never a silent bump.

Consequences (tested): writing the same unit twice produces exactly one
instance; a changed content payload produces exactly one additional
instance at `max+1`.

The gate applies to the unit-creation class (`Publish`). The other classes
delegate to the core engine's own idempotency/refusal semantics
(`Relate` re-points without bumping the instance and treats duplicate
edges as no-ops; `Transition` is a forward/backward state machine).

## Provenance and adopt (ADR-032)

`Publish` writes **workspace-native** knowledge: the persisted reference
is attributed to the provenance sentinel `source_repo = "runtime"`, so it
**never** appears in any repository snapshot. To make a steward-authored
unit travel with a repository, the caller runs an **explicit** adopt; the
next push then emits it into `<repo>/exchange/snapshots`. `Publish` never
auto-adopts and never auto-syncs.

| Write class | Method(s) | Provenance of the persisted reference | Reaches a repository snapshot? |
| --- | --- | --- | --- |
| **Workspace-native publish** | `Publish` | `source_repo = "runtime"` | **no** — never; visible only through the workspace store (and `api` reads across the workspace) |
| **Repo-context publish** | `Publish` then `Adopt` (explicit) | after `Adopt`: `source_repo = <repo name>` | **yes** — the next `Sync{Push:true}` assembles it into `<repo>/exchange/snapshots` |
| **Transport pull** | `Sync{Pull:true}` | `source_repo = <repo name>` | seeded from the repository snapshot/docs into the store |
| **Transport push** | `Sync{Push:true}` | unchanged (repository units) | emits the repository's units into the snapshot |
| **Provenance re-attribution** | `Adopt` (`SyncAdopt`) | `"runtime"` → `<repo name>` | reference-only; payloads untouched; makes the next push carry the unit |
| **State transition** | `Transition` | unchanged | via the repository's own push after the change |
| **Relationship write** | `Relate` | unchanged | via the repository's own push after the change |
| **Note** | `Note` | workspace-native (draft until published) | via the repository's own push after publish/adopt |

`Adopt` is reference-only (ADR-032 Option C2): it re-attributes the
reference to the repository provenance; the immutable, content-addressed
payloads are never touched, and a conflict (the repository already
references the identity with a different payload) is skipped, never
overwritten.

Store-backed proof (`TestStewardAuthoredUnitTravelsAfterAdopt`): a
steward-authored unit is published (workspace-native), the snapshot
directory does **not** yet exist, `Adopt` re-attributes it, a push runs,
and `exchange.LoadSnapshot(<repo>/exchange/snapshots)` contains the unit.

## Workers have no reachable store path

`WorkerEnv()` returns the allow-listed environment a steward worker
subprocess runs with: **no `HOME`, no `EKA_HOME`, no `PATH`, no
token/credential variable**. With it:

- the EKA workspace root cannot be resolved
  (`EKA_HOME` unset, `HOME` unset → `<home>/.eka` is unreachable), so
  opening the default store path fails;
- a bare-name `eka` executable cannot be resolved (no `PATH`).

What can and cannot be asserted portably:

- **Asserted.** With `WorkerEnv()`: `WorkerHomeUnresolvable` is true;
  `api.HomeDir()` / `api.Open()` fail when `HOME`/`EKA_HOME` are absent;
  a bare-name `eka` is not resolvable under the environment's (absent)
  `PATH` (positive control: the same lookup resolves when a `PATH` is
  present).
- **Not asserted.** An environment cannot stop a process from executing a
  binary it is handed by **absolute path** — that is a sandbox concern
  outside the environment's control. A worker that must run a specific
  binary receives its absolute path, never a `PATH` lookup.

Enforced by `TestWorkerEnvBlocksStoreAccess` (`internal/ekawrite`).

## Integrity after a run

After exercising the write path, `rt.Integrity().Verify()` reports zero
violations (`TestIntegrityAfterRun`).
