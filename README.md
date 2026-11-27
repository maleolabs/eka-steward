# eka-steward

**eka-steward** is a thin, deterministic **standalone binary** that
orchestrates AI coding-agent CLIs over the
[Agent Client Protocol](https://agentclientprotocol.com) (ACP) to drive an
EKA project from discussion to execution.

It is a CONSUMER of [`eka-core`](https://github.com/maleolabs/eka-core/v2):
the steward imports **only** the public contract
`github.com/maleolabs/eka-core/v2/api` and holds no domain logic of its own —
all knowledge access belongs in eka-core.

Module path: `github.com/maleolabs/eka-steward/v2` · Steward `2.0.0` · `eka-core v2.0.1`

## Scope of this release

`2.0.0` is a **scaffold**. It ships the product identity, the module
contract, the CI/release pipeline and the version surface. The pieces that
make it an orchestrator — the ACP **control loop** and the **ACP client** —
are separate work items and are deliberately **absent** here. No
abstraction is pre-built for them. The deterministic **driver registry**
(`internal/driver`, see [`docs/drivers.md`](docs/drivers.md)) lands on the
same foundation.

## Invocation: standalone binary, no `eka steward` shim

Steward ships as a standalone binary only:

```sh
go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.0.0
```

plus the release binaries (`install.sh` / `install.ps1`).

There is deliberately **no `eka steward` shim** that would host the steward
inside the `eka` CLI. The plugin/proxy host is **deadline-bounded** and runs
with a **minimal environment** — both hostile to a long-running orchestrator
that manages child agent processes over ACP. `eka-cli` remains the escape
hatch for one-shot, in-CLI knowledge operations. Hosting steward behind MCP
was **rejected as the driver** for the same reasons (a request/response
tool host is the wrong shape for a supervising loop). See
[`docs/adr/0001-standalone-binary.md`](docs/adr/0001-standalone-binary.md).

## Layout

```
cmd/                     Cobra command tree (the only production package)
  eka-steward/main.go    binary entrypoint → cmd.Execute
  root.go                root command, exit-code contract
  version.go             version var (ldflags) + `version` command
  *_test.go              boundary, layout-pin and version tests
internal/escapehatch/    the one audited subprocess adapter (allow-list)
internal/decision/      pure, deterministic selection/order kernel (stdlib only)
internal/loop/          deterministic control loop (Plan/Step/Run, lock, cgroup sizing, resume state)
internal/pool/          bounded worker pool: concurrency + deterministic backpressure (no EKA)
internal/guard/         worker containment: env allow-list, per-worker worktree, permission policy, driver pinning
internal/driver/        deterministic driver registry (parse/resolve/select + probe seam)
internal/role/          steward worker-role config (parse + resolve; no EKA)
internal/sizing/        pure worker-count sizing (no I/O, no LLM)
internal/render/        pure, bounded context-payload renderer (stdlib only)
docs/adr/                architecture decision records
docs/boundary.md         the two hard boundary rules (api-only, one escape hatch)
docs/control-loop.md     control loop: seams, pure Plan vs Step, lock, cgroup, state
docs/backpressure.md     bounded worker pool: sizing-bounded concurrency, deterministic overflow, metrics
docs/checkpoint.md       crash-safe checkpoint/resume: intent log, reconcile, crash matrix
docs/recovery.md         bounded retry, cancel semantics, failure -> no partial write
docs/determinism.md      code decides which/when/if, the LLM decides what
docs/drivers.md          driver registry: config, adapters, probing (never writes EKA)
docs/guardrails.md       worker containment: four layers, VCS-isolation-not-a-boundary, residual risks
docs/roles.md            roles are steward config; no role reads or writes EKA
docs/sizing.md           the deterministic worker-count formula (no LLM input)
docs/context.md          the bounded, byte-stable context payload (never writes EKA)
scripts/bump.sh          version bump + tag automation
anvil.yaml               anvil project + lifecycle definition
.anvil/pipelines/        CI and release build pipelines
install.sh, install.ps1  release installers
```

The **layout is pinned**: `cmd/layout_test.go` asserts the exact set of
top-level entries and fails on drift, so a new top-level file is a
deliberate, reviewed change.

## Usage

```sh
eka-steward --version      # bare build version, e.g. 2.0.0
eka-steward version        # build version + eka-core contract version
eka-steward --help
```

## Development

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...
anvil pipeline ci # format + vet + windows cross-compile + race tests
```

### Consuming eka-core: `api` only

Production code may import `github.com/maleolabs/eka-core/v2/api` and
nothing else from eka-core. The kernel-internal packages — `runtime`,
`store`, `workspace`, `sync` — are private implementation details and are
forbidden in non-test code (ADR-014, ADR-030). `cmd/boundary_arch_test.go`
walks every non-test `.go` file and fails on any such import. The steward
never writes the EKA store directly.

The one place the steward leaves its own process — running another CLI —
goes through the single adapter package `internal/escapehatch`; no other
non-test package may import `os/exec`, and the adapter runs only
allow-listed executable base names. MCP is not a dependency. Both rules
and the allow-list policy are documented in
[`docs/boundary.md`](docs/boundary.md) and enforced by tests.

## Determinism

Steward's core split — **code decides *which* / *when* / *if*, the LLM
decides *what*** — is specified in
[`docs/determinism.md`](docs/determinism.md): the code-owned vs LLM-owned
table, every control-flow decision point shown to be LLM-free, the total
dispatch order, and the determinism test rule (same input → byte-identical
output; an LLM-call spy must never fire). The pure ordering/selection
kernel that pins the order is `internal/decision` — stdlib only, no I/O,
clock, randomness or LLM.

## Roles

Worker roles are **steward config** — a role resolves deterministically
to a driver name + a prompt template, and **no role reads or writes
EKA**. The config shape, resolution rules and strictness rules live in
[`docs/roles.md`](docs/roles.md); the parser/validator is
`internal/role` (stdlib + `gopkg.in/yaml.v3` only; imports no EKA
package and no filesystem/network/subprocess).

## Drivers

A *driver* is how the steward launches and talks to one coding-agent CLI —
steward config, never EKA knowledge. Drivers are declared **strictly** as
`{tool, cmd, args}` under a `drivers:` list; the wire **kind** is derived
from the tool's registered adapter, not configured. Known tools:
`opencode`/`gemini` are `acp` (JSON-RPC over stdio), `claude`/`codex` are
`cli` (no ACP). Resolution and selection are deterministic; an unknown tool
is a named error (`driver.ErrUnknownTool`). Capability probing is **recorded**
on the resolved driver (`Capabilities{Kind, Version, ACPVersion, Raw}`)
through a `Runner` seam — a fake in unit tests, an
`internal/escapehatch`-backed `ExecRunner` in production (the only file in
the package that spawns). The shape, adapter table, probe seam and the
deferred streaming ACP client live in [`docs/drivers.md`](docs/drivers.md);
the parser/seam is `internal/driver`.

## Worker sizing

The number of workers is **deterministic and LLM-free** — a pure function
of a machine snapshot and config (`internal/sizing`, stdlib only).
`clamp(min(CPUCores/2, MemoryBytes/4GiB), 1, MaxWorkers)`, optionally
reduced by a cost ceiling. The formula, the clamp, the cost-ceiling rule
and the edge-behaviour table are in
[`docs/sizing.md`](docs/sizing.md). The snapshot is filled from **cgroup
v2** (`memory.max` / `cpu.max`, never host totals) by `internal/loop`; an
unknown/`max`/missing limit falls back conservatively to the 1-worker floor
(see [`docs/control-loop.md`](docs/control-loop.md)).

## Control loop

`internal/loop` is the deterministic control loop that binds the pure
decision kernel (`internal/decision`), the single write path
(`internal/ekawrite`), the cgroup-backed sizing (`internal/sizing`) and a
persisted resume state. `Plan` is a pure selection (LLM-free, byte-stable,
frozen in a committed golden file); `Step` dispatches exactly one candidate
through the `Dispatcher` seam and records any mutation through the `Writer`
seam (→ `ekawrite`); `Run` drives `Step` to quiescence. A single-instance
lock (crash-safe `flock` on unix, exclusive-create on windows) refuses a
second steward on the same workspace. The lifecycle, the three seams, the
lock semantics, the cgroup fallback and the versioned state format are in
[`docs/control-loop.md`](docs/control-loop.md).

The loop is **crash-safe**: each step is a checkpointed write-ahead
transaction (intent → dispatch → apply → done), and a restart reconciles any
in-flight intent without re-dispatching — a same-form re-issue is a no-op
(a transition at/after its target; a publish through `ekawrite`'s replay
gate), so no EKA write is applied twice and completed work is not
re-dispatched. The checkpoint location, the intent protocol, the reconcile
rules per write class and the crash matrix are in
[`docs/checkpoint.md`](docs/checkpoint.md).

## Backpressure

With `Config.QueueLimit > 0` the loop runs through a **bounded worker pool**
(`internal/pool`): `Run` admits at most `QueueLimit` candidates in the pinned
`internal/decision` order and drains them with up to `sizing.Workers(...)`
workers. Concurrency is bounded by the sizing policy, the queue is bounded, and
a candidate past the limit is refused with `pool.ErrQueueFull` — a
**deterministic** overflow (the deterministic accepted set is a pure function of
the candidates, `QueueLimit` and the persisted state), never a nondeterministic
drop: refused candidates stay undone and are re-selected by the next `Run`.
Saturation (queue depth, in-flight, dispatcher, rejections, peak) is exposed
through the minimal `pool.Metrics` seam. `QueueLimit == 0` keeps the unchanged
sequential loop; `MaxWorkers == 1` stays byte-identical to the golden plan
sequence. The model, the exact overflow rule, the metrics seam and the
bounded-resource guarantees are in
[`docs/backpressure.md`](docs/backpressure.md).

## Recovery: retry, cancel & no partial write

`internal/loop` bounds what happens when a dispatch **fails**, **times out** or
is **cancelled**, and guarantees a failed worker leaves **no partial EKA
write**. A `RetryPolicy` (`MaxAttempts` = total attempts, so `1` means *no
retry*; a deterministic, jitter-free `Backoff` on an injectable clock) is
driven **by the loop, not the pool**, so each attempt is its own write-ahead
transaction: the intent is rolled back the moment an attempt fails and the
candidate is left un-`Done` (deferred, never dropped) after exhaustion.
`Config.AttemptTimeout` makes a hung worker a **retryable** timeout, bounded by
`MaxAttempts`. `Loop.Cancel()` cancels the **current batch** — every in-flight
worker's context is cancelled, its intent rolled back (no write), its candidate
left un-`Done`, and `Run` returns `nil` (a clean, deterministic stop); `Cancel`
is idempotent. Retries and cancellations are reported through the `pool.Metrics`
seam (`Retries`, `Cancellations`). The policy, the attempt-per-transaction
protocol, the cancel semantics, the failure→no-partial-write table and where
each property is enforced are in [`docs/recovery.md`](docs/recovery.md).

## Context payload

The payload handed to a worker is rendered by `internal/render` — a pure,
stdlib-only function (`Render(Input, Options) ([]byte, error)`) that turns a
caller-ordered selection of knowledge into deterministic markdown. Identical
input yields byte-identical output: no map iteration, no clock, no
randomness, no LLM, and it never writes EKA. A hard byte budget caps the
payload; overflow is deterministic under a documented truncation order
(`note` → `knowledge` → `constraint` → `acceptance` → `instruction` →
header, unknown kinds first), cutting at a line boundary and never splitting
a UTF-8 rune. The shape, the budget, the precedence table and the UTF-8 rule
are in [`docs/context.md`](docs/context.md); the exact bytes are pinned by
`internal/render/testdata/context.golden`.

## Containment (guardrails)

Every worker runs **contained by default**, with **no opt-out in v1**
(`internal/guard`). Four independent layers:

- **Environment** — `guard.WorkerEnv` reuses `ekawrite.WorkerEnv`: a worker
  runs with the allow-listed env only (no `HOME`, no `EKA_HOME`, no `PATH`,
  no credential variable), so the default EKA store path and host
  credential dotfiles are not derivable from the ambient environment.
- **Worktree** — each worker gets its own git worktree via
  `internal/escapehatch` (allow-list `git`). This is **VCS isolation, not a
  security boundary**: it separates checkouts, it does not confine
  filesystem access.
- **Permission policy** — an ACP permission request is **default-deny**:
  only a `read`-class request inside the workspace root may be allowed;
  every wait is **bounded** so a worker **never hangs**; an unknown
  `optionId` is never approval.
- **Driver pinning** — `guard.RequirePinned` refuses any driver without an
  exact `version` and sha256 `checksum`, and verifies the resolved binary.

The four layers, the permission decision table, the pinning rule, and the
honest residual risks (absolute-path execution bypasses `PATH`; the worktree
is not confinement; a checksum TOCTOU window) are in
[`docs/guardrails.md`](docs/guardrails.md).

## Deliberate omissions vs eka-cli

Steward mirrors eka-cli's conventions (Cobra `cmd/` tree, anvil pipelines,
installers, `scripts/bump.sh`) but drops everything that made eka-cli a
repository generator or an EKA repository itself:

- **No EKA standard declaration / embedded skeleton** (`EKA`,
  `standardembed*.go`, `hooksembed.go`, `bootstrap/`, `templates/`).
  Steward is not a repo generator; the standard declaration lives in
  eka-standard and the bootstrap engine in eka-core. Vendoring them here
  would duplicate a source of truth.
- **No Layer-2 capture hooks / `platform_sync` lifecycle.** There is no
  vendored `eka-standard/templates/hooks` to symlink, so the anvil
  `platform_sync` step is dropped (noted in `anvil.yaml`).
- **No `feedback` / MCP / telemetry surface.** Steward drives agents; it
  is not the knowledge CLI.
- **CI runs the minimal gate** (format, vet, windows cross-compile, race
  tests). eka-cli's installer e2e jobs and repository-scope `eka verify`
  gate are omitted: steward is not an EKA repository, and its installers
  are thin adaptations of eka-cli's already-covered ones.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
