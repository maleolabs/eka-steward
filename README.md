# eka-steward

**eka-steward** is a thin, deterministic **standalone binary** that
orchestrates AI coding-agent CLIs over the
[Agent Client Protocol](https://agentclientprotocol.com) (ACP) to drive an
EKA project from discussion to execution.

It is a CONSUMER of [`eka-core`](https://github.com/maleolabs/eka-core/v2):
the steward imports **only** the public contract
`github.com/maleolabs/eka-core/v2/api` and holds no domain logic of its own —
all knowledge access belongs in eka-core.

Module path: `github.com/maleolabs/eka-steward/v2` · Steward `2.2.0` · `eka-core v2.0.1`

## Scope of this release

`2.2.0` aligns the CLI's human output with the **eka-cli design system**, so
`eka steward ...` looks like `eka`, and makes the machine report explicit:

- **Design-system output** (`internal/ui`, `cmd/render.go`) — the same left
  margin, ANSI 256-color palette, Unicode icons and aligned
  header/table/summary blocks `eka` renders. `run` prints a themed human report
  by default; `version` renders through the same style (plain, margin-indented).
- **`run --json`** — the deterministic machine report (`obs.Report.Write`),
  byte-identical to the pre-`2.2.0` output, on the raw writer (no margin, no
  color). Only the human default is restyled; the machine bytes never change.

`2.1.0` added the **runtime surface** on top of the `2.0.0` orchestrator core:

- **`run`** (`internal/app`, `cmd/run.go`) — assembles the production seams
  (config, EKA runtime, read-only Source, the one write path, the ACP worker
  dispatcher, the Recorder and the TTY-free approval channel) and drives a
  project to quiescence under the single-instance lock.
- **ACP streaming client** (`internal/acp`) — live JSON-RPC 2.0 over an agent's
  stdio: per-connection version dispatch, `session/update` streaming, permission
  routing through the guard policy and the human-approval channel, deterministic
  cancel. Spawn stays in `internal/escapehatch` (streaming `Start`/`Proc`).
- **Machine protocol** (`internal/protocol`, [`docs/protocol.md`](docs/protocol.md))
  — the versioned, language-agnostic product boundary: JSON-RPC 2.0 over stdio
  or a local unix socket (`serve`), with idempotency keys and deterministic
  responses.
- **Event stream** (`internal/obs/stream.go`) — ordered, monotonic, redacted
  notifications over the protocol (`events.subscribe`).
- **eka-cli plugin** (`internal/plugin`) — official plugin distribution and
  `eka steward` dispatch (see below).
- **Hardened credential source** (`internal/secrets/file.go`) — a `StateDir`
  file-backed resolver (`O_NOFOLLOW`, regular file, current-uid owner, mode
  `0600`, size cap), the credential source under the plugin host's minimal
  environment.

`2.0.0` shipped the deterministic orchestrator **core**: the ACP **control
loop** (`internal/loop` — pure `Plan` vs `Step`, single-instance lock,
cgroup sizing, checkpoint/resume), the **worker pool** with backpressure
(`internal/pool`), deterministic work selection (`internal/plan`), the
per-driver **trust + capabilities** model (`internal/driver`), **role
guardrails** (`internal/guard`), **human approval** (`internal/approval`),
the **secrets broker** (`internal/secrets`), **review roles**, the
**discussion phase**, and **observability** (`internal/obs`). All EKA
writes go through one path (`internal/ekawrite`).

**Deferred.** OS-keyring / peer-authenticated IPC credential delivery (option
B/C of `adr:steward-as-eka-cli-plugin`) is **not shipped**: the hardened
file-backed source (option A) is the accepted v1 design. The follow-up
(`steward-credential-keyring`) is canceled, not retired, so it stays
re-activatable.

See [`RELEASE_NOTES.md`](RELEASE_NOTES.md) and [`docs/release.md`](docs/release.md).

## Invocation: standalone binary + official eka-cli plugin

Steward ships as a **standalone binary** — the primary artifact:

```sh
go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.2.0
```

plus the release binaries (`install.sh` / `install.ps1`).

It is also an **official `eka-cli` plugin**, so `eka-cli` is a human entry
point with no PATH install:

```sh
eka plugin install steward     # resolves the official registry entry steward → maleolabs/eka-steward
eka steward <args>             # dispatches to eka-steward <args>
```

The install is **checksum-verified** (fail-closed against the release's
`SHA256SUMS.txt`) and the binary hash is **re-verified on every dispatch**.
Dispatch inherits the CLI's stdio and propagates the exit code, and the plugin
host runs the binary under a **bounded environment whitelist**
(`plugin.PluginEnv()`: `PATH`, `HOME`, `EKA_PLUGIN_DIR`, `EKA_HOME`,
`SystemRoot`). The plugin shell **adds no deadline** to command
dispatch (only the manifest/`install` contract probes are bounded), so both the
one-shot `run` and the long-running `serve` dispatch correctly. Steward
implements the plugin contract (`manifest --json`, `install --json`;
`internal/plugin`) and declares no `install`/`mcp` capability.

This **amends** [`docs/adr/0001-standalone-binary.md`](docs/adr/0001-standalone-binary.md):
steward remains a standalone binary, and the plugin is a **distribution/dispatch
adapter over it, not a host**. The original concern — a deadline-bounded
in-process host is the wrong shape for a supervising loop — still holds, which
is why the plugin dispatches the binary as a subprocess rather than hosting the
loop. Governing decisions: `adr:steward-as-eka-cli-plugin`,
`adr:steward-machine-protocol`.

## Layout

```
cmd/                     Cobra command tree (the only production package)
  eka-steward/main.go    binary entrypoint → cmd.Execute
  root.go                root command, exit-code contract
  version.go             version var (ldflags) + `version` command
  run.go, serve.go       `run` (drive to quiescence) + `serve` (machine protocol over stdio/socket)
  render.go              the themed human `run` report (header + tables + summary) via internal/ui
  manifest.go, install.go  eka-cli plugin subcommands (`manifest --json`, `install --json`)
  *_test.go              boundary, layout-pin and version tests
internal/escapehatch/    the one audited subprocess adapter (allow-list); streaming Start/Proc spawn
internal/app/            run surface: assembles the production seams, drives the loop to quiescence, single-instance lock
internal/acp/            live ACP client: JSON-RPC 2.0 over stdio, version dispatch, permission routing, cancel
internal/protocol/       versioned machine protocol server: JSON-RPC 2.0 over stdio or a unix socket
internal/plugin/         eka-cli plugin self-description: manifest/install (contract v1)
internal/ekawrite/       the ONE EKA write path (attribution, replay gate, ADR-032 provenance/adopt)
internal/secrets/        credential broker: declared-only worker creds, hardened file source, egress policy, scoped git auth
internal/decision/      pure, deterministic selection/order kernel (stdlib only)
internal/discuss/       discussion phase: bounded, resumable turns recorded as EKA decisions
internal/loop/          deterministic control loop (Plan/Step/Run, lock, cgroup sizing, resume state)
internal/pool/          bounded worker pool: concurrency + deterministic backpressure (no EKA)
internal/obs/           observability: structured step log, metrics, run report, secret redaction
internal/guard/         worker containment: env allow-list, per-worker worktree, permission policy, driver pinning
internal/driver/        deterministic driver registry (parse/resolve/select + probe seam)
internal/role/          steward worker-role config (parse + resolve; no EKA)
internal/config/        versioned, strict combined config schema (composes role/driver/sizing; no EKA)
internal/sizing/        pure worker-count sizing (no I/O, no LLM)
internal/render/        pure, bounded context-payload renderer (stdlib only)
internal/ui/            design-system mirror: margin, 256-color palette, icons, aligned header/table/summary (stdlib only)
internal/plan/          pure dependency-graph planner: topological + container + state order (no I/O, no LLM)
docs/adr/                architecture decision records
docs/boundary.md         the two hard boundary rules (api-only, one escape hatch)
docs/control-loop.md     control loop: seams, pure Plan vs Step, lock, cgroup, state
docs/selection.md        work-selection spec: topological over depends-on, container order, blocked rule, no LLM
docs/discuss.md          discussion phase: lifecycle, code vs LLM split, recorded dec-, resume, next-phase input
docs/backpressure.md     bounded worker pool: sizing-bounded concurrency, deterministic overflow, metrics
docs/checkpoint.md       crash-safe checkpoint/resume: intent log, reconcile, crash matrix
docs/recovery.md         bounded retry, cancel semantics, failure -> no partial write
docs/upgrade.md          schema migration registry (checkpoint + config), version range, refusal, forward-only rollback note
docs/observability.md    structured step log, metric names, run report, secret redaction, live event stream
docs/protocol.md         the versioned machine protocol spec: transport, methods, events, error codes, integration guide, golden transcripts
docs/write-path.md       the one audited EKA write path: attribution, replay gate, ADR-032 provenance/adopt
docs/review.md           review stage: policy, deterministic reviewer, review-note gate
docs/approval.md         human approval: escalation rule, no-TTY channel, typed artifact, bounded wait, resume
docs/determinism.md      code decides which/when/if, the LLM decides what
docs/drivers.md          driver registry: config, adapters, probing (never writes EKA)
docs/acp-spike.md        ACP client spike: live agent/v2 evidence, permission path, structured-context handoff
docs/guardrails.md       worker containment: four layers, VCS-isolation-not-a-boundary, residual risks
docs/secrets.md          per-driver credential declarations, the broker, egress allow-list + enforcement gap, scoped git auth
docs/roles.md            roles are steward config; no role reads or writes EKA
docs/config.md           the one versioned, strict config schema (roles/drivers/sizing/sandbox/loop) + migration policy
docs/sizing.md           the deterministic worker-count formula (no LLM input)
docs/context.md          the bounded, byte-stable context payload (never writes EKA)
docs/release.md          release train, version source, pinned eka-core/api, ACP posture, install path, independence from the EKA line
docs/release-checklist.md  the release owner's steps: tag → workflow → confirm assets → verify install
RELEASE_NOTES.md         paste-ready release notes (pinned core, ACP posture, WI highlights)
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
eka-steward --version        # bare build version, e.g. 2.2.0
eka-steward version          # build version + eka-core contract version
eka-steward run …            # drive a project to quiescence (human report; --json machine report)
eka-steward serve …          # host the machine protocol (stdio; --socket <path> for a unix socket)
eka-steward manifest --json  # the eka-cli plugin self-description
eka-steward install --json   # the (empty) plugin artifact-install result
eka-steward --help
```

`run` and `serve` share the same flags (`--project`, `--config`, `--role`,
`--driver`, `--repo`, `--state-dir`, `--dispatch-state`); `--project` is
required, and `--config`/`--role` are required in production:

```sh
eka-steward run   --project <id> --config steward.yaml --role implementer
eka-steward serve --project <id> --config steward.yaml --role implementer
```

A consumer drives `serve` over the protocol (initialize → plan/step/run → status
→ shutdown); a minimal stdlib-only client and the golden transcripts are in
[`docs/protocol.md`](docs/protocol.md).

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

## Configuration

The steward's configuration is **one versioned, strict document** combining
every section — `roles`, `drivers`, `sizing`, `sandbox` and `loop` — under a
single `version` field. `internal/config` parses it strictly (unknown fields
refused anywhere), reuses the per-section parsers (`internal/role`,
`internal/driver`, `internal/sizing`) so config and standalone behaviour
agree, gates and migrates the version, and validates across sections (at
least one role, every role's driver declared, `sizing.maxWorkers >= 1`,
sandbox contained by default). The full schema, the strictness rules, the
validation rules and the version/migration policy live in
[`docs/config.md`](docs/config.md).

## Roles

Worker roles are **steward config** — a role resolves deterministically
to a driver name + a prompt template, and **no role reads or writes
EKA**. The config shape, resolution rules and strictness rules live in
[`docs/roles.md`](docs/roles.md); the parser/validator is
`internal/role` (stdlib + `gopkg.in/yaml.v3` only; imports no EKA
package and no filesystem/network/subprocess).

### Review

When `loop.Config.Review` is enabled, a worker's output is routed to a
**deterministically-selected review role distinct from the author** before the
work item completes. `SelectReviewer` takes the sorted `ReviewRoles()` and skips
the author role — same input, same reviewer; no eligible reviewer is a
deterministic error, never a self-review. The verdict (`approve` /
`changes-requested`; `reject` is an alias) is recorded through
`internal/ekawrite` as a **resolved EKA review note** (the `eka note --role
review` convention), and the loop advances/marks done **only** on approval. The
zero-value policy disables review entirely, so the loop is byte-identical to the
pre-review loop. The policy, the selection rule, the note convention, the gate
and the crash-safety boundary are in [`docs/review.md`](docs/review.md).

### Human approval

When `loop.Config.Approval` is enabled, a permission request the pure guard
policy **cannot decide** (`execute`/`edit`/`delete`/`move`, or a read outside the
workspace root) is escalated to a human **before** dispatch instead of being
silently denied. Clearly-deniable requests (a non-`allow_once` option, a
forbidden in-root read) stay `Deny`; `Policy.DenyUndecidable` opts out of
escalation entirely. The default channel is a **no-TTY** file/queue
(`internal/approval`): a structured request file is published and a structured
response file is polled with a **bounded wait** whose timeout is always a deny.
The decision is recorded as a **resolved review-note** artifact through
`internal/ekawrite` (the schema fixes the `role` vocabulary, so `role: review`
carries it), with a deterministic replay gate so a resume records it **exactly
once**. The escalation rule, the channel, the typed-artifact convention and the
resumption guarantee are in [`docs/approval.md`](docs/approval.md).

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
the package that spawns). A driver is **KNOWN only when pinned** (exact
version + sha256 checksum); `driver.ParseTrusted` / `config.ParseTrusted` are
the fail-closed acceptance points, and `claude`/`codex` vendor CLI adapters
carry an in-repo pin/audit table plus a deterministic `capabilities.json`
artifact. The shape, adapter table, probe seam, trust rule and vendor pin table
live in [`docs/drivers.md`](docs/drivers.md); the parser/seam is
`internal/driver`, and the streaming ACP client that dispatches a worker is
`internal/acp` (see [`docs/protocol.md`](docs/protocol.md) for the protocol
boundary).

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

The persisted **schemas** (the checkpoint and the config document) migrate
**forward** through an explicit, ordered registry, and a version the running
steward cannot reach is **refused** deterministically rather than guessed at —
never silent corruption. Reverting steward does **not** revert EKA: applied
writes are **forward-only** and a schema downgrade is unsupported and refused.
The registries, the supported version range, the in-flight-across-upgrade
behaviour and the rollback note are in [`docs/upgrade.md`](docs/upgrade.md).

## Discussion

`internal/discuss` implements the **discussion phase**: a bounded, resumable,
deterministic turn loop that drives a worker to produce decisions for one work
item, records them as EKA knowledge, and derives the execute phase's input from
that recorded knowledge. **CODE owns the orchestration** — the round count
(`Config.Rounds`, clamped to `MaxRounds`), the role (validated against
`internal/role`), and the stop condition; the **worker owns the content**. Each
round's bounded context is assembled with `internal/render` and handed to the
worker through the existing `Dispatcher` seam (extended with the payload, as
review extended it). A decision is recorded as a **`dec-` decision record**
published through `internal/ekawrite`; because its creation goes through the
replay gate, a resumed re-issue is a no-op (no `max+1` instance). Each round is
a checkpointed transaction over the loop's write-ahead intent log, so a kill at
any boundary resumes without a duplicate record and with a deterministic round
sequence. `BuildExecuteInput` reads the `dec-` units back through
`api.Knowledge` — never from in-memory state — and renders the next phase's
input. The lifecycle, the code-vs-LLM split, the `dec-` choice, the resumability
protocol and the store-derived next-phase input are in
[`docs/discuss.md`](docs/discuss.md).

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

## Observability: structured log, metrics & run report

`internal/obs` turns the loop's observability seams into a structured **JSON
step log**, the **metrics** and a deterministic **run report**, and redacts
secrets before anything is written. Wire ONE `obs.Recorder` into both
`loop.Config.Metrics` and `loop.Config.Observer`:

- **Log** — one `log/slog` JSON record per loop step with `driver`, `role`,
  `outcome`, `candidateId`, `stepKind`, `attempt`, `durationMs`.
- **Metrics** (`pool.Metrics` + step count) — `steps`, `retries`,
  `cancellations`, `peakInFlight`, `queueDepth`, `dispatched`, `rejected`
  (and `inFlight`, `peakQueueDepth`), read back with `Recorder.Snapshot()`.
- **Report** — `Report{ Steps, Writes }` serialized deterministically
  (`Report.Write` / `Report.MarshalJSON`, sorted, no map iteration) listing the
  steps (candidateId, kind, outcome, role, driver, duration) and the EKA writes
  (line, type, id, instanceVersion, kind, result).
- **Redaction** — `sk-…`, `AIza…`, `ghp_…`, `Bearer …` tokens and an operator
  deny-list are masked (`■■■`) before any log or report byte.

The record/metric/report shapes and the redaction rule — including the residual
risk that secrets inside model output are caught only by pattern — are in
[`docs/observability.md`](docs/observability.md).

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

## Secrets & egress

A worker receives **only** the credentials its driver **declares**
(`internal/secrets` + `internal/driver/credentials.go`): the credential-free
`guard.WorkerEnv` base plus exactly the declared names from an injected
resolver — never `os.Environ()`, so `GH_TOKEN`, `SSH_AUTH_SOCK`, `AWS_*` and
`HOME` do not leak. The steward's own git remote ops use a **scoped**,
broker-provided token, not the host credential. Worker egress is a
**declarative** allow-list of model endpoints — validated and tested, but
**not** enforced by the steward (a sandbox/netns/firewall must enforce it). See
[`docs/secrets.md`](docs/secrets.md).

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

## Release

`eka-steward` ships on its **own `v*` tag train** in its own repository, with
its own [`.github/workflows/release.yml`](.github/workflows/release.yml) —
independent of the `eka-cli` / `eka-mcp` / `eka-core` release train. The
version is `anvil.yaml` → `project.version`, guarded by the workflow against
the tag; the binary is stamped with `cmd.version` via `ldflags`. Current
release: [`RELEASE_NOTES.md`](RELEASE_NOTES.md) (`v2.2.0`).

The pinned dependency, the ACP transport posture, the platform/asset naming
contract, and the install paths (`install.sh` / `install.ps1` /
`go install …@v2.2.0`) are documented in [`docs/release.md`](docs/release.md).
The official plugin install path (`eka plugin install steward`) is in the same
document.
The release owner's step-by-step is
[`docs/release-checklist.md`](docs/release-checklist.md).

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
  vendored `eka-standard/templates/hooks` to symlink, so no `platform_sync`
  step exists in the anvil pipelines (`anvil.yaml` declares no lifecycle).
- **No `feedback` / MCP / telemetry surface.** Steward drives agents; it is
  not the knowledge CLI and is not an MCP server. The eka-cli plugin
  (`internal/plugin`) declares no `mcp`/`install` capability and installs no
  artifacts.
- **CI runs the minimal gate** (format, vet, windows cross-compile, race
  tests). eka-cli's installer e2e jobs and repository-scope `eka verify`
  gate are omitted: steward is not an EKA repository, and its installers
  are thin adaptations of eka-cli's already-covered ones.

## License

Apache-2.0 — see [`LICENSE`](LICENSE).
