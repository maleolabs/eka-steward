# eka-steward configuration

**The steward's configuration is ONE versioned, strict document.** The
`roles`, `drivers`, `sizing`, `sandbox` and `loop` sections live under a
single schema with a `version` field, validated by `internal/config`. A value
accepted here is accepted by the section's standalone parser and vice versa —
this package **composes** `internal/role`, `internal/driver` and
`internal/sizing` rather than restating their rules.

Config is **steward config**: it lives in a steward file and nowhere else, and
no section reads or writes EKA. `config.Parse` performs no filesystem,
network or subprocess I/O — the document bytes are passed IN.

## Schema

```yaml
# eka-steward config — one versioned document for every section.
version: 1                 # required for current-version docs; see Versioning

roles:                     # >= 1 role required
  - name: implementer      # unique; non-empty
    driver: opencode       # must reference a declared driver
    template: prompts/implementer.md
  - name: reviewer
    driver: claude
    template: prompts/reviewer.md
    review: true           # optional; true marks a review role

drivers:                   # the declared tools; kind is derived, not configured
  - tool: opencode         # must name a known tool (an adapter exists)
    cmd: opencode
    args: [acp]            # optional argv
    version: 2.4.1         # optional pin (guard.RequirePinned)
    checksum: <sha256 hex> # optional pin
  - tool: claude
    cmd: claude

sizing:                    # worker-count policy (internal/sizing)
  maxWorkers: 4            # hard upper bound; must be >= 1
  costCeiling: 2           # optional; 0 means "no ceiling"

sandbox:                   # containment; ON BY DEFAULT, no opt-out
  worktreeBase: /var/lib/eka-steward/worktrees   # optional; absolute path

loop:                      # operator-owned control-loop scalars
  maxAttempts: 3           # total dispatch attempts (>= 0; 0 => clamped to 1)
  queueLimit: 8            # bounded-pool admission (>= 0; 0 => sequential)
  attemptTimeout: 30s      # per-attempt deadline string (>= 0; 0 => none)
```

| Section | Field | Rule |
| --- | --- | --- |
| `roles` | `name` | unique, non-empty |
| | `driver` | non-empty, must resolve to a declared driver |
| | `template` | non-empty |
| | `review` | optional bool |
| `drivers` | `tool` | unique, non-empty, must have a registered adapter |
| | `cmd` | non-empty |
| | `args` | optional argv |
| | `version` | optional pin |
| | `checksum` | optional sha256 pin |
| `sizing` | `maxWorkers` | required, `>= 1` (via `internal/sizing`) |
| | `costCeiling` | optional; `0` means "no ceiling" |
| `sandbox` | `worktreeBase` | optional; if set, must be an absolute path |
| `loop` | `maxAttempts` | `>= 0` (`0` is clamped to `1` by the loop) |
| | `queueLimit` | `>= 0` (`0` runs the sequential path) |
| | `attemptTimeout` | duration `>= 0` (`0` means no per-attempt deadline) |

`drivers[].kind` is **not** configurable: the wire kind (`acp` / `cli`) is
derived from the tool's registered adapter. A `kind` key is an unknown field
and is refused.

## Strictness

Parsing is strict — `yaml.Decoder.KnownFields(true)`. An unknown field
**anywhere** — top level, inside a role, a driver, `sizing`, `sandbox` or
`loop` — is refused. This is also what makes the sandbox opt-out impossible:
there is deliberately no `disable` / `bypass` field, so such a key is rejected
as unknown (the same construction `internal/guard` uses).

Refused with a deterministic, field-naming error:

- unknown fields, at every level;
- malformed YAML and an empty/blank document;
- a role with an empty `name` / `driver` / `template`, or a duplicate name;
- a driver with an empty `tool` / `cmd`, a duplicate tool, or a tool with no
  registered adapter;
- **no roles** (`roles: []` or absent) — a config must declare at least one;
- a role whose `driver` is not a declared driver;
- `sizing.maxWorkers < 1`;
- a `sandbox.worktreeBase` that is not absolute;
- a negative `loop` value, or a malformed duration.

Ordering is fixed (sizing, sandbox, loop, roles, drivers, at-least-one-role,
driver resolution), so a multi-fault document yields the same first error
every time. `Parse` is a pure function of its bytes — it iterates no map to
produce its result, so the same input always yields the same output.

## Versioning

The current schema version is `1` (`config.Version`). The version gate is
`Migrate`:

- `version: 1` — validated as-is.
- **absent `version`** — decodes as `0` and is migrated (documented default);
  unlike a stamped current document, it need not carry `sizing`.
- `version: 0` — the pre-schema document (roles + drivers only); migrated to
  `1` (see below).
- any **future or unknown** version (`2`, `99`, `-1`, ...) — refused
  deterministically, never silently accepted.

### Migration path

| From | To | Change |
| --- | --- | --- |
| `0` | `1` | `sizing` was added in v1. A v0 document has no `sizing`, so migration sets `sizing.maxWorkers` to the **conservative floor of 1** (never a guessed higher count). `sandbox` and `loop` were also added in v1; their zero values are already correct (contained, no-op), so nothing is added for them. The explicit `version` stamp is set to `1`. |

An explicit `sizing.maxWorkers` already present is preserved by the migration;
only a missing (`0`) value gains the default. Migrations are applied in order
from the document's version up to the current one, and `Migrate` is
idempotent for a current-version document.

To add a future `n -> n+1` change: bump `config.Version`, add a
`migrateV<n>toV<n+1>` step to the `Migrate` loop, document the row above, and
add a `version: <n>` fixture plus a migration test. The refusal of a version
newer than `config.Version` is what keeps a newer document from being read by
an older steward.

## Verified by

| Property | Test (`internal/config/config_test.go`) |
| --- | --- |
| golden fixture, every section populated, canonical order | `TestParseValidGolden` |
| strict + per-section refusals, field-naming errors | `TestParseRefusesStrictly` |
| version gate (future/unknown refused, deterministically) | `TestParseVersionGate` |
| v0 -> v1 migration (explicit and defaulted version) | `TestMigrateV0ToCurrent` |
| `Migrate` direct refusals / idempotence | `TestMigrateRejectsUnsupported` |
| `Parse` is a pure function of its bytes | `TestParseDeterministic` |
| sandbox contained by default, no opt-out | `TestSandboxContainedByDefault` |

Fixtures: [`internal/config/testdata/valid.yaml`](../internal/config/testdata/valid.yaml),
[`internal/config/testdata/v0.yaml`](../internal/config/testdata/v0.yaml).
