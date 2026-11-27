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
make it an orchestrator — the ACP **control loop**, the **driver registry**
and the **ACP client** — are separate work items and are deliberately
**absent** here. No abstraction is pre-built for them.

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
docs/adr/                architecture decision records
docs/boundary.md         the two hard boundary rules (api-only, one escape hatch)
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
