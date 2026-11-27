# eka-steward drivers

**Drivers are steward config; no driver reads or writes EKA.**

A *driver* is how the steward launches and talks to one coding-agent CLI.
It is a steward-side configuration record — never an EKA knowledge object —
and it resolves **deterministically** to a launch spec (`cmd` + `args`) plus
the wire **kind** the steward uses to talk to it. Drivers live in a steward
config file and nowhere else. The package (`internal/driver`) parses and
resolves those bytes; it does not run a worker session or implement the
streaming ACP client — those are separate work items.

## Config shape

The document has a single `drivers` list. Parsing is **strict**. A driver is
declared as exactly `{tool, cmd, args}`; the wire kind is **not** config — it
is derived from the tool's registered adapter.

```yaml
# driver config — steward-owned, never EKA knowledge
drivers:
  - tool: opencode          # must name a known tool (an adapter exists)
    cmd: opencode
    args: [acp]             # `opencode acp` speaks ACP on stdio
    version: 2.4.1          # optional pin (guard.RequirePinned)
    checksum: <sha256 hex>  # optional pin (guard.RequirePinned)

  - tool: claude            # Claude Code — no ACP, CLI adapter
    cmd: claude

  - tool: codex             # Codex — no ACP, CLI adapter
    cmd: codex
```

| Field | Required | Meaning |
| --- | --- | --- |
| `tool` | yes | unique tool name; must have a registered adapter; non-empty |
| `cmd` | yes | executable base name (checked against the escape-hatch allow-list at run time); non-empty |
| `args` | no | argv passed to `cmd` verbatim |
| `version` | no | exact tool version (a **pin**: `guard.RequirePinned` refuses a driver without it) |
| `checksum` | no | sha256 (lowercase hex) of the resolved binary (a **pin**: `guard.RequirePinned` verifies it) |

## Adapter kinds

Every known tool maps to a fixed adapter that decides **how the steward
talks to it** and **how it is probed**. The kind is copied onto `Spec.Kind`,
so the difference is explicit and machine-checkable.

| Tool | Kind | Probe | Notes |
| --- | --- | --- | --- |
| `opencode` | `acp` | `ACPProbe` — one v1 `initialize` round-trip | `opencode acp`; negotiates ACP **v1** on the wire |
| `gemini` | `acp` | `ACPProbe` | optional extra; `gemini --acp`; negotiates ACP v1 |
| `claude` | `cli` | `CLIProbe` — `claude --version` | Claude Code does **not** speak ACP |
| `codex` | `cli` | `CLIProbe` — `codex --version` | Codex does **not** speak ACP |

`opencode`/`gemini` are ACP-capable (they speak JSON-RPC over stdio);
`claude`/`codex` are CLI-only and are driven as plain command-line tools. See
[`docs/acp-spike.md`](acp-spike.md) for the live handshakes.

## Resolution and selection (deterministic)

Resolution is a pure function of the config bytes — same input, same output.

- `Parse(data []byte) (*Registry, error)` decodes and validates strictly,
  then sorts drivers by `tool`. The returned `Registry` is immutable.
- `Registry.Names()` lists every tool in deterministic (sorted) order.
- `Registry.Resolve(tool) (Spec, error)` returns the launch spec, or an
  error naming an **unknown tool**.
- `Registry.Select(tool) (Driver, bool)` is the deterministic selection:
  the same tool always yields the same spec; an unknown tool returns
  `false` (no error).
- `Registry.Has(tool) bool` satisfies `role.DriverLookup`, so
  `role.Set.ValidateDrivers` can check role drivers against the registry
  without importing it.

## Capability probing (recorded)

`Registry.Probe(ctx, tool, run)` runs the tool's adapter and returns a
structured `Capabilities`

```go
type Capabilities struct {
	Kind       Kind              // acp | cli
	Version    string            // tool/agent version, if reported
	ACPVersion int               // ACP protocolVersion; 0 for a CLI adapter
	Raw        map[string]string // flattened probe detail (deterministic)
}
```

`Registry.SelectProbed(ctx, tool, run)` resolves the driver, probes it and
returns a `Driver{Spec, Caps}` with the capabilities **stored on the resolved
driver**. For ACP, `Raw` is the agent's `agentCapabilities` flattened to
`dotted.key = "value"` (sorted, so identical input → identical map). For a
CLI adapter, `Version` is the first non-empty line of `--version`.

### The `Runner` seam

Probing never spawns directly. It goes through one small interface:

```go
type Runner interface {
	Run(ctx context.Context, spec Spec) ([]byte, error)
}
```

- **Unit tests** pass a fake `Runner` (canned stdout, no subprocess), so the
  probe parse is testable in isolation. The ACP test replays the exact
  `initialize` frame captured from `opencode acp` in the spike.
- **Production** wires `ExecRunner` (`NewExecRunner(cmds...)` /
  `Registry.Runner()`), a thin adapter over [`internal/escapehatch`](boundary.md)
  whose allow-list is exactly the declared driver `cmd` values. This is the
  **only** file in `internal/driver` that imports `escapehatch`; the pure
  core (`registry.go`) imports no effectful package and the probe parse
  (`probe.go`) imports no subprocess package. A purity test enforces both.

## Unknown-tool behaviour

An unknown tool is a deterministic, named failure. `Parse` rejects a config
that declares a tool with no adapter (`driver: unknown tool "ghost"`);
`Resolve` and `Probe`/`SelectProbed` return an error wrapping
`ErrUnknownTool` that names the tool; `Select` returns `false`. Test with
`errors.Is(err, driver.ErrUnknownTool)`.

## Strictness rules

`Parse` rejects, with a deterministic error:

- unknown fields (top-level or inside a driver) — via `KnownFields(true)`;
- a field named `kind` (the kind is derived, not configured);
- a driver with an **empty `tool`**;
- a driver with an **empty `cmd`**;
- **duplicate tool** names;
- a tool with **no registered adapter**;
- malformed YAML, and empty/blank input.

An empty driver list (`drivers: []`) is a valid empty registry.

## Relation to the streaming ACP client (deferred)

`ACPProbe` is a **one-shot** capability probe: it reads the agent's single
`initialize` response and stops. It is **not** the streaming ACP client.

The `Runner` returns whole captured stdout, and `internal/escapehatch.Run`
has no stdin pipe yet, so the real `initialize` request cannot be fed to
`opencode acp` through `ExecRunner` today. That is why the ACP probe is
implemented and tested against the `Runner` seam (fake runner), and real
subprocess probing in this work item is limited to CLI adapters
(`claude --version`, `codex --version`), which need no stdin. The streaming
ACP client — a real stdio pipe, permission round-trips, prompt turns — is a
**later work item** (see the ADR draft in [`docs/acp-spike.md`](acp-spike.md)
§11) and will add the audited stdin-bearing adapter there.
## Trust model — KNOWN vs UNKNOWN, fail-closed

A driver is **KNOWN** only when it carries **both** an exact `version` and a
sha256 `checksum` (the pin). Anything else is **UNKNOWN** and is **refused**
at the acceptance points — this is fail-closed: a driver cannot reach the
loop unless it is KNOWN.

`driver.Spec.Trust()` is the pure classification (`TrustKnown` /
`TrustUnknown`); it hashes nothing. Verification has two layers, matching the
two things that must hold:

| Layer | Package | What it checks | I/O? |
| --- | --- | --- | --- |
| Structural | `driver.RequireKnown` / `driver.ErrUntrusted` | both pin fields present | none (pure) |
| Binary | `guard.RequirePinned` / `guard.VerifyBinary` / `guard.ErrUnpinned` | resolved binary's sha256 matches the checksum | os + PATH (no `os/exec`) |

**Acceptance points (where a driver is accepted, so an unpinned one cannot
reach the loop):**

- **Registry build** — `driver.ParseTrusted(data)` parses *and* enforces the
  KNOWN rule (`RequireKnown`); `(*Registry).RequireKnown()` is the registry
  form. `driver.Parse` is unchanged: pinning stays an **acceptance gate, not a
  parse rule** (a permissive `Parse` still accepts an unpinned driver so the
  standalone parser and `internal/config` agree).
- **Config load** — `config.ParseTrusted(data)` parses *and* enforces the
  KNOWN rule; `(*config.Config).RequireKnownDrivers()` is the config form.
  `config.Parse` is likewise unchanged.

The refusals are deterministic and name the tool:

```
driver: driver not pinned: tool "claude" has no pin: an exact version and a checksum are required
guard: driver not pinned: tool "claude" checksum mismatch: got <hex> want <hex>
driver: unknown tool "ghost"
```

`errors.Is(err, driver.ErrUntrusted)` for the structural refusal,
`errors.Is(err, guard.ErrUnpinned)` for the binary refusal,
`errors.Is(err, driver.ErrUnknownTool)` for an unregistered tool.

## Vendor CLI adapters — pin and audit

`claude` (Claude Code) and `codex` (Codex) are CLI adapters. Their pin/audit
record is the in-repo table `driver.VendorAdapters()`
(`internal/driver/vendorpin.go`): each entry pins the **exact vendor CLI
version** and the adapter's **dependency set** (module + exact version, no
ranges).

> **Honest boundary — pinned vs asserted.** This change is **offline**
> (`GOPROXY=off`); the two vendor CLI versions and their dependency versions
> are **documented placeholders** (`VendorAdapter.Verified == false`), not
> network-resolved. Nothing here claims a network-verified pin: the test
> `VendorAdaptersAreHonestlyMarked` fails if a placeholder is marked
> `Verified`. Upgrade path: replace each placeholder with the version printed
> by `<cmd> --version` for the pinned release and flip `Verified` to `true` in
> the same reviewed change (the `ponytail:` comment names this).

| Tool | CLI | Pinned CLI version | Dependencies (all pinned) | Verified |
| --- | --- | --- | --- | --- |
| `claude` | Claude Code | `0.0.0-unresolved` (placeholder) | `claude`, `@agentclientprotocol/sdk` | **no** |
| `codex` | Codex | `0.0.0-unresolved` (placeholder) | `codex`, `@agentclientprotocol/sdk` | **no** |

The table is covered by `internal/driver/vendorpin_test.go`: it asserts the
adapter set is exactly `{claude, codex}`, every CLI adapter has a record,
every dependency is pinned, the table is returned as a copy, and no
placeholder claims to be verified.

## Capability probe artifact — `capabilities.json`

Probing a driver produces `Capabilities` (above). The recorded **artifact**
is a fixed, deterministic `capabilities.json`, built by
`driver.BuildCapabilities(reg, probed, probedAt)` and serialized by
`driver.WriteCapabilities` (bytes in / bytes out — the driver package does no
file I/O). `driver.ReadCapabilities` parses and validates it.

Schema (`CapabilitiesSchema = 1`):

```jsonc
{
  "schema": 1,
  "drivers": [
    {
      "tool": "opencode",          // driver key (records sorted by it)
      "version": "2.4.1",          // PROBED version, if reported
      "pinnedVersion": "2.4.1",    // required pin (Spec.Version)
      "checksum": "<sha256 hex>",  // required pin (Spec.Checksum)
      "protocol": "acp/v1",        // "acp/vN" or "cli"
      "capabilities": [            // flattened probe detail, sorted "key=value"
        "loadSession=true"
      ],
      "probedAt": "2026-01-01T00:00:00Z" // caller-injected; omitted when empty
    }
  ]
}
```

**Determinism.** Records are sorted by `tool`; `capabilities` entries are
sorted; there is no map in the payload and no wall clock — `probedAt` is
**passed in** (an empty value is omitted), so identical inputs yield
byte-identical output. A golden test pins `internal/driver/testdata/
capabilities.golden` and asserts byte-identity across runs. Regenerate with
`UPDATE_GOLDEN=1 go test ./internal/driver -run TestCapabilitiesGolden`.

**Probing flow.** `Registry.Probe` / `SelectProbed` run a driver's adapter
through the `Runner` seam (fake in tests; `ExecRunner` in production, the only
`escapehatch` caller). `BuildCapabilities` requires a probe result for
**every** declared driver, so the artifact records the whole trusted set — no
process is spawned outside `internal/escapehatch`.
