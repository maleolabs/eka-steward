# Work selection

`internal/plan` is the steward's pure, deterministic work-selection planner:
given a dependency graph of work units it decides the **order** in which they
may be dispatched and **which** one comes next, honouring `depends-on`
prerequisites. It is the planner behind the control loop's `Plan` / `Step`
(`docs/control-loop.md`).

It keeps the steward's core split (`docs/determinism.md`): **code decides
*which* / *when* / *if***. Selection is a pure function of the graph — no LLM,
no clock, no I/O. See [the no-LLM invariant](#no-llm-invariant).

## Model

```go
type Node struct {
    ID             string // unique within the node set; final tie-break key
    State          string // lifecycle execution-state (planned, todo, ...)
    ContainerID    string // the container the unit belongs to ("" = none)
    ContainerOrder int    // container rank; lower dispatches first (0 = unranked)
    Priority       int    // decision total-order primary key (higher first)
    CreatedSeq     uint64 // decision total-order FIFO key (ascending)
}

type Edge struct{ From, To string } // To depends-on From
```

An edge `{From: P, To: D}` means **D depends on P**. P is ordered before D and,
until P is terminal, D is blocked.

The planner reads only these fields. Everything else about a unit is opaque
data to it.

## The selection order

Selection layers three rules, in this priority:

1. **Topological over `depends-on`.** A unit is ordered after every prerequisite
   it depends on. This is Kahn's algorithm over the dependency DAG.
2. **Container order.** Among units whose remaining prerequisites are
   satisfied, the lower `ContainerOrder` dispatches first. This groups work by
   its container/milestone.
3. **State adjacency, then the decision total order.** Within one container
   order, the pinned `internal/decision` total order applies, whose state
   rank is the lifecycle adjacency `planned → todo → in-progress → blocked →
   done → cancelled` (so `planned` precedes `todo`).

### Exact tie-break contract

After the topological constraint, the deterministic key between two eligible
units is, in order:

| # | Key | Direction | Rule |
| --- | --- | --- | --- |
| 1 | `ContainerOrder` | ascending | lower container rank first; `0` = unranked |
| 2 | `Priority` | descending | higher priority first |
| 3 | `State` rank | ascending | lifecycle order, `planned` (0) before `todo` (1) |
| 4 | `CreatedSeq` | ascending | FIFO within equal container, priority and state |
| 5 | `ID` | ascending | final tie-break; makes the order total |

Keys 2–5 are exactly `internal/decision`'s pinned order (reused, not
duplicated: `internal/plan` imports `decision.Compare`, `decision.StateRank`
and `decision.Dispatchable`). `ContainerOrder` is the only new, plan-level
key, and it is 0 for every unit in the flat case.

**Backward compatibility.** With no dependency edges and one container order
(all units `ContainerOrder == 0`), key 1 is constant and the key reduces to
`decision.Order`. `Order` is therefore byte-identical to the pre-planner
order, so the control loop's committed golden sequence
(`internal/loop/testdata/steps.golden`) is unchanged. This is pinned by
`TestNoEdgesMatchesDecisionOrder` (`internal/plan`) and the existing loop
golden tests.

> **Deviation from the design sketch.** The work item sketched the tie-break
> as `(container order, state rank, priority desc, createdSeq, ID)`. Ordering
> *state rank before priority* would reorder the flat fixture (`planned` node
> `d` would jump ahead of a higher-priority `todo`) and break the pinned loop
> golden and the documented `internal/decision` contract. To keep one coherent
> order we insert `ContainerOrder` ahead of the existing decision tuple rather
> than reshuffling the decision keys themselves. Container grouping (rule 2)
> and state adjacency (rule 3) are both still honoured.

## Dispatchable, blocked, and selected

A unit is **dispatchable** iff its `State` is one of the ready states
`planned` or `todo` (`decision.Dispatchable`). `in-progress` is already
dispatched; `blocked`, `done`, `cancelled` and unknown states are not ready.

A unit is **blocked** iff it has at least one `depends-on` prerequisite (an
in-scope `From`) that is **not terminal**. Terminal states are `done` and
`cancelled`.

> **A unit with an incomplete prerequisite is blocked and is never selected**,
> even when it has the highest priority and a ready state. The blocked unit
> becomes dispatchable only once every prerequisite reaches a terminal state.

`Selectable` returns the ordered nodes that are both dispatchable and not
blocked; `SelectNext` returns its head. `Blocked` lists the non-terminal
blocked units, in the deterministic order.

An edge whose `From` or `To` is not in the node set is **ignored**: an
out-of-scope prerequisite cannot be ordered or driven terminal, so it is not a
planning input. This keeps the planner usable over a project scope where a
unit may reference a prerequisite in another project.

## Cycles

A `depends-on` cycle is a deterministic **error**, never an infinite loop:
`Order` returns an error naming the unordered nodes (sorted, so the message is
byte-stable for the same input). `Selectable`/`Blocked` propagate the same
error; `SelectNext` returns `(Node{}, false)` — with no valid order, nothing is
safely dispatchable. A self-edge (`From == To`) is a cycle.

## Determinism

`Order` sorts a copy and never mutates its input. The key is a total order (IDs
are unique), so the result is **byte-identical for any permutation of the same
graph** and across repeated calls. Tests pin 20+ runs and shuffled inputs
(`TestOrderIsDeterministicAcrossRunsAndPermutations`), a duplicate-ID error,
and the cycle error.

## Integration with the loop

`internal/loop` plans over the Source's graph through the optional
`GraphSource` capability:

```go
type GraphSource interface {
    Graph(ctx context.Context) ([]plan.Node, []plan.Edge, error)
}
```

- `StoreSource` implements `Graph` **read-only** through `api.Knowledge`: each
  work item becomes a node (ID = `"<ns>/<type>:<id>"`, state =
  `execution-state`). A `depends-on` relationship to a container (type `ctr`)
  sets `ContainerID`; any other `depends-on` target becomes an edge
  `{From: target, To: unit}`. Targets are normalized to the qualified line form
  (a trailing `:instance-version` is dropped) so an endpoint matches a node ID.
  `ContainerOrder` is 0 in this cut — EKA carries no container ordinal — and
  the planner treats 0 as unranked.
- A Source that does **not** implement `GraphSource` is planned as a flat
  candidate set (one container order, no edges), which is byte-identical to the
  pre-planner order.

`Loop.Plan` returns `plan.Selectable` as steps; `Loop.Step` dispatches its
head; the pooled path (`QueueLimit > 0`) admits the same `Selectable` set.
Units already recorded `done` in the checkpoint are dropped before planning. A
cycle surfaces as a `Plan`/`Step` error.

## No-LLM invariant

Selection is code-owned and LLM-free **by construction**: the planner imports
only `cmp`, `fmt`, `slices`, `strings` and `internal/decision` (itself
effect-free). With this import set it has no way to call an agent / LLM client,
read a clock, draw randomness or touch the filesystem or network.
`TestPlanPackageImportsArePure` (`internal/plan/purity_test.go`) pins the
allow-list and fails on any effectful import. The loop's `Plan` is additionally
guarded by the LLM-call spy test (`TestPlanNeverCallsDispatcher`): a dispatcher
that panics on any call observes zero calls during planning.

## Fixture and golden

`internal/plan/testdata/graph.golden` freezes the exact outcome for the
fixture graph in `internal/plan/plan_test.go`, which contains a diamond
(`A→B, A→C, B&C→D`), a container/order tie (`c1` order 0 vs `c2` order 1), a
state-adjacency case (`planned` before `todo`), and a blocked unit (`E`
depends-on `F`, still `planned`) that must never be selected:

```json
{
  "order":     ["Z","F","A","C","B","G","E","D","H"],
  "selection": ["F","A","G","C","B","E","D","H"],
  "blocked":   ["C","B","E","D"],
  "first":     "F"
}
```

`E` has the highest priority but is blocked by `F`, so the head is `F`, never
`E`. `Blocked` lists `C`, `B`, `E`, `D`; none appears in `Selectable` while its
prerequisite is incomplete.