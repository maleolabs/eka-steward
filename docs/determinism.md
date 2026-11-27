# eka-steward determinism

Steward's core promise in one line: **code decides *which*, *when* and
*if*; the LLM decides *what*.**

Every control-flow decision — what to work on next, when to dispatch,
whether to retry, how many workers to run, when to stop — is a
deterministic, LLM-free computation over explicit inputs. The LLM is only
ever asked for the **content** of a unit of work, inside an ACP worker
session, and its output is treated as opaque data that never feeds back
into a control decision.

This document defines the split, enumerates every control-flow decision
point of the planned orchestrator, states the ordering contract enforced
by `internal/decision`, and pins the determinism test rule.

## 1. Code-owned vs LLM-owned

| Decision | Owner | Where |
| --- | --- | --- |
| **which** task next | **code** | work selection over the candidate graph (`internal/decision`) |
| **when** to dispatch | **code** | control-loop cadence + backpressure admission |
| **if** to retry | **code** | bounded retry policy (attempt count + error class) |
| **how many** workers | **code** | sizing formula `clamp(min(cores/2, ramGiB/4), 1, MaxWorkers)` + cost cap |
| **whether** to stop | **code** | completion / quiescence predicate over CKO state |
| **what** to write, **which** files (the content of work) | **LLM** | inside an ACP worker session only |

The LLM owns exactly one thing: the *content* produced while fulfilling a
unit of work (the text written, the files changed, the diff proposed). It
has **no control authority**. It cannot choose the next task, cannot
decide when or whether to dispatch, cannot authorize a retry, cannot set
its own concurrency and cannot declare the run complete. Any agent
suggestion that sounds like control ("stop here", "do this instead",
"retry me") is data for the steward's code to accept or ignore by policy,
never a decision the LLM is allowed to make.

## 2. Control-flow decision points: all LLM-free

The planned orchestrator's control flow is the union of the M7 steward
work items. Every decision point that affects *which / when / if* is
enumerated below. Each is a pure function of explicit, steward-owned
inputs — none consults an LLM, and each is covered by a work item whose
acceptance criteria require a deterministic result.

| # | Decision point | Inputs (all steward-owned) | LLM input? | Seam |
| --- | --- | --- | --- | --- |
| 1 | which candidate is next | candidate set (ID, priority, state, created-seq) | none | `internal/decision.SelectNext` |
| 2 | dispatch order of a batch | candidate set | none | `internal/decision.Order` |
| 3 | when to dispatch the next step | loop state + backpressure admission | none | control-loop |
| 4 | how many workers (sizing) | cgroup v2 `cpu.max` / `memory.max`, `cfg.MaxWorkers`, cost cap | none | worker-sizing-policy |
| 5 | admit vs deny when saturated | pool occupancy + queue-depth limits | none | worker-pool-backpressure |
| 6 | whether / how many times to retry | attempt count + error class + bounds | none | cancel-retry-recovery |
| 7 | whether to cancel an in-flight worker | deadline / cancellation signal | none | cancel-retry-recovery |
| 8 | which driver (tool / cmd / args) | registered drivers + capability probe | none | driver-registry |
| 9 | which role a worker plays | role model + task type | none | worker-role-model |
| 10 | whether review is required, and which reviewer | review policy + author role | none | review-roles |
| 11 | whether discussion advances to execution | discussion-outcome state | none | discuss-phase |
| 12 | context payload truncation order | size budget + stable ordering | none | context-renderer |
| 13 | resume vs re-dispatch a step | checkpoint digest vs line digest | none | checkpoint-resume |
| 14 | whether to record a result to EKA | worker output + line digest | none | eka-write-path |
| 15 | whether to stop the run | CKO-graph quiescence predicate | none | control-loop |

Two notes on the table:

- **Row 12 is ordering, not content.** The renderer decides *which*
  knowledge is included and *in what order* (code-owned, byte-stable for
  identical inputs) under a hard size budget; the model only ever *reads*
  the rendered payload. Truncation is deterministic with a documented
  order, so it is a control decision, not a content decision.
- **Row 2 is the kernel.** The planned orchestrator's "compute the next
  step in code" requirement (control-loop) reduces, at its heart, to
  ordering a candidate set and picking the head. That kernel is
  `internal/decision`, specified in §3.

The only place the LLM is invoked is *inside* a worker session, to produce
content for the unit of work the code already chose. The session's
response re-enters the steward as plain data (files, text, a diff) and is
recorded through the single write path; at no point is it consulted to
make decision points 1–15.

## 3. Ordering contract (`internal/decision`)

`internal/decision` is the pure ordering / selection kernel. It imports
only the standard library and calls nothing that can observe the outside
world (see §4). It is **not** a scheduler, queue, worker pool, retry
engine or control loop — those own time, concurrency and persistence; this
package owns only the total order they share.

A `Candidate` is one dispatchable unit of work:

```go
type Candidate struct {
	ID         string // unique within a candidate set; final tie-break key
	Priority   int    // primary key, descending
	State      string // lifecycle state; second key, by rank
	CreatedSeq uint64 // monotonic creation sequence; third key, ascending
}
```

### 3.1 The total order

`Order(cs)` returns the candidates sorted by the following key tuple. It
sorts a **copy** (never mutates the caller's slice) and the key is a total
order, so the output is a permutation of the input and is **byte-identical
for any shuffle of the same candidate multiset**.

| Key | Direction | Rule |
| --- | --- | --- |
| 1. `Priority` | **descending** | higher priority dispatches first |
| 2. `State` rank | **ascending** | smaller rank first (table below) |
| 3. `CreatedSeq` | **ascending** | FIFO within equal priority and state |
| 4. `ID` | **ascending** | final tie-break; makes the order total when IDs are unique |

**State rank table** (ascending: this is lifecycle order). Any `State` not
listed ranks `StateRankUnknown = 6`, i.e. after every recognized state:

| State | Rank |
| --- | --- |
| `planned` | 0 |
| `todo` | 1 |
| `in-progress` | 2 |
| `blocked` | 3 |
| `done` | 4 |
| `cancelled` | 5 |
| *(any other)* | `6` (`StateRankUnknown`) |

The State rank table and the four key directions are the pinned contract.
Changing either is a determinism-breaking change: it must be made
deliberately and this document updated with it.

### 3.2 Total, and stable

- **Total:** any two distinct candidates are ordered by the tuple, and the
  final `ID` key breaks every remaining tie. Within a candidate set `ID`
  is unique, so no two distinct candidates compare equal and the order is
  total.
- **Stable:** because the key is total, the sorted result does not depend
  on the input order. "Stable under shuffle" here means *invariance*: every
  permutation of the same multiset produces the same output bytes.

### 3.3 Selection

`SelectNext(cs)` returns the highest-priority **dispatchable** candidate
and `false` when there is none. A candidate is dispatchable iff its
`State` is `planned` or `todo` — the ready states. `in-progress` is
already dispatched and `blocked` / `done` / `cancelled` must not be
(re)dispatched. Selection is the minimum of the total order over the
dispatchable subset, so it too is independent of the input order.

## 4. Determinism test rule

The rule the steward holds itself to, and how it is enforced:

- **Same input → byte-identical decision output.** For a fixed input, a
  decision produces the same bytes on every call and under every
  permutation of the input. Tests pin this for `Order` and `SelectNext`
  (`internal/decision/decision_test.go`): per-key table cases, a
  total-order + shuffle-invariance property test over a fixed seed, an
  across-calls byte-identity test, and an input-immutability test.
- **An LLM-call spy must never fire.** A test double that records any call
  to an agent / LLM client must observe **zero** calls while a control
  decision is computed. At the decision seam this holds *by construction*:
  `internal/decision` imports only `cmp` and `slices`, so it has no client
  to call. `TestDecisionPackageImportsArePure` pins this with an import
  allow-list and fails on any effectful / nondeterministic import (`os`,
  `os/exec`, `net`, `time`, `math/rand`, `context`, …).
- **Golden step sequence.** The control-loop work item must freeze the
  emitted step sequence for a fixture store and assert that the same store
  yields byte-identical steps over N runs, with the scheduler LLM-call spy
  wired at its seam and required to record zero calls.

This is what makes "deterministic" checkable rather than aspirational: a
decision is deterministic iff its inputs are fixed, its output is
byte-stable, and no LLM is consulted.

## Where this is enforced

| Rule | Test |
| --- | --- |
| total order, every tie-break level | `internal/decision/decision_test.go` (`TestOrderPinsEveryTieBreakLevel`) |
| total order + shuffle invariance | `TestOrderIsTotalOrderAndStableUnderShuffle` |
| byte-identical across calls, input not mutated | `TestOrderIsByteIdenticalAcrossCalls`, `TestOrderDoesNotMutateInput` |
| selection is highest dispatchable, order-independent | `TestSelectNextPicksHighestPriorityDispatchable`, `TestSelectNextIsOrderIndependent` |
| no LLM / no I/O by construction | `TestDecisionPackageImportsArePure` |
