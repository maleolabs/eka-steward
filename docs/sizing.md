# eka-steward worker sizing

**Sizing is a pure function with NO LLM input.** The number of workers
the steward runs is decided deterministically from a machine snapshot and
operator config — never from a model. This is the steward's core split
(`docs/determinism.md`): **code decides *which* / *when* / *if*, the LLM
decides *what***. Sizing is one of the "code decides" points.

The rule lives in `internal/sizing` — stdlib only, no I/O, clock,
environment or LLM. It consumes a `Resources` struct; it does **not**
read `cgroup` v2 (`memory.max` / `cpu.max`) itself. Filling `Resources`
from the kernel is a separate, later control-loop work item.

## Formula

```
base = min(floor(CPUCores / 2), floor(MemoryBytes / (4 * GiB)))   # GiB = 1<<30
n    = clamp(base, 1, MaxWorkers)
if CostCeiling > 0 && CostCeiling < n:
    n = CostCeiling
n    = max(n, 1)
```

Two terms, take the smaller:

- **CPU term** — half the logical cores, floored.
- **Memory term** — one worker per 4 GiB, floored.

The smaller of the two wins: a machine is only as wide as its scarcest
resource.

## Clamp and cost ceiling

- The result is clamped to `[1, MaxWorkers]`. `MaxWorkers` is the
  operator's hard upper bound and must be `>= 1`; `MaxWorkers <= 0` is a
  **configuration error** and `Workers` returns it (there is no sensible
  default to guess, so it refuses rather than invent one).
- `CostCeiling` optionally caps the count *further*. `0` means "no
  ceiling"; a value `> 0` only ever **lowers** the result, never raises
  it. A ceiling of `1` yields `1`.
- The final count is re-clamped to `>= 1`: the steward itself must run, so
  nothing here can produce `0` workers.

## Edge behaviour

| Machine / config | Result | Why |
| --- | --- | --- |
| `CPUCores <= 0` or `MemoryBytes == 0` | `1` | Never an error — a degraded but working count beats refusing to run. Documented: the floor of the formula is 0, clamped up to the minimum of 1. |
| 1 core, plenty of RAM | `1` | CPU term `floor(1/2) = 0` → clamped to 1. |
| 64 cores, 2 GiB RAM | `1` | Memory term `floor(2/4) = 0` → clamped to 1. |
| High `MaxWorkers` on a small machine | `1` | `MaxWorkers` is a ceiling, never a floor. |
| Large machine, `MaxWorkers = 5` | `5` | Clamped down to `MaxWorkers`. |
| `CostCeiling` below the result | the ceiling | Ceiling reduces. |
| `CostCeiling` above the result | the result | Ceiling never raises. |
| `CostCeiling = 0` | the result | `0` means "no ceiling". |
| `MaxWorkers <= 0` | error | Configuration error, deterministic message. |

## Purity

`internal/sizing/purity_test.go` pins the property: every non-test file
imports no EKA module and no effectful stdlib package (`os`, process
spawn, network, `syscall`). With that import set there is no way to read
a clock, a cgroup, the environment or an LLM, so the count is a pure
function of `Resources` and `Config`. Go's stdlib `runtime` package is
intentionally not forbidden — it is a different thing from the EKA
modules.
