# eka-steward boundary

This document states the two hard rules that keep `eka-steward` a
well-behaved consumer of EKA, and the policy behind the one place the
steward is allowed to leave its own process. Both rules are enforced by
tests — they are not conventions on the honour system.

## Rule 1 — `api` only; the steward never writes the EKA store

`eka-steward` is a **CONSUMER** of
[`eka-core`](https://github.com/maleolabs/eka-core/v2). Its only
supported contract is the public package
`github.com/maleolabs/eka-core/v2/api`.

The kernel-internal packages — `runtime`, `store`, `workspace`, `sync` —
are private implementation details of eka-core. They must **never**
appear in non-test source here, because importing them would let the
steward reach the EKA store directly and would couple it to internals
that can change without a contract bump.

Enforced by `cmd/boundary_arch_test.go`
(`TestNoInternalCoreImportsInNonTestCode`): every non-test `.go` file is
parsed and the test fails on any forbidden import. The pinned
`knownCoreGap` map is empty — the boundary is complete, and a stale
entry fails the test too.

Rationale: knowledge access belongs in eka-core. The steward orchestrates
work; it does not own a second write path to the store.

## Rule 2 — one escape-hatch adapter, with an allow-list

The one legitimate reason the steward leaves its own process is to run
another CLI (a coding agent, an `eka` command, a tool). Every such
subprocess call goes through the single adapter package
**`internal/escapehatch`**. No other non-test package may import
`os/exec`.

Enforced by `cmd/boundary_arch_test.go`
(`TestSubprocessCallsOnlyInEscapeHatchAdapter`): every non-test `.go`
file is parsed and the test fails if any file outside
`internal/escapehatch` imports `os/exec`. The adapter path is pinned, and
a stale pin (the adapter stops importing `os/exec`) fails the test.

Rationale: one adapter means the whole subprocess surface is small,
deterministic and reviewable in a single place — and a process spawn can
never quietly become a second path to the EKA store (Rule 1).

### Allow-list policy

`escapehatch.AllowList` is an explicit set of permitted executable
**base names**. A command is refused deterministically unless its base
name is in the list; an unknown command returns an error that names it
(`ErrNotAllowed`). There is **no default and no wildcard**: an empty
`AllowList` runs nothing. Callers name every command on purpose, so the
set of things the steward can execute is auditable at the call site.

The adapter additionally guarantees, by construction:

- **No shell.** The caller supplies an argv; the executable is `exec`'d
  directly (`exec.CommandContext`), never through `sh -c`. There is no
  interpolation, so shell metacharacters in arguments are inert.
- **No environment leakage.** Only the caller's `Env` reaches the child;
  a `nil` `Env` is an **empty** environment, not the parent's.
- **Cancellation is honoured.** A cancelled `context.Context` returns
  that context's error.
- **Deterministic result.** `Result` carries the exit code and the
  captured stdout/stderr; a clean non-zero exit is returned as data
  (exit code, no error), while a failure to start the process is an
  error.

## Not a dependency: MCP

The MCP surface is **not** a steward dependency. `go.mod` requires no
`eka-mcp`/MCP module, asserted by `cmd/boundary_arch_test.go`
(`TestNoMCPDependency`). See
[`docs/adr/0001-standalone-binary.md`](adr/0001-standalone-binary.md) for
why hosting the steward behind MCP was rejected as the driver.

## Where this is enforced

| Rule | Test |
| --- | --- |
| `api`-only, no kernel-internal imports | `TestNoInternalCoreImportsInNonTestCode` |
| subprocess calls only in `internal/escapehatch` | `TestSubprocessCallsOnlyInEscapeHatchAdapter` |
| no MCP module required | `TestNoMCPDependency` |
| adapter behaviour (allow-list, env, cancellation) | `internal/escapehatch/escapehatch_test.go` |
