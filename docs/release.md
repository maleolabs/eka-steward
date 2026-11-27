# eka-steward release

This document is the release reference for the `eka-steward` binary: its
**release train**, the **version source**, the **pinned `eka-core/api`**
version and the compatibility statement, the **ACP transport posture**, the
**distribution/install path**, and the fact that the release is
**independent of the EKA line's release train**.

The step-by-step the release owner executes is
[`docs/release-checklist.md`](release-checklist.md). The paste-ready notes for
a given version are [`RELEASE_NOTES.md`](../RELEASE_NOTES.md).

## Independent release train

`eka-steward` ships on its **own tag namespace `v*`**, in its **own
repository** (`github.com/maleolabs/eka-steward`), driven by its **own**
[`.github/workflows/release.yml`](../.github/workflows/release.yml). It is
**not** coupled to the `eka-cli` / `eka-mcp` / `eka-core` release train:

- Tags are per-repository. `eka-steward`'s `v2.2.0` is a distinct object
  from any `v2.2.0` in `eka-cli`, `eka-mcp` or `eka-core`; there is no shared
  tag, no shared workflow, and no cross-repo tag trigger.
- The version is decided **here** (the git tag / workflow input), guarded
  against **this repo's** `anvil.yaml`. No other repository's version gates a
  steward release.
- The only cross-repo coupling at release time is a **source dependency**,
  not a release coupling: steward compiles against a **pinned** published
  `eka-core` module (below). Upgrading that pin is a normal dependency bump
  in this repo, independent of when the core/cli/mcp lines cut their own
  releases.

## Version source

| Where | Value | Role |
| --- | --- | --- |
| `anvil.yaml` → `project.version` | `2.2.0` | the project version; the workflow **guards** the release against it |
| git tag | `v2.2.0` | triggers the release workflow (tag push); the `v` is the tag spelling only |
| binary (`cmd.version`) | `2.2.0` | stamped at build via `-ldflags "-X github.com/maleolabs/eka-steward/v2/cmd.version=<ver>"` |
| `api.ContractVersion` | `v1` | the `eka-core` **contract** version the binary was compiled against (distinct axis — see below) |

The build stamps `cmd.version` from `git describe --tags --always` with the
leading `v` stripped, so the binary reports the same spelling as
`anvil.yaml` (`2.2.0`), not the tag spelling (`v2.2.0`). A default (unstamped)
build reports `dev`. See
[`.anvil/pipelines/build.yaml`](../.anvil/pipelines/build.yaml).

`scripts/bump.sh <patch|minor|major>` is the canonical bump: it runs the CI
gate, writes `anvil.yaml` via `anvil project version set`, commits, pushes,
tags `v<ver>` and pushes the tag — which triggers the release workflow.

## Pinned `eka-core/api`

`go.mod` pins the concrete module tag:

```
github.com/maleolabs/eka-core/v2 v2.0.1
```

- **Module path** `github.com/maleolabs/eka-core/v2` (the `/v2` major-version
  suffix is part of the import path).
- **API-only.** Production code may import **only**
  `github.com/maleolabs/eka-core/v2/api`. The kernel-internal packages
  (`runtime`, `store`, `workspace`, `sync`) are private implementation
  details and are forbidden in non-test code (ADR-014, ADR-030), enforced by
  `cmd/boundary_arch_test.go`, not on the honour system. Steward is a
  **consumer**: it holds no domain logic and never writes the store outside
  its single audited write path (`internal/ekawrite`).

### Compatibility statement

> **eka-steward `2.2.0` is compiled against and depends on
> `github.com/maleolabs/eka-core/v2 v2.0.1` (the `api` package only).** The
> steward build version (`cmd.version`, this release: `2.2.0`) and the
> `eka-core` public-contract version (`api.ContractVersion`, currently `v1`)
> are **separate axes**: the build version names the steward implementation,
> the contract version names the wire/contract surface of `eka-core`. Moving
> to a newer `eka-core` patch/minor within `v2` is a dependency bump; a `v3`
> of the contract would be a new steward major.

## ACP transport posture

The steward talks to coding-agent CLIs over the **Agent Client Protocol**
(ACP), JSON-RPC 2.0, one object per line, over the child's stdio. The
posture is fixed by the published ADR `adr:steward-acp-bridge` (drafted in
[`docs/acp-spike.md`](acp-spike.md)):

- **ACP v1 (`protocolVersion: 1`) is the shipping target**, behind a
  **per-connection version dispatcher**. The client sends the highest version
  it supports (initially `1`); if a future agent returns `2`, the dispatcher
  routes to a **v2 handler**.
- **v2 is additive**, per `v2_migration.md`. The steward does **not** request
  `2` until a v2 handler exists, because a `2` request against a v1 agent
  silently drops `clientCapabilities` (losing `embeddedContext`). v2 adoption
  is a handler swap, not a rewrite.
- **Driver adapters.** `opencode` (and optional `gemini`) are **ACP-capable**
  (they speak ACP v1 over stdio). `claude` (Claude Code) and `codex` are
  **CLI-only** — they do not speak ACP and are driven as plain command-line
  tools via a CLI adapter. See [`docs/drivers.md`](drivers.md).
- **Permission policy: default-deny, bounded wait.** Deny by default; allow
  `read`-class tool calls only inside the session workspace root; escalate
  `execute`/`edit`/`delete`/`move` to an explicit decision; never auto-select
  `allow_always`. Every permission wait is **bounded** (default 120 s,
  configurable); on expiry or `session/cancel` the steward answers
  `cancelled` so a worker never hangs. Unknown `optionId`/`outcome` is never
  treated as approval. Human escalation is the approval channel
  ([`docs/approval.md`](approval.md)); a code-escalated request is surfaced to
  a human, not silently denied.
- **Subprocess boundary.** The stdio **spawn** stays in the audited
  [`internal/escapehatch`](../internal/escapehatch/escapehatch.go) family —
  the **only** package in the repo that imports `os/exec` (enforced by test).
  The streaming-stdio adapter is `escapehatch.Start` (wired stdin/stdout pipes);
  no other package may spawn.

## Distribution / install path

Release assets follow a fixed **naming contract** the installers resolve:

```
eka-steward-<os>-<arch>            # posix: linux|darwin × amd64|arm64
eka-steward-<os>-<arch>.exe        # windows
install.sh  install.ps1            # staged into the release
SHA256SUMS.txt                     # checksums over every asset
<source artifact>                  # `anvil artifact package`
```

**Path A — installer (recommended for end users).** The release publishes
`install.sh` (POSIX) and `install.ps1` (Windows) as assets.

```sh
# Linux / macOS
curl -fsSL https://github.com/maleolabs/eka-steward/releases/download/v2.2.0/install.sh | sh
# pin a version / target dir:
curl -fsSL .../v2.2.0/install.sh | sh -s -- --version v2.2.0 --to ~/.local/bin
```

```powershell
# Windows (PowerShell 5.1+ / 7+)
$s = irm https://github.com/maleolabs/eka-steward/releases/download/v2.2.0/install.ps1
iex "$s -Version v2.2.0"
```

Verification is **fail-closed**: the installer requires `SHA256SUMS.txt` and
aborts on a missing checksum file or a mismatch — no binary is installed
unverified. Supported platforms: `install.sh` → linux amd64/arm64, darwin
amd64/arm64; `install.ps1` → windows amd64/arm64. (The release build also
emits windows amd64/arm64 `.exe` assets.)

**Path B — `go install`.**

```sh
go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.2.0
```

**Path C — source artifact.** The release attaches the `anvil artifact
package` tarball for offline/pinned builds.

There is deliberately **no `eka steward` shim** hosting the steward inside the
`eka` CLI, and no MCP host — see
[`docs/adr/0001-standalone-binary.md`](adr/0001-standalone-binary.md).

## Build proof (release workflow)

The workflow compiles the tagged tree with the CI gate green first, then
builds every platform binary with `anvil pipeline build --output
dist/binaries`, packages the source with `anvil artifact package`, stages the
installers, generates `SHA256SUMS.txt`, and creates the GitHub Release
(`gh release create`). The exact local invocations are:

```sh
export GOPROXY=off GOSUMDB=off
go build ./...                                              # whole tree
GOOS=windows GOARCH=amd64 go build ./...                    # the windows gate
go build -o eka-steward ./cmd/eka-steward                   # the binary (unstamped → "dev")
go build -ldflags="-X github.com/maleolabs/eka-steward/v2/cmd.version=2.2.0" \
    -o eka-steward ./cmd/eka-steward                        # stamped, as the release build does
```

`go install github.com/maleolabs/eka-steward/v2/cmd/eka-steward@v2.2.0`
requires the tag to exist — that tag is created by the release owner, not by
this document.
