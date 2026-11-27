# eka-steward upgrade, migration & rollback

Two persisted artifacts carry a **schema version** and are migrated forward when
a new steward reads them: the control loop's **checkpoint** (`internal/loop`,
schema in [`checkpoint.md`](checkpoint.md)) and the **config** document
(`internal/config`, schema in [`config.md`](config.md)). Both migrate through an
**explicit, ordered registry** — the same shape in both packages — and both
**refuse** a version they cannot reach, deterministically, rather than guessing.
This document defines the registries, the supported version range and refusal
semantics, the in-flight-across-upgrade behaviour, and the **rollback note**.

## 1. The migration registry shape

A migration is one ordered step `{From, To, Apply}`. The registry is a slice,
**oldest first**; the walk applies every step from the artifact's version up to
the current one:

```
path := []
for v := from; v < current; {
        m := stepFrom(v)            # deterministic slice lookup, no map iteration
        if m == nil: REFUSE         # a gap is never silently skipped
        path = append(path, m)
        v = m.To
}
for m in path: m.Apply(artifact)    # two-pass: apply only once the path is proven
artifact.version = current
```

Properties the shape guarantees:

- **Ordered and total.** Steps apply in slice order, one per version bump. There
  is no jump and no merge.
- **Never partial.** A version below the registry's floor, above the current
  version, or on a gap with no step is refused **before** it can advance past the
  refusal — the artifact is not half-migrated (state migrations hold the parsed
  value in memory and only commit on a successful walk).
- **Deterministic.** The lookup is over a slice, not a map, so the same bytes
  always take the same path; migrating is a pure function of the input.

To add a future `n -> n+1` change: append `{From: n, To: n+1}`, advance the
current version, add a fixture and a test — **never edit or remove an existing
step**, because an already-shipped step is part of the on-disk contract.

## 2. Checkpoint migration (`internal/loop`)

| | Value |
| --- | --- |
| Current `stateVersion` | `2` (adds `State.InFlight`, the write-ahead intent log) |
| Registry | `checkpointMigrations = [{1 -> 2}]` |
| Oldest readable (floor) | `1` |
| Newest readable | `2` (current) |
| Refusal type | `loop.ErrCheckpointUnmigratable` (match with `errors.Is`) |

- **v1 -> v2.** Version 1 carried `Done` only (no intent log). The v2 field set
  is otherwise identical — `InFlight` already defaults to empty — so the step is
  exactly the version advance: it rewrites no field and, critically, **drops no
  field**. Any entry the loader decoded is preserved untouched (a real v1 has an
  empty intent log; the walk preserves whatever is there). `Done` and `Cursor`
  are preserved verbatim.
- **Refusal.** A version `> 2` is refused as *"newer than this steward
  supports"*; a version `< 1` (including `0` and negatives) as *"older than the
  oldest supported"*; a version in range but on a gap as *"no migration path"*.
  Every refusal names the offending version and the supported range
  (`1..2`), is `errors.Is(err, ErrCheckpointUnmigratable)` true, and is
  byte-identical across runs.

## 3. Config migration (`internal/config`)

| | Value |
| --- | --- |
| Current `Version` | `1` |
| Registry | `configMigrations = [{0 -> 1}]` |
| Oldest readable (floor) | `0` (the pre-schema document: roles + drivers only) |
| Newest readable | `1` (current) |
| Refusal | a plain deterministic error naming the version and range |

- **v0 -> v1.** `sizing` was added in v1. A v0 document has none, so the step
  sets `sizing.maxWorkers` to the **conservative floor of 1** (never a guessed
  higher count); an explicit value is preserved. `sandbox` and `loop` were also
  added in v1 but their zero values are already correct (contained, no-op), so
  nothing is added for them.
- **Absent `version`** decodes as `0` and migrates the same way (documented
  default).
- **Refusal.** `version > 1` — *"newer than this steward supports"*;
  `version < 0` — *"not supported"*; a gap — *"no migration path"*. Refusals
  are deterministic (`docs/config.md` § Versioning).

## 4. In-flight run across an upgrade

The checkpoint's intent log (`State.InFlight`) is what makes a step
crash-recoverable: an intent is persisted **before** a mutation and cleared once
it is confirmed applied (the write-ahead protocol of
[`checkpoint.md`](checkpoint.md) §2). The migration must therefore carry it
**across the version bump**:

- The v1 -> v2 step rewrites no field, so an intent decoded from the file
  survives the upgrade untouched (§2). A v1 file that carries no intent log
  simply migrates to an empty `InFlight` — the correct result, since v1 had no
  intent log.
- After the file is loaded and migrated, the **current** release's `Resume`
  reconciles every surviving intent exactly as it would for a current-version
  file: a `transition` intent is re-issued only when the line is not yet
  at/after its target, a `publish` intent goes through `ekawrite`'s replay gate,
  and after a successful reconcile the intent moves to `Done`. The intent logged
  by release N is therefore completed by release N+1 **without re-dispatch and
  without a double apply** (the exactly-once guarantee of `docs/checkpoint.md`).

So an upgrade that lands **between** a persisted intent and its `Done` move is
safe: the upgraded run resumes the owed write rather than losing or repeating
it.

## 5. Rollback note — EKA writes are forward-only

**Reverting steward does not revert EKA.** This is the honest, load-bearing
statement for a rollback:

- Every mutation the steward performs is applied to the **EKA workspace store**
  through the single audited write path (`internal/ekawrite`, `docs/write-path.md`)
  and is **durable EKA knowledge**. An applied transition, publish, note or
  approval artifact is persisted in the workspace, not in the steward.
- Reverting steward (and its pinned eka-core `api` contract — `go.mod` currently
  pins `eka-core v2.0.1`, steward `2.0.0`, see `README.md`) changes **future
  behaviour only**. It does **not** undo, roll back or rewrite any EKA write the
  engine already applied. Those writes are **forward-only**; undoing one is a new
  explicit EKA mutation, never a side effect of downgrading the binary.
- A resumed reconcile after a downgrade is exactly as safe as after an upgrade:
  the reconcile is idempotent by construction (an at/after-target transition is a
  no-op; a same-payload publish is a no-op through the replay gate), so a
  downgrade does not re-apply an owed write twice.

**Downgrade of the persisted schemas is unsupported and refused.** The
registries migrate **forward only**; there is no down-migration:

- A checkpoint written by a newer steward (`version > stateVersion`) is refused
  by an older steward with `ErrCheckpointUnmigratable` (*"newer than this steward
  supports"*), deterministically. The older steward does not read it, does not
  rewrite it, and does not silently downgrade it.
- A config document with `version` above the running steward's `Version` is
  likewise refused (*"newer than this steward supports"*).

The mechanism is the same forward walk in §1: the walk has no edge that lowers a
version, and the "newer than supported" gate is checked before any step runs.
Reverting a steward across a schema bump therefore means running the **older**
binary only against checkpoints/config it supports — or accepting that a rolled
back steward will refuse a newer file rather than corrupt it.

## 6. Verified by

| Property | Test |
| --- | --- |
| v1 checkpoint loads + migrates to current, state preserved, byte-stable | `internal/loop` `TestCheckpointV1MigratesToCurrent` |
| older / negative / future / far-future checkpoint refused, typed + deterministic | `internal/loop` `TestCheckpointUnmigratableRefused` |
| in-flight intent survives migration and is reconciled by the current release | `internal/loop` `TestCheckpointInFlightSurvivesMigrationAndResumes` |
| generic ordered walk (N -> N+1 -> N+2) and gap refusal | `internal/loop` `TestMigrateStateRegistryWalk` |
| downgrade (newer-than-supported checkpoint) refused — backs the rollback note | `internal/loop` `TestCheckpointDowngradeRefused` |
| config registry N -> N+1 -> N+2 deterministic across a bump | `internal/config` `TestMigrateConfigRegistryAcrossBump` |
| config registry floor / future / gap refusals | `internal/config` `TestMigrateConfigRegistryRefusals` |
| v0 -> v1 migration deterministic | `internal/config` `TestMigrateV0Deterministic` |
| v0 -> v1 migration, future/unknown refused (existing) | `internal/config` `TestMigrateV0ToCurrent`, `TestParseVersionGate` |
