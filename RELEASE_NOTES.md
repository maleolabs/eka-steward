# eka-steward v2.2.1

Thin, deterministic **standalone binary** that orchestrates AI coding-agent
CLIs over the [Agent Client Protocol](https://agentclientprotocol.com) (ACP)
to drive an EKA project from discussion to execution. A **consumer** of
`eka-core`: it imports only the public `api` contract and never writes the
store outside its single audited write path.

Module: `github.com/maleolabs/eka-steward/v2`

## Compatibility (pinned `eka-core`)

- Compiled against and depends on **`github.com/maleolabs/eka-core/v2
  v2.0.1`**, **`api` package only** (module path carries the `/v2` major
  suffix). Kernel-internal packages (`runtime`, `store`, `workspace`, `sync`)
  are forbidden in non-test code, enforced by test.
- Steward build version (`2.2.1`) and the `eka-core` public-contract version
  (`api.ContractVersion`, currently `v1`) are separate axes. `eka-steward
  version` prints both.

## Fixed in `2.2.1` (themed help + output container)

`2.2.0` themed `run` and `version` but left the help surface as raw cobra
output (no margin, no color). `2.2.1` completes the alignment:

- **Themed help** (`cmd/help_render.go`) — every command's help renders inside
  the output container (`internal/ui/container.go`: uniform left margin +
  leading/trailing blank line) with command names in info, flag names and the
  help hint in dim — mirroring `eka`'s help renderer.
- **`--version` through the margin** — the bare build version prints
  margin-indented, matching `eka --version`.
- **No duplicate command list** — the root `Long` drops its hand-written
  command block; cobra's rendered `Available Commands:` is the single source.
- Non-TTY output stays plain text with no ANSI (colors only on a color TTY);
  `run --json` and the machine surface are untouched.

## New in `2.2.0` (design-system-aligned CLI output)

`2.2.0` makes `eka steward ...` look like `eka` and makes the machine report
explicit:

- **Design-system output** — a new `internal/ui` package mirrors the eka-cli
  `cmd/ui` tokens verbatim (deliberate duplication: steward is a separate,
  api-only module and must not import eka-cli): the two-space left margin, the
  ANSI 256-color palette (info/success/warning/error/dim), the Unicode icon set
  and the aligned `Header`/`Table`/`Summary` renderers. `cmd/render.go` renders
  the `run` human report through it.
- **`run` human report by default** — a themed context header, the STEPS and
  WRITES tables and a `Summary:` block instead of raw JSON.
- **`run --json`** — the deterministic machine report (`obs.Report.Write`),
  **byte-identical** to the pre-`2.2.0` output, written on the raw (unmargined,
  uncolored) writer. The machine surface is unchanged.
- **Determinism preserved** — colors only on a TTY when `NO_COLOR` is unset and
  `TERM != "dumb"`; non-TTY output is plain text plus UTF-8 icons, byte-stable,
  with no ANSI escapes. TTY detection is the stdlib char-device check (no
  `golang.org/x/term`).

## New in `2.1.0` (runtime surface)

`2.1.0` closes the gap between the `2.0.0` orchestrator core and a usable,
consumable surface:

- **`run`** (`eka/sto:steward-run-command`) — wires the control loop into a CLI
  command: assembles the production seams (config, `api.Ensure`, the read-only
  Source, the one write path, the ACP dispatcher, the Recorder, the TTY-free
  approver), drives the project to quiescence under the single-instance lock,
  and prints a run report. Resume happens inside `run`.
- **Streaming ACP client** (`eka/sto:acp-streaming-client`) — `internal/acp`:
  JSON-RPC 2.0 over the agent's stdio, per-connection version dispatch
  (v1 shipping, v2 additive), `session/update` streaming, permission routing
  through `guard.Policy` with bounded approval escalation (default-deny),
  deterministic cancel, and `fs/read` containment against a symlink-resolved
  workspace root. Production streaming spawn (`Start`/`Proc`) lives in the one
  audited adapter `internal/escapehatch`.
- **Machine protocol** (`eka/sto:steward-protocol`) — `internal/protocol`: a
  versioned, language-agnostic JSON-RPC 2.0 server over stdio by default or a
  local unix socket (`serve --socket`). Methods: initialize, plan, step, run,
  resume, cancel, approve, status, shutdown; idempotency keys; deterministic
  error codes; framing mirrors eka-mcp (line cap, resync, flush-per-reply).
- **Event stream** (`eka/sto:steward-event-stream`) — `internal/obs/stream.go`:
  `internal/obs` exposed as live protocol notifications (`events.subscribe` /
  `events.unsubscribe`). Ordered, monotonic, gap-free `seq`; bounded
  per-subscriber buffer with a single deterministic `gap` event on overflow;
  redaction on every payload.
- **Protocol specification** (`eka/sto:steward-protocol-docs`) —
  [`docs/protocol.md`](docs/protocol.md): transport/framing, lifecycle and
  version negotiation, the 11-method table, the 6 event types, the 13 error
  codes, concurrency/cancel, idempotency, determinism guarantees, a stdlib-only
  Python full-cycle integration guide, and the pinned golden transcripts.
- **Official eka-cli plugin** (`eka/sto:steward-cli-plugin`) — `internal/plugin`
  plus `cmd/manifest.go` / `cmd/install.go`: the plugin contract-v1
  self-description (`manifest --json`) and a deterministic no-op
  `install --json`. eka-core v2.1.0 adds the official-registry entry
  `steward → maleolabs/eka-steward` and eka-cli registers the `steward`
  command, so `eka steward <args>` dispatches to `eka-steward <args>`.
- **Hardened credential source** (`eka/sto:steward-credential-source`) —
  `internal/secrets/file.go`: a `StateDir`-based dotenv resolver opened under a
  hardening contract (no symlink, regular file, current-uid owner, mode `0600`,
  64 KiB cap), wired to the broker as the production source so steward runs
  under the plugin host's minimal environment. `EKA_HOME` was added to the
  plugin environment whitelist in both eka-core and eka-cli.

## Distribution: official eka-cli plugin

Alongside the standalone binary, steward is an **official eka-cli plugin** — no
system PATH install:

```sh
eka plugin install steward     # resolves the official registry entry, checksum-verified
eka steward <args>             # dispatches to eka-steward <args>
```

- **Install** is fail-closed against the release `SHA256SUMS.txt`; the binary
  hash is **re-verified on every dispatch** (anti-TOCTOU).
- **Dispatch** inherits the CLI's stdin/stdout/stderr, the plugin owns its flags
  (`DisableFlagParsing`), and the exit code propagates.
- The host runs the binary under a **bounded environment whitelist**
  (`plugin.PluginEnv()`: `PATH`, `HOME`, `EKA_PLUGIN_DIR`, `EKA_HOME`,
  `SystemRoot`). Command dispatch adds **no deadline** (only the manifest/install
  contract probes are bounded), so both one-shot `run` and long-running `serve`
  dispatch correctly.
- The standalone binary remains the **primary artifact**; the plugin is a
  distribution/dispatch adapter over it, not an in-process host
  (`adr:steward-as-eka-cli-plugin`, amending `docs/adr/0001-standalone-binary.md`).

## Deferred (not shipped)

- **OS keyring / peer-authenticated IPC credential delivery** (option B/C of
  `adr:steward-as-eka-cli-plugin`) is **not implemented**. The hardened
  file-backed source (option A) is the accepted v1 design; the follow-up
  (`eka/sto:steward-credential-keyring`) is **canceled, not retired**, so it
  stays re-activatable. It is off the critical path and would add cross-platform
  keyring/IPC dependencies beyond steward's stdlib-only boundary.

## ACP transport posture

- **ACP v1 (`protocolVersion: 1`) is the shipping target**, behind a
  **per-connection version dispatcher**; **v2 is additive** (routed to a v2
  handler when/if an agent returns `2`). The steward does not request `2`
  until a v2 handler exists.
- Adapters: `opencode`/`gemini` speak **ACP v1**; `claude`/`codex` are
  **CLI-only** (driven as plain CLIs, no ACP).
- **Permission: default-deny, bounded wait** (default 120 s → deny/cancel).
  Escalations surface to the human approval channel, never silently denied.
- **Subprocess spawn confined to the audited `internal/escapehatch`** — the
  only `os/exec` importer in the repo.

## 2.0.0 core (carried over)

Highlights of the underlying orchestrator core (container `ctr:steward` / M7):

**Foundation & boundary**
- `steward-scaffold` — repo from eka-cli conventions: layout, `cmd`
  entrypoint, CI, license, release pipeline; standalone-binary invocation.
- `steward-facade-boundary` — eka-core as a Go library (`api` only), CLI as
  the escape hatch for not-yet-librarized ops, MCP rejected; steward is the
  sole store writer.
- `eka-write-path` — the **one** EKA write path (`internal/ekawrite`): single
  audited mutation surface, deterministic attribution, replay-safe
  (no-op-on-replay), ADR-032 provenance/adopt semantics.
- `determinism-spec` — code decides **which/when/if**; the LLM decides
  **what**, only inside ACP worker sessions.

**Planning, roles & sizing**
- `work-selection-ordering` — deterministic next-step selection: topological
  over `depends-on`, container order, `planned → todo` adjacency, blocked
  rule; no LLM.
- `context-renderer` — bounded, byte-stable context payload (hard size
  budget, stable order); read-only over EKA.
- `worker-role-model` — roles as steward config binding role → driver →
  prompt; roles never touch EKA and never decide control flow.
- `worker-sizing-policy` — `clamp(min(cores/2, ramGiB/4), 1, MaxWorkers)`
  plus a cost-aware cap; cgroup-aware; code-owned.

**Control loop & drivers**
- `control-loop` — single-instance workspace lock, read-only EKA, code-computed
  next step, ACP worker dispatch, results recorded via the write path;
  resumable and deterministic.
- `driver-registry` — `{tool, cmd, args}` drivers + opencode/claude/codex
  adapters, capability/version probing, deterministic selection.
- `driver-authoring-trust` — known driver requires a pinned exact version +
  checksum; unknown/unpinned refused (fail-closed); vendor CLI pins.
- `acp-client-spike` — live v1/v2 probe + permission-callback evidence behind
  the ACP bridge ADR (see ACP posture above).

**Discussion, review & approval**
- `discuss-phase` — code-orchestrated structured discussion; decisions
  recorded as EKA knowledge for later phases.
- `review-roles` — independent reviewer before completion; selection
  code-driven, outcomes persisted.
- `steward-human-approval` — code policy cannot decide → surface a structured
  approval prompt to a human, record the decision as a typed artifact, resume
  deterministically.

**Resilience & upgrade**
- `checkpoint-resume` — atomic (temp+fsync+rename) checkpoints outside EKA;
  deterministic resume without duplicate writes; intent→result reconcile.
- `cancel-retry-recovery` — deterministic session cancel, bounded retry,
  recovery without corrupting EKA state.
- `steward-upgrade-migration` — migrate checkpoints + config across releases;
  behaviour of an in-flight run and of reverting steward on applied writes.

**Safety & operations**
- `role-guardrails` — real containment: per-worker scratch HOME, OS-level FS
  confinement, per-worker git worktree (VCS isolation, **not** a security
  boundary), default-deny ACP permission with timeout⇒deny.
- `steward-secrets-broker` — per-driver least-privilege credential injection
  (never `os.Environ()`), scoped git auth, egress policy.
- `worker-pool-backpressure` — bounded concurrency + queue-depth limits;
  deterministic saturation behaviour.
- `steward-observability` — structured step log, per-step metrics, deterministic
  run report; auditable without reading model output.
- `config-schema-versioning` — one versioned, strict config schema
  (roles/drivers/sizing/sandbox/loop) + documented migration.
- `steward-release-2-0` — the `2.0.0` release: own tag train, notes, binaries,
  compatibility statement.

## Install

```sh
# Linux / macOS — checksum-verified (fail-closed) installer
curl -fsSL https://github.com/maleolabs/eka-steward/releases/download/v2.2.1/install.sh | sh

# Windows (PowerShell 5.1+ / 7+)
$s = irm https://github.com/maleolabs/eka-steward/releases/download/v2.2.1/install.ps1
iex "$s -Version v2.2.1"

# or from source
go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.2.1

# or as an official eka-cli plugin (no PATH install)
eka plugin install steward
```

Assets: `eka-steward-<os>-<arch>[.exe]` (linux/darwin/windows × amd64/arm64),
`install.sh`, `install.ps1`, `SHA256SUMS.txt`, and the source artifact.

## Independent release train

Ships on its **own `v*` tag namespace** in `github.com/maleolabs/eka-steward`,
with its own release workflow. This tag/release is **not** coupled to the
`eka-cli` / `eka-mcp` / `eka-core` release train; the only cross-repo link is
the pinned **source dependency** on `eka-core/v2 v2.0.1`.

## Also of note

- `eka-steward version` prints `eka-steward 2.2.1` and `eka-core api v1`; a
  plain (unstamped) build prints `dev`.
- The **live ACP client now ships** (`internal/acp`, standard library only). It
  is driven by the production dispatcher; the audited `internal/escapehatch`
  adapter provides the streaming spawn. The non-Go probe harness under
  `internal/acp/probe/` remains the spike record
  ([`docs/acp-spike.md`](docs/acp-spike.md)).
- **Same binary, two surfaces:** `run` drives a project to quiescence; `serve`
  hosts the same engine over the versioned machine protocol for any-language
  consumers.

Full reference: [`docs/release.md`](docs/release.md) ·
[`docs/release-checklist.md`](docs/release-checklist.md) ·
[`docs/protocol.md`](docs/protocol.md).
