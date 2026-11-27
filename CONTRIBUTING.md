# Contributing

Thank you for contributing to **eka-steward**, the standalone ACP
orchestrator for EKA projects. These rules keep the repository safe,
reviewable, and traceable.

## Branching model

Two long-lived branches:

| Branch | Purpose |
|---|---|
| `main` | Stable / release. Only ever updated from `develop` via a pull request. |
| `develop` | Development. All work happens here or on branches cut from here. |

## Development workflow

1. **Always branch from `develop`.** Use `feature/*`, `fix/*`, `refactor/*`,
   `docs/*`, or `chore/*` — never work directly on `main`.
2. **Merge to `main` via PR from `develop`.**

## Quality gate

Changes are delivered through the **anvil pipeline CI**:

```sh
anvil pipeline ci
```

The gate runs formatting, vet, windows cross-compile, and the race test
suite; a failing gate blocks merge. Manually:

```sh
gofmt -l .        # must print nothing
go vet ./...
go test ./...
```

## Layering rule (enforced by a test)

Steward is a consumer of eka-core. Production code may import
`github.com/maleolabs/eka-core/v2/api` and **nothing else** from eka-core —
`runtime`, `store`, `workspace` and `sync` are private. `go test` runs
`cmd/boundary_arch_test.go`, which fails on any forbidden import.

Keep it thin: no pre-built abstraction for an unwritten feature. The ACP
control loop, driver registry and ACP client are separate work items.

## Layout rule (enforced by a test)

The top-level layout is pinned in `cmd/layout_test.go`. Adding or removing a
top-level entry is a deliberate change: update the expected list in the same
PR and justify it.
