# ADR-0001 — Ship eka-steward as a standalone binary (no `eka steward` shim)

- Status: Accepted
- Date: 2026-11-27
- Deciders: EKA Steward maintainers

> **Superseded in part (v2.1.0).** The "no `eka steward` shim is built in this
> line" decision below is amended by `adr:steward-as-eka-cli-plugin`: steward
> now ships as an **official eka-cli plugin** that *dispatches the standalone
> binary as a subprocess*. The binding constraint here — no deadline-bounded
> **in-process** host for the loop, and any delegation must be a deadline-free
> subprocess — still holds and is honored by the shipped plugin.

## Context

`eka-steward` orchestrates AI coding-agent CLIs over ACP to drive an EKA
project from discussion to execution. It is a long-running supervisor: it
spawns and monitors child agent processes, exchanges streaming ACP traffic,
and keeps control-loop state across a session.

The `eka` CLI (eka-cli) exposes knowledge operations and hosts plugins
through a plugin/proxy surface. The natural temptation is to expose steward
as an `eka steward` subcommand (a plugin or proxy entry) so users discover
it next to every other EKA command.

Two host constraints make that surface a poor fit for a supervising loop:

1. **Deadline-bounded host.** The plugin/proxy entrypoint is bounded by a
   request/response deadline. A control loop that outlives a single request
   (waiting on agent turns, retrying, coordinating) exceeds that budget.
2. **Minimal environment.** The host runs with a deliberately minimal
   environment. A long-running orchestrator needs a fuller environment
   (child process management, working-directory control, terminal/session
   handling) than a knowledge command host provides.

Hosting steward behind MCP was considered and rejected as the **driver** for
the same reason: a request/response tool host is the wrong shape for a
supervising loop, not a limitation of MCP itself.

## Decision

Steward ships as a **standalone binary** as its only product surface:

- `go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.0.0`
- release binaries via `install.sh` / `install.ps1`

No `eka steward` shim is built in this line. `eka-cli` remains the escape
hatch for one-shot, in-CLI knowledge operations; steward owns the
long-running orchestration loop.

## Consequences

- Steward can be a plain OS process: it controls its own lifetime, signals,
  stdin/stdout and child processes without a host deadline.
- Discovery is one extra install step (`go install` / installer) rather than
  riding `eka`'s command tree. Documented in the README.
- The two binaries can evolve on independent release cadences, coupled only
  through eka-core's versioned `api` contract.
- If a shim is ever wanted, it must delegate to the standalone binary as a
  subprocess and impose no deadline on it — it cannot host the loop
  in-process.
